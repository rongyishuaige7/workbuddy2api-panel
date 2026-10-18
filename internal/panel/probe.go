package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// ProbeResult 测活结果（纯观测模式：记录耗时、状态码与响应片段，遇到 6004 限流解析恢复重置时刻）。
type ProbeResult struct {
	OK                bool   `json:"ok"`
	UID               string `json:"uid"`
	Nickname          string `json:"nickname,omitempty"`
	Realm             string `json:"realm,omitempty"`
	Model             string `json:"model"`
	Status            int    `json:"status"`
	LatencyMs         int64  `json:"latency_ms"`
	Snippet           string `json:"snippet,omitempty"`
	Error             string `json:"error,omitempty"`
	ResetAt           string `json:"reset_at,omitempty"`
	ResetRemainingSec int64  `json:"reset_remaining_sec,omitempty"`
}

// cleanSnippet 将原始响应体或 SSE 块整理为易读的简要摘要。
func cleanSnippet(raw string) string {
	s := strings.TrimSpace(raw)
	s = strings.ReplaceAll(s, "\r", "")
	if s == "" {
		return ""
	}

	// 1. 若为错误信封 JSON（如 {"code":6004,"msg":"..."} 或 {"error":{"code":6004,"message":"..."}}）
	var errObj struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal([]byte(s), &errObj); err == nil {
		if errObj.Code != 0 || errObj.Msg != "" {
			return fmt.Sprintf("code=%d msg=%s", errObj.Code, errObj.Msg)
		}
	}
	var errNested struct {
		Error struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(s), &errNested); err == nil {
		if errNested.Error.Message != "" || errNested.Error.Code != nil {
			return fmt.Sprintf("code=%v msg=%s", errNested.Error.Code, errNested.Error.Message)
		}
	}

	// 2. 若为 SSE 流格式（寻找首个包含回复内容或信封的行）
	lines := strings.Split(s, "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "data:") {
			jsonStr := strings.TrimSpace(strings.TrimPrefix(l, "data:"))
			if jsonStr == "[DONE]" {
				return "[DONE]"
			}
			var m struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
				Code int    `json:"code"`
				Msg  string `json:"msg"`
			}
			if err := json.Unmarshal([]byte(jsonStr), &m); err == nil {
				if len(m.Choices) > 0 && m.Choices[0].Delta.Content != "" {
					return "回复: " + strings.TrimSpace(m.Choices[0].Delta.Content)
				}
				if m.Code != 0 || m.Msg != "" {
					return fmt.Sprintf("code=%d msg=%s", m.Code, m.Msg)
				}
			}
			var nested struct {
				Error struct {
					Code    any    `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(jsonStr), &nested); err == nil {
				if nested.Error.Message != "" || nested.Error.Code != nil {
					return fmt.Sprintf("code=%v msg=%s", nested.Error.Code, nested.Error.Message)
				}
			}
		}
	}

	// 3. 通用首行整理并截断
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			l = strings.ReplaceAll(l, "\n", " ")
			for strings.Contains(l, "  ") {
				l = strings.ReplaceAll(l, "  ", " ")
			}
			if len(l) > 100 {
				return l[:97] + "..."
			}
			return l
		}
	}

	if len(s) > 100 {
		return s[:97] + "..."
	}
	return s
}

// runAccountProbe 对单一账号发起最小化对话测活请求（观察 TTFB 延迟与状态）。
func (p *Panel) runAccountProbe(ctx context.Context, a *auth.Auth, model string) ProbeResult {
	if model == "" {
		model = "deepseek-v4.1-flash"
	}
	bareModel := model
	if idx := strings.IndexByte(bareModel, ':'); idx >= 0 {
		prefix := bareModel[:idx]
		if prefix == "cn" || prefix == "global" {
			bareModel = bareModel[idx+1:]
		}
	}
	res := ProbeResult{
		UID:      a.UID,
		Nickname: a.Nickname,
		Realm:    a.Realm(),
		Model:    bareModel,
	}

	if p.cfg.Upstream == nil {
		res.Error = "upstream client not configured"
		return res
	}

	if err := ctx.Err(); err != nil {
		res.Error = err.Error()
		return res
	}
	if p.cfg.Pool == nil {
		res.Error = "account pool not configured"
		return res
	}
	lease := p.cfg.Pool.AcquireLease(a.UID)
	if lease == nil {
		res.Status = http.StatusTooManyRequests
		res.Error = "account concurrency limit reached"
		return res
	}
	defer lease.Release()
	a = lease.Auth

	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	// 临近过期先尝试刷新 token
	if a.NeedsRefresh(time.Minute) {
		if err := p.cfg.Upstream.RefreshTokenContext(probeCtx, a); err != nil {
			res.Error = "token refresh failed: " + err.Error()
			return res
		}
		_ = a.SaveAtomic()
	}

	if a.AccessTokenValue() == "" {
		res.Error = "access_token is empty"
		return res
	}

	body, err := json.Marshal(map[string]any{
		"model": bareModel,
		"messages": []map[string]any{
			{"role": "user", "content": "1"},
		},
		"max_tokens": 3,
		"stream":     true,
	})
	if err != nil {
		res.Error = "marshal body: " + err.Error()
		return res
	}

	start := time.Now()
	rc, status, respBody, err := p.cfg.Upstream.ChatStreamContext(probeCtx, a, body, "", upstream.ChatMeta{})
	res.LatencyMs = time.Since(start).Milliseconds()
	res.Status = status

	var streamChunk string
	if err != nil {
		res.Error = err.Error()
		if len(respBody) > 0 {
			res.Snippet = cleanSnippet(string(respBody))
		}
	} else if status >= 400 {
		res.Snippet = cleanSnippet(string(respBody))
		res.Error = fmt.Sprintf("HTTP %d: %s", status, res.Snippet)
	} else if rc != nil {
		defer rc.Close()
		text, readErr := upstream.ReadProbeResponse(rc)
		if readErr != nil {
			res.Error = readErr.Error()
			var ue *upstream.Error
			if errors.As(readErr, &ue) {
				streamChunk = ue.Msg
				res.Snippet = cleanSnippet(ue.Msg)
			}
		} else {
			res.OK = true
			res.Snippet = "回复: " + text
		}
	} else {
		res.Error = "upstream returned no response body"
	}

	// 统一排查 6004 模型限流与官方恢复重置时刻。
	// 测活是合成的小请求（user="1", max_tokens=3），不能代表真实流量：
	// 这里只把 6004/重置时间回报给面板，**绝不写路由冷却**。否则点一下测活
	// 就可能把账号/模型锁 10 分钟甚至到上游 resetAt，直接造成"一测就限流"。
	checkStr := strings.Join([]string{string(respBody), res.Error, res.Snippet, streamChunk}, " ")
	is6004 := !res.OK && upstream.IsModelRateLimit(string(respBody)+streamChunk)
	if is6004 {
		if resetAt, ok := upstream.ParseRateReset(checkStr); ok {
			res.ResetAt = resetAt.Format(time.RFC3339)
			rem := int64(time.Until(resetAt).Seconds())
			if rem < 0 {
				rem = 0
			}
			res.ResetRemainingSec = rem
		}
		log.Printf("WARN: [panel] probe %s model=%s: 6004 rate limit (no routing cooldown, reset=%s)",
			logfmt.Label(a.UID, a.Nickname), bareModel, res.ResetAt)
	}

	return res
}

// accountProbe 单号测活接口（POST /panel/api/accounts/{uid}/probe）。
func (p *Panel) accountProbe(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Pool == nil {
		writeErr(w, http.StatusNotImplemented, "pool not available")
		return
	}
	uid := r.PathValue("uid")
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}

	model := r.URL.Query().Get("model")
	if model == "" && r.Body != nil {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "" {
			model = req.Model
		}
	}
	if model == "" {
		model = "deepseek-v4.1-flash"
	}

	res := p.runAccountProbe(r.Context(), a, model)
	writeJSON(w, http.StatusOK, res)
}

// probeAll 全量测活接口（POST /panel/api/probe_all）。
func (p *Panel) probeAll(w http.ResponseWriter, r *http.Request) {
	if p.cfg.Pool == nil {
		writeErr(w, http.StatusNotImplemented, "pool not available")
		return
	}

	model := r.URL.Query().Get("model")
	if model == "" && r.Body != nil {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model != "" {
			model = req.Model
		}
	}
	if model == "" {
		model = "deepseek-v4.1-flash"
	}

	accounts := p.cfg.Pool.List()
	if len(accounts) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": []ProbeResult{}})
		return
	}

	results := make([]ProbeResult, len(accounts))
	sem := make(chan struct{}, 5)
	var wg sync.WaitGroup

	for i, st := range accounts {
		wg.Add(1)
		go func(idx int, uid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			a := p.cfg.Pool.AuthByUID(uid)
			if a == nil {
				results[idx] = ProbeResult{
					OK:    false,
					UID:   uid,
					Model: model,
					Error: "account auth not found",
				}
				return
			}
			results[idx] = p.runAccountProbe(r.Context(), a, model)
		}(i, st.UID)
	}

	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"results": results,
	})
}
