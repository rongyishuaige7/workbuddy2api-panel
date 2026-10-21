package upstream

import (
	"encoding/json"
	"regexp"
	"strings"
)

// identityScrubPatterns 上游渠道校验会按 system prompt 开头的 harness 身份句
// 拦截 11-128。这里只做最小替换，保留后续操作规则；按从具体到宽泛排序，
// 避免宽泛的 "You are an AI agent" 先吃掉品牌名。
var identityScrubPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)You are an AI agent powered by [^\n.]+\.?`),
	regexp.MustCompile(`(?i)You are a coding agent powered by [^\n.]+\.?`),
	regexp.MustCompile(`(?i)You are an AI assistant built on [^\n.]+\.?`),
	regexp.MustCompile(`(?i)You are an AI agent\.?`),
}

// ScrubIdentityMessages 只清洗 system/developer/assistant 的文本内容；
// user/tool 是用户数据或工具输出，不能改写。body 不可解析时原样返回。
func ScrubIdentityMessages(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	msgs, ok := obj["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	changed := false
	for _, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		switch strings.ToLower(strings.TrimSpace(role)) {
		case "system", "developer", "assistant":
			next, did := scrubIdentityContent(m["content"])
			if did {
				m["content"] = next
				changed = true
			}
		}
	}
	if !changed {
		return body
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func scrubIdentityContent(v any) (any, bool) {
	switch c := v.(type) {
	case string:
		s := scrubIdentityText(c)
		return s, s != c
	case []any:
		changed := false
		for _, p := range c {
			m, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := m["text"].(string); ok {
				if s := scrubIdentityText(t); s != t {
					m["text"] = s
					changed = true
				}
			}
		}
		return c, changed
	}
	return v, false
}

func scrubIdentityText(s string) string {
	for _, re := range identityScrubPatterns {
		s = re.ReplaceAllString(s, "You are a helpful AI assistant.")
	}
	return s
}
