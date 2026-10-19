// Package keystats 按 API 密钥聚合的用量统计（进程内 + 定期落盘）。
//
// 与 reqlog 的分工：reqlog 记录逐请求明细（用于排障/审计），本包只保留每个密钥的
// 累计与当日计数，供面板「API 密钥」视图即时展示。重启后从 data/key_usage.json
// 恢复；按密钥 ID 聚合，轮换密钥值不会丢历史（ID 不变）。
package keystats

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// Entry 单个密钥的累计统计。
type Entry struct {
	Requests         int64     `json:"requests"`
	Success          int64     `json:"success"`
	Errors           int64     `json:"errors"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TodayRequests    int64     `json:"today_requests"`
	TodayTokens      int64     `json:"today_tokens"`
	LastUsed         time.Time `json:"last_used"`
	Day              string    `json:"day"` // 本地日期 YYYY-MM-DD，用于跨天归零
}

// Recorder 并发安全的统计器。
type Recorder struct {
	mu      sync.Mutex
	path    string
	entries map[string]*Entry
	dirty   bool

	stopCh  chan struct{}
	doneCh  chan struct{}
	started bool
}

// New 构建统计器；path 为空表示只存内存（测试用）。
func New(path string) *Recorder {
	return &Recorder{path: path, entries: make(map[string]*Entry)}
}

// Load 从 path 恢复历史统计；文件不存在视为空。
func (r *Recorder) Load() error {
	if r.path == "" {
		return nil
	}
	raw, err := os.ReadFile(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var m map[string]*Entry
	if err := json.Unmarshal(raw, &m); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if m != nil {
		r.entries = m
	}
	return nil
}

// Record 记一次请求。status 为最终 HTTP 状态；prompt/completion 为本次 token
// 增量（未知时传 0 或负值，负值会被忽略）。
func (r *Recorder) Record(id string, status int, prompt, completion int64) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[id]
	if e == nil {
		e = &Entry{}
		r.entries[id] = e
	}
	today := time.Now().Format("2006-01-02")
	if e.Day != today {
		e.Day = today
		e.TodayRequests = 0
		e.TodayTokens = 0
	}
	e.Requests++
	e.TodayRequests++
	if status >= 200 && status < 300 {
		e.Success++
	} else {
		e.Errors++
	}
	if prompt > 0 {
		e.PromptTokens += prompt
	}
	if completion > 0 {
		e.CompletionTokens += completion
	}
	if tok := prompt + completion; tok > 0 {
		e.TodayTokens += tok
	}
	e.LastUsed = time.Now()
	r.dirty = true
}

// Snapshot 返回当前统计的拷贝（调用方随便读，不影响内部状态）。
func (r *Recorder) Snapshot() map[string]Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Entry, len(r.entries))
	for id, e := range r.entries {
		out[id] = *e
	}
	return out
}

// Start 启动定期落盘（30s 一次；无变更不写盘）。
func (r *Recorder) Start() {
	if r.path == "" || r.started {
		return
	}
	r.stopCh = make(chan struct{})
	r.doneCh = make(chan struct{})
	r.started = true
	go func() {
		defer close(r.doneCh)
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				r.flush()
			case <-r.stopCh:
				r.flush()
				return
			}
		}
	}()
}

// Stop 停止定期落盘并做最后一次 flush。
func (r *Recorder) Stop() {
	if !r.started {
		return
	}
	close(r.stopCh)
	<-r.doneCh
	r.started = false
}

func (r *Recorder) flush() {
	r.mu.Lock()
	if !r.dirty || r.path == "" {
		r.mu.Unlock()
		return
	}
	data, err := json.Marshal(r.entries)
	r.dirty = false
	r.mu.Unlock()
	if err != nil {
		log.Printf("[keystats] marshal: %v", err)
		return
	}
	tmp := r.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		log.Printf("[keystats] write %s: %v", tmp, err)
		r.markDirty()
		return
	}
	if err := os.Rename(tmp, r.path); err != nil {
		log.Printf("[keystats] replace %s: %v", r.path, err)
		r.markDirty()
	}
}

func (r *Recorder) markDirty() {
	r.mu.Lock()
	r.dirty = true
	r.mu.Unlock()
}
