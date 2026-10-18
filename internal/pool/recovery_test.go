package pool

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

func TestFallbackNeverBypassesModelLock(t *testing.T) {
	for _, before := range []bool{false, true} {
		for _, modelKind := range []string{"missing", "limited"} {
			t.Run(fmt.Sprintf("%s/lock_first=%v", modelKind, before), func(t *testing.T) {
				p := New("")
				p.Add(&auth.Auth{UID: "a"})
				lock := func() {
					if modelKind == "missing" {
						p.BlockModelBackoff("a", "m", "11102 model not available")
					} else {
						p.CooldownSoftForModel("a", time.Minute, time.Now().Add(time.Hour), "m", "6004 model rate limit")
					}
				}
				if before {
					lock()
				}
				p.Cooldown("a", CoolSoft, time.Hour, "account limit")
				if !before {
					lock()
				}
				if l := p.PickLease(nil, "m", "cn"); l != nil {
					l.Release()
					t.Fatal("fallback bypassed model lock")
				}
				l := p.PickLease(nil, "other", "cn")
				if l == nil {
					t.Fatal("other model should allow account recovery")
				}
				l.Release()
			})
		}
	}
}

func TestRecoverySingleFlightAndInterval(t *testing.T) {
	p := New("")
	p.Add(&auth.Auth{UID: "a"})
	p.Cooldown("a", CoolSoft, time.Hour, "rate limit")
	var wg sync.WaitGroup
	leases := make(chan *Lease, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if l := p.PickLease(nil, "m", "cn"); l != nil {
				leases <- l
			}
		}()
	}
	wg.Wait()
	close(leases)
	if len(leases) != 1 {
		t.Fatalf("concurrent probes=%d, want 1 even with unlimited normal capacity", len(leases))
	}
	l := <-leases
	// 即使普通冷却已自然到期，仍不能在试探完成前放入其他请求。
	p.mu.Lock()
	p.byUID["a"].until = time.Now().Add(-time.Second)
	p.mu.Unlock()
	if next := p.PickLease(nil, "m", "cn"); next != nil {
		next.Release()
		t.Fatal("probe overlap at cooldown expiry")
	}
	p.mu.Lock()
	p.byUID["a"].until = time.Now().Add(time.Hour)
	p.mu.Unlock()
	l.Release()
	if next := p.PickLease(nil, "m", "cn"); next != nil {
		next.Release()
		t.Fatal("probe interval was bypassed")
	}
	p.mu.Lock()
	p.byUID["a"].nextRecovery = time.Now().Add(-time.Second)
	p.mu.Unlock()
	next := p.PickLease(nil, "m", "cn")
	if next == nil {
		t.Fatal("probe not allowed after interval")
	}
	p.NoteSuccess("a")
	next.Release()
	normal := p.PickLease(nil, "m", "cn")
	if normal == nil {
		t.Fatal("successful recovery did not return account to normal routing")
	}
	normal.Release()
	st, _ := p.Status("a")
	if st.Cooling || st.RecoveryInFlight || st.InFlight != 0 {
		t.Fatal("recovery left account blocked")
	}
}
