package panel

import (
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
		Messages: []promptlog.Message{{"role": "user", "content": "hello-monitor"}},
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
	if rec2.Code != http.StatusOK || !strings.Contains(rec2.Body.String(), "hello-monitor") {
		t.Fatalf("list code=%d body=%s", rec2.Code, rec2.Body.String())
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
