package upstream

import (
	"errors"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"io"
	"net/http"
	"strings"
	"testing"
)

type probeReadFailure struct{}

func (probeReadFailure) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestReadProbeResponseLimitsAndReadFailures(t *testing.T) {
	if _, err := ReadProbeResponse(probeReadFailure{}); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error lost: %v", err)
	}
	huge := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n:" + strings.Repeat("x", 1<<20)
	if _, err := ReadProbeResponse(strings.NewReader(huge)); err == nil {
		t.Fatal("oversized response accepted")
	}
}

func TestNightChatDoesNotReportSSEErrorAsSuccess(t *testing.T) {
	calls := 0
	c := testClient(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("data:{\"code\":6004,\"msg\":\"token limit\"}\n\n"))}, nil
	})
	n, err := c.RunNightChats(&auth.Auth{UID: "a"}, 1)
	if n != 0 || err == nil || calls != 1 {
		t.Fatalf("successes=%d error=%v requests=%d", n, err, calls)
	}
}
