// Package promptlog 按 API 密钥归档请求的输入提示词（显式开启后才会写入）。
//
// 隐私边界：
//   - 只有全局 logging.prompt_capture_enabled 打开、且该 key 的 capture_prompts=true
//     时才记录；
//   - 只记录请求的 messages（完整消息对象，含 tool_calls/tool_call_id 等）与工具名，
//     不记录响应、Authorization、token、工具 schema；
//   - 优先保留用户输入，裁剪前保存消息身份，单条/单请求都有上限；
//   - 文件 0600、目录 0700，按天分文件，按保留天数/总容量自动清理。
package promptlog

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Message 一条完整输入消息（保留 role/content/tool_calls/name 等全部字段）。
type Message map[string]any

// Entry 一次请求的提示词快照。
type Entry struct {
	Version          int       `json:"version,omitempty"`
	SessionHash      string    `json:"session_hash,omitempty"`
	MessageIndexes   []int     `json:"message_indexes,omitempty"`
	Inputs           []Input   `json:"inputs,omitempty"`
	InputsOmitted    int       `json:"inputs_omitted,omitempty"`
	OriginalMessages int       `json:"original_messages,omitempty"`
	FreshState       string    `json:"fresh_state,omitempty"`
	FreshReason      string    `json:"fresh_reason,omitempty"`
	Time             time.Time `json:"time"`
	KeyID            string    `json:"key_id"`
	KeyName          string    `json:"key_name"`
	RequestID        string    `json:"request_id"`
	Model            string    `json:"model"`
	Status           int       `json:"status"`
	Messages         []Message `json:"messages"`
	// Omitted 因预算省略的消息条数；Tools 请求声明的工具名（不含 schema）。
	Omitted   int      `json:"omitted,omitempty"`
	Tools     []string `json:"tools,omitempty"`
	Truncated bool     `json:"truncated"`
	Bytes     int      `json:"bytes"`
}

// CaptureResult Capture 的返回。
type CaptureResult struct {
	SessionHash      string
	MessageIndexes   []int
	Inputs           []Input
	InputsOmitted    int
	OriginalMessages int
	Messages         []Message
	Omitted          int
	Tools            []string
	Truncated        bool
	Bytes            int
}

// Config 归档配置。
type Config struct {
	Dir           string
	RetentionDays int
	MaxBytes      int64
}

// Recorder 按天追加 JSONL 的归档器。
type Recorder struct {
	cfg Config

	mu   sync.Mutex
	file *os.File
	day  string

	stopCh  chan struct{}
	doneCh  chan struct{}
	started bool

	counts   map[string]int
	countsAt time.Time
}

const (
	maxMessageRunes = 20000
	defaultRetain   = 3
	defaultMaxBytes = 100 << 20
	defaultMaxChars = 100000
)

// New 构建归档器；Dir 为空时 Record 为 no-op（面板仍可返回空列表）。
func New(cfg Config) *Recorder {
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = defaultRetain
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	return &Recorder{cfg: cfg}
}

// harnessPrefixes 是 DSH 运行时注入到 user 角色的上下文标记。这些内容不是用户
// 真实输入，面板默认不展示；完整消息仍保留在归档里，可切换「全部消息」查看。
var harnessPrefixes = []string{
	"Time sampled while preparing turn",
	"Browser time zone for this request",
	"Current runtime context",
	"Current DSH file policy",
	"Current working directory:",
	"Approval policy:",
	"Current DeepSeek Harness implementation checkout",
	"Current DSH Web GUI URL",
	"Instructions from:",
	"<system-reminder>",
	"background job ",
	"A skill is a reusable set",
	"The following skills are available in this session",
	"Use only names in this replacement catalog",
	"A user may also invoke a skill directly",
	"Skills load from",
	"Attached image(s) from tool result",
}

// IsHarnessContext 判断一段 user 内容是否是 harness 注入的上下文/通知。
func IsHarnessContext(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for _, p := range harnessPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	if strings.Contains(s, "<system-reminder>") && strings.Contains(s, "A skill is a reusable set") {
		return true
	}
	return false
}

