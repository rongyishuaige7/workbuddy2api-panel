package upstream

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAggregateBusinessErrors(t *testing.T) {
	for _, tc := range []struct {
		payload string
		kind    ErrKind
	}{
		{`{"error":{"code":6004,"message":"daily token usage exceeds limit"}}`, ErrSoftRate},
		{`{"error":{"code":11102,"message":"service info not found"}}`, ErrModelBlocked},
		{`{"error":{"code":12153,"message":"session expired"}}`, ErrSessionDead},
		{`{"error":{"code":11128,"message":"request illegal: channel validation failed"}}`, ErrChannel},
		{`{"error":{"message":"unknown failure"}}`, ErrClient},
		{`{"code":6004,"msg":"daily token usage exceeds limit"}`, ErrSoftRate},
	} {
		t.Run(tc.kind.String()+tc.payload, func(t *testing.T) {
			_, err := Aggregate(strings.NewReader("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: " + tc.payload + "\n\ndata: [DONE]\n\n"))
			var ue *Error
			if !errors.As(err, &ue) || ue.Kind != tc.kind || ue.Msg != tc.payload {
				t.Fatalf("got %v, want %s with original envelope", err, tc.kind)
			}
			var observed string
			rec := httptest.NewRecorder()
			if err := StreamHint(rec, strings.NewReader("data: "+tc.payload+"\n\ndata: [DONE]\n\n"), nil, WithErrorFrameObserver(func(s string) { observed = s })); err != nil {
				t.Fatal(err)
			}
			if observed != tc.payload {
				t.Fatalf("observer=%q", observed)
			}
		})
	}
	if FrameError(`{"code":0,"error":null,"choices":[]}`) != nil {
		t.Fatal("success envelope classified as error")
	}
}
