package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
)

func TestRequestDiagnosticsCompareSuccessWithWAFToolRound(t *testing.T) {
	const privateText = "private-prompt-not-for-archive"
	const privateResult = "private-tool-result-not-for-archive"
	const traceID = "a6774c8b46f486372b0107d84c19b15a"
	rlog := reqlog.New(reqlog.Config{})
	t.Cleanup(rlog.Close)
	up := newFakeUpstream(t, nil)
	up.GlobalEnabled = true
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body struct {
			Messages []json.RawMessage `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		status, response := 200, sseOK
		headers := http.Header{"Content-Type": {"text/event-stream"}}
		if len(body.Messages) > 2 {
			status, response = 403, wafPage+strings.Repeat(" ", 300)+"Request ID: "+traceID
			headers.Set("Content-Type", "text/html; charset=utf-8")
			headers.Set("X-Request-ID", traceID)
			headers.Set("Set-Cookie", "private-cookie")
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(response))}, nil
	})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "one-account", AccessToken: "private-auth-token", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}),
		Upstream: up, GlobalEnabled: true, RequestLog: rlog})
	first := fmt.Sprintf(`{"model":"global:deepseek-v4.1-flash","conversation_id":"private-session","messages":[{"role":"user","content":%q}]}`, privateText)
	second := fmt.Sprintf(`{"model":"global:deepseek-v4.1-flash","conversation_id":"private-session","messages":[{"role":"user","content":%q},{"role":"assistant","tool_calls":[{"id":"private-call-id","type":"function","function":{"name":"private-tool-name","arguments":"{}"}}]},{"role":"tool","tool_call_id":"private-call-id","content":%q}]}`, privateText, privateResult)
	for i, body := range []string{first, second} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		want := []int{200, 503}[i]
		if rec.Code != want {
			t.Fatalf("request %d: %d %s", i, rec.Code, rec.Body)
		}
	}
	rows := rlog.Snapshot().Recent
	failed, success := rows[0], rows[1]
	if failed.RequestShape == nil || success.RequestShape == nil || failed.RequestDiff == nil {
		t.Fatal("missing request diagnostics")
	}
	for _, diff := range []*reqlog.RequestDifference{failed.RequestDiff.FirstSuccess, failed.RequestDiff.LastSuccess} {
		if diff.ReferenceRequestID != success.RequestID || diff.CommonPrefix != 1 || diff.FirstChangedMessage != 1 || diff.MessagesDelta != 2 || diff.BytesDelta != len(second)-len(first) || diff.ToolsChanged || diff.SettingsChanged {
			t.Fatalf("wrong success-to-WAF difference: %+v", diff)
		}
	}
	a := failed.AttemptDetails[0]
	if a.ResponseTrace == nil || a.ResponseTrace.ContentType != "text/html" || a.ResponseTrace.BodyKind != "waf_block_page" || a.ResponseTrace.OutboundBytes == 0 || a.ResponseTrace.OutboundFingerprint == "" || a.UpstreamRequestID != traceID || a.CooldownSource != "local_waf_backoff" || a.ResponseTrace.RetryAfter {
		t.Fatalf("missing WAF response trace: %+v", a)
	}
	raw, _ := json.Marshal(rows)
	for _, secret := range []string{privateText, privateResult, "private-session", "private-auth-token", "private-cookie", "private-tool-name", "private-call-id", "WAF Block Page"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("diagnostics contain private data %q", secret)
		}
	}
}

func TestRequestDiagnosticsBoundsExpiryAndConcurrentCompletion(t *testing.T) {
	d := newRequestDiagnostics()
	now := time.Now()
	old := d.snapshot([]byte(`{"messages":[{"role":"user","content":"one"}]}`), "older", now)
	newer := d.snapshot([]byte(`{"messages":[{"role":"user","content":"two"}]}`), "newer", now.Add(time.Second))
	d.success("session", newer)
	d.success("session", old)
	comparison := d.compare("session", newer)
	if comparison.FirstSuccess.ReferenceRequestID != "older" || comparison.LastSuccess.ReferenceRequestID != "newer" {
		t.Fatalf("out-of-order completion corrupted baseline: %+v", comparison)
	}
	other := newRequestDiagnostics().snapshot([]byte(`{"messages":[{"role":"user","content":"one"}]}`), "other", now)
	if old.shape.Fingerprint == other.shape.Fingerprint {
		t.Fatal("fingerprints must use process-private keys")
	}
	var wg sync.WaitGroup
	for i := 0; i < diagnosticSessions*2; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); key := fmt.Sprint(i); d.success(key, newer); _ = d.compare(key, old) }(i)
	}
	wg.Wait()
	if len(d.baselines) > diagnosticSessions {
		t.Fatal("unbounded session baselines")
	}
	d.baselines["expired"] = requestBaselines{first: *old, last: *old, touched: now.Add(-diagnosticTTL - time.Second)}
	if d.compare("expired", newer) != nil {
		t.Fatal("expired baseline must not be used")
	}
	messages := strings.Repeat(`{"role":"private-role-must-not-be-logged","content":"secret"},`, diagnosticMessages+10)
	large := []byte(`{"messages":[` + strings.TrimSuffix(messages, ",") + `]}`)
	long := d.snapshot(large, "long", now)
	if len(long.stamps) != diagnosticMessages || len(long.shape.Tail) != diagnosticTail || long.shape.Tail[0].Role != "other" {
		t.Fatal("message summary is unbounded or has arbitrary roles")
	}
	if diff := requestDifference(*long, *long); !diff.Truncated || diff.FirstChangedMessage != -1 {
		t.Fatalf("must label comparison beyond the retained prefix: %+v", diff)
	}
}

func TestRequestDiagnosticsTrackToolsAndSettingsSeparately(t *testing.T) {
	d := newRequestDiagnostics()
	base := d.snapshot([]byte(`{"messages":[],"tools":[],"temperature":0}`), "first", time.Now())
	changed := d.snapshot([]byte(`{"messages":[],"tools":[{"secret":"private"}],"temperature":1}`), "next", time.Now())
	diff := requestDifference(*changed, *base)
	if !diff.ToolsChanged || !diff.SettingsChanged || diff.FirstChangedMessage != -1 {
		t.Fatalf("tool/settings changes lost: %+v", diff)
	}
}
