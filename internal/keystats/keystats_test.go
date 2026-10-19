package keystats

import (
	"path/filepath"
	"testing"
	"time"
)

func TestRecordAndSnapshot(t *testing.T) {
	r := New("")
	r.Record("k1", "model-a", 200, 10, 5)
	r.Record("k1", "model-a", 500, 0, 0)
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
	today := time.Now().Format("2006-01-02")
	d := e.Days[today]
	if d == nil || d.Requests != 2 || d.PromptTokens != 10 || d.CompletionTokens != 5 {
		t.Fatalf("day=%+v", d)
	}
	m := d.Models["model-a"]
	if m == nil || m.Requests != 2 || m.PromptTokens != 10 {
		t.Fatalf("model=%+v", m)
	}
}

func TestDayRollover(t *testing.T) {
	r := New("")
	r.Record("k1", "model-a", 200, 1, 1)
	r.mu.Lock()
	r.entries["k1"].Day = "2000-01-01"
	r.entries["k1"].TodayRequests = 99
	r.entries["k1"].TodayTokens = 99
	r.mu.Unlock()
	r.Record("k1", "model-a", 200, 2, 3)
	e := r.Snapshot()["k1"]
	if e.TodayRequests != 1 || e.TodayTokens != 5 {
		t.Fatalf("today should reset: %+v", e)
	}
	if e.Requests != 2 {
		t.Fatalf("totals must survive day rollover: %+v", e)
	}
	if _, ok := e.Days["2000-01-01"]; ok {
		t.Fatal("stale day bucket should be pruned")
	}
}

func TestPersistAndLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "key_usage.json")
	r := New(p)
	r.Record("k1", "model-a", 200, 3, 4)
	r.flush()

	r2 := New(p)
	if err := r2.Load(); err != nil {
		t.Fatal(err)
	}
	e := r2.Snapshot()["k1"]
	if e.Requests != 1 || e.PromptTokens != 3 || e.CompletionTokens != 4 {
		t.Fatalf("loaded=%+v", e)
	}
	today := time.Now().Format("2006-01-02")
	if d := e.Days[today]; d == nil || d.Requests != 1 {
		t.Fatalf("day history not persisted: %+v", e.Days)
	}
}

func TestDailyUsageTodayTokensPruneRemove(t *testing.T) {
	r := New("")
	r.Record("k1", "model-a", 200, 10, 5)
	r.Record("k1", "model-b", 200, 1, 2)
	if got := r.TodayTokens("k1"); got != 18 {
		t.Fatalf("TodayTokens=%d", got)
	}
	u := r.DailyUsage("k1", 3)
	if len(u) != 3 {
		t.Fatalf("days=%d", len(u))
	}
	last := u[len(u)-1]
	if last.Requests != 2 || last.PromptTokens != 11 || last.CompletionTokens != 7 {
		t.Fatalf("last=%+v", last)
	}
	if len(last.Models) != 2 || last.Models["model-a"].PromptTokens != 10 {
		t.Fatalf("models=%+v", last.Models)
	}
	// 缺省日补零
	if u[0].Requests != 0 {
		t.Fatalf("empty day should be zero: %+v", u[0])
	}

	r.Record("k2", "", 200, 0, 0)
	if n := r.Prune(map[string]bool{"k1": true}); n != 1 {
		t.Fatalf("pruned=%d", n)
	}
	if _, ok := r.Snapshot()["k2"]; ok {
		t.Fatal("orphan should be pruned")
	}
	r.Remove("k1")
	if len(r.Snapshot()) != 0 {
		t.Fatal("removed key should be gone")
	}
}
