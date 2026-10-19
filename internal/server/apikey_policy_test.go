package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
)

// TestFilterModelList 受限密钥只看到白名单内的模型；不受限时原样返回。
func TestFilterModelList(t *testing.T) {
	data := []map[string]any{
		{"id": "cn:glm-5.2"},
		{"id": "global:deepseek-v4.1-flash"},
	}
	restricted := httpauth.Policy{Models: []string{"global:deepseek-v4.1-flash"}}
	got := filterModelList(data, restricted)
	if len(got) != 1 || got[0]["id"] != "global:deepseek-v4.1-flash" {
		t.Fatalf("filtered=%v", got)
	}
	if all := filterModelList(data, httpauth.Policy{}); len(all) != 2 {
		t.Fatalf("unrestricted len=%d want 2", len(all))
	}
}

// TestChatRejectsDisallowedModel 白名单外模型必须在选号/出站之前被 403。
func TestChatRejectsDisallowedModel(t *testing.T) {
	h := NewHandler(Config{APIKey: "k"})
	body := `{"model":"cn:glm-5.2","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), apiKeyPolicyKey{},
		httpauth.Policy{Name: "friend", Models: []string{"global:deepseek-v4.1-flash"}}))
	rec := httptest.NewRecorder()
	h.chatCompletions(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "model_not_allowed") {
		t.Errorf("body=%s", rec.Body.String())
	}
}

// TestStatusRejectsRestrictedKey 受限密钥不开放 /status（避免泄露账号池状态）。
func TestStatusRejectsRestrictedKey(t *testing.T) {
	h := NewHandler(Config{APIKey: "k"})
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req = req.WithContext(context.WithValue(req.Context(), apiKeyPolicyKey{},
		httpauth.Policy{Name: "friend", Models: []string{"global:deepseek-v4.1-flash"}}))
	rec := httptest.NewRecorder()
	h.status(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}
