package server

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

func newCooldownTestPool(t *testing.T, realm string) (*pool.Pool, string) {
	t.Helper()
	p := pool.New("")
	a := &auth.Auth{UID: "acct-" + realm, AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if _, err := auth.BackfillRealmFor(a, realm); err != nil {
		t.Fatal(err)
	}
	p.Add(a)
	return p, a.UID
}

func untilRemaining(t *testing.T, p *pool.Pool, uid string) time.Duration {
	t.Helper()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatalf("account %s not found", uid)
	}
	return time.Until(st.Until)
}

// TestGlobalSoftRateUsesAggressiveBase 14003 在 global 域走 10s 基数；
// CN 仍走 soft_rate=600s。
func TestGlobalSoftRateUsesAggressiveBase(t *testing.T) {
	body := `{"code":14003,"msg":"too many requests"}`

	globalPool, globalUID := newCooldownTestPool(t, "global")
	h := NewHandler(Config{Pool: globalPool, SoftCooldown: 600 * time.Second, GlobalSoftCooldown: 10 * time.Second, GlobalWafCooldown: 5 * time.Second})
	h.applyErrorPolicy(globalUID, upstream.ErrSoftRate, body, "deepseek-v4.1-flash", "global", nil)
	if rem := untilRemaining(t, globalPool, globalUID); rem < 8*time.Second || rem > 12*time.Second {
		t.Fatalf("global 14003 cooldown=%s want ~10s", rem)
	}

	cnPool, cnUID := newCooldownTestPool(t, "cn")
	h = NewHandler(Config{Pool: cnPool, SoftCooldown: 600 * time.Second, GlobalSoftCooldown: 10 * time.Second, GlobalWafCooldown: 5 * time.Second})
	h.applyErrorPolicy(cnUID, upstream.ErrSoftRate, body, "deepseek-v4.1-flash", "cn", nil)
	if rem := untilRemaining(t, cnPool, cnUID); rem < 590*time.Second || rem > 610*time.Second {
		t.Fatalf("cn 14003 cooldown=%s want ~600s", rem)
	}
}

// TestGlobalWafUsesAggressiveBase global WAF 走 5s 基数（含 ±25% 抖动）；
// CN 仍走 60s 基数。
func TestGlobalWafUsesAggressiveBase(t *testing.T) {
	globalPool, globalUID := newCooldownTestPool(t, "global")
	h := NewHandler(Config{Pool: globalPool, SoftCooldown: 600 * time.Second, GlobalSoftCooldown: 10 * time.Second, GlobalWafCooldown: 5 * time.Second})
	h.applyErrorPolicy(globalUID, upstream.ErrWafBlock, "WAF Block Page", "deepseek-v4.1-flash", "global", nil)
	if rem := untilRemaining(t, globalPool, globalUID); rem < 3*time.Second || rem > 7*time.Second {
		t.Fatalf("global waf cooldown=%s want ~5s", rem)
	}

	cnPool, cnUID := newCooldownTestPool(t, "cn")
	h = NewHandler(Config{Pool: cnPool, SoftCooldown: 600 * time.Second, GlobalSoftCooldown: 10 * time.Second, GlobalWafCooldown: 5 * time.Second})
	h.applyErrorPolicy(cnUID, upstream.ErrWafBlock, "WAF Block Page", "deepseek-v4.1-flash", "cn", nil)
	if rem := untilRemaining(t, cnPool, cnUID); rem < 40*time.Second || rem > 80*time.Second {
		t.Fatalf("cn waf cooldown=%s want ~60s", rem)
	}
}

// TestGlobalZeroCooldownDisables 显式 0 表示不冷却：14003 不再把账号移出池。
func TestGlobalZeroCooldownDisables(t *testing.T) {
	p, uid := newCooldownTestPool(t, "global")
	h := NewHandler(Config{Pool: p, SoftCooldown: 600 * time.Second, GlobalSoftCooldown: 0, GlobalWafCooldown: 0})
	h.applyErrorPolicy(uid, upstream.ErrSoftRate, `{"code":14003,"msg":"too many requests"}`, "deepseek-v4.1-flash", "global", nil)
	st, _ := p.Status(uid)
	if st.Cooling {
		t.Fatalf("global 0s cooldown should not cool, got until=%s", st.Until)
	}
	if !st.Until.IsZero() {
		t.Fatalf("global 0s cooldown until=%s want zero", st.Until)
	}
	if st.SoftStreak != 0 {
		t.Fatalf("global 0s cooldown soft_streak=%d want 0", st.SoftStreak)
	}
}