// Record 追加一条记录；失败返回错误（调用方只记日志，不影响请求）。
func (r *Recorder) Record(e Entry) error {
	if r == nil || r.cfg.Dir == "" {
		return nil
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.ensureFileLocked(e.Time); err != nil {
		return err
	}
	_, err = r.file.Write(line)
	r.counts = nil
	return err
}

// List 返回某个 key 最近的记录（新→旧，最多 limit 条）。
func (r *Recorder) List(keyID string, limit int) ([]Entry, error) {
	if r == nil || r.cfg.Dir == "" {
		return nil, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	files := r.filesLocked()
	out := make([]Entry, 0, limit)
	for _, p := range files {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
		var entries []Entry
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.KeyID == keyID {
				entries = append(entries, e)
			}
		}
		f.Close()
		for i := len(entries) - 1; i >= 0; i-- {
			out = append(out, entries[i])
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// Clear 删除某个 key 的全部提示词记录，返回删除条数。
func (r *Recorder) Clear(keyID string) (int, error) {
	if r == nil || r.cfg.Dir == "" {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.rewriteLocked(func(e Entry) bool { return e.KeyID != keyID })
	r.counts = nil
	return n, nil
}

// Prune 删除不在 valid 集合里的 key 的记录（清理已删除 key 的残留）。
func (r *Recorder) Prune(valid map[string]bool) (int, error) {
	if r == nil || r.cfg.Dir == "" {
		return 0, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := r.rewriteLocked(func(e Entry) bool { return e.KeyID == "" || valid[e.KeyID] })
	r.counts = nil
	return n, nil
}

// rewriteLocked 逐文件重写，keep 返回 false 的行被删除并计数。
func (r *Recorder) rewriteLocked(keep func(Entry) bool) int {
	if r.file != nil {
		r.file.Close()
		r.file = nil
		r.day = ""
	}
	removed := 0
	for _, p := range r.filesLocked() {
		in, err := os.Open(p)
		if err != nil {
			continue
		}
		tmp := p + ".tmp"
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			in.Close()
			continue
		}
		sc := bufio.NewScanner(in)
		sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			if !keep(e) {
				removed++
				continue
			}
			out.Write(sc.Bytes())
			out.Write([]byte("\n"))
		}
		in.Close()
		out.Close()
		if err := os.Rename(tmp, p); err != nil {
			os.Remove(tmp)
		}
	}
	return removed
}

// Counts 返回各 key 的归档条数（10s 缓存；写入后失效）。用于密钥列表展示。
func (r *Recorder) Counts() map[string]int {
	if r == nil || r.cfg.Dir == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.counts != nil && time.Since(r.countsAt) < 10*time.Second {
		out := make(map[string]int, len(r.counts))
		for k, v := range r.counts {
			out[k] = v
		}
		return out
	}
	counts := make(map[string]int)
	for _, p := range r.filesLocked() {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 64*1024), 8*1024*1024)
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.KeyID != "" {
				counts[e.KeyID]++
			}
		}
		f.Close()
	}
	r.counts = counts
	r.countsAt = time.Now()
	out := make(map[string]int, len(counts))
	for k, v := range counts {
		out[k] = v
	}
	return out
}

// Start 启动每小时清理（保留天数/容量上限）。
func (r *Recorder) Start() {
	if r == nil || r.cfg.Dir == "" || r.started {
		return
	}
	r.mu.Lock()
	r.pruneLocked(time.Now())
	r.mu.Unlock()
	r.stopCh = make(chan struct{})
	r.doneCh = make(chan struct{})
	r.started = true
	go func() {
		defer close(r.doneCh)
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				r.mu.Lock()
				r.pruneLocked(time.Now())
				r.mu.Unlock()
			case <-r.stopCh:
				return
			}
		}
	}()
}

// Close 停止清理并关闭当前文件。
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	if r.started {
		close(r.stopCh)
		<-r.doneCh
		r.started = false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.file != nil {
		r.file.Close()
		r.file = nil
		r.day = ""
	}
}

func (r *Recorder) ensureFileLocked(t time.Time) error {
	day := t.Format("2006-01-02")
	if r.file != nil && r.day == day {
		return nil
	}
	if r.file != nil {
		r.file.Close()
		r.file = nil
	}
	if err := os.MkdirAll(r.cfg.Dir, 0o700); err != nil {
		return err
	}
	p := filepath.Join(r.cfg.Dir, "prompts-"+day+".jsonl")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	r.file = f
	r.day = day
	r.pruneLocked(t)
	return nil
}

// pruneLocked 删除超期文件，并按总容量从最旧开始删。
func (r *Recorder) pruneLocked(now time.Time) {
	files := r.filesLocked()
	cutoff := now.AddDate(0, 0, -r.cfg.RetentionDays).Format("2006-01-02")
	for _, p := range files {
		day := dayFromName(filepath.Base(p))
		if day == "" {
			continue
		}
		if day < cutoff {
			os.Remove(p)
		}
	}
	files = r.filesLocked()
	var total int64
	for _, p := range files {
		if fi, err := os.Stat(p); err == nil {
			total += fi.Size()
		}
	}
	for i := len(files) - 1; i >= 0 && total > r.cfg.MaxBytes; i-- {
		p := files[i]
		if r.file != nil {
			if fi, err := r.file.Stat(); err == nil && fi.Name() == filepath.Base(p) {
				continue
			}
		}
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if os.Remove(p) == nil {
			total -= fi.Size()
		}
	}
}

// filesLocked 返回按日期倒序的归档文件。
func (r *Recorder) filesLocked() []string {
	if r.cfg.Dir == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(r.cfg.Dir, "prompts-*.jsonl"))
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	return files
}

func dayFromName(name string) string {
	name = strings.TrimSuffix(strings.TrimPrefix(name, "prompts-"), ".jsonl")
	if len(name) != 10 {
		return ""
	}
	return name
}
