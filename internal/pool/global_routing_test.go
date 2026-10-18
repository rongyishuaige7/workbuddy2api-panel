package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestGlobalStableSessionIgnoresUnverifiedQuota(t *testing.T) {
	for _, tc := range []struct {
		name, domain string
		explicit     bool
		unbind       bool
	}{
		{"global", "www.workbuddy.ai", false, false},
		{"global_explicit_quota", "www.workbuddy.ai", true, true},
		{"cn_unchanged", "www.codebuddy.cn", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New("")
			p.Add(&auth.Auth{UID: "warm", Domain: tc.domain})
			p.Add(&auth.Auth{UID: "idle", Domain: tc.domain})
			p.SetTokenUsageOf(func(uid, model string, _ time.Duration) int64 {
				if uid == "warm" {
					return 300_000_000
				}
				return 0
			})
			if tc.explicit {
				p.SetTokenCeilingOf(func(_, _ string) int64 { return 100_000_000 })
			}
			lease, unbind := p.PickStickyLease("warm", "deepseek-v4.1-flash", "")
			defer lease.Release()
			if unbind != tc.unbind || (lease == nil) != tc.unbind {
				t.Fatalf("lease=%v unbind=%v, want unbind=%v", lease, unbind, tc.unbind)
			}
			if lease != nil && lease.Auth.UID != "warm" {
				t.Fatalf("stable session moved to %s", lease.Auth.UID)
			}
		})
	}
}

func TestGlobalStableSessionStillHonorsRealLimits(t *testing.T) {
	for _, modelOnly := range []bool{false, true} {
		p := New("")
		p.Add(&auth.Auth{UID: "warm", Domain: "www.workbuddy.ai"})
		if modelOnly {
			p.CooldownSoftForModel("warm", time.Minute, time.Now().Add(time.Hour), "deepseek-v4.1-flash", "6004")
		} else {
			p.CooldownSoftRate("warm", time.Minute, time.Time{}, "14003")
		}
		lease, unbind := p.PickStickyLease("warm", "deepseek-v4.1-flash", "global")
		defer lease.Release()
		if lease != nil || !unbind {
			t.Fatalf("modelOnly=%v lease=%v unbind=%v: real limit must invalidate binding", modelOnly, lease, unbind)
		}
	}
}

func TestGlobalNewSessionDoesNotExcludeHighUsageAccount(t *testing.T) {
	p := New("")
	p.SetRandomSource(func(_ int64) int64 { return 0 })
	p.Add(&auth.Auth{UID: "warm", Domain: "www.workbuddy.ai"})
	p.Add(&auth.Auth{UID: "idle", Domain: "www.workbuddy.ai"})
	p.SetCredits("warm", 1000, 1000)
	p.SetCredits("idle", 1, 1)
	p.SetTokenUsageOf(func(uid, _ string, _ time.Duration) int64 {
		if uid == "warm" {
			return 300_000_000
		}
		return 0
	})
	lease := p.PickLease(nil, "deepseek-v4.1-flash", "global")
	defer lease.Release()
	if lease == nil || lease.Auth.UID != "warm" {
		t.Fatalf("lease=%v: an unverified global quota must not exclude warm", lease)
	}
}

func TestGlobalRealCooldownCannotBeBypassedByFallback(t *testing.T) {
	for _, tc := range []struct {
		name, domain string
		blocked      bool
	}{
		{"global", "www.workbuddy.ai", true},
		{"cn_unchanged", "www.codebuddy.cn", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New("")
			p.Add(&auth.Auth{UID: "limited", Domain: tc.domain})
			p.CooldownSoftRate("limited", time.Minute, time.Time{}, "429 rate limit")
			lease := p.PickLease(nil, "deepseek-v4.1-flash", "")
			defer lease.Release()
			if (lease == nil) != tc.blocked {
				t.Fatalf("lease=%v blocked=%v", lease, tc.blocked)
			}
			if tc.blocked {
				p.mu.Lock()
				p.byUID["limited"].until = time.Now().Add(-time.Second)
				p.mu.Unlock()
				recovered := p.PickLease(nil, "deepseek-v4.1-flash", "global")
				defer recovered.Release()
				if recovered == nil {
					t.Fatal("account should return after the real cooldown expires")
				}
			}
		})
	}
}

// TestGlobalModelAndAccountHonorReset global 6004 与账号级 429/Retry-After 都
// 尊重上游明确 resetAt。78 分钟实测显示 10s 本地重试会反复撞 6004（四个账号
// 各 118–131 次、98 次顶层 429），故恢复为按上游时间锁定。
func TestGlobalModelAndAccountHonorReset(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "limited", Domain: "www.workbuddy.ai"})
	p.SetSoftRateMax(time.Minute)
	reset := time.Now().Add(6 * time.Hour)
	p.CooldownSoftForModel("limited", time.Minute, reset, "deepseek-v4.1-flash", "6004")
	st, _ := p.Status("limited")
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("model cooldown missing: %+v", st.RateLimitedModels)
	}
	if !st.RateLimitedModels[0].Until.Equal(reset) {
		t.Fatalf("global model cooldown=%v want=%v", st.RateLimitedModels[0].Until, reset)
	}
	p.CooldownSoftRate("limited", time.Minute, reset, "429 rate limit (retry-after)")
	st, _ = p.Status("limited")
	if !st.Until.Equal(reset) {
		t.Fatalf("account cooldown=%v want=%v", st.Until, reset)
	}
}
