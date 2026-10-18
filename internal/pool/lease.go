package pool

import (
	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"sync"
)

// Lease 将释放绑定到本次占用的 entry，UID 移除/重建不会影响其他请求。
// Release 可重复调用；不得复制 Lease。
type Lease struct {
	Auth  *auth.Auth
	entry *entry
	pool  *Pool
	once  sync.Once
}

func (l *Lease) Release() {
	if l != nil {
		l.once.Do(func() { l.pool.mu.Lock(); defer l.pool.mu.Unlock(); releaseEntry(l.entry) })
	}
}

func (p *Pool) leaseLocked(a *auth.Auth) *Lease {
	if a == nil {
		return nil
	}
	e := p.byUID[a.UID]
	e.inFlight.Add(1)
	return &Lease{Auth: a, entry: e, pool: p}
}

// AcquireLease 供固定账号的后台请求占用名额；不改变账号健康状态。
func (p *Pool) AcquireLease(uid string) *Lease {
	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.byUID[uid]
	if e == nil || p.inFlightFull(e) {
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
