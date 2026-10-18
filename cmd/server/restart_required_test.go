package main

import "testing"

func hasRestartField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

// TestRestartRequiredFieldsOnlyChanged 只报告真实变化的装配期字段：
// 热生效字段改了不应触发重启提示，装配期字段改了才出现。
func TestRestartRequiredFieldsOnlyChanged(t *testing.T) {
	old := Default()
	if got := restartRequiredFields(old, Default()); len(got) != 0 {
		t.Fatalf("no-op save returned restart fields: %v", got)
	}

	hot := Default()
	hot.APIKey = "new-key"
	hot.Cooldown.SoftRate = "30s"
	hot.Pool.CreditFloor = 50
	hot.Pool.MaxInFlight = 8
	hot.Schedule.CheckinHours = []int{1, 13}
	hot.Schedule.CheckinEnabled = false
	hot.Features.SanitizeBlacklistFingerprints = false
	hot.Logging.RequestClientInfo = false
	if got := restartRequiredFields(old, hot); len(got) != 0 {
		t.Fatalf("hot-only changes returned restart fields: %v", got)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"listen", func(c *Config) { c.Listen = ":9999" }, "listen"},
		{"auth_dir", func(c *Config) { c.AuthDir = "./other-auths" }, "auth_dir"},
		{"state_file", func(c *Config) { c.StateFile = "./other-state.json" }, "state_file"},
		{"upstream timeout", func(c *Config) { c.Upstream.TimeoutSeconds = 30 }, "upstream.timeout_seconds"},
		{"upstream header timeout", func(c *Config) { c.Upstream.HeaderTimeoutSeconds = 30 }, "upstream.header_timeout_seconds"},
		{"upstream idle timeout", func(c *Config) { c.Upstream.IdleTimeoutSeconds = 30 }, "upstream.idle_timeout_seconds"},
		{"upstream ua", func(c *Config) { c.Upstream.UserAgent = "custom/1.0" }, "upstream.user_agent"},
		{"upstream client version", func(c *Config) { c.Upstream.ClientVersion = "9.9.9" }, "upstream.client_version"},
		{"upstream cli version", func(c *Config) { c.Upstream.CliVersion = "9.9.9" }, "upstream.cli_version"},
		{"upstream client name", func(c *Config) { c.Upstream.ClientName = "WorkBuddy" }, "upstream.client_name"},
		{"upstream device token", func(c *Config) { c.Upstream.DeviceToken = "tok" }, "upstream.device_token"},
		{"upstream device token file", func(c *Config) { c.Upstream.DeviceTokenFile = "/tmp/tok" }, "upstream.device_token_file"},
		{"upstream passthrough ip", func(c *Config) { c.Upstream.PassthroughIP = true }, "upstream.passthrough_ip"},
		{"global enabled", func(c *Config) { c.Global.Enabled = false }, "global.enabled"},
		{"global chat base", func(c *Config) { c.Global.ChatBase = "https://example.test" }, "global.chat_base"},
		{"global billing base", func(c *Config) { c.Global.BillingBase = "https://example.test" }, "global.billing_base"},
		{"session sticky enabled", func(c *Config) { c.SessionSticky.Enabled = false }, "session_sticky.enabled"},
		{"session sticky ttl", func(c *Config) { c.SessionSticky.TTL = "1h" }, "session_sticky.ttl"},
		{"session sticky gc", func(c *Config) { c.SessionSticky.GCInterval = "1m" }, "session_sticky.gc_interval"},
		{"prompt mode", func(c *Config) { c.Prompt.Mode = "custom" }, "prompt.mode"},
		{"prompt file", func(c *Config) { c.Prompt.File = "/tmp/prompt.md" }, "prompt.file"},
		{"archive enabled", func(c *Config) { c.Logging.RequestArchiveEnabled = false }, "logging.request_archive_enabled"},
		{"archive retention", func(c *Config) { c.Logging.RequestRetentionDays = 3 }, "logging.request_retention_days"},
		{"archive max mb", func(c *Config) { c.Logging.RequestArchiveMaxMB = 20 }, "logging.request_archive_max_mb"},
		{"server read timeout", func(c *Config) { c.Server.ReadTimeout = "60s" }, "server.read_timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			newCfg := Default()
			tc.mutate(newCfg)
			got := restartRequiredFields(old, newCfg)
			if !hasRestartField(got, tc.want) {
				t.Fatalf("restart fields %v missing %q", got, tc.want)
			}
		})
	}
}

// TestRestartRequiredFieldsUpstashCleared 清空 Upstash 同样是装配期变化：
// 旧实现只在「新配置非空」时列 upstash，清空时反而不提示。
func TestRestartRequiredFieldsUpstashCleared(t *testing.T) {
	old := Default()
	old.Upstash.URL = "https://example.upstash.io"
	old.Upstash.Token = "token"
	if got := restartRequiredFields(old, Default()); !hasRestartField(got, "upstash") {
		t.Fatalf("clearing upstash should require restart, got %v", got)
	}
}

// TestRestartRequiredFieldsFallback 旧配置无法解析时退回完整清单，且清单必须
// 覆盖 prompt / sticky 这两个此前漏掉的装配期字段。
func TestRestartRequiredFieldsFallback(t *testing.T) {
	got := restartRequiredFields(nil, Default())
	for _, want := range []string{"listen", "prompt.mode", "prompt.file", "session_sticky.enabled", "upstash"} {
		if !hasRestartField(got, want) {
			t.Fatalf("fallback restart fields %v missing %q", got, want)
		}
	}
}
