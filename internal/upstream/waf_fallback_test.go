package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

type wafFallbackTransport func(*http.Request) (*http.Response, error)

func (f wafFallbackTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestGlobalWAFFallsBackToV2 /console 返回 WAF 403 时，同一请求体应自动改走
// /v2/chat/completions；/v2 成功则整次请求成功，不进入账号冷却。
func TestGlobalWAFFallsBackToV2(t *testing.T) {
	var paths []string
	c := &Client{
		HTTP: &http.Client{Transport: wafFallbackTransport(func(r *http.Request) (*http.Response, error) {
			paths = append(paths, r.URL.Path)
			if r.URL.Path == "/console/chat/completions" {
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Header:     http.Header{"Content-Type": []string{"text/html"}},
					Body:       io.NopCloser(strings.NewReader("<html><title>WAF Block Page</title></html>")),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body: io.NopCloser(strings.NewReader(
					"data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n")),
			}, nil
		})},
		ChatBaseCN:     "https://fake.example",
		ChatBaseGlobal: "https://fake.example",
		GlobalEnabled:  true,
	}
	a := &auth.Auth{UID: "g", AccessToken: "token", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	rc, status, _, err := c.ChatStreamContext(context.Background(), a, body, "", ChatMeta{})
	if err != nil {
		t.Fatalf("ChatStreamContext: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status=%d want 200", status)
	}
	if _, err := io.ReadAll(rc); err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
	if len(paths) != 2 || paths[0] != "/console/chat/completions" || paths[1] != "/v2/chat/completions" {
		t.Fatalf("paths=%v want [/console/chat/completions /v2/chat/completions]", paths)
	}
}

// TestGlobalWAFFallbackChannelKeepsWAF /console WAF 后 /v2 返回 11-128 渠道错误时，
// 必须保留原始 WAF（让 handler 轮转/冷却），不能把 403 变成 400。
func TestGlobalWAFFallbackChannelKeepsWAF(t *testing.T) {
	c := &Client{
		HTTP: &http.Client{Transport: wafFallbackTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path == "/console/chat/completions" {
				return &http.Response{
					StatusCode: http.StatusForbidden,
					Header:     http.Header{"Content-Type": []string{"text/html"}},
					Body:       io.NopCloser(strings.NewReader("<html><title>WAF Block Page</title></html>")),
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":"11-128","msg":"request illegal"}`)),
			}, nil
		})},
		ChatBaseCN:     "https://fake.example",
		ChatBaseGlobal: "https://fake.example",
		GlobalEnabled:  true,
	}
	a := &auth.Auth{UID: "g", AccessToken: "token", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, status, _, err := c.ChatStreamContext(context.Background(), a, body, "", ChatMeta{})
	var ue *Error
	if !errors.As(err, &ue) {
		t.Fatalf("want *upstream.Error, got %v", err)
	}
	if status != http.StatusForbidden || ue.Kind != ErrWafBlock {
		t.Fatalf("status=%d kind=%s want 403/%s", status, ue.Kind, ErrWafBlock)
	}
}
