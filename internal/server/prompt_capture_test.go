package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/promptlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestPromptCaptureAuthAndModelRejection(t *testing.T) {
	rec := promptlog.New(promptlog.Config{Dir: t.TempDir()})
	defer rec.Close()
	live := livecfg.New(livecfg.Snapshot{APIKey: "owner-test", APIKeys: []httpauth.Policy{{ID: "k", Name: "friend", Key: "key-test", CapturePrompts: true, Models: []string{"allowed"}}}})
	h := NewHandler(Config{Pool: pool.New(""), Upstream: upstream.New(), Live: live, PromptLog: rec, PromptMaxChars: 1})
	request := func(key string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"denied","prompt_cache_key":"conversation","messages":[{"role":"user","content":"input"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		h.ServeHTTP(httptest.NewRecorder(), req)
	}
	request("key-test")
	entries, err := rec.List("k", 10)
	if err != nil || len(entries) != 1 {
		t.Fatalf("rejected model input missing: count=%d err=%v", len(entries), err)
	}
	e := entries[0]
	if e.Status != 403 || e.Model != "denied" || e.KeyName != "friend" || e.Version != 2 || e.SessionHash == "" || e.RequestID == "" || len(e.Inputs) != 1 || len(e.Messages) != 0 {
		t.Fatalf("invalid capture metadata: %+v", e)
	}
	request("owner-test")
	request("invalid-test")
	s := live.Load()
	s.APIKeys[0].Disabled = true
	live.Store(s)
	request("key-test")
	s.APIKeys[0].Disabled = false
	s.APIKeys[0].CapturePrompts = false
	live.Store(s)
	request("key-test")
	if entries, _ = rec.List("k", 10); len(entries) != 1 {
		t.Fatal("disabled/opted-out key was captured")
	}
	if entries, _ = rec.List("owner", 10); len(entries) != 0 {
		t.Fatal("owner must not be captured")
	}
}
