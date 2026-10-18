package pool

import (
	"sync"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// Lease 将释放绑定到本次占用的 entry，UID 移除/重建不会影响其他请求。
// Release 可重复调用；不得复制 Lease。
type Lease struct {
	Auth     *auth.Auth
	entry    *entry
	pool     *Pool
	once     sync.Once
	recovery bool
}

func (l *Lease) Release() {
	if l != nil {
		l.once.Do(func() {
			l.pool.mu.Lock()
			defer l.pool.mu.Unlock()
			releaseEntry(l.entry)
			if l.recovery {
				l.entry.recoveryInFlight = false
				l.entry.nextRecovery = time.Now().Add(recoveryProbeInterval)
			}
		})
	}
}

func (p *Pool) leaseLocked(a *auth.Auth) *Lease {
	if a == nil {
		return nil
	}
	e := p.byUID[a.UID]
	recovery := !e.healthy(time.Now())
	if recovery {
		e.recoveryInFlight = true
	}
	e.inFlight.Add(1)
	return &Lease{Auth: a, entry: e, pool: p, recovery: recovery}
}

// AcquireLease 供固定账号的后台请求占用名额；不改变账号健康状态。
func (p *Pool) AcquireLease(uid string) *Lease {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.byUID[uid]
	if e == nil || p.inFlightFull(e) || e.recoveryInFlight || (!e.healthy(time.Now()) && e.inFlight.Load() > 0) {
		return nil
	}
	return p.leaseLocked(e.a)
}

// PickLease 在同一临界区内选号和占用名额。
func (p *Pool) PickLease(tried map[string]bool, model, realm string) *Lease {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.leaseLocked(p.pickLocked(tried, model, realm))
}

func (p *Pool) PickStickyLease(uid, model, realm string) (*Lease, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, unbind := p.pickStickyLocked(uid, model, realm)
	return p.leaseLocked(a), unbind
}
