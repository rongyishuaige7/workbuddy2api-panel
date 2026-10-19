package keystats

import (
	"path/filepath"
	"testing"
)

func TestRecordAndSnapshot(t *testing.T) {
	r := New("")
	r.Record("k1", 200, 10, 5)
	r.Record("k1", 500, 0, 0)
	e := r.Snapshot()["k1"]
	if e.Requests != 2 || e.Success != 1 || e.Errors != 1 {
		t.Fatalf("entry=%+v", e)
	}
	if e.PromptTokens != 10 || e.CompletionTokens != 5 {
		t.Fatalf("tokens=%+v", e)
	}
	if e.TodayRequests != 2 || e.TodayTokens != 15 {
		t.Fatalf("today=%+v", e)
	}
	if e.LastUsed.IsZero() {
		t.Fatal("last_used should be set")
	}
}

func TestDayRollover(t *testing.T) {
	r := New("")
	r.Record("k1", 200, 1, 1)
	r.mu.Lock()
	r.entries["k1"].Day = "2000-01-01"
	r.entries["k1"].TodayRequests = 99
	r.entries["k1"].TodayTokens = 99
	r.mu.Unlock()
	r.Record("k1", 200, 2, 3)
	e := r.Snapshot()["k1"]
	if e.TodayRequests != 1 || e.TodayTokens != 5 {
		t.Fatalf("today should reset: %+v", e)
	}
	if e.Requests != 2 {
		t.Fatalf("totals must survive day rollover: %+v", e)
	}
}

func TestPersistAndLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key_usage.json")
	r := New(p)
	r.Record("k1", 200, 3, 4)
	r.flush()

	r2 := New(p)
	if err := r2.Load(); err != nil {
		t.Fatal(err)
	}
	e := r2.Snapshot()["k1"]
	if e.Requests != 1 || e.PromptTokens != 3 || e.CompletionTokens != 4 {
		t.Fatalf("loaded=%+v", e)
	}
}
