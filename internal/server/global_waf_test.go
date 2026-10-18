package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

const wafPage = `<html><title>WAF Block Page</title></html>`

func TestWAFRotationScopedToRealm(t *testing.T) {
	for _, tc := range []struct {
		name, realm, domain string
		stream              bool
		wantStatus, calls   int
	}{
		{"global_sync", "global", "www.workbuddy.ai", false, 200, 5},
		{"global_stream", "global", "www.workbuddy.ai", true, 200, 5},
		{"cn_unchanged", "cn", "www.codebuddy.cn", false, 503, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			up := newFakeUpstream(t, func(authz string) (int, string, bool) {
				calls++
				if authz == "Bearer good" {
					return http.StatusOK, sseOK, true
				}
				return http.StatusForbidden, wafPage, false
			})
			up.GlobalEnabled = true
			p := testPoolWith(
				&auth.Auth{UID: "bad1", AccessToken: "bad1", Domain: tc.domain, ExpiresAt: 9999999999},
				&auth.Auth{UID: "bad2", AccessToken: "bad2", Domain: tc.domain, ExpiresAt: 9999999999},
				&auth.Auth{UID: "good", AccessToken: "good", Domain: tc.domain, ExpiresAt: 9999999999},
			)
			p.SetCredits("bad1", 3000, 3000)
			p.SetCredits("bad2", 2000, 2000)
			h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true, GlobalWafCooldown: 5 * time.Second})
			body := fmt.Sprintf(`{"model":"%s:deepseek-v4.1-flash","messages":[{"role":"user","content":"hello"}],"stream":%v}`, tc.realm, tc.stream)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
			if rec.Code != tc.wantStatus || calls != tc.calls {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, calls, rec.Body)
			}
			if tc.realm == "global" && h.wafIP.active() {
				t.Fatal("global failures must not activate the CN IP gate")
			}
			for _, uid := range []string{"bad1", "bad2"} {
				st, _ := p.Status(uid)
				if !st.Cooling || st.InFlight != 0 {
					t.Fatalf("failed account must cool down and release its lease: %+v", st)
				}
			}
		})
	}
}

func TestGlobalWAFRetriesAreBoundedAndIsolatedFromCN(t *testing.T) {
	calls := 0
	up := newFakeUpstream(t, func(_ string) (int, string, bool) {
		calls++
		return http.StatusForbidden, wafPage, false
	})
	up.GlobalEnabled = true
	p := testPoolWith()
	for i := 0; i < 5; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprint(i), Domain: "www.workbuddy.ai", ExpiresAt: 9999999999})
	}
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true, MaxRotate: 3})
	h.wafIP.noteWaf("cn1")
	h.wafIP.noteWaf("cn2")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"global:deepseek-v4.1-flash","messages":[]}`)))
	if calls != 6 || rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"code":"waf_blocked"`) {
		t.Fatalf("calls=%d status=%d body=%s", calls, rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "waf_ip_blocked") {
		t.Fatal("CN IP gate must not relabel global errors")
	}
}
