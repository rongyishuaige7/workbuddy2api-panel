package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func sseOK() *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: io.NopCloser(strings.NewReader(
			"data: {\"choices\":[{\"delta\":{\"content\":\"OK\"}}]}\n\ndata: [DONE]\n\n")),
	}
}

func waf403() *http.Response {
	return &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     http.Header{"Content-Type": []string{"text/html"}},
		Body:       io.NopCloser(strings.NewReader("<html><title>WAF Block Page</title></html>")),
	}
}

func newWAFPreferClient(paths *[]string, handler func(path string) *http.Response) *Client {
	return &Client{
		HTTP: &http.Client{Transport: wafFallbackTransport(func(r *http.Request) (*http.Response, error) {
			*paths = append(*paths, r.URL.Path)
			return handler(r.URL.Path), nil
		})},
		ChatBaseCN:     "https://fake.example",
		ChatBaseGlobal: "https://fake.example",
		GlobalEnabled:  true,
	}
}

func callChat(t *testing.T, c *Client, a *auth.Auth) error {
	t.Helper()
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	rc, _, _, err := c.ChatStreamContext(context.Background(), a, body, "", ChatMeta{})
	if err == nil {
		if _, rerr := io.ReadAll(rc); rerr != nil {
			t.Fatal(rerr)
		}
		_ = rc.Close()
	}
	return err
}

// TestGlobalWAFPrefersV2WithinTTL 验证：/console WAF + /v2 成功后，TTL 内
// 同账号下一次直接走 /v2；TTL 过期后回到 /console 优先。
func TestGlobalWAFPrefersV2WithinTTL(t *testing.T) {
	now := time.Unix(1700000000, 0)
	var paths []string
	c := newWAFPreferClient(&paths, func(path string) *http.Response {
		if path == "/console/chat/completions" {
			return waf403()
		}
		return sseOK()
	})
	c.WAFV2TTL = time.Minute
	c.nowFunc = func() time.Time { return now }
	a := &auth.Auth{UID: "acct-1", AccessToken: "token", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}

	run := func() []string {
		paths = nil
		if err := callChat(t, c, a); err != nil {
			t.Fatalf("call: %v", err)
		}
		out := append([]string(nil), paths...)
		return out
	}

	if got := run(); len(got) != 2 || got[0] != "/console/chat/completions" || got[1] != "/v2/chat/completions" {
		t.Fatalf("first paths=%v", got)
	}
	if got := run(); len(got) != 1 || got[0] != "/v2/chat/completions" {
		t.Fatalf("preferred paths=%v want [/v2/chat/completions]", got)
	}
	now = now.Add(2 * time.Minute)
	if got := run(); len(got) != 2 || got[0] != "/console/chat/completions" || got[1] != "/v2/chat/completions" {
		t.Fatalf("after ttl paths=%v", got)
	}
}

// TestGlobalWAFPreferenceDisabled TTL=0 时保持旧行为：每次都先 /console。
func TestGlobalWAFPreferenceDisabled(t *testing.T) {
	var paths []string
	c := newWAFPreferClient(&paths, func(path string) *http.Response {
		if path == "/console/chat/completions" {
			return waf403()
		}
		return sseOK()
	})
	a := &auth.Auth{UID: "acct-2", AccessToken: "token", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}
	for i := 0; i < 2; i++ {
		paths = nil
		if err := callChat(t, c, a); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if len(paths) != 2 || paths[0] != "/console/chat/completions" {
			t.Fatalf("call %d paths=%v", i, paths)
		}
	}
}

// TestGlobalWAFV2ChannelErrorDoesNotMark /v2 回退返回 11-128 时不能标记偏好，
// 否则下一次会直接撞 /v2 的渠道错误。
func TestGlobalWAFV2ChannelErrorDoesNotMark(t *testing.T) {
	var paths []string
	c := newWAFPreferClient(&paths, func(path string) *http.Response {
		if path == "/console/chat/completions" {
			return waf403()
		}
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"code":"11-128","msg":"request illegal"}`)),
		}
	})
	c.WAFV2TTL = time.Minute
	a := &auth.Auth{UID: "acct-3", AccessToken: "token", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}
	for i := 0; i < 2; i++ {
		paths = nil
		_ = callChat(t, c, a)
		if len(paths) != 2 || paths[0] != "/console/chat/completions" {
			t.Fatalf("call %d paths=%v want console first", i, paths)
		}
	}
}

// TestPreferredV2ChannelErrorFallsBackToConsole 已标记账号在 /v2 遇到渠道错误时，
// 应撤销偏好并回退 /console，而不是直接失败。
func TestPreferredV2ChannelErrorFallsBackToConsole(t *testing.T) {
	var paths []string
	mode := "normal"
	c := newWAFPreferClient(&paths, func(path string) *http.Response {
		if mode == "normal" {
			if path == "/console/chat/completions" {
				return waf403()
			}
			return sseOK()
		}
		if path == "/v2/chat/completions" {
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"code":"11-128","msg":"request illegal"}`)),
			}
		}
		return sseOK()
	})
	c.WAFV2TTL = time.Minute
	a := &auth.Auth{UID: "acct-4", AccessToken: "token", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999}
	if err := callChat(t, c, a); err != nil {
		t.Fatalf("seed call: %v", err)
	}
	mode = "channel"
	paths = nil
	if err := callChat(t, c, a); err != nil {
		t.Fatalf("fallback call: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/v2/chat/completions" || paths[1] != "/console/chat/completions" {
		t.Fatalf("fallback paths=%v", paths)
	}
	paths = nil
	if err := callChat(t, c, a); err != nil {
		t.Fatalf("after clear: %v", err)
	}
	if len(paths) != 1 || paths[0] != "/console/chat/completions" {
		t.Fatalf("after clear paths=%v want console only", paths)
	}
}
