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
	"os"
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

	mu          sync.Mutex
	lifecycleMu sync.Mutex
	countMu     sync.Mutex
	file        *os.File
	day         string
	size        int64
	generation  uint64

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
