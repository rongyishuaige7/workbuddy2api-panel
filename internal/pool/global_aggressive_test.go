package pool

import (
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func addAggressiveTestAccount(t *testing.T, p *Pool, realm string) string {
	t.Helper()
	a := &auth.Auth{UID: "uid-" + realm, AccessToken: "token", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	if _, err := auth.BackfillRealmFor(a, realm); err != nil {
		t.Fatal(err)
	}
	p.Add(a)
	return a.UID
}

// TestGlobalShortCooldownAllowsFallback global 10s 短冷却允许兜底提前试探；
// 试探后 3s 内不再重复（避免每请求重打）。
func TestGlobalShortCooldownAllowsFallback(t *testing.T) {
	p := New("")
	uid := addAggressiveTestAccount(t, p, "global")
	p.CooldownSoftRate(uid, 10*time.Second, time.Time{}, "429 rate limit")

	got := p.PickExcludingForRealm(nil, "deepseek-v4.1-flash", "global")
	if got == nil || got.UID != uid {
		t.Fatalf("global short cooldown should allow fallback probe, got %+v", got)
	}
	if again := p.PickExcludingForRealm(nil, "deepseek-v4.1-flash", "global"); again != nil {
		t.Fatalf("fallback should be throttled for 3s, got %+v", again)
	}
}

// TestGlobalLongCooldownStillBlocks 上游明确给出分钟级重置时间时，兜底不能提前撞。
func TestGlobalLongCooldownStillBlocks(t *testing.T) {
	p := New("")
	uid := addAggressiveTestAccount(t, p, "global")
	p.CooldownSoftRate(uid, 10*time.Minute, time.Time{}, "429 rate limit")

	if got := p.PickExcludingForRealm(nil, "deepseek-v4.1-flash", "global"); got != nil {
		t.Fatalf("global long cooldown must not fallback-probe, got %+v", got)
	}
}

// TestCNLongCooldownFallbackUnchanged CN 兜底行为保持原样（不受 global 激进规则影响）。
func TestCNLongCooldownFallbackUnchanged(t *testing.T) {
	p := New("")
	uid := addAggressiveTestAccount(t, p, "cn")
	p.CooldownSoftRate(uid, 10*time.Minute, time.Time{}, "429 rate limit")

	got := p.PickExcludingForRealm(nil, "deepseek-v4.1-flash", "cn")
	if got == nil || got.UID != uid {
		t.Fatalf("cn fallback behavior changed, got %+v", got)
	}
}

// TestGlobalRecoveryLeaseUsesShortInterval 恢复租约释放后，global 下一次试探
// 间隔是 3s，而不是 CN 的 30s。
func TestGlobalRecoveryLeaseUsesShortInterval(t *testing.T) {
	p := New("")
	uid := addAggressiveTestAccount(t, p, "global")
	p.CooldownSoftRate(uid, 10*time.Second, time.Time{}, "429 rate limit")

	lease := p.PickLease(nil, "deepseek-v4.1-flash", "global")
	if lease == nil {
		t.Fatal("expected recovery lease")
	}
	lease.Release()
	st, ok := p.Status(uid)
	if !ok {
		t.Fatal("account missing")
	}
	rem := time.Until(st.NextRecoveryAt)
	if rem < 1*time.Second || rem > 5*time.Second {
		t.Fatalf("global next recovery=%s want ~3s", rem)
	}
}

// TestCN6004HonorsReset CN 6004 仍按上游重置时间（这里 30m，未触发 2h 封顶）。
func TestCN6004HonorsReset(t *testing.T) {
	p := New("")
	uid := addAggressiveTestAccount(t, p, "cn")
	reset := time.Now().Add(30 * time.Minute)
	p.CooldownSoftForModel(uid, 600*time.Second, reset, "deepseek-v4.1-flash", "6004 model rate limit")
	st, _ := p.Status(uid)
	if len(st.RateLimitedModels) != 1 {
		t.Fatalf("rate_limited_models=%+v", st.RateLimitedModels)
	}
	if rem := time.Until(st.RateLimitedModels[0].Until); rem < 29*time.Minute || rem > 31*time.Minute {
		t.Fatalf("cn 6004 until=%s want ~30m", rem)
	}
}

// TestExpiredSoftCooldownReasonHidden 账号级软冷却过期后不再透出旧 reason，
// 避免面板显示「可用 + 429 rate limit + 模型限流」互相矛盾。
func TestExpiredSoftCooldownReasonHidden(t *testing.T) {
	p := New("")
	uid := addAggressiveTestAccount(t, p, "cn")
	p.CooldownSoftRate(uid, 5*time.Millisecond, time.Time{}, "429 rate limit")
	time.Sleep(20 * time.Millisecond)
	st, _ := p.Status(uid)
	if st.Cooling {
		t.Fatalf("expected expired cooldown, got until=%s", st.Until)
	}
	if st.Reason != "" {
		t.Fatalf("expired cooldown reason=%q want empty", st.Reason)
	}
}
