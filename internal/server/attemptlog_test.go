package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/reqlog"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
)

func TestGlobalAttemptArchiveRecordsRateLimitMigration(t *testing.T) {
	const sessionID = "private-conversation-id"
	const privateText = "private-prompt-never-in-archive"
	const upstreamID = "a6774c8b46f486372b0107d84c19b15a"
	dir := t.TempDir()
	rlog := reqlog.New(reqlog.Config{Dir: dir, Enabled: true})
	t.Cleanup(rlog.Close)
	up := newFakeUpstream(t, nil)
	up.GlobalEnabled = true
	up.HTTP.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		status, body := http.StatusOK, sseOK
		headers := http.Header{}
		if r.Header.Get("Authorization") == "Bearer private-bad-token" {
			status = http.StatusTooManyRequests
			body = `{"code":14003,"msg":"too many requests","requestId":"` + upstreamID + `"}`
			headers.Set("Retry-After", "60")
		}
		return &http.Response{StatusCode: status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	p := testPoolWith(
		&auth.Auth{UID: "bad-account-full-uid", AccessToken: "private-bad-token", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
		&auth.Auth{UID: "good-account-full-uid", AccessToken: "private-good-token", Domain: "www.workbuddy.ai", ExpiresAt: 9999999999},
	)
	sess := session.New(session.Config{})
	sess.Bind(sessionID, "bad-account-full-uid")
	h := NewHandler(Config{Pool: p, Upstream: up, GlobalEnabled: true, RequestLog: rlog, Session: sess})
	body := fmt.Sprintf(`{"model":"global:deepseek-v4.1-flash","conversation_id":%q,"messages":[{"role":"user","content":%q}]}`, sessionID, privateText)
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status=%d body=%s", i, rec.Code, rec.Body)
		}
	}
	snapshot := rlog.Snapshot()
	first, second := snapshot.Recent[1], snapshot.Recent[0]
	if first.SessionHash == "" || first.SessionHash == sessionID || first.SessionHash != second.SessionHash || first.RequestBytes != len(body) {
		t.Fatalf("session/size diagnostics first=%+v second=%+v", first, second)
	}
	if first.Attempts != 2 || len(first.AttemptDetails) != 2 {
		t.Fatalf("migration attempts=%+v", first)
	}
	failed, success := first.AttemptDetails[0], first.AttemptDetails[1]
	if failed.UpstreamStatus != 429 || failed.ErrorKind != "soft_rate" || failed.BusinessCode != "14003" || failed.UpstreamRequestID != upstreamID || failed.RetryAfterMs != 60_000 || failed.Route != "sticky" || failed.Realm != "global" {
		t.Fatalf("failed hop=%+v", failed)
	}
	if success.UpstreamStatus != 200 || success.ErrorKind != "" || success.Route != "balanced" || success.PromptTokens != 1 {
		t.Fatalf("success hop=%+v", success)
	}
	if len(second.AttemptDetails) != 1 || second.AttemptDetails[0].Route != "sticky" {
		t.Fatalf("second request should stick to recovered route: %+v", second)
	}
	if failed.StartedAt.IsZero() || failed.DurationMs < 1 {
		t.Fatalf("hop timing missing: %+v", failed)
	}
	rlog.Close()
	files, err := filepath.Glob(filepath.Join(dir, "requests-*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("archive files=%v err=%v", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{sessionID, privateText, "private-bad-token", "private-good-token", "bad-account-full-uid", "too many requests"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("archive contains private input %q", secret)
		}
	}
	var archived reqlog.Event
	if err := json.Unmarshal([]byte(strings.SplitN(string(raw), "\n", 2)[0]), &archived); err != nil || len(archived.AttemptDetails) != 2 {
		t.Fatalf("archive did not preserve migration diagnostics: %+v err=%v", archived, err)
	}
}

func TestAttemptArchiveDistinguishesSSEErrorFromHTTPStatus(t *testing.T) {
	up := newFakeUpstream(t, func(_ string) (int, string, bool) {
		return 200, "data: {\"error\":{\"message\":\"{\\\"code\\\":6004,\\\"msg\\\":\\\"rate limit\\\"}\"}}\n\ndata: [DONE]\n\n", true
	})
	rlog := reqlog.New(reqlog.Config{})
	h := NewHandler(Config{Pool: testPoolWith(&auth.Auth{UID: "a", ExpiresAt: 9999999999}), Upstream: up, RequestLog: rlog})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"glm-5.2","stream":true,"messages":[]}`)))
	e := rlog.Snapshot().Recent[0]
	if e.OK || len(e.AttemptDetails) != 1 {
		t.Fatalf("unexpected event: %+v", e)
	}
	a := e.AttemptDetails[0]
	if a.UpstreamStatus != 200 || a.ErrorKind != "soft_rate" || a.BusinessCode != "6004" {
		t.Fatalf("SSE business error must retain HTTP 200 plus its classification: %+v", a)
	}
}

func TestAttemptDiagnosticBoundsAndPrivacy(t *testing.T) {
	for _, body := range []string{
		`{"code":"secret-token","requestId":"Bearer secret-token","msg":"private"}`,
		strings.Repeat("x", (1<<20)+1),
	} {
		if code, id := errorDiagnosticIDs(body); code != "" || id != "" {
			t.Fatalf("untrusted IDs escaped whitelist: code=%q id=%q", code, id)
		}
	}
	s := &chatStat{}
	for i := 0; i < 100; i++ {
		s.recordAttemptDetail(reqlog.Attempt{Account: fmt.Sprint(i)}, time.Now(), 1)
	}
	if len(s.attemptDetails) > maxAttemptDetails || s.attemptDetails[0].Account != "0" || s.attemptDetails[len(s.attemptDetails)-1].Account != "99" {
		t.Fatalf("bounded archive lost first/final hop: %+v", s.attemptDetails)
	}
}
