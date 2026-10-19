package promptlog

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func captureMessages(t *testing.T, messages []Message, budget int) CaptureResult {
	t.Helper()
	body, err := json.Marshal(map[string]any{"messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	return Capture(body, budget)
}

func TestCapturePrioritizesInputOverToolHistory(t *testing.T) {
	messages := []Message{{"role": "system", "content": "instructions"}, {"role": "user", "content": "original task"}}
	for i := 0; i < 150; i++ {
		messages = append(messages, Message{"role": "tool", "content": strings.Repeat("工具", 1000)})
	}
	messages = append(messages, Message{"role": "user", "content": "new requirement"}, Message{"role": "assistant", "tool_calls": []any{map[string]any{"arguments": strings.Repeat("x", 100000)}}})
	r := captureMessages(t, messages, 2000)
	for _, want := range []string{"original task", "new requirement"} {
		found := false
		for _, m := range r.Messages {
			if m["content"] == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing input %q", want)
		}
	}
	if len(r.Inputs) != 2 || r.Inputs[1].Index != 152 || !r.Inputs[1].Captured || r.Omitted == 0 {
		t.Fatalf("incorrect metadata: %+v", r.Inputs)
	}
}

func TestCaptureUnicodeAndNoUnnecessaryHeadTruncation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []Message
		budget   int
	}{
		{"unicode", []Message{{"role": "user", "content": strings.Repeat("中", 900)}}, 1000},
		{"head", []Message{{"role": "system", "content": strings.Repeat("中", 10000)}, {"role": "user", "content": strings.Repeat("文", 10000)}}, 100000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := captureMessages(t, tc.messages, tc.budget)
			if r.Truncated || len(r.Messages) != len(tc.messages) {
				t.Fatalf("unexpected truncation: %+v", r.Inputs)
			}
			for i, m := range tc.messages {
				if r.Messages[i]["content"] != m["content"] {
					t.Fatal("content changed")
				}
			}
		})
	}
}

func TestCaptureIdentityBeforeTruncationAndImages(t *testing.T) {
	messages := []Message{{"role": "user", "content": strings.Repeat("a", 4000)}}
	small, large := captureMessages(t, messages, 100), captureMessages(t, messages, 10000)
	if small.Inputs[0].Hash != large.Inputs[0].Hash || !small.Inputs[0].Truncated || large.Inputs[0].Truncated {
		t.Fatal("identity must not depend on archive budget")
	}
	image := func(url string) CaptureResult {
		return captureMessages(t, []Message{{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}}}}, 1000)
	}
	a, b := image("data:image/png;base64,AAAA"), image("data:image/png;base64,BBBB")
	if a.Messages[0]["content"] != "[图片]" || b.Messages[0]["content"] != "[图片]" || a.Inputs[0].Hash == b.Inputs[0].Hash {
		t.Fatal("images need separate identities without storing their data")
	}
}

func TestCaptureOversizedExtensionsAndMetadataLimit(t *testing.T) {
	r := captureMessages(t, []Message{{"role": "user", "content": "keep me", "extension": strings.Repeat("x", 50000)}}, 1000)
	if len(r.Messages) != 1 || r.Messages[0]["content"] != "keep me" || !r.Truncated {
		t.Fatal("extension discarded user text")
	}
	messages := make([]Message, 2100)
	for i := range messages {
		messages[i] = Message{"role": "user", "content": fmt.Sprintf("input %d", i)}
	}
	r = captureMessages(t, messages, 1000)
	if len(r.Inputs) != 2048 || r.InputsOmitted != 52 || r.Inputs[len(r.Inputs)-1].Index != 2099 {
		t.Fatalf("unbounded or incorrect identities: %d %d", len(r.Inputs), r.InputsOmitted)
	}
	r = captureMessages(t, messages[:1], 1)
	if len(r.Messages) != 0 || len(r.Inputs) != 1 || r.Inputs[0].Captured {
		t.Fatal("tiny budget must retain identity of omitted text")
	}
}

func TestCaptureToolNamesAndExplicitSessions(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"messages": []Message{{"role": "user", "content": "hello"}}, "tools": []any{map[string]any{"name": strings.Repeat("名", 50000)}}})
	r := Capture(body, 10000)
	if len(r.Tools) != 1 || utf8.RuneCountInString(r.Tools[0]) != 64 || !r.Truncated {
		t.Fatal("tool names not bounded")
	}
	if r.SessionHash != "" {
		t.Fatal("content-derived identity cannot identify a conversation")
	}
	a := Capture([]byte(`{"metadata":{"conversation_id":"s"},"messages":[{"role":"user","content":"hello"}]}`), 1000)
	b := Capture([]byte(`{"prompt_cache_key":"s","messages":[{"role":"user","content":"hello"}]}`), 1000)
	if a.SessionHash == "" || a.SessionHash != b.SessionHash || strings.Contains(a.SessionHash, "s") {
		t.Fatal("explicit conversation identity must be hashed consistently")
	}
}
