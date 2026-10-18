package main

import (
	"testing"
	"time"
)

// TestGlobalCooldownDefaults global 域激进冷却默认值：429/14003 10s、WAF 5s。
func TestGlobalCooldownDefaults(t *testing.T) {
	c, err := ParseConfig([]byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.GlobalSoftRateDur != 10*time.Second {
		t.Errorf("global_soft_rate=%s want 10s", c.GlobalSoftRateDur)
	}
	if c.GlobalWafRateDur != 5*time.Second {
		t.Errorf("global_waf_rate=%s want 5s", c.GlobalWafRateDur)
	}
	if c.Cooldown.GlobalSoftRate != "10s" || c.Cooldown.GlobalWafRate != "5s" {
		t.Errorf("raw defaults=%q/%q", c.Cooldown.GlobalSoftRate, c.Cooldown.GlobalWafRate)
	}
}

// TestGlobalCooldownParsed 显式配置覆盖默认，0 合法（不冷却）。
func TestGlobalCooldownParsed(t *testing.T) {
	c, err := ParseConfig([]byte(`{"cooldown":{"global_soft_rate":"3s","global_waf_rate":"0s"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.GlobalSoftRateDur != 3*time.Second {
		t.Errorf("global_soft_rate=%s want 3s", c.GlobalSoftRateDur)
	}
	if c.GlobalWafRateDur != 0 {
		t.Errorf("global_waf_rate=%s want 0", c.GlobalWafRateDur)
	}
}

// TestGlobalCooldownNegativeRejected 负值无意义，启动即报错。
func TestGlobalCooldownNegativeRejected(t *testing.T) {
	if _, err := ParseConfig([]byte(`{"cooldown":{"global_soft_rate":"-1s"}}`)); err == nil {
		t.Fatal("want error for negative global_soft_rate")
	}
	if _, err := ParseConfig([]byte(`{"cooldown":{"global_waf_rate":"-1s"}}`)); err == nil {
		t.Fatal("want error for negative global_waf_rate")
	}
}
