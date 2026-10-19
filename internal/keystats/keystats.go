// Package keystats 按 API 密钥聚合的用量统计（进程内 + 定期落盘）。
//
// 与 reqlog 的分工：reqlog 记录逐请求明细（用于排障/审计），本包只保留每个密钥的
// 累计、当日与按天/按模型的历史，供面板「API 密钥」视图展示与日 token 配额判定。
// 重启后从 data/key_usage.json 恢复；按密钥 ID 聚合，轮换密钥值不会丢历史。
package keystats

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// historyDays 保留的历史天数（按天桶上限）。
const historyDays = 90

// Model 单日单模型的用量。
type Model struct {
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// Day 单日用量。
type Day struct {
	Requests         int64             `json:"requests"`
	PromptTokens     int64             `json:"prompt_tokens"`
	CompletionTokens int64             `json:"completion_tokens"`
	Models           map[string]*Model `json:"models,omitempty"`
}

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
	// Days 按天历史（最多 historyDays 天），key 为 YYYY-MM-DD。
	Days map[string]*Day `json:"days,omitempty"`
}

// DayUsage 面板曲线用的一天数据（缺失日补零）。
type DayUsage struct {
	Day              string                `json:"day"`
	Requests         int64                 `json:"requests"`
	PromptTokens     int64                 `json:"prompt_tokens"`
	CompletionTokens int64                 `json:"completion_tokens"`
	Models           map[string]ModelUsage `json:"models"`
}

// ModelUsage 面板模型明细。
type ModelUsage struct {
	Requests         int64 `json:"requests"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
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

// Record 记一次请求。model 为空（非 chat 端点）时只记请求数，不记模型维度。
// status 为最终 HTTP 状态；prompt/completion 为本次 token 增量（未知传 0 或负值）。
func (r *Recorder) Record(id, model string, status int, prompt, completion int64) {
	if id == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	now := time.Now()
	today := now.Format("2006-01-02")
	e := r.entries[id]
	if e == nil {
		e = &Entry{}
		r.entries[id] = e
	}
	if e.Day != today {
		e.Day = today
		e.TodayRequests = 0
		e.TodayTokens = 0
		r.pruneHistoryLocked(e, now)
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
	e.LastUsed = now

	if e.Days == nil {
		e.Days = make(map[string]*Day)
	}
	d := e.Days[today]
	if d == nil {
		d = &Day{}
		e.Days[today] = d
	}
	d.Requests++
	if prompt > 0 {
		d.PromptTokens += prompt
	}
	if completion > 0 {
		d.CompletionTokens += completion
	}
	if model != "" {
		if d.Models == nil {
			d.Models = make(map[string]*Model)
		}
		m := d.Models[model]
		if m == nil {
			m = &Model{}
			d.Models[model] = m
		}
		m.Requests++
		if prompt > 0 {
			m.PromptTokens += prompt
		}
		if completion > 0 {
			m.CompletionTokens += completion
		}
	}
	r.dirty = true
}

// TodayTokens 返回 id 今天已消耗的 token（用于日配额判定）；跨天视为 0。
func (r *Recorder) TodayTokens(id string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[id]
	if e == nil {
		return 0
	}
	if e.Day != time.Now().Format("2006-01-02") {
		return 0
	}
	return e.TodayTokens
}

// Snapshot 返回当前统计的深拷贝（Days/Models 均复制，调用方随便读）。
func (r *Recorder) Snapshot() map[string]Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]Entry, len(r.entries))
	for id, e := range r.entries {
		out[id] = copyEntry(e)
	}
	return out
}

// DailyUsage 返回 id 最近 days 天的用量（按日期升序，缺失日补零）。
func (r *Recorder) DailyUsage(id string, days int) []DayUsage {
	if days <= 0 {
		days = 30
	}
	if days > historyDays {
		days = historyDays
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.entries[id]
	out := make([]DayUsage, 0, days)
	now := time.Now()
	for i := days - 1; i >= 0; i-- {
		day := now.AddDate(0, 0, -i).Format("2006-01-02")
		du := DayUsage{Day: day, Models: map[string]ModelUsage{}}
		if e != nil {
			if d := e.Days[day]; d != nil {
				du.Requests = d.Requests
				du.PromptTokens = d.PromptTokens
				du.CompletionTokens = d.CompletionTokens
				for mk, mv := range d.Models {
					du.Models[mk] = ModelUsage{
						Requests: mv.Requests, PromptTokens: mv.PromptTokens, CompletionTokens: mv.CompletionTokens,
					}
				}
			}
		}
		out = append(out, du)
	}
	return out
}

// Remove 删除单个密钥的统计（孤儿清理/删除密钥时用）。
func (r *Recorder) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.entries[id]; ok {
		delete(r.entries, id)
		r.dirty = true
	}
}

// Prune 删除不在 valid 集合里的统计，返回清理条数。
func (r *Recorder) Prune(valid map[string]bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for id := range r.entries {
		if !valid[id] {
			delete(r.entries, id)
			n++
		}
	}
	if n > 0 {
		r.dirty = true
	}
	return n
}

func (r *Recorder) pruneHistoryLocked(e *Entry, now time.Time) {
	cutoff := now.AddDate(0, 0, -historyDays).Format("2006-01-02")
	for day := range e.Days {
		if day < cutoff {
			delete(e.Days, day)
		}
	}
}

func copyEntry(e *Entry) Entry {
	out := *e
	if e.Days != nil {
		out.Days = make(map[string]*Day, len(e.Days))
		for k, d := range e.Days {
			dd := *d
			if d.Models != nil {
				dd.Models = make(map[string]*Model, len(d.Models))
				for mk, mv := range d.Models {
					m := *mv
					dd.Models[mk] = &m
				}
			}
			out.Days[k] = &dd
		}
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
