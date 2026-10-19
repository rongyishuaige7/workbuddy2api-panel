package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/promptlog"
)

func TestKeyPromptsEndpoints(t *testing.T) {
	rec := promptlog.New(promptlog.Config{Dir: t.TempDir(), RetentionDays: 3, MaxBytes: 1 << 20})
	if err := rec.Record(promptlog.Entry{
		Time: time.Now(), KeyID: "k1", KeyName: "friend", Model: "global:deepseek-v4.1-flash", Status: 200,
		Messages: []promptlog.Message{
			{"role": "developer", "content": "dev-only"},
			{"role": "user", "content": "hello-monitor"},
			{"role": "user", "content": "Time sampled while preparing turn 1, step 1: ctx"},
			{"role": "tool", "content": "tool-out"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	saved := []APIKeyView{{ID: "k1", Name: "friend", Key: "sk-1", CapturePrompts: true}}
	p := New(Config{
		APIKey:    "panel-key",
		PromptLog: rec,
		ListAPIKeys: func() ([]APIKeyView, error) {
			return saved, nil
		},
		SaveAPIKeys: func(k []APIKeyView) error { saved = k; return nil },
	})
	do := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer panel-key")
		r := httptest.NewRecorder()
		p.ServeHTTP(r, req)
		return r
	}

	keysResp := do(http.MethodGet, "/panel/api/keys")
	if !strings.Contains(keysResp.Body.String(), `"prompt_count":1`) {
		t.Fatalf("keys should expose prompt_count: %s", keysResp.Body.String())
	}

	rec2 := do(http.MethodGet, "/panel/api/keys/k1/prompts")
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), "hello-monitor") || !strings.Contains(rec2.Body.String(), "dev-only") {
		t.Fatalf("list code=%d body=%s", rec2.Code, rec2.Body.String())
	}
	userOnly := do(http.MethodGet, "/panel/api/keys/k1/prompts?role=user")
	if userOnly.Code != http.StatusOK || !strings.Contains(userOnly.Body.String(), "hello-monitor") ||
		!strings.Contains(userOnly.Body.String(), "Time sampled") ||
		strings.Contains(userOnly.Body.String(), "dev-only") || strings.Contains(userOnly.Body.String(), "tool-out") {
		t.Fatalf("role=user body=%s", userOnly.Body.String())
	}
	realOnly := do(http.MethodGet, "/panel/api/keys/k1/prompts?role=real")
	if realOnly.Code != http.StatusOK || !strings.Contains(realOnly.Body.String(), "hello-monitor") ||
		strings.Contains(realOnly.Body.String(), "Time sampled") ||
		strings.Contains(realOnly.Body.String(), "dev-only") || strings.Contains(realOnly.Body.String(), "tool-out") {
		t.Fatalf("role=real body=%s", realOnly.Body.String())
	}

	rec2 = do(http.MethodPost, "/panel/api/keys/k1/prompts/clear")
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), `"removed":1`) {
		t.Fatalf("clear code=%d body=%s", rec2.Code, rec2.Body.String())
	}

	rec2 = do(http.MethodGet, "/panel/api/keys/k1/prompts")
	if strings.Contains(rec2.Body.String(), "hello-monitor") {
		t.Fatal("prompts should be cleared")
	}
}

func TestPromptArchivesRequireAdministrativeKey(t *testing.T) {
	rec := promptlog.New(promptlog.Config{Dir: t.TempDir()})
	defer rec.Close()
	if err := rec.Record(promptlog.Entry{KeyID: "k1"}); err != nil {
		t.Fatal(err)
	}
	p := New(Config{PromptLog: rec, ListAPIKeys: func() ([]APIKeyView, error) { return []APIKeyView{{ID: "k1"}}, nil }})
	for _, tc := range []struct{ method, path string }{{"GET", "/panel/api/keys/k1/prompts"}, {"POST", "/panel/api/keys/k1/prompts/clear"}, {"POST", "/panel/api/prompts/prune"}} {
		w := httptest.NewRecorder()
		p.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
		if w.Code != http.StatusForbidden || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("anonymous archive access: %s %d", tc.path, w.Code)
		}
	}
	if rec.Counts()["k1"] != 1 {
		t.Fatal("unauthenticated mutation changed archive")
	}
}

func TestKeyPromptsFreshViewAPI(t *testing.T) {
	rec := promptlog.New(promptlog.Config{Dir: t.TempDir()})
	defer rec.Close()
	now := time.Now()
	for i := 0; i < 2; i++ {
		body := `{"prompt_cache_key":"s","messages":[{"role":"user","content":"继续"}]}`
		if i == 1 {
			body = `{"prompt_cache_key":"s","messages":[{"role":"user","content":"继续"},{"role":"user","content":"继续"}]}`
		}
		c := promptlog.Capture([]byte(body), 1000)
		if err := rec.Record(promptlog.Entry{Version: 2, Time: now.Add(time.Duration(i) * time.Second), KeyID: "k1", SessionHash: c.SessionHash, Messages: c.Messages, MessageIndexes: c.MessageIndexes, Inputs: c.Inputs}); err != nil {
			t.Fatal(err)
		}
	}
	p := New(Config{APIKey: "admin", PromptLog: rec, ListAPIKeys: func() ([]APIKeyView, error) { return []APIKeyView{{ID: "k1"}}, nil }})
	req := httptest.NewRequest("GET", "/panel/api/keys/k1/prompts?role=fresh", nil)
	req.Header.Set("Authorization", "Bearer admin")
	w := httptest.NewRecorder()
	p.ServeHTTP(w, req)
	var data struct {
		Entries []promptlog.Entry `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(data.Entries) != 2 || data.Entries[0].FreshState != "new" || len(data.Entries[0].Messages) != 1 || data.Entries[1].FreshState != "unknown" {
		t.Fatalf("fresh API returned incorrect view: %s", w.Body.String())
	}
}

func TestEmptyPromptViewsReturnAnArray(t *testing.T) {
	rec := promptlog.New(promptlog.Config{Dir: t.TempDir()})
	defer rec.Close()
	p := New(Config{APIKey: "admin", PromptLog: rec, ListAPIKeys: func() ([]APIKeyView, error) { return []APIKeyView{{ID: "empty"}}, nil }})
	for _, role := range []string{"fresh", "real", "user", "all"} {
		req := httptest.NewRequest("GET", "/panel/api/keys/empty/prompts?role="+role, nil)
		req.Header.Set("Authorization", "Bearer admin")
		w := httptest.NewRecorder()
		p.ServeHTTP(w, req)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		entries, ok := body["entries"].([]any)
		if w.Code != 200 || !ok || len(entries) != 0 {
			t.Errorf("role=%s must return entries:[], got %s", role, w.Body.String())
		}
	}
}
