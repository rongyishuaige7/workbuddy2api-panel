package promptlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCapture(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hello"},{"role":"user","content":[{"type":"text","text":"img"}]}]}`)
	msgs, truncated, bytes := Capture(body, 1000)
	if len(msgs) != 3 || truncated || bytes == 0 {
		t.Fatalf("msgs=%d truncated=%v bytes=%d", len(msgs), truncated, bytes)
	}
	if msgs[1].Content != "hello" {
		t.Fatalf("content=%v", msgs[1].Content)
	}
	raw, _ := json.Marshal(msgs[2].Content)
	if !strings.Contains(string(raw), `"type":"text"`) {
		t.Fatalf("array content lost: %s", raw)
	}

	big := strings.Repeat("x", 30000)
	msgs, truncated, _ = Capture([]byte(`{"messages":[{"role":"user","content":"`+big+`"}]}`), 1000)
	if len(msgs) != 1 || !truncated {
		t.Fatalf("truncate: msgs=%d truncated=%v", len(msgs), truncated)
	}
	if s, ok := msgs[0].Content.(string); !ok || len([]rune(s)) > 1000 {
		t.Fatalf("truncated content len=%d", len([]rune(msgs[0].Content.(string))))
	}

	if m, _, _ := Capture([]byte(`{}`), 1000); m != nil {
		t.Fatal("no messages should capture nothing")
	}
	if m, _, _ := Capture([]byte(`not-json`), 1000); m != nil {
		t.Fatal("invalid json should capture nothing")
	}
}

func TestRecordListClear(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, RetentionDays: 3, MaxBytes: 1 << 20})
	now := time.Now()
	if err := r.Record(Entry{Time: now, KeyID: "k1", KeyName: "a", Model: "m", Status: 200, Messages: []Message{{Role: "user", Content: "one"}}}); err != nil {
		t.Fatal(err)
	}
	_ = r.Record(Entry{Time: now, KeyID: "k1", KeyName: "a", Model: "m", Status: 200, Messages: []Message{{Role: "user", Content: "two"}}})
	_ = r.Record(Entry{Time: now, KeyID: "k2", KeyName: "b", Model: "m", Status: 200, Messages: []Message{{Role: "user", Content: "other"}}})

	list, err := r.List("k1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Messages[0].Content != "two" {
		t.Fatalf("list=%+v", list)
	}
	n, err := r.Clear("k1")
	if err != nil || n != 2 {
		t.Fatalf("clear n=%d err=%v", n, err)
	}
	if list, _ := r.List("k1", 10); len(list) != 0 {
		t.Fatalf("k1 should be empty: %d", len(list))
	}
	if list, _ := r.List("k2", 10); len(list) != 1 {
		t.Fatalf("k2 should survive: %d", len(list))
	}
	r.Close()
}

func TestPruneRetention(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "prompts-2020-01-01.jsonl")
	if err := os.WriteFile(old, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(Config{Dir: dir, RetentionDays: 3, MaxBytes: 1 << 20})
	r.mu.Lock()
	r.pruneLocked(time.Now())
	r.mu.Unlock()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old prompt log should be pruned")
	}
}
