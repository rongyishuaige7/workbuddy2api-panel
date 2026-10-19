// Package ratelimit 进程内的每密钥固定窗口限速。
//
// 单实例部署下无需 Redis：每个密钥一个 1 分钟窗口，窗口内计数达到 rpm 即拒绝。
// 固定窗口实现简单、可预测；代价是窗口边界附近可能有短暂双倍突发，对「给他人
// 一把 key 防止刷爆账号池」这个场景足够。
package ratelimit

import (
	"sync"
	"time"
)

// Limiter 并发安全的每密钥限速器。
type Limiter struct {
	mu      sync.Mutex
	windows map[string]*window
	now     func() time.Time // 测试注入；生产恒为 time.Now
}

type window struct {
	start time.Time
	count int
}

// New 构建限速器。
func New() *Limiter {
	return &Limiter{windows: make(map[string]*window), now: time.Now}
}

// Allow 判断 id 当前是否放行。rpm<=0 表示不限速（恒放行）。
// 拒绝时返回建议等待时长（Retry-After）。
func (l *Limiter) Allow(id string, rpm int) (bool, time.Duration) {
	if rpm <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.windows[id]
	if w == nil || now.Sub(w.start) >= time.Minute {
		w = &window{start: now}
		l.windows[id] = w
	}
	if w.count >= rpm {
		d := time.Minute - now.Sub(w.start)
		if d < 0 {
			d = 0
		}
		return false, d
	}
	w.count++
	return true, 0
}

// GC 清理超过 maxIdle 未活动的窗口，避免删除/轮换密钥后 map 无限增长。
func (l *Limiter) GC(maxIdle time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for id, w := range l.windows {
		if now.Sub(w.start) > maxIdle {
			delete(l.windows, id)
		}
	}
}
