package promptlog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeArchive(t *testing.T, dir, name string, entries ...Entry) string {
	t.Helper()
	var b bytes.Buffer
	for _, e := range entries {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func TestRewriteKeepsOtherKeysAndLargeLegacyRows(t *testing.T) {
	r := New(Config{Dir: t.TempDir()})
	defer r.Close()
	now := time.Now()
	name := "prompts-" + now.Format("2006-01-02") + ".jsonl"
	large := strings.Repeat("x", 9<<20)
	writeArchive(t, r.cfg.Dir, name, Entry{Time: now, KeyID: "remove"}, Entry{Time: now, KeyID: "keep", Messages: []Message{{"role": "user", "content": large}}}, Entry{Time: now, KeyID: "keep", RequestID: "tail"})
	n, err := r.Clear("remove")
	if err != nil || n != 1 {
		t.Fatalf("clear=%d %v", n, err)
	}
	entries, err := r.List("keep", 10)
	if err != nil || len(entries) != 2 {
		t.Fatalf("other key lost after large row: %d %v", len(entries), err)
	}
	if len(entries[1].Messages[0]["content"].(string)) != len(large) {
		t.Fatal("large legacy content changed")
	}
	counts, err := r.CountSnapshot()
	if err != nil || counts["keep"] != 2 {
		t.Fatalf("counts=%v %v", counts, err)
	}
}
func TestDamagedArchiveCannotReplaceOriginal(t *testing.T) {
	r := New(Config{Dir: t.TempDir()})
	defer r.Close()
	path := writeArchive(t, r.cfg.Dir, "prompts-2026-10-19.jsonl", Entry{KeyID: "remove"}, Entry{KeyID: "keep"})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("{broken\n")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	before, _ := os.ReadFile(path)
	if n, err := r.Clear("remove"); err == nil || n != 0 {
		t.Fatalf("damaged archive incorrectly committed: %d %v", n, err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("damaged original was replaced with incomplete output")
	}
	if _, err := r.List("keep", 10); err == nil {
		t.Fatal("reader concealed corruption")
	}
	if _, err := r.CountSnapshot(); err == nil {
		t.Fatal("counter concealed corruption")
	}
	tmp, _ := filepath.Glob(filepath.Join(r.cfg.Dir, ".prompt-rewrite-*"))
	if len(tmp) != 0 {
		t.Fatal("rewrite temp file leaked")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("injected read failure") }

type failingWriter struct{ short bool }

func (w failingWriter) Write(b []byte) (int, error) {
	if w.short {
		return len(b) - 1, nil
	}
	return 0, errors.New("injected write failure")
}
func TestRewritePropagatesIOFailures(t *testing.T) {
	row := []byte("{\"key_id\":\"keep\"}\n")
	for _, reader := range []io.Reader{failingReader{}, io.MultiReader(bytes.NewReader(row), failingReader{})} {
		if _, err := rewriteStream(reader, io.Discard, func(rowHeader) bool { return true }); err == nil {
			t.Fatal("read failure ignored")
		}
	}
	for _, writer := range []io.Writer{failingWriter{}, failingWriter{short: true}} {
		if _, err := rewriteStream(bytes.NewReader(row), writer, func(rowHeader) bool { return true }); err == nil {
			t.Fatal("write failure ignored")
		}
	}
}
func TestStartupProtectsTodayThenRotatesOversizedLegacy(t *testing.T) {
	r := New(Config{Dir: t.TempDir(), MaxBytes: 1024})
	defer r.Close()
	now := time.Now()
	path := writeArchive(t, r.cfg.Dir, "prompts-"+now.Format("2006-01-02")+".jsonl", Entry{Time: now, KeyID: "keep", Messages: []Message{{"content": strings.Repeat("x", 5000)}}})
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("startup deleted only current-day archive")
	}
	if err := r.Record(Entry{KeyID: "keep", RequestID: "new"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("oversized closed legacy segment was not retired after rotation")
	}
	entries, err := r.List("keep", 10)
	if err != nil || len(entries) != 1 || entries[0].RequestID != "new" {
		t.Fatalf("rotation: %v %v", entries, err)
	}
}
func TestCapacityRotationAndFileOrder(t *testing.T) {
	r := New(Config{Dir: t.TempDir(), MaxBytes: 1200})
	defer r.Close()
	for i := 0; i < 20; i++ {
		if err := r.Record(Entry{KeyID: "k", RequestID: fmt.Sprint(i), Messages: []Message{{"content": strings.Repeat("x", 200)}}}); err != nil {
			t.Fatal(err)
		}
	}
	files, err := r.archivesLocked()
	if err != nil {
		t.Fatal(err)
	}
	var size int64
	for _, f := range files {
		info, err := os.Stat(f.path)
		if err != nil {
			t.Fatal(err)
		}
		size += info.Size()
	}
	if size > r.cfg.MaxBytes || len(files) == 0 {
		t.Fatalf("capacity not enforced: %d", size)
	}
	counts := r.Counts()
	entries, err := r.List("k", 200)
	if err != nil || counts["k"] != len(entries) || entries[0].RequestID != "19" {
		t.Fatalf("rotation/counts: %v %v", counts, err)
	}
	// Legacy base name must sort before numbered segments of the same day.
	dir := t.TempDir()
	writeArchive(t, dir, "prompts-2026-10-19.jsonl")
	writeArchive(t, dir, "prompts-2026-10-19-000002.jsonl")
	writeArchive(t, dir, "prompts-2026-10-19-000010.jsonl")
	other := New(Config{Dir: dir})
	f, _ := other.archivesLocked()
	if f[0].seq != 10 || f[1].seq != 2 || f[2].seq != 0 {
		t.Fatal("segment ordering is lexical instead of chronological")
	}
}
func TestRetentionIncludesTodayAndTightensPermissions(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0755)
	r := New(Config{Dir: dir, RetentionDays: 3})
	defer r.Close()
	now := time.Now()
	paths := make([]string, 4)
	for i := range paths {
		paths[i] = writeArchive(t, dir, "prompts-"+now.AddDate(0, 0, -i).Format("2006-01-02")+".jsonl", Entry{KeyID: "k"})
		os.Chmod(paths[i], 0644)
	}
	if err := r.Start(); err != nil {
		t.Fatal(err)
	}
	for i, path := range paths {
		info, err := os.Stat(path)
		if i == 3 {
			if !os.IsNotExist(err) {
				t.Fatal("retention=3 kept fourth date")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatal("file permissions too broad")
		}
	}
	info, _ := os.Stat(dir)
	if info.Mode().Perm() != 0700 {
		t.Fatal("directory permissions too broad")
	}
	if c := r.Counts(); c["k"] != 3 {
		t.Fatal("counts after retention incorrect")
	}
	// An idle append descriptor must not exempt an expired date forever.
	r.mu.Lock()
	r.file, _ = os.OpenFile(paths[2], os.O_APPEND|os.O_WRONLY, 0600)
	r.day = now.AddDate(0, 0, -2).Format("2006-01-02")
	err := r.pruneLocked(now.AddDate(0, 0, 3))
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if c := r.Counts(); c["k"] != 0 {
		t.Fatal("expired active segment survived retention or counts cache is stale")
	}
}
func TestListUsesStartTimeAndSnapshotSurvivesClear(t *testing.T) {
	r := New(Config{Dir: t.TempDir()})
	defer r.Close()
	now := time.Now()
	for _, i := range []int{3, 1, 2} {
		if err := r.Record(Entry{Time: now.Add(time.Duration(i) * time.Second), KeyID: "k", RequestID: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := r.List("k", 2)
	if err != nil || len(list) != 2 || list[0].RequestID != "3" || list[1].RequestID != "2" {
		t.Fatalf("start-time order: %v %v", list, err)
	}
	files, _, err := r.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer closeSnapshots(files)
	if _, err := r.Clear("k"); err != nil {
		t.Fatal(err)
	}
	n := 0
	err = walkLines(bufio.NewReader(io.NewSectionReader(files[0].file, 0, files[0].size)), func([]byte, int64) error { n++; return nil })
	if err != nil || n != 3 {
		t.Fatal("clear invalidated open snapshot")
	}
	if c := r.Counts(); c["k"] != 0 {
		t.Fatal("clear cache stale")
	}
}
func TestConcurrentListCountsClearAndRecord(t *testing.T) {
	r := New(Config{Dir: t.TempDir(), MaxBytes: 2 << 20})
	defer r.Close()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if err := r.Record(Entry{KeyID: "keep", RequestID: fmt.Sprint(i)}); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			if _, err := r.List("keep", 5); err != nil {
				t.Error(err)
			}
			if _, err := r.CountSnapshot(); err != nil {
				t.Error(err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			if _, err := r.Clear("other"); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	if c := r.Counts(); c["keep"] != 50 {
		t.Fatalf("concurrent operations lost records: %v", c)
	}
	if _, err := r.Prune(map[string]bool{}); err != nil {
		t.Fatal(err)
	}
	if c := r.Counts(); len(c) != 0 {
		t.Fatal("prune cache stale")
	}
}
func TestRecordBoundAndFinalNewline(t *testing.T) {
	r := New(Config{Dir: t.TempDir()})
	defer r.Close()
	if err := r.Record(Entry{KeyID: "k", Model: strings.Repeat("x", maxEntryBytes)}); err == nil {
		t.Fatal("oversized entry accepted")
	}
	now := time.Now()
	path := writeArchive(t, r.cfg.Dir, "prompts-"+now.Format("2006-01-02")+".jsonl", Entry{KeyID: "k", Time: now})
	raw, _ := os.ReadFile(path)
	if err := os.WriteFile(path, bytes.TrimSuffix(raw, []byte{'\n'}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.Record(Entry{KeyID: "k"}); err != nil {
		t.Fatal(err)
	}
	if c := r.Counts(); c["k"] != 2 {
		t.Fatal("append joined two JSON records")
	}
}
