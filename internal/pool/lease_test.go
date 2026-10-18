package pool

import (
	"fmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"sync"
	"testing"
	"time"
)

func TestPickLeaseConcurrentCapacity(t *testing.T) {
	p := New("")
	p.SetMaxInFlight(1)
	for i := 0; i < 8; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprint(i)})
	}
	leases := make(chan *Lease, 64)
	var wg sync.WaitGroup
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
	if len(leases) != 8 {
		t.Fatalf("acquired %d leases, want 8", len(leases))
	}
	seen := map[string]bool{}
	for l := range leases {
		if seen[l.Auth.UID] {
			t.Fatal("duplicate account lease")
		}
		seen[l.Auth.UID] = true
		l.Release()
		l.Release()
	}
	for _, st := range p.List() {
		if st.InFlight != 0 {
			t.Fatalf("leaked lease for %s", st.UID)
		}
	}
}

func TestLeaseReleaseDoesNotTouchRecreatedUID(t *testing.T) {
	p := New("")
	p.SetMaxInFlight(1)
	p.Add(&auth.Auth{UID: "a"})
	old := p.AcquireLease("a")
	if old == nil {
		t.Fatal("missing old lease")
	}
	p.Remove("a")
	p.Add(&auth.Auth{UID: "a"})
	current := p.AcquireLease("a")
	if current == nil {
		t.Fatal("missing current lease")
	}
	defer current.Release()
	old.Release()
	old.Release()
	st, _ := p.Status("a")
	if st.InFlight != 1 {
		t.Fatal("old release changed new entry")
	}
}

func TestIdlePaidAccountPrecedesBusyFreeAccount(t *testing.T) {
	p := New("")
	p.SetCostExploreInterval(0)
	p.SetRandomSource(func(n int64) int64 { return n - 1 })
	for _, u := range []string{"busy", "idle"} {
		p.Add(&auth.Auth{UID: u})
	}
	p.NoteModelCost("busy", "m", 0, 1000)
	p.NoteModelCost("idle", "m", 1, 1000)
	l := p.AcquireLease("busy")
	defer l.Release()
	picked := p.PickLease(nil, "m", "cn")
	if picked == nil {
		t.Fatal("missing pick")
	}
	defer picked.Release()
	if picked.Auth.UID != "idle" {
		t.Fatal("cost filtering defeated idle priority")
	}
}

func TestPickQueriesUsageOncePerCandidate(t *testing.T) {
	p := New("")
	calls := map[string]int{}
	for i := 0; i < 10; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprint(i)})
	}
	p.SetTokenUsageOf(func(u, m string, w time.Duration) int64 { calls[u]++; return 10 })
	p.PickExcludingForRealm(nil, "deepseek-v4.1-flash", "cn")
	if len(calls) != 10 {
		t.Fatalf("queried %d accounts", len(calls))
	}
	for _, n := range calls {
		if n != 1 {
			t.Fatalf("usage queried %d times for a candidate", n)
		}
	}
}
