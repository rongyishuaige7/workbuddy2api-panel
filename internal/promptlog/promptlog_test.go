package promptlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCaptureKeepsFullFieldsAndTools(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"messages": []any{
			map[string]any{"role": "developer", "content": "sys"},
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": `{"path":"a"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "call_1", "content": "file-body"},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "read_file"}},
			map[string]any{"type": "function", "function": map[string]any{"name": "write_file"}},
		},
	})
	res := Capture(body, 1000)
	if len(res.Messages) != 4 {
		t.Fatalf("messages=%d", len(res.Messages))
	}
	if res.Messages[1]["content"] != "hello" {
		t.Fatalf("content=%v", res.Messages[1]["content"])
	}
	if _, ok := res.Messages[2]["tool_calls"]; !ok {
		t.Fatal("tool_calls must be preserved")
	}
	if res.Messages[3]["tool_call_id"] != "call_1" {
		t.Fatalf("tool_call_id=%v", res.Messages[3]["tool_call_id"])
	}
	if len(res.Tools) != 2 || res.Tools[0] != "read_file" || res.Tools[1] != "write_file" {
		t.Fatalf("tools=%v", res.Tools)
	}
}

func TestCaptureTailFirstAndOmitted(t *testing.T) {
	msgs := make([]any, 0, 6)
	for i := 1; i <= 6; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": strings.Repeat("x", 100) + "-" + string(rune('0'+i))})
	}
	body, _ := json.Marshal(map[string]any{"messages": msgs})
	res := Capture(body, 250)
	if len(res.Messages) == 0 || res.Omitted == 0 {
		t.Fatalf("messages=%d omitted=%d", len(res.Messages), res.Omitted)
	}
	last := res.Messages[len(res.Messages)-1]["content"].(string)
	if !strings.HasSuffix(last, "-6") {
		t.Fatalf("newest message should be kept, last=%q", last)
	}
	if len(res.Messages)+res.Omitted != 6 {
		t.Fatalf("messages=%d omitted=%d", len(res.Messages), res.Omitted)
	}
}

func TestCaptureTruncatesLongContent(t *testing.T) {
	big := strings.Repeat("中", 30000)
	body, _ := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": big},
	}})
	res := Capture(body, 1000)
	if len(res.Messages) != 1 || !res.Truncated {
		t.Fatalf("messages=%d truncated=%v", len(res.Messages), res.Truncated)
	}
	s, _ := res.Messages[0]["content"].(string)
	if len([]rune(s)) > 1000 {
		t.Fatalf("content runes=%d", len([]rune(s)))
	}

	if r := Capture([]byte(`{}`), 1000); r.Messages != nil {
		t.Fatal("no messages should capture nothing")
	}
	if r := Capture([]byte(`not-json`), 1000); r.Messages != nil {
		t.Fatal("invalid json should capture nothing")
	}
}

func TestRecordListClearPrune(t *testing.T) {
	dir := t.TempDir()
	r := New(Config{Dir: dir, RetentionDays: 3, MaxBytes: 1 << 20})
	now := time.Now()
	if err := r.Record(Entry{Time: now, KeyID: "k1", KeyName: "a", Model: "m", Status: 200, Messages: []Message{{"role": "user", "content": "one"}}}); err != nil {
		t.Fatal(err)
	}
	_ = r.Record(Entry{Time: now, KeyID: "k1", KeyName: "a", Model: "m", Status: 200, Messages: []Message{{"role": "user", "content": "two"}}})
	_ = r.Record(Entry{Time: now, KeyID: "k2", KeyName: "b", Model: "m", Status: 200, Messages: []Message{{"role": "user", "content": "other"}}})

	list, err := r.List("k1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Messages[0]["content"] != "two" {
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

	removed, err := r.Prune(map[string]bool{"k1": true})
	if err != nil || removed != 1 {
		t.Fatalf("prune removed=%d err=%v", removed, err)
	}
	if list, _ := r.List("k2", 10); len(list) != 0 {
		t.Fatalf("k2 should be pruned: %d", len(list))
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

func TestCaptureHeadAndTail(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "SYS"},
		map[string]any{"role": "user", "content": "ORIGINAL-TASK"},
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs, map[string]any{"role": "tool", "content": strings.Repeat("y", 200) + "-" + string(rune('0'+i))})
	}
	msgs = append(msgs, map[string]any{"role": "user", "content": "LATEST"})
	body, _ := json.Marshal(map[string]any{"messages": msgs})
	res := Capture(body, 800)
	if res.Omitted == 0 {
		t.Fatalf("middle messages should be omitted, messages=%d", len(res.Messages))
	}
	joined := ""
	for _, m := range res.Messages {
		if s, ok := m["content"].(string); ok {
			joined += s + "\n"
		}
	}
	if !strings.Contains(joined, "ORIGINAL-TASK") || !strings.Contains(joined, "LATEST") {
		t.Fatalf("head and tail must both survive, got %q", joined)
	}
}

func TestCounts(t *testing.T) {
	r := New(Config{Dir: t.TempDir(), RetentionDays: 3, MaxBytes: 1 << 20})
	now := time.Now()
	_ = r.Record(Entry{Time: now, KeyID: "k1", Messages: []Message{{"role": "user", "content": "a"}}})
	_ = r.Record(Entry{Time: now, KeyID: "k1", Messages: []Message{{"role": "user", "content": "b"}}})
	_ = r.Record(Entry{Time: now, KeyID: "k2", Messages: []Message{{"role": "user", "content": "c"}}})
	c := r.Counts()
	if c["k1"] != 2 || c["k2"] != 1 {
		t.Fatalf("counts=%v", c)
	}
	if _, err := r.Clear("k1"); err != nil {
		t.Fatal(err)
	}
	c = r.Counts()
	if c["k1"] != 0 || c["k2"] != 1 {
		t.Fatalf("after clear counts=%v", c)
	}
}

func TestIsHarnessContext(t *testing.T) {
	yes := []string{
		"Time sampled while preparing turn 1, step 1: ...",
		"Browser time zone for this request: Asia/Shanghai.",
		"Current runtime context. This snapshot supersedes...",
		"<system-reminder> A skill is a reusable set of instructions...",
		"background job pwsh-123 (pwsh: ...)",
		"Instructions from: ~/.dsh/AGENTS.md",
		"Current working directory: /x",
	}
	for _, s := range yes {
		if !IsHarnessContext(s) {
			t.Errorf("should be harness context: %q", s)
		}
	}
	for _, s := range []string{"牛魔", "帮我看看这个 bug", "Time is money", ""} {
		if IsHarnessContext(s) {
			t.Errorf("should be user input: %q", s)
		}
	}
}

func TestCaptureFlattensMultimodal(t *testing.T) {
	body, _ := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "这是真正的输入"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,AAAA"}},
		}},
	}})
	res := Capture(body, 1000)
	if len(res.Messages) != 1 {
		t.Fatalf("messages=%d", len(res.Messages))
	}
	s, ok := res.Messages[0]["content"].(string)
	if !ok || !strings.Contains(s, "这是真正的输入") || !strings.Contains(s, "[图片]") {
		t.Fatalf("flattened=%q", s)
	}
}
