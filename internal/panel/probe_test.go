package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func TestCleanSnippet(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "empty",
			input:    "",
			expected: "",
		},
		{
			name:     "error JSON",
			input:    `{"code":6004,"msg":"User daily token usage exceeds limit"}`,
			expected: "code=6004 msg=User daily token usage exceeds limit",
		},
		{
			name:     "SSE done",
			input:    "data: [DONE]\n\n",
			expected: "[DONE]",
		},
		{
			name:     "SSE delta text",
			input:    `data: {"choices":[{"delta":{"content":"Pong"},"index":0}]}` + "\n\n",
			expected: "回复: Pong",
		},
		{
			name:     "SSE error envelope",
			input:    `data: {"code":12153,"msg":"Offline user session not found"}` + "\n\n",
			expected: "code=12153 msg=Offline user session not found",
		},
		{
			name:     "plain text long truncated",
			input:    strings.Repeat("a", 120),
			expected: strings.Repeat("a", 97) + "...",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := cleanSnippet(tc.input)
			if got != tc.expected {
				t.Errorf("cleanSnippet(%q) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestAccountProbeEndpoint(t *testing.T) {
	// 1. Mock upstream server
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authH := r.Header.Get("Authorization")
		if strings.Contains(authH, "dead-token") {
			w.WriteHeader(http.StatusBadRequest)
			future := time.Now().Add(2 * time.Hour).In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04:05")
			_, _ = fmt.Fprintf(w, `{"code":6004,"msg":"将在 %s UTC+8 重置"}`, future)
			return
		}
		// Success SSE
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	upClient := &upstream.Client{
		HTTP:        srv.Client(),
		ChatBaseCN:  srv.URL,
		IdleTimeout: 5 * time.Second,
	}

	pl := pool.New("")
	a1 := &auth.Auth{
		UID:         "u1",
		Nickname:    "Nick1",
		AccessToken: "good-token",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}
	a2 := &auth.Auth{
		UID:         "u2",
		Nickname:    "Nick2",
		AccessToken: "dead-token",
		ExpiresAt:   time.Now().Add(time.Hour).Unix(),
	}
	pl.Add(a1)
	pl.Add(a2)

	pnl := New(Config{
		Pool:     pl,
		Upstream: upClient,
		APIKey:   "secret",
	})

	// Case 1: Account not found
	{
		req := httptest.NewRequest("POST", "/panel/api/accounts/nonexistent/probe", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		pnl.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for nonexistent, got %d", rec.Code)
		}
	}

	// Case 2: Success account probe
	{
		req := httptest.NewRequest("POST", "/panel/api/accounts/u1/probe?model=deepseek-v4.1-flash", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		pnl.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var res ProbeResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("failed to decode json: %v", err)
		}
		if !res.OK {
			t.Errorf("expected OK=true, got false: error=%s", res.Error)
		}
		if res.UID != "u1" || res.Status != 200 {
			t.Errorf("unexpected res: %+v", res)
		}
		if !strings.Contains(res.Snippet, "OK") {
			t.Errorf("expected snippet to contain OK, got %q", res.Snippet)
		}
	}

	// Case 3: Rate limited account probe
	{
		req := httptest.NewRequest("POST", "/panel/api/accounts/u2/probe", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		pnl.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var res ProbeResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatalf("failed to decode json: %v", err)
		}
		if res.OK {
			t.Errorf("expected OK=false for rate limited, got true")
		}
		if res.Status != http.StatusBadRequest {
			t.Errorf("expected status 400, got %d", res.Status)
		}
		if !strings.Contains(res.Snippet, "6004") {
			t.Errorf("expected snippet to contain 6004, got %q", res.Snippet)
		}
		if res.ResetAt == "" {
			t.Errorf("expected reset_at to be populated")
		}
		if res.ResetRemainingSec <= 0 {
			t.Errorf("expected reset_remaining_sec > 0, got %d", res.ResetRemainingSec)
		}
		// Verify pool status now tracks this model rate limit
		st, ok := pl.Status("u2")
		if !ok {
			t.Fatalf("expected u2 in pool status")
		}
		var foundModel bool
		for _, rlm := range st.RateLimitedModels {
			if rlm.Model == "deepseek-v4.1-flash" {
				foundModel = true
				break
			}
		}
		if !foundModel {
			t.Errorf("expected u2 in pool status to have deepseek-v4.1-flash in RateLimitedModels, got %+v", st.RateLimitedModels)
		}
	}

	// Case 4: Probe all
	{
		req := httptest.NewRequest("POST", "/panel/api/probe_all", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		pnl.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		var resp struct {
			OK      bool          `json:"ok"`
			Results []ProbeResult `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode json: %v", err)
		}
		if !resp.OK {
			t.Errorf("expected resp.OK=true")
		}
		if len(resp.Results) != 2 {
			t.Errorf("expected 2 results, got %d", len(resp.Results))
		}
	}
}
