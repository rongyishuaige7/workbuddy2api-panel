package server

import (
	"fmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNonStreamSSEErrorUsesFailurePolicy(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		t.Run(map[bool]string{false: "single", true: "rotate"}[rotate], func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "bad", AccessToken: "fake-bad", ExpiresAt: 9999999999})
			if rotate {
				p.Add(&auth.Auth{UID: "good", AccessToken: "fake-good", ExpiresAt: 9999999999})
			}
			p.SetCredits("bad", 2000, 0)
			s := session.New(session.Config{})
			up := newFakeUpstream(t, func(a string) (int, string, bool) {
				if a == "Bearer fake-good" {
					return 200, sseOK, true
				}
				return 200, "data: {\"error\":{\"code\":6004,\"message\":\"daily token usage exceeds limit\"}}\n\ndata: [DONE]\n\n", true
			})
			h := NewHandler(Config{Pool: p, Upstream: up, Session: s})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","conversation_id":"failure-test","messages":[],"stream":false}`)))
			st, _ := p.Status("bad")
			if st.SuccessCount != 0 {
				t.Fatalf("failed account recorded success: %d", st.SuccessCount)
			}
			if rotate {
				if w.Code != 200 {
					t.Fatalf("status=%d body=%s", w.Code, w.Body)
				}
			} else {
				if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "6004") {
					t.Fatalf("status=%d body=%s", w.Code, w.Body)
				}
				if s.Count() != 0 {
					t.Fatalf("failed request bound a session")
				}
			}
		})
	}
}

func TestChannelValidationDoesNotDegrade(t *testing.T) {
	for _, mode := range []string{"passthrough", "append"} {
		t.Run(mode, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "a", AccessToken: "fake-a", ExpiresAt: 9999999999})
			calls := 0
			up := newFakeUpstream(t, func(a string) (int, string, bool) {
				calls++
				return 400, `{"code":11128,"msg":"request illegal: channel validation failed"}`, false
			})
			h := NewHandler(Config{Pool: p, Upstream: up, PromptMode: mode})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","messages":[],"stream":false}`)))
			if h.degrade.Active() || calls != 1 || w.Code != 400 || !strings.Contains(w.Body.String(), "channel_validation_failed") {
				t.Fatalf("degraded=%v calls=%d status=%d", h.degrade.Active(), calls, w.Code)
			}
			st, _ := p.Status("a")
			if st.Cooling || st.Disabled || st.ErrTotal != 0 {
				t.Fatal("channel failure penalized account")
			}
		})
	}
}

func TestNewSessionUsesPoolPolicy(t *testing.T) {
	p := pool.New("")
	p.SetCostExploreInterval(0)
	for _, u := range []string{"free", "paid"} {
		p.Add(&auth.Auth{UID: u, AccessToken: "fake-" + u, ExpiresAt: time.Now().Add(time.Hour).Unix()})
		p.SetCredits(u, 350, 350)
	}
	p.NoteModelCost("free", "glm-5.2", 0, 1000)
	p.NoteModelCost("paid", "glm-5.2", 1, 1000)
	s := session.New(session.Config{AvailableForModel: func(m string) []string { return p.WeightedAvailableUIDsForModelRealm(m, "cn") }})
	selected := ""
	up := newFakeUpstream(t, func(a string) (int, string, bool) { selected = a; return 200, sseOK, true })
	h := NewHandler(Config{Pool: p, Upstream: up, Session: s})
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","conversation_id":"b","messages":[{"role":"user","content":"test"}],"stream":false}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status=%d", w.Code)
	}
	if selected != "Bearer fake-free" {
		t.Fatal("new-session hash allocation bypassed free-account routing policy and selected paid account")
	}
}

func TestTemporaryOffloadPreservesBinding(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "a", AccessToken: "fake-a", ExpiresAt: 9999999999}, &auth.Auth{UID: "b", AccessToken: "fake-b", ExpiresAt: 9999999999})
			p.SetMaxInFlight(2)
			p.SetCostExploreInterval(0)
			p.NoteModelCost("a", "glm-5.2", 0, 1000)
			p.NoteModelCost("b", "glm-5.2", 1, 1000)
			held := p.AcquireLease("a")
			defer held.Release()
			s := session.New(session.Config{})
			s.Bind("offload-session", "a")
			chosen := ""
			up := newFakeUpstream(t, func(a string) (int, string, bool) { chosen = a; return 200, sseOK, true })
			h := NewHandler(Config{Pool: p, Upstream: up, Session: s})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(fmt.Sprintf(`{"model":"glm-5.2","conversation_id":"offload-session","messages":[],"stream":%v}`, stream))))
			if w.Code != 200 || chosen != "Bearer fake-b" {
				t.Fatalf("status=%d; offload did not select idle paid account", w.Code)
			}
			if uid, ok := s.Lookup("offload-session"); !ok || uid != "a" {
				t.Fatal("temporary offload overwrote original binding")
			}
		})
	}
}
