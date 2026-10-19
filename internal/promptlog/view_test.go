package promptlog

import (
	"strings"
	"testing"
	"time"
)

func viewEntry(t *testing.T, id, session string, texts ...string) Entry {
	t.Helper()
	messages := []Message{}
	for _, text := range texts {
		messages = append(messages, Message{"role": "user", "content": text})
	}
	r := captureMessages(t, messages, 10000)
	return Entry{Version: CaptureVersion, KeyID: "k", RequestID: id, SessionHash: session, Time: time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC).Add(time.Duration(id[0]) * time.Minute), Messages: r.Messages, MessageIndexes: r.MessageIndexes, Inputs: r.Inputs}
}
func TestFreshViewSequenceAndSessions(t *testing.T) {
	a := viewEntry(t, "a", "one", "task", "继续")
	b := viewEntry(t, "b", "one", "task", "继续", "继续")
	c := viewEntry(t, "c", "two", "task", "继续")
	d := viewEntry(t, "z", "one", "task", "继续", "继续") // >10 minutes: tool continuation
	out := FilterEntries([]Entry{d, c, b, a}, "fresh")
	if out[0].FreshState != "no_new" || out[1].FreshReason != "window_start" || out[2].FreshState != "new" || out[3].FreshReason != "window_start" {
		t.Fatalf("states: %+v", out)
	}
	if len(out[2].Messages) != 1 || out[2].Messages[0]["content"] != "继续" {
		t.Fatal("repeated legitimate input was suppressed")
	}
	if len(b.Messages) != 3 || len(out[2].MessageIndexes) != 1 || out[2].MessageIndexes[0] != 2 {
		t.Fatal("view mutated archive or returned invalid indexes")
	}
}
func TestFreshViewUnknownAndReset(t *testing.T) {
	a := viewEntry(t, "a", "s", "old")
	b := viewEntry(t, "b", "s", "compressed")
	c := viewEntry(t, "c", "s", "compressed", "new")
	b.InputsOmitted = 1
	out := FilterEntries([]Entry{a, b, c}, "fresh")
	if out[0].FreshReason != "window_start" || out[1].FreshReason != "metadata_incomplete" || out[2].FreshReason != "window_start" {
		t.Fatal("incomplete metadata did not reset comparison")
	}
	b.InputsOmitted = 0
	out = FilterEntries([]Entry{a, b, c}, "fresh")
	if out[1].FreshReason != "history_changed" || out[2].FreshState != "new" {
		t.Fatal("compressed history was asserted as new")
	}
	a.Version = 0
	if e := FilterEntries([]Entry{a}, "fresh")[0]; e.FreshReason != "legacy" {
		t.Fatal("legacy must be unknown")
	}
	a.Version = CaptureVersion
	a.SessionHash = ""
	if e := FilterEntries([]Entry{a}, "fresh")[0]; e.FreshReason != "no_session" {
		t.Fatal("unidentified session must be unknown")
	}
}
func TestFreshViewOmittedTextAndTruncation(t *testing.T) {
	a := viewEntry(t, "a", "s", strings.Repeat("x", 100))
	b := viewEntry(t, "b", "s", strings.Repeat("x", 100), "new input")
	a.Messages[0]["content"] = "x"
	a.Inputs[0].Truncated = true
	b.Messages = b.Messages[:1]
	b.MessageIndexes = b.MessageIndexes[:1]
	b.Inputs[1].Captured = false
	out := FilterEntries([]Entry{a, b}, "fresh")
	if out[1].FreshState != "new" || len(out[1].Messages) != 1 || !strings.Contains(out[1].Messages[0]["content"].(string), "正文未保留") {
		t.Fatal("omitted new input disappeared")
	}
}
