package promptlog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	CaptureVersion   = 2
	maxInputMetadata = 2048
	maxCaptureChars  = 500000
)

// Input describes the original user message, before flattening or truncation.
// Hash includes multimodal content, so distinct images have distinct identities.
type Input struct {
	Index     int    `json:"index"`
	Hash      string `json:"hash"`
	Chars     int    `json:"chars"`
	Harness   bool   `json:"harness"`
	Captured  bool   `json:"captured"`
	Truncated bool   `json:"truncated"`
}

// Capture reserves 80% of a constrained payload for user input. Metadata is
// collected first, so omitted or truncated text cannot change turn identity.
func Capture(body []byte, maxChars int) CaptureResult {
	var req struct {
		ConversationID    any              `json:"conversation_id"`
		ConversationCamel any              `json:"conversationId"`
		PromptCacheKey    any              `json:"prompt_cache_key"`
		Metadata          any              `json:"metadata"`
		Messages          []map[string]any `json:"messages"`
		Tools             []struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if json.Unmarshal(body, &req) != nil || len(req.Messages) == 0 {
		return CaptureResult{}
	}
	if maxChars <= 0 {
		maxChars = defaultMaxChars
	}
	if maxChars > maxCaptureChars {
		maxChars = maxCaptureChars
	}
	res := CaptureResult{OriginalMessages: len(req.Messages)}
	// Content-derived routing keys can collide across independent conversations.
	// Only explicit conversation identities may suppress repeated input.
	meta, _ := req.Metadata.(map[string]any)
	for _, value := range []any{meta["conversation_id"], meta["conversationId"], req.ConversationID, req.ConversationCamel, req.PromptCacheKey} {
		if key, ok := value.(string); ok && key != "" {
			h := sha256.Sum256([]byte(key))
			res.SessionHash = hex.EncodeToString(h[:16])
			break
		}
	}
	userCount := 0
	for _, m := range req.Messages {
		if m["role"] == "user" {
			userCount++
		}
	}
	realIndexes := []int{}
	userOrdinal := 0
	for i, m := range req.Messages {
		if m["role"] != "user" {
			continue
		}
		text := contentText(m["content"])
		harness := IsHarnessContext(text)
		if !harness && strings.TrimSpace(text) != "" {
			realIndexes = append(realIndexes, i)
		}
		if userCount <= maxInputMetadata || userOrdinal < maxInputMetadata/2 || userOrdinal >= userCount-maxInputMetadata/2 {
			raw, _ := json.Marshal(struct {
				Name    any `json:"name"`
				Content any `json:"content"`
			}{m["name"], m["content"]})
			h := sha256.Sum256(raw)
			res.Inputs = append(res.Inputs, Input{Index: i, Hash: hex.EncodeToString(h[:]), Chars: utf8.RuneCountInString(text), Harness: harness})
		} else {
			res.InputsOmitted++
		}
		userOrdinal++
	}

	// Tool names share a small portion of the payload budget; names themselves
	// are bounded as well. Never retain their schemas.
	toolBudget := maxChars / 20
	toolUsed := 0
	seenTools := map[string]bool{}
	for _, t := range req.Tools {
		name := t.Name
		if name == "" {
			name = t.Function.Name
		}
		if name == "" || seenTools[name] {
			continue
		}
		seenTools[name] = true
		runes := []rune(name)
		if len(runes) > 64 {
			name = string(runes[:64])
			res.Truncated = true
		}
		raw, _ := json.Marshal(name)
		cost := utf8.RuneCount(raw) + 1
		if len(res.Tools) == 200 || toolUsed+cost > toolBudget {
			res.Truncated = true
			continue
		}
		res.Tools = append(res.Tools, name)
		toolUsed += cost
	}
	budget := max(0, maxChars-toolUsed-32) // array/object separators and field names
	type candidate struct {
		msg   Message
		chars int
		cut   bool
	}
	candidates := make([]candidate, len(req.Messages))
	allFit, total := true, 0
	for i, m := range req.Messages {
		msg, _, cut, ok := fitMessage(m, maxMessageRunes)
		if !ok && m["role"] == "user" {
			// An oversized extension field must not discard the user's text.
			msg, _, _, ok = fitMessage(map[string]any{"role": "user", "content": contentText(m["content"])}, maxMessageRunes)
			cut = true
		}
		if !ok {
			allFit = false
			continue
		}
		raw, _ := json.Marshal(msg)
		cost := utf8.RuneCount(raw) + 1
		candidates[i] = candidate{msg, cost, cut}
		total += cost
	}
	selected := map[int]candidate{}
	used := 0
	take := func(i, cap int) {
		c := candidates[i]
		if c.msg == nil || cap <= 0 {
			return
		}
		if old, ok := selected[i]; ok {
			cap += old.chars
		}
		if c.chars > cap {
			m, _, cut, ok := fitMessage(c.msg, cap-1)
			if !ok {
				return
			}
			raw, _ := json.Marshal(m)
			c = candidate{m, utf8.RuneCount(raw) + 1, c.cut || cut}
		}
		if old, ok := selected[i]; ok {
			used -= old.chars
		}
		selected[i] = c
		used += c.chars
	}
	if allFit && total <= budget {
		for i := range candidates {
			take(i, budget-used)
		}
	} else {
		userBudget := budget * 4 / 5
		if len(realIndexes) > 1 {
			take(realIndexes[0], userBudget/4)
		}
		for j := len(realIndexes) - 1; j >= 0; j-- {
			i := realIndexes[j]
			if _, ok := selected[i]; !ok {
				take(i, userBudget-used)
			}
		}
		if len(realIndexes) > 0 {
			take(realIndexes[0], userBudget-used)
		}
		for i, m := range req.Messages {
			if m["role"] == "system" || m["role"] == "developer" {
				take(i, (budget-used)/2)
				break
			}
		}
		for i := len(candidates) - 1; i >= 0; i-- {
			if _, ok := selected[i]; !ok {
				take(i, budget-used)
			}
		}
		// Return unused context allocation to partially captured user input.
		for j := len(realIndexes) - 1; j >= 0 && used < budget; j-- {
			take(realIndexes[j], budget-used)
		}
	}
	for i := range req.Messages {
		if c, ok := selected[i]; ok {
			res.Messages = append(res.Messages, c.msg)
			res.MessageIndexes = append(res.MessageIndexes, i)
			res.Truncated = res.Truncated || c.cut
		}
	}
	for j := range res.Inputs {
		if c, ok := selected[res.Inputs[j].Index]; ok {
			res.Inputs[j].Captured = true
			res.Inputs[j].Truncated = c.cut
		}
	}
	res.Omitted = len(req.Messages) - len(res.Messages)
	raw, _ := json.Marshal(struct {
		Messages []Message `json:"messages"`
		Tools    []string  `json:"tools,omitempty"`
	}{res.Messages, res.Tools})
	res.Bytes = len(raw)
	return res
}

func contentText(v any) string {
	if text, ok := flattenContent(v); ok {
		return text
	}
	if v == nil {
		return ""
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}

func flattenContent(v any) (string, bool) {
	switch c := v.(type) {
	case string:
		return c, true
	case []any:
		parts := make([]string, 0, len(c))
		for _, p := range c {
			switch part := p.(type) {
			case string:
				parts = append(parts, part)
			case map[string]any:
				typ, _ := part["type"].(string)
				switch typ {
				case "text", "input_text":
					if t, ok := part["text"].(string); ok {
						parts = append(parts, t)
					}
				case "image_url", "image", "input_image":
					parts = append(parts, "[图片]")
				default:
					if t, ok := part["text"].(string); ok {
						parts = append(parts, t)
					} else {
						parts = append(parts, "["+typ+"]")
					}
				}
			}
		}
		return strings.Join(parts, "\n"), true
	}
	return "", false
}

// fitMessage uses serialized Unicode character counts, including JSON overhead.
// Binary search keeps the longest fitting prefix instead of dropping 25% at once.
func fitMessage(m map[string]any, cap int) (Message, int, bool, bool) {
	if cap <= 0 {
		return nil, 0, false, false
	}
	cp := make(Message, len(m))
	for k, v := range m {
		cp[k] = v
	}
	cut := false
	if s, ok := flattenContent(cp["content"]); ok {
		runes := []rune(s)
		if len(runes) > maxMessageRunes {
			runes = runes[:maxMessageRunes]
			cut = true
		}
		cp["content"] = string(runes)
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		return nil, 0, false, false
	}
	if utf8.RuneCount(raw) <= cap {
		return cp, len(raw), cut, true
	}
	s, ok := cp["content"].(string)
	if !ok {
		return nil, 0, false, false
	}
	runes := []rune(s)
	cp["content"] = ""
	raw, _ = json.Marshal(cp)
	if utf8.RuneCount(raw) > cap {
		return nil, 0, false, false
	}
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		cp["content"] = string(runes[:mid])
		raw, _ = json.Marshal(cp)
		if utf8.RuneCount(raw) <= cap {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	cp["content"] = string(runes[:lo])
	raw, _ = json.Marshal(cp)
	return cp, len(raw), true, true
}
