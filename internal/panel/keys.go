// keys.go 面板「API 密钥」视图：多 key 的增删改、启停、轮换、用量与配额。
//
// 配置的读写都经 main 注入的闭包：ListAPIKeys 返回当前 api_keys，SaveAPIKeys
// 走与配置页相同的校验 + 落盘 + 热生效路径。面板不直接依赖 cmd/server 的类型。
package panel

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/linguo2625469/workbuddy2api-panel/internal/keystats"
)

// APIKeyView 一把下游密钥的配置视图（含明文 key，仅面板密钥持有者可见）。
type APIKeyView struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Key         string   `json:"key"`
	Models      []string `json:"models"`
	RPM         int      `json:"rpm"`
	DailyTokens int64    `json:"daily_tokens"`
	Disabled    bool     `json:"disabled"`
}

// keyStatsView 列表用的精简统计（不带按天历史，避免列表响应过大）。
type keyStatsView struct {
	Requests         int64     `json:"requests"`
	Success          int64     `json:"success"`
	Errors           int64     `json:"errors"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
	TodayRequests    int64     `json:"today_requests"`
	TodayTokens      int64     `json:"today_tokens"`
	LastUsed         time.Time `json:"last_used"`
}

func statsView(e keystats.Entry) keyStatsView {
	return keyStatsView{
		Requests: e.Requests, Success: e.Success, Errors: e.Errors,
		PromptTokens: e.PromptTokens, CompletionTokens: e.CompletionTokens,
		TodayRequests: e.TodayRequests, TodayTokens: e.TodayTokens,
		LastUsed: e.LastUsed,
	}
}

type apiKeyRow struct {
	APIKeyView
	Stats keyStatsView `json:"stats"`
}

type keySaveBody struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Models      []string `json:"models"`
	RPM         int      `json:"rpm"`
	DailyTokens int64    `json:"daily_tokens"`
	Disabled    bool     `json:"disabled"`
}

func (p *Panel) keysAvailable(w http.ResponseWriter) bool {
	if p.cfg.ListAPIKeys == nil || p.cfg.SaveAPIKeys == nil {
		writeErr(w, http.StatusNotImplemented, "keys api not available")
		return false
	}
	return true
}

func (p *Panel) listKeys(w http.ResponseWriter, r *http.Request) {
	if !p.keysAvailable(w) {
		return
	}
	keys, err := p.cfg.ListAPIKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list keys: "+err.Error())
		return
	}
	stats := map[string]keystats.Entry{}
	if p.cfg.KeyStats != nil {
		stats = p.cfg.KeyStats.Snapshot()
	}
	rows := make([]apiKeyRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, apiKeyRow{APIKeyView: k, Stats: statsView(stats[k.ID])})
	}
	orphans := 0
	if p.cfg.KeyStats != nil {
		valid := map[string]bool{"owner": true}
		for _, k := range keys {
			valid[k.ID] = true
		}
		for id := range stats {
			if !valid[id] {
				orphans++
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": rows, "orphan_count": orphans})
}

// saveKey 新建（无 id）或更新（有 id）。密钥值只由服务端生成，客户端不能提交。
func (p *Panel) saveKey(w http.ResponseWriter, r *http.Request) {
	if !p.keysAvailable(w) {
		return
	}
	var b keySaveBody
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "parse body: "+err.Error())
		return
	}
	if b.RPM < 0 {
		writeErr(w, http.StatusBadRequest, "rpm 不能为负")
		return
	}
	if b.DailyTokens < 0 {
		writeErr(w, http.StatusBadRequest, "daily_tokens 不能为负")
		return
	}
	b.Name = strings.TrimSpace(b.Name)
	if utf8.RuneCountInString(b.Name) > 64 {
		writeErr(w, http.StatusBadRequest, "名称最长 64 个字符")
		return
	}
	models := cleanModels(b.Models)

	p.keysMu.Lock()
	defer p.keysMu.Unlock()
	keys, err := p.cfg.ListAPIKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list keys: "+err.Error())
		return
	}

	if b.ID == "" {
		key, err := newAPIKey()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "generate key: "+err.Error())
			return
		}
		id := keyID(key)
		keys = append(keys, APIKeyView{
			ID: id, Name: b.Name, Key: key, Models: models,
			RPM: b.RPM, DailyTokens: b.DailyTokens, Disabled: b.Disabled,
		})
		if err := p.cfg.SaveAPIKeys(keys); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": id, "key": key})
		return
	}

	idx := -1
	for i := range keys {
		if keys[i].ID == b.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	keys[idx].Name = b.Name
	keys[idx].Models = models
	keys[idx].RPM = b.RPM
	keys[idx].DailyTokens = b.DailyTokens
	keys[idx].Disabled = b.Disabled
	if err := p.cfg.SaveAPIKeys(keys); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (p *Panel) toggleKey(w http.ResponseWriter, r *http.Request) {
	if !p.keysAvailable(w) {
		return
	}
	id := r.PathValue("id")
	p.keysMu.Lock()
	defer p.keysMu.Unlock()
	keys, err := p.cfg.ListAPIKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list keys: "+err.Error())
		return
	}
	idx := -1
	for i := range keys {
		if keys[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	keys[idx].Disabled = !keys[idx].Disabled
	if err := p.cfg.SaveAPIKeys(keys); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "disabled": keys[idx].Disabled})
}

// rotateKey 换一把新密钥值，保留 ID（统计连续）与模型/限速/配额/启停配置。
func (p *Panel) rotateKey(w http.ResponseWriter, r *http.Request) {
	if !p.keysAvailable(w) {
		return
	}
	id := r.PathValue("id")
	p.keysMu.Lock()
	defer p.keysMu.Unlock()
	keys, err := p.cfg.ListAPIKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list keys: "+err.Error())
		return
	}
	idx := -1
	for i := range keys {
		if keys[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	key, err := newAPIKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "generate key: "+err.Error())
		return
	}
	keys[idx].Key = key
	if err := p.cfg.SaveAPIKeys(keys); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": key})
}

func (p *Panel) removeKey(w http.ResponseWriter, r *http.Request) {
	if !p.keysAvailable(w) {
		return
	}
	id := r.PathValue("id")
	p.keysMu.Lock()
	defer p.keysMu.Unlock()
	keys, err := p.cfg.ListAPIKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list keys: "+err.Error())
		return
	}
	out := keys[:0]
	found := false
	for _, k := range keys {
		if k.ID == id {
			found = true
			continue
		}
		out = append(out, k)
	}
	if !found {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	if err := p.cfg.SaveAPIKeys(out); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// keyUsage 返回指定 key 最近 N 天的按天/按模型用量（缺失日补零）。
func (p *Panel) keyUsage(w http.ResponseWriter, r *http.Request) {
	if p.cfg.KeyStats == nil || p.cfg.ListAPIKeys == nil {
		writeErr(w, http.StatusNotImplemented, "key stats not available")
		return
	}
	id := r.PathValue("id")
	keys, err := p.cfg.ListAPIKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list keys: "+err.Error())
		return
	}
	name := ""
	found := false
	for _, k := range keys {
		if k.ID == id {
			name = k.Name
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			days = n
		}
	}
	if days < 1 {
		days = 1
	}
	if days > 90 {
		days = 90
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "name": name, "days": p.cfg.KeyStats.DailyUsage(id, days),
	})
}

// pruneKeyStats 清理已不在配置里的密钥统计（含 owner 之外的历史残留）。
func (p *Panel) pruneKeyStats(w http.ResponseWriter, r *http.Request) {
	if p.cfg.KeyStats == nil || p.cfg.ListAPIKeys == nil {
		writeErr(w, http.StatusNotImplemented, "key stats not available")
		return
	}
	keys, err := p.cfg.ListAPIKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "list keys: "+err.Error())
		return
	}
	valid := map[string]bool{"owner": true}
	for _, k := range keys {
		valid[k.ID] = true
	}
	n := p.cfg.KeyStats.Prune(valid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": n})
}

// newAPIKey 生成 32 字符随机密钥（192 bit）。
func newAPIKey() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "sk-" + base64.RawURLEncoding.EncodeToString(buf), nil
}

// keyID 由密钥值派生稳定 ID（与 cmd/server 的 deriveKeyID 同算法）。
func keyID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "k_" + hex.EncodeToString(sum[:])[:10]
}

// cleanModels 去空白、去空项、去重，保持原顺序。
func cleanModels(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}
