package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/httpauth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/keystats"
	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/ratelimit"
)

// TestWithAuthDisabledAndRateLimit 覆盖禁用与每密钥限速，并验证统计只记一次。
func TestWithAuthDisabledAndRateLimit(t *testing.T) {
	stats := keystats.New("")
	live := livecfg.New(livecfg.Snapshot{
		APIKeys: []httpauth.Policy{{ID: "k1", Name: "friend", Key: "sk-1", RPM: 1}},
	})
	h := NewHandler(Config{KeyStats: stats, RateLimit: ratelimit.New(), Live: live})
	next := func(w http.ResponseWriter, r *http.Request) {
		h.recordKeyRequest(r, http.StatusOK)
		w.WriteHeader(http.StatusOK)
	}
	handler := h.withAuth(next)
	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("Authorization", "Bearer sk-1")
		rec := httptest.NewRecorder()
		handler(rec, req)
		return rec
	}

	if rec := call(); rec.Code != http.StatusOK {
		t.Fatalf("first code=%d", rec.Code)
	}
	rec := call()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second code=%d", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("429 should carry Retry-After")
	}

	live.Store(livecfg.Snapshot{
		APIKeys: []httpauth.Policy{{ID: "k1", Name: "friend", Key: "sk-1", Disabled: true}},
	})
	if rec := call(); rec.Code != http.StatusForbidden {
		t.Fatalf("disabled code=%d", rec.Code)
	}

	e := stats.Snapshot()["k1"]
	if e.Requests != 3 || e.Success != 1 || e.Errors != 2 {
		t.Fatalf("stats=%+v", e)
	}
}
