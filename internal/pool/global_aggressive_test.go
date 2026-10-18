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
