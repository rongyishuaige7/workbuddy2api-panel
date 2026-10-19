package promptlog

import (
	"fmt"
	"sort"
	"strings"
)

// FilterEntries builds a view without modifying archived message objects.
// Fresh comparisons use complete pre-truncation identities within each key/session.
func FilterEntries(entries []Entry, mode string) []Entry {
	out := make([]Entry, len(entries))
	copy(out, entries)
	if mode != "fresh" && mode != "real" && mode != "user" {
		return out
	}
	if mode != "fresh" {
		for i := range out {
			out[i].Messages, out[i].MessageIndexes = userMessages(out[i], mode == "real", nil)
		}
		return out
	}
	order := make([]int, len(out))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := out[order[i]], out[order[j]]
		if a.Time.Equal(b.Time) {
			return a.RequestID < b.RequestID
		}
		return a.Time.Before(b.Time)
	})
	history := map[string][]Input{}
	for _, i := range order {
		e := &out[i]
		unknown := func(reason string) {
			e.FreshState = "unknown"
			e.FreshReason = reason
			e.Messages, e.MessageIndexes = userMessages(*e, true, nil)
		}
		key := e.KeyID + "|" + e.SessionHash
		if e.Version < CaptureVersion || len(e.MessageIndexes) != len(e.Messages) {
			delete(history, key)
			unknown("legacy")
			continue
		}
		if e.SessionHash == "" {
			unknown("no_session")
			continue
		}
		if e.InputsOmitted > 0 {
			delete(history, key)
			unknown("metadata_incomplete")
			continue
		}
		inputs := []Input{}
		for _, input := range e.Inputs {
			if !input.Harness && input.Chars > 0 {
				inputs = append(inputs, input)
			}
		}
		if len(inputs) == 0 {
			e.FreshState = "no_new"
			e.Messages = []Message{}
			e.MessageIndexes = nil
			delete(history, key)
			continue
		}
		prev, exists := history[key]
		history[key] = inputs
		if !exists {
			unknown("window_start")
			continue
		}
		prefix := len(prev) <= len(inputs)
		if prefix {
			for j := range prev {
				if prev[j].Hash != inputs[j].Hash {
					prefix = false
					break
				}
			}
		}
		if !prefix {
			unknown("history_changed")
			continue
		}
		fresh := inputs[len(prev):]
		e.FreshState = "no_new"
		if len(fresh) > 0 {
			e.FreshState = "new"
		}
		wanted := map[int]bool{}
		for _, input := range fresh {
			wanted[input.Index] = true
		}
		e.Messages, e.MessageIndexes = userMessages(*e, true, wanted)
	}
	return out
}

func userMessages(e Entry, realOnly bool, wanted map[int]bool) ([]Message, []int) {
	metadata := map[int]Input{}
	for _, input := range e.Inputs {
		metadata[input.Index] = input
	}
	type positioned struct {
		index   int
		message Message
	}
	items := []positioned{}
	found := map[int]bool{}
	for j, m := range e.Messages {
		if m["role"] != "user" {
			continue
		}
		index := j
		if len(e.MessageIndexes) == len(e.Messages) {
			index = e.MessageIndexes[j]
		}
		if wanted != nil && !wanted[index] {
			continue
		}
		if realOnly {
			if input, ok := metadata[index]; ok {
				if input.Harness || input.Chars == 0 {
					continue
				}
			} else {
				text := strings.TrimSpace(contentText(m["content"]))
				if text == "" || IsHarnessContext(text) || strings.HasPrefix(text, "[内容过大，已省略") {
					continue
				}
			}
		}
		items = append(items, positioned{index, m})
		found[index] = true
	}
	for _, input := range e.Inputs {
		if found[input.Index] || (wanted != nil && !wanted[input.Index]) || (realOnly && (input.Harness || input.Chars == 0)) {
			continue
		}
		items = append(items, positioned{input.Index, Message{"role": "user", "content": fmt.Sprintf("[这条输入的正文未保留：原始 %d 字，超出归档预算]", input.Chars)}})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].index < items[j].index })
	out := make([]Message, 0, len(items))
	indexes := make([]int, 0, len(items))
	for _, item := range items {
		indexes = append(indexes, item.index)
		out = append(out, item.message)
	}
	return out, indexes
}
