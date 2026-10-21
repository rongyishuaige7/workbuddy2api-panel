package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestScrubIdentityMessagesScope(t *testing.T) {
	body := []byte(`{
		"model":"deepseek-v4.1-flash",
		"messages":[
			{"role":"system","content":"You are an AI agent powered by DeepSeek Harness. Keep rules."},
			{"role":"developer","content":"you are a coding agent powered by Foo. Do X."},
			{"role":"assistant","content":"You are an AI assistant built on Bar. Done."},
			{"role":"user","content":"You are an AI agent powered by DeepSeek Harness."},
			{"role":"tool","content":"You are an AI agent powered by DeepSeek Harness."}
		]
	}`)
	out := ScrubIdentityMessages(body)
	var obj struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatal(err)
	}
	for i, m := range obj.Messages {
		switch m.Role {
		case "system", "developer", "assistant":
			if !strings.HasPrefix(m.Content, "You are a helpful AI assistant.") {
				t.Errorf("role %s not scrubbed: %q", m.Role, m.Content)
			}
			if strings.Contains(m.Content, "DeepSeek Harness") || strings.Contains(m.Content, "powered by") || strings.Contains(m.Content, "built on") {
				t.Errorf("role %s still has identity: %q", m.Role, m.Content)
			}
		case "user", "tool":
			if !strings.Contains(m.Content, "You are an AI agent powered by DeepSeek Harness.") {
				t.Errorf("role %s must stay untouched: %q", m.Role, m.Content)
			}
		default:
			t.Errorf("unexpected role[%d]=%s", i, m.Role)
		}
	}
	// 幂等
	if again := ScrubIdentityMessages(out); string(again) != string(out) {
		t.Errorf("scrub is not idempotent")
	}
}

func TestScrubIdentityCaseInsensitive(t *testing.T) {
	body := []byte(`{"messages":[{"role":"system","content":"YOU ARE AN AI AGENT POWERED BY DEEPSEEK HARNESS."}]}`)
	out := string(ScrubIdentityMessages(body))
	if !strings.Contains(out, "You are a helpful AI assistant.") || strings.Contains(strings.ToLower(out), "deepseek harness") {
		t.Fatalf("out=%s", out)
	}
}

func TestPrepareBodyIdentityScrubToggle(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash","messages":[{"role":"system","content":"You are an AI agent powered by DeepSeek Harness. Keep."}]}`)
	off := (&Client{}).prepareBody(body, "cn", "u", "")
	if !strings.Contains(string(off), "DeepSeek Harness") {
		t.Fatalf("default must not scrub: %s", off)
	}
	on := (&Client{IdentityScrub: true}).prepareBody(body, "cn", "u", "")
	if strings.Contains(string(on), "DeepSeek Harness") || !strings.Contains(string(on), "You are a helpful AI assistant.") {
		t.Fatalf("enabled must scrub: %s", on)
	}
}
