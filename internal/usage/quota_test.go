package usage

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestQuotaDayBoundaryIndependentOfDisplayTimezone(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("UTC-4", -4*3600)
	defer func() { time.Local = old }()
	now := time.Now().In(quotaLocation)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, quotaLocation)
	r := New("")
	r.Add(midnight.Add(-time.Nanosecond).In(time.Local), "cn", "a", "m", Delta{HasPromptTokens: true, PromptTokens: 7, HasCompletion: true, CompletionTokens: 3}, true)
	r.Add(midnight.In(time.Local), "cn", "a", "m", Delta{HasTotal: true, TotalTokens: 20}, true)
	r.Add(midnight.In(time.Local), "cn", "b", "m", Delta{HasTotal: true, TotalTokens: 99}, true)
	if n := r.quotaTokensAt(midnight.Add(-time.Nanosecond), "a", "m"); n != 10 {
		t.Fatalf("previous day=%d", n)
	}
	if n := r.QuotaTokensToday("a", "m"); n != 20 {
		t.Fatalf("current day=%d", n)
	}
	if n := r.quotaTokensAt(midnight.AddDate(0, 0, 1), "a", "m"); n != 0 {
		t.Fatalf("stale usage after midnight=%d", n)
	}
	r.Add(midnight.AddDate(0, 0, 1).In(time.Local), "cn", "a", "m", Delta{HasTotal: true, TotalTokens: 30}, true)
	if n := r.quotaTokensAt(midnight.AddDate(0, 0, 1), "a", "m"); n != 30 {
		t.Fatalf("new day=%d", n)
	}
}

func TestQuotaReloadAndConservativeBoundaryBucket(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("UTC+5:30", 19800)
	defer func() { time.Local = old }()
	now := time.Now().In(quotaLocation)
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, quotaLocation)
	path := filepath.Join(t.TempDir(), "usage.json")
	r := New(path)
	// 当地 21 点桶跨越 UTC+8 午夜（21:30）；旧格式无法精确拆分，重载时两天均保守计入。
	r.Add(midnight.Add(-10*time.Minute).In(time.Local), "cn", "a", "m", Delta{HasTotal: true, TotalTokens: 10}, true)
	r.Save()
	loaded := New(path)
	if n := loaded.QuotaTokensToday("a", "m"); n != 10 {
		t.Fatalf("boundary bucket was lost: %d", n)
	}
	loaded.Add(midnight.Add(10*time.Minute).In(time.Local), "cn", "a", "m", Delta{HasTotal: true, TotalTokens: 5}, true)
	loaded.Rollup(time.Now())
	if n := loaded.QuotaTokensToday("a", "m"); n != 15 {
		t.Fatalf("increment/rollup changed quota: %d", n)
	}
}

func TestQuotaConcurrentRecording(t *testing.T) {
	r := New("")
	now := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				r.Add(now, "cn", "a", "m", Delta{HasTotal: true, TotalTokens: 1}, true)
				r.QuotaTokensToday("a", "m")
			}
		}()
	}
	wg.Wait()
	if n := r.QuotaTokensToday("a", "m"); n != 4000 {
		t.Fatalf("lost updates: %d", n)
	}
}
