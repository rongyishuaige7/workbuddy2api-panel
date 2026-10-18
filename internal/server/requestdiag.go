package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

const (
	diagnosticSessions = 128
	diagnosticMessages = 512
	diagnosticTail     = 8
	diagnosticTTL      = 30 * time.Minute
)

type requestSnapshot struct {
	shape    reqlog.RequestShape
	bytes    int
	tools    string
	settings string
	stamps   []string
	started  time.Time
	id       string
}

type requestBaselines struct {
	first, last requestSnapshot
	touched     time.Time
}

type requestDiagnostics struct {
	mu        sync.Mutex
	key       [32]byte
	baselines map[string]requestBaselines
}

func newRequestDiagnostics() *requestDiagnostics {
	d := &requestDiagnostics{baselines: make(map[string]requestBaselines)}
	if _, err := rand.Read(d.key[:]); err != nil {
		return nil // Diagnostics are optional; never emit unkeyed content fingerprints.
	}
	return d
}

func (d *requestDiagnostics) fingerprint(raw []byte) string {
	h := hmac.New(sha256.New, d.key[:])
	_, _ = h.Write(raw)
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func (d *requestDiagnostics) snapshot(body []byte, id string, started time.Time) *requestSnapshot {
	if d == nil {
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil || obj == nil {
		return nil
	}
	var messages, tools, functions []json.RawMessage
	if json.Unmarshal(obj["messages"], &messages) != nil {
		return nil
	}
	_ = json.Unmarshal(obj["tools"], &tools)
	_ = json.Unmarshal(obj["functions"], &functions)
	s := &requestSnapshot{bytes: len(body), started: started, id: id}
	s.shape = reqlog.RequestShape{Fingerprint: d.fingerprint(body), Messages: len(messages),
		Tools: len(tools) + len(functions), Roles: make(map[string]int)}
	// Length-prefixed JSON arrays keep tools/functions in separate namespaces.
	toolJSON, _ := json.Marshal([]json.RawMessage{defaultJSON(obj["tools"]), defaultJSON(obj["functions"])})
	s.tools = d.fingerprint(toolJSON)
	settings := make(map[string]json.RawMessage)
	for _, key := range []string{"model", "stream", "max_tokens", "max_completion_tokens", "temperature", "top_p", "tool_choice", "parallel_tool_calls", "reasoning", "reasoning_effort", "thinking", "response_format", "stream_options"} {
		if value, ok := obj[key]; ok {
			settings[key] = value
		}
	}
	settingsJSON, _ := json.Marshal(settings)
	s.settings = d.fingerprint(settingsJSON)
	for i, raw := range messages {
		var message struct {
			Role      string            `json:"role"`
			ToolCalls []json.RawMessage `json:"tool_calls"`
		}
		_ = json.Unmarshal(raw, &message)
		role := message.Role
		switch role {
		case "system", "developer", "user", "assistant", "tool", "function":
		default:
			role = "other"
		}
		s.shape.Roles[role]++
		stamp := d.fingerprint(raw)
		if i < diagnosticMessages {
			s.stamps = append(s.stamps, stamp)
		}
		if i >= len(messages)-diagnosticTail {
			s.shape.Tail = append(s.shape.Tail, reqlog.MessageShape{Index: i, Role: role,
				Bytes: len(raw), ToolCalls: len(message.ToolCalls), Fingerprint: stamp})
		}
	}
	return s
}

func defaultJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

func requestDifference(current, reference requestSnapshot) *reqlog.RequestDifference {
	prefix := 0
	for prefix < len(current.stamps) && prefix < len(reference.stamps) && current.stamps[prefix] == reference.stamps[prefix] {
		prefix++
	}
	changed := prefix
	if prefix == current.shape.Messages && prefix == reference.shape.Messages {
		changed = -1
	}
	truncated := prefix == diagnosticMessages && (current.shape.Messages > prefix || reference.shape.Messages > prefix)
	if truncated {
		changed = -1 // Do not claim the first changed message is known beyond the bound.
	}
	return &reqlog.RequestDifference{ReferenceRequestID: reference.id, BytesDelta: current.bytes - reference.bytes,
		MessagesDelta: current.shape.Messages - reference.shape.Messages, CommonPrefix: prefix,
		FirstChangedMessage: changed, ToolsChanged: current.tools != reference.tools,
		SettingsChanged: current.settings != reference.settings, Truncated: truncated}
}

func (d *requestDiagnostics) compare(key string, current *requestSnapshot) *reqlog.RequestComparison {
	if d == nil || key == "" || current == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	b, ok := d.baselines[key]
	if !ok {
		return nil
	}
	if time.Since(b.touched) >= diagnosticTTL {
		delete(d.baselines, key)
		return nil
	}
	return &reqlog.RequestComparison{FirstSuccess: requestDifference(*current, b.first), LastSuccess: requestDifference(*current, b.last)}
}

func (d *requestDiagnostics) success(key string, current *requestSnapshot) {
	if d == nil || key == "" || current == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	for k, b := range d.baselines {
		if now.Sub(b.touched) >= diagnosticTTL {
			delete(d.baselines, k)
		}
	}
	b, ok := d.baselines[key]
	if !ok {
		if len(d.baselines) >= diagnosticSessions {
			var oldest string
			for k, candidate := range d.baselines {
				if oldest == "" || candidate.touched.Before(d.baselines[oldest].touched) {
					oldest = k
				}
			}
			delete(d.baselines, oldest)
		}
		b.first, b.last = *current, *current
	} else {
		if current.started.Before(b.first.started) {
			b.first = *current
		}
		if current.started.After(b.last.started) {
			b.last = *current
		}
	}
	b.touched = now
	d.baselines[key] = b
}
