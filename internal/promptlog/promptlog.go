// Package promptlog 按 API 密钥归档请求的输入提示词（显式开启后才会写入）。
//
// 隐私边界：
//   - 只有全局 logging.prompt_capture_enabled 打开、且该 key 的 capture_prompts=true
//     时才记录；
//   - 只记录请求的 messages（角色 + 内容），不记录响应、Authorization、token；
//   - 单条消息与单请求都有长度上限，超出标记 truncated；
//   - 文件 0600、目录 0700，按天分文件，按保留天数/总容量自动清理。
package promptlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Message 一条输入消息（content 可能是字符串或多模态数组）。
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	Name    string `json:"name,omitempty"`
}

// Entry 一次请求的提示词快照。
type Entry struct {
	Time      time.Time `json:"time"`
	KeyID     string    `json:"key_id"`
	KeyName   string    `json:"key_name"`
	RequestID string    `json:"request_id"`
	Model     string    `json:"model"`
	Status    int       `json:"status"`
	Messages  []Message `json:"messages"`
	Truncated bool      `json:"truncated"`
	Bytes     int       `json:"bytes"`
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
}

const (
	maxMessageRunes = 8000
	defaultRetain   = 3
	defaultMaxBytes = 50 << 20
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

// Capture 从 chat 请求体提取 messages。返回消息、是否截断、捕获字节数。
// maxChars<=0 时回落 20000。解析失败/无 messages 返回 nil。
func Capture(body []byte, maxChars int) ([]Message, bool, int) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Name    string          `json:"name"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return nil, false, 0
	}
	if maxChars <= 0 {
		maxChars = 20000
	}
	out := make([]Message, 0, len(req.Messages))
	total := 0
	truncated := false
	for _, m := range req.Messages {
		remaining := maxChars - total
		if remaining <= 0 {
			truncated = true
			break
		}
		var content any
		size := 0
		var s string
		if err := json.Unmarshal(m.Content, &s); err == nil {
			limit := maxMessageRunes
			if limit > remaining {
				limit = remaining
			}
			if utf8.RuneCountInString(s) > limit {
				s = string([]rune(s)[:limit])
				truncated = true
			}
			content = s
			size = len(s)
		} else {
			size = len(m.Content)
			if size > remaining {
				content = fmt.Sprintf("[内容过大，已省略 %d 字节]", size)
				truncated = true
				size = len(content.(string))
			} else {
				content = m.Content // json.RawMessage 原样嵌入
			}
		}
		out = append(out, Message{Role: m.Role, Name: m.Name, Content: content})
		total += size
	}
	if len(out) == 0 {
		return nil, truncated, 0
	}
	return out, truncated, total
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
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
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
		sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			if e.KeyID == keyID {
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
	return removed, nil
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
	// 容量上限：保留当前文件，从最旧删到满足上限。
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
