package promptlog

import (
	"bufio"
	"bytes"
	"container/heap"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxEntryBytes = 4 << 20
const maxLegacyLineBytes = 64 << 20
const maxSegmentBytes = 8 << 20

// Record appends bounded entries to the current ingestion day. Entry.Time remains
// the request start time, so long requests finishing after midnight sort correctly.
func (r *Recorder) Record(e Entry) error {
	if r == nil || r.cfg.Dir == "" {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	e.FreshState, e.FreshReason = "", ""
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if len(line) > maxEntryBytes {
		return fmt.Errorf("prompt entry exceeds %d bytes", maxEntryBytes)
	}
	line = append(line, '\n')
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if err := r.ensureFileLocked(now, len(line)); err != nil {
		return err
	}
	before := r.size
	n, err := r.file.Write(line)
	if err != nil || n != len(line) {
		if err == nil {
			err = io.ErrShortWrite
		}
		err = errors.Join(err, r.file.Truncate(before))
		return err
	}
	r.size += int64(n)
	r.generation++
	if r.counts != nil && e.KeyID != "" {
		r.counts[e.KeyID]++
	}
	// Capacity is enforced after each append, rather than only every hour.
	if err := r.pruneLocked(now); err != nil {
		log.Printf("[promptlog] cleanup: %v", err)
	}
	return nil
}

type archiveFile struct {
	path, day string
	seq       int
}

func parseArchive(name string) (string, int, bool) {
	if !strings.HasPrefix(name, "prompts-") || !strings.HasSuffix(name, ".jsonl") {
		return "", 0, false
	}
	middle := strings.TrimSuffix(strings.TrimPrefix(name, "prompts-"), ".jsonl")
	if len(middle) < 10 {
		return "", 0, false
	}
	day := middle[:10]
	if t, err := time.Parse("2006-01-02", day); err != nil || t.Format("2006-01-02") != day {
		return "", 0, false
	}
	if len(middle) == 10 {
		return day, 0, true
	}
	if len(middle) != 17 || middle[10] != '-' {
		return "", 0, false
	}
	seq, err := strconv.Atoi(middle[11:])
	return day, seq, err == nil && seq > 0
}
func dayFromName(name string) string {
	day, _, ok := parseArchive(name)
	if !ok {
		return ""
	}
	return day
}
func (r *Recorder) archivesLocked() ([]archiveFile, error) {
	entries, err := os.ReadDir(r.cfg.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []archiveFile{}
	for _, e := range entries {
		day, seq, ok := parseArchive(e.Name())
		if !ok {
			continue
		}
		if !e.Type().IsRegular() {
			return nil, fmt.Errorf("archive is not a regular file: %s", e.Name())
		}
		out = append(out, archiveFile{filepath.Join(r.cfg.Dir, e.Name()), day, seq})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].day != out[j].day {
			return out[i].day > out[j].day
		}
		return out[i].seq > out[j].seq
	})
	return out, nil
}

func (r *Recorder) secureDirLocked() error {
	if err := os.MkdirAll(r.cfg.Dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(r.cfg.Dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("prompt directory is not a directory")
	}
	return os.Chmod(r.cfg.Dir, 0700)
}
func (r *Recorder) closeFileLocked() error {
	if r.file == nil {
		return nil
	}
	err := r.file.Close()
	r.file = nil
	r.day = ""
	r.size = 0
	return err
}
func (r *Recorder) ensureFileLocked(now time.Time, incoming int) error {
	day := now.Format("2006-01-02")
	segment := min(int64(maxSegmentBytes), r.cfg.MaxBytes)
	if r.file != nil && r.day == day && (r.size == 0 || r.size+int64(incoming) <= segment) {
		return nil
	}
	if err := r.closeFileLocked(); err != nil {
		return err
	}
	if err := r.secureDirLocked(); err != nil {
		return err
	}
	files, err := r.archivesLocked()
	if err != nil {
		return err
	}
	seq := 0
	path := filepath.Join(r.cfg.Dir, "prompts-"+day+".jsonl")
	for _, f := range files {
		if f.day == day {
			seq = f.seq
			path = f.path
			break
		}
	}
	if info, err := os.Stat(path); err == nil {
		if info.Size() > 0 && info.Size()+int64(incoming) > segment {
			if seq >= 999999 {
				return errors.New("prompt archive segment limit reached")
			}
			seq++
			path = filepath.Join(r.cfg.Dir, fmt.Sprintf("prompts-%s-%06d.jsonl", day, seq))
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	fail := func(err error) error { return errors.Join(err, f.Close()) }
	if err := f.Chmod(0600); err != nil {
		return fail(err)
	}
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	size := info.Size()
	// Preserve a valid final JSON row without a newline, and isolate any damaged
	// trailing row. Readers report malformed data rather than silently losing it.
	if size > 0 {
		var tail [1]byte
		if _, err := f.ReadAt(tail[:], size-1); err != nil {
			return fail(err)
		}
		if tail[0] != '\n' {
			if _, err := f.Write([]byte{'\n'}); err != nil {
				return fail(err)
			}
			size++
		}
	}
	r.file = f
	r.day = day
	r.size = size
	return nil
}

type fileSnapshot struct {
	file *os.File
	size int64
}

func (r *Recorder) snapshot() ([]fileSnapshot, uint64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	files, err := r.archivesLocked()
	if err != nil {
		return nil, 0, err
	}
	out := []fileSnapshot{}
	fail := func(err error) ([]fileSnapshot, uint64, error) { closeSnapshots(out); return nil, 0, err }
	for _, p := range files {
		f, err := os.Open(p.path)
		if err != nil {
			return fail(err)
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return fail(err)
		}
		out = append(out, fileSnapshot{f, info.Size()})
	}
	return out, r.generation, nil
}
func closeSnapshots(files []fileSnapshot) {
	for _, f := range files {
		_ = f.file.Close()
	}
}

// walkLines reuses a buffer across rows and supports old rows beyond Scanner's
// former 8 MiB cap. Callback bytes are borrowed only for its duration.
func walkLines(reader *bufio.Reader, fn func([]byte, int64) error) error {
	var scratch []byte
	var offset int64
	for {
		part, err := reader.ReadSlice('\n')
		if len(scratch)+len(part) > maxLegacyLineBytes {
			return fmt.Errorf("archive row exceeds %d bytes", maxLegacyLineBytes)
		}
		if err == bufio.ErrBufferFull {
			scratch = append(scratch, part...)
			continue
		}
		var line []byte
		if len(scratch) > 0 {
			scratch = append(scratch, part...)
			line = scratch
		} else {
			line = part
		}
		if len(line) > 0 {
			if callErr := fn(line, offset); callErr != nil {
				return callErr
			}
			offset += int64(len(line))
		}
		scratch = scratch[:0]
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type rowHeader struct {
	KeyID     string    `json:"key_id"`
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id"`
}

func parseHeader(line []byte) (rowHeader, error) {
	var h rowHeader
	err := json.Unmarshal(line, &h)
	return h, err
}

type recentRow struct {
	file   *os.File
	offset int64
	length int
	header rowHeader
	order  int
}
type recentRows []recentRow

func (h recentRows) Len() int { return len(h) }
func (h recentRows) Less(i, j int) bool {
	a, b := h[i], h[j]
	if !a.header.Time.Equal(b.header.Time) {
		return a.header.Time.Before(b.header.Time)
	}
	if a.header.RequestID != b.header.RequestID {
		return a.header.RequestID < b.header.RequestID
	}
	return a.order < b.order
}
func (h recentRows) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *recentRows) Push(x any)   { *h = append(*h, x.(recentRow)) }
func (h *recentRows) Pop() any     { old := *h; v := old[len(old)-1]; *h = old[:len(old)-1]; return v }

// List scans headers from fixed-size file snapshots without holding the append
// lock. Only the newest limit payloads are decoded, ordered by request start time.
func (r *Recorder) List(keyID string, limit int) ([]Entry, error) {
	if r == nil || r.cfg.Dir == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	files, _, err := r.snapshot()
	if err != nil {
		return nil, err
	}
	defer closeSnapshots(files)
	rows := &recentRows{}
	heap.Init(rows)
	reader := bufio.NewReaderSize(bytes.NewReader(nil), 64<<10)
	order := 0
	// Visit oldest files first to keep equal-time rows in append order.
	for i := len(files) - 1; i >= 0; i-- {
		f := files[i]
		reader.Reset(io.NewSectionReader(f.file, 0, f.size))
		err := walkLines(reader, func(line []byte, offset int64) error {
			h, err := parseHeader(line)
			if err != nil {
				return fmt.Errorf("decode prompt header: %w", err)
			}
			order++
			if h.KeyID != keyID {
				return nil
			}
			row := recentRow{f.file, offset, len(line), h, order}
			if rows.Len() < limit {
				heap.Push(rows, row)
			} else {
				candidate := recentRows{(*rows)[0], row}
				if candidate.Less(0, 1) {
					(*rows)[0] = row
					heap.Fix(rows, 0)
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	out := make([]Entry, rows.Len())
	for i := len(out) - 1; i >= 0; i-- {
		row := heap.Pop(rows).(recentRow)
		raw := make([]byte, row.length)
		if _, err := row.file.ReadAt(raw, row.offset); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Counts retains its convenience API for callers without an error channel.
func (r *Recorder) Counts() map[string]int {
	counts, err := r.CountSnapshot()
	if err != nil {
		log.Printf("[promptlog] counts: %v", err)
	}
	return counts
}
func copyCounts(src map[string]int) map[string]int {
	out := make(map[string]int, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
func (r *Recorder) CountSnapshot() (map[string]int, error) {
	if r == nil || r.cfg.Dir == "" {
		return nil, nil
	}
	r.countMu.Lock()
	defer r.countMu.Unlock()
	r.mu.Lock()
	if r.counts != nil && time.Since(r.countsAt) < 10*time.Second {
		out := copyCounts(r.counts)
		r.mu.Unlock()
		return out, nil
	}
	r.mu.Unlock()
	files, gen, err := r.snapshot()
	if err != nil {
		return nil, err
	}
	defer closeSnapshots(files)
	counts := map[string]int{}
	reader := bufio.NewReaderSize(bytes.NewReader(nil), 64<<10)
	for _, f := range files {
		reader.Reset(io.NewSectionReader(f.file, 0, f.size))
		if err := walkLines(reader, func(line []byte, _ int64) error {
			h, err := parseHeader(line)
			if err == nil && h.KeyID != "" {
				counts[h.KeyID]++
			}
			return err
		}); err != nil {
			return nil, err
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.generation == gen {
		r.counts = copyCounts(counts)
		r.countsAt = time.Now()
	}
	return counts, nil
}

func (r *Recorder) Clear(keyID string) (int, error) {
	return r.rewrite(func(h rowHeader) bool { return h.KeyID != keyID })
}
func (r *Recorder) Prune(valid map[string]bool) (int, error) {
	return r.rewrite(func(h rowHeader) bool { return h.KeyID == "" || valid[h.KeyID] })
}
func (r *Recorder) rewrite(keep func(rowHeader) bool) (int, error) {
	if r == nil || r.cfg.Dir == "" {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.closeFileLocked(); err != nil {
		return 0, err
	}
	files, err := r.archivesLocked()
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, f := range files {
		n, err := rewriteFile(f.path, keep)
		if n > 0 {
			removed += n
			r.generation++
			r.counts = nil
		}
		if err != nil {
			return removed, err
		}
	}
	return removed, nil
}
func rewriteStream(in io.Reader, out io.Writer, keep func(rowHeader) bool) (int, error) {
	removed := 0
	err := walkLines(bufio.NewReaderSize(in, 64<<10), func(line []byte, _ int64) error {
		h, err := parseHeader(line)
		if err != nil {
			return fmt.Errorf("refuse rewrite of damaged archive: %w", err)
		}
		if !keep(h) {
			removed++
			return nil
		}
		n, err := out.Write(line)
		if err != nil {
			return err
		}
		if n != len(line) {
			return io.ErrShortWrite
		}
		return nil
	})
	return removed, err
}
func rewriteFile(path string, keep func(rowHeader) bool) (removed int, resultErr error) {
	in, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	out, err := os.CreateTemp(filepath.Dir(path), ".prompt-rewrite-*")
	if err != nil {
		in.Close()
		return 0, err
	}
	tmp := out.Name()
	defer func() { _ = os.Remove(tmp) }()
	n, err := rewriteStream(in, out, keep)
	err = errors.Join(err, in.Close())
	if err == nil {
		err = out.Sync()
	}
	err = errors.Join(err, out.Close())
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, err
	}
	// Sync the rename for crash durability where the platform supports directories.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return n, err
	}
	err = errors.Join(dir.Sync(), dir.Close())
	return n, err
}

// Start tightens existing permissions and starts retention even if capture is off.
func (r *Recorder) Start() error {
	if r == nil || r.cfg.Dir == "" {
		return nil
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.started {
		return nil
	}
	r.mu.Lock()
	err := r.secureDirLocked()
	if err == nil {
		var files []archiveFile
		files, err = r.archivesLocked()
		if err == nil {
			for _, f := range files {
				if err = os.Chmod(f.path, 0600); err != nil {
					break
				}
			}
		}
	}
	if err == nil {
		err = r.pruneLocked(time.Now())
	}
	r.mu.Unlock()
	if err != nil {
		return err
	}
	stop, done := make(chan struct{}), make(chan struct{})
	r.stopCh, r.doneCh, r.started = stop, done, true
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				r.mu.Lock()
				err := r.pruneLocked(time.Now())
				r.mu.Unlock()
				if err != nil {
					log.Printf("[promptlog] cleanup: %v", err)
				}
			}
		}
	}()
	return nil
}
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.lifecycleMu.Lock()
	defer r.lifecycleMu.Unlock()
	if r.started {
		close(r.stopCh)
		<-r.doneCh
		r.started = false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.closeFileLocked(); err != nil {
		log.Printf("[promptlog] close: %v", err)
	}
}

// RetentionDays includes today. At startup the latest current-day segment is
// protected even before an append descriptor exists; an oversized legacy segment
// will roll on the next write. The active segment is the sole capacity exception.
func (r *Recorder) pruneLocked(now time.Time) error {
	files, err := r.archivesLocked()
	if err != nil {
		return err
	}
	cutoff := now.AddDate(0, 0, 1-r.cfg.RetentionDays).Format("2006-01-02")
	if r.file != nil && r.day < cutoff {
		if err := r.closeFileLocked(); err != nil {
			return err
		}
	}
	active := ""
	if r.file != nil {
		active = r.file.Name()
	} else if len(files) > 0 && files[0].day == now.Format("2006-01-02") {
		active = files[0].path
	}
	var total int64
	sizes := map[string]int64{}
	for _, f := range files {
		info, err := os.Stat(f.path)
		if err != nil {
			return err
		}
		if f.day < cutoff && f.path != active {
			if err := os.Remove(f.path); err != nil {
				return err
			}
			r.generation++
			r.counts = nil
			continue
		}
		sizes[f.path] = info.Size()
		total += info.Size()
	}
	for i := len(files) - 1; i >= 0 && total > r.cfg.MaxBytes; i-- {
		f := files[i]
		size, exists := sizes[f.path]
		if !exists || f.path == active {
			continue
		}
		if err := os.Remove(f.path); err != nil {
			return err
		}
		total -= size
		r.generation++
		r.counts = nil
	}
	return nil
}
