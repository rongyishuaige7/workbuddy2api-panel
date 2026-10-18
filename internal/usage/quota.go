package usage

import "time"

// 配额周期与面板本地日历分开：与上游已知重置文案的 UTC+8 一致。
var quotaLocation = time.FixedZone("UTC+8", 8*60*60)

type quotaKey struct{ uid, model string }

func quotaDay(t time.Time) string { return t.In(quotaLocation).Format(dayLayout) }

// QuotaTokensToday 是 O(1) 的调度查询，不遍历永久保留的历史桶。
func (r *Recorder) QuotaTokensToday(uid, model string) int64 {
	if r == nil || uid == "" || model == "" {
		return 0
	}
	return r.quotaTokensAt(time.Now(), uid, model)
}

func (r *Recorder) quotaTokensAt(now time.Time, uid, model string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.quotaDays[quotaDay(now)][quotaKey{uid, model}]
}

// Add 与加载时更新；只保留前一日、当日、后一日，查询跨日无需全表重建。
func (r *Recorder) addQuotaLocked(at time.Time, uid, model string, tokens int64) {
	today := time.Now().In(quotaLocation)
	first, last := quotaDay(today.AddDate(0, 0, -1)), quotaDay(today.AddDate(0, 0, 1))
	for d := range r.quotaDays {
		if d < first || d > last {
			delete(r.quotaDays, d)
		}
	}
	day := quotaDay(at)
	if day < first || day > last {
		return
	}
	if r.quotaDays[day] == nil {
		r.quotaDays[day] = make(map[quotaKey]int64)
	}
	r.quotaDays[day][quotaKey{uid, model}] += tokens
}

// 旧桶没有时区，沿用 bucketTime 的本地时区解释；跨配额午夜的桶保守计入两天。
// 只在加载时遍历历史，运行中的 Add 按请求实际时间准确累计，不增改磁盘 schema。
func (r *Recorder) rebuildQuotaLocked() {
	r.quotaDays = make(map[string]map[quotaKey]int64)
	for _, b := range r.buckets {
		start, ok := bucketTime(b.Scope)
		if !ok {
			continue
		}
		end := start.Add(time.Hour)
		if len(b.Scope) > 0 && b.Scope[0] == 'd' {
			end = start.AddDate(0, 0, 1)
		}
		r.addQuotaLocked(start, b.UID, b.Model, b.TT)
		last := end.Add(-time.Nanosecond)
		if quotaDay(last) != quotaDay(start) {
			r.addQuotaLocked(last, b.UID, b.Model, b.TT)
		}
	}
}
