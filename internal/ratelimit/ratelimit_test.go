package ratelimit

import (
	"testing"
	"time"
)

func TestAllowWindowAndReset(t *testing.T) {
	l := New()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }

	if ok, _ := l.Allow("k1", 2); !ok {
		t.Fatal("first request should pass")
	}
	if ok, _ := l.Allow("k1", 2); !ok {
		t.Fatal("second request should pass")
	}
	ok, retry := l.Allow("k1", 2)
	if ok {
		t.Fatal("third request should be limited")
	}
	if retry <= 0 || retry > time.Minute {
		t.Fatalf("retry=%v", retry)
	}

	// 窗口滚动后重新计数。
	now = now.Add(time.Minute)
	if ok, _ := l.Allow("k1", 2); !ok {
		t.Fatal("new window should pass")
	}
}

func TestUnlimitedAndSeparateKeys(t *testing.T) {
	l := New()
	for i := 0; i < 100; i++ {
		if ok, _ := l.Allow("unlimited", 0); !ok {
			t.Fatal("rpm=0 should never limit")
		}
	}
	if ok, _ := l.Allow("a", 1); !ok {
		t.Fatal("a first")
	}
	if ok, _ := l.Allow("b", 1); !ok {
		t.Fatal("b is independent")
	}
	if ok, _ := l.Allow("a", 1); ok {
		t.Fatal("a second should be limited")
	}
}

func TestGC(t *testing.T) {
	l := New()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.now = func() time.Time { return now }
	l.Allow("old", 5)
	now = now.Add(10 * time.Minute)
	l.GC(5 * time.Minute)
	if _, ok := l.windows["old"]; ok {
		t.Fatal("stale window should be collected")
	}
}
