package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestV1UnknownEndpointJSON404 未实现的 /v1/* 端点必须返回可读的 JSON 404，
// 而不是 Go mux 的纯文本 "404 page not found"（客户端曾只显示 404）。
func TestV1UnknownEndpointJSON404(t *testing.T) {
	h := NewHandler(Config{})
	for _, path := range []string{"/v1/responses", "/v1/embeddings", "/v1", "/v1/chat/completions/"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: code=%d want 404", path, rec.Code)
			continue
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Errorf("%s: content-type=%q", path, ct)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "not_found") || !strings.Contains(body, "/v1/chat/completions") || !strings.Contains(body, "/v1/models") {
			t.Errorf("%s: body=%s", path, body)
		}
	}
}

// TestV1WrongMethodStill405 已知端点用错方法仍应是 405 + Allow，不能被兜底 404 覆盖。
func TestV1WrongMethodStill405(t *testing.T) {
	h := NewHandler(Config{})
	cases := []struct {
		method, path, allow string
	}{
		{http.MethodGet, "/v1/chat/completions", "POST"},
		{http.MethodPost, "/v1/models", "GET"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: code=%d want 405", c.method, c.path, rec.Code)
		}
		if allow := rec.Header().Get("Allow"); !strings.Contains(allow, c.allow) {
			t.Errorf("%s %s: Allow=%q want %q", c.method, c.path, allow, c.allow)
		}
	}
}
