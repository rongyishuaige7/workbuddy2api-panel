package pool

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/usage"
)

// 本文件为 P 组性能审查的量化基准（go test -bench 可复现），结论见 REVIEW-conflicts-perf.md。

// benchPool 构建 46 账号的池（对齐生产规模），全部 healthy 且 credits 各不相同。
func benchPool(b *testing.B) *Pool {
	p := New("")
	for i := 0; i < 46; i++ {
		p.Add(&auth.Auth{UID: fmt.Sprintf("u%02d", i)})
		p.SetCredits(fmt.Sprintf("u%02d", i), int64(1000-i*13%900), 0)
	}
	return p
}

// BenchmarkPick46Accounts P1：46 账号全扫描 + 全排序 + 三因子权重抽签的每次耗时。
func BenchmarkPick46Accounts(b *testing.B) {
	p := benchPool(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p.Pick()
	}
}

// BenchmarkStateSerialize46Accounts P2：46 账号 state.json 序列化（stateOverviewLocked + MarshalIndent）。
func BenchmarkStateSerialize46Accounts(b *testing.B) {
	p := benchPool(b)
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.MarshalIndent(sf, "", "  "); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStateSerializeCompact46 P2 对照：json.Marshal（非缩进）耗时，量化写放大的下界。
func BenchmarkStateSerializeCompact46(b *testing.B) {
	p := benchPool(b)
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(sf); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSnapshotMarshal46 P2/P4 相关：saveLocked 落盘时额外镜像一次 snapshot（含 savedAt）的序列化成本。
func BenchmarkSnapshotMarshal46(b *testing.B) {
	p := benchPool(b)
	p.mu.Lock()
	sf := p.stateOverviewLocked()
	p.mu.Unlock()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		now := time.Now()
		if _, err := json.Marshal(snapshot{stateFile: sf, SavedAt: now}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGoroutineSpawn P4 量化：单次 fire-and-forget goroutine 起搏的 CPU 成本下界
// （对应对 Session.SetBind / pool SaveState 每次镜像派生 goroutine 的开销，不含网络，
//
//	网络走 5s 超时 fire-and-forget）。
func BenchmarkGoroutineSpawn(b *testing.B) {
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		go func() { _ = 1 }()
	}
}

// 调度不得随永久保留的历史桶数量线性退化。
func BenchmarkPickWithQuotaHistory(b *testing.B) {
	for _, n := range []int{0, 10_000, 50_000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			p := benchPool(b)
			r := usage.New("")
			now := time.Now()
			for i := 0; i < n; i++ {
				r.Add(now.Add(-time.Duration(i/46)*time.Hour), "cn", fmt.Sprintf("u%02d", i%46), "deepseek-v4.1-flash", usage.Delta{HasTotal: true, TotalTokens: 100}, true)
			}
			p.SetTokenUsageOf(func(u, m string, _ time.Duration) int64 { return r.QuotaTokensToday(u, m) })
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p.PickExcludingForRealm(nil, "deepseek-v4.1-flash", "cn")
			}
		})
	}
}
