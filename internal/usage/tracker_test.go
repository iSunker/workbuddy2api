package usage

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// TestRecordRingBufferAndOrder 锁定消费明细的两个契约：
//  1. 只保留最近 maxRecords 条（超出丢最旧）
//  2. Records() 最新在前；Snapshot() 内嵌明细同样最新在前
func TestRecordRingBufferAndOrder(t *testing.T) {
	tr := New(filepath.Join(t.TempDir(), "usage.json"), 500)
	for i := 0; i < maxRecords+20; i++ {
		tr.Record(Record{Model: "m", Credit: 0.01, TotalTokens: 10})
	}
	recs := tr.Records()
	if len(recs) != maxRecords {
		t.Fatalf("len(records) = %d, want %d", len(recs), maxRecords)
	}
	// 倒序：最新在前 —— 用时间戳单调性间接验证（同一批 Record 时间递增）
	for i := 1; i < len(recs); i++ {
		if recs[i].At.After(recs[i-1].At) {
			t.Fatalf("records not newest-first at %d: %s > %s", i, recs[i].At, recs[i-1].At)
		}
	}
	// Snapshot 内嵌明细不能为空、且不因二次取锁而死锁（会被 go test 超时捕获）
	if got := len(tr.Snapshot().Records); got != maxRecords {
		t.Fatalf("snapshot records = %d, want %d", got, maxRecords)
	}
}

// TestRecordPersists 明细要落盘（容器重建后看板仍有历史）。
func TestRecordPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	tr := New(path, 500)
	tr.Record(Record{Model: "hy4-preview", PromptTokens: 10, CompletionTokens: 20, TotalTokens: 30, Credit: 0.5, LatencyMS: 1234})

	again := New(path, 500)
	recs := again.Records()
	if len(recs) != 1 {
		t.Fatalf("reloaded records = %d, want 1", len(recs))
	}
	r := recs[0]
	if r.Model != "hy4-preview" || r.TotalTokens != 30 || r.Credit != 0.5 || r.LatencyMS != 1234 {
		t.Fatalf("record round-trip mismatch: %+v", r)
	}
}

// TestTokenUsage 宽松解析三种数值形态，并在缺 total 时兜底相加。
func TestTokenUsage(t *testing.T) {
	cases := []struct {
		name      string
		in        map[string]any
		p, c, tot int
	}{
		{"float64", map[string]any{"prompt_tokens": 10.0, "completion_tokens": 20.0, "total_tokens": 30.0}, 10, 20, 30},
		{"json.Number", map[string]any{"prompt_tokens": json.Number("11"), "completion_tokens": json.Number("22")}, 11, 22, 33},
		{"missing total", map[string]any{"prompt_tokens": 5.0, "completion_tokens": 7.0}, 5, 7, 12},
		{"nil", nil, 0, 0, 0},
	}
	for _, tc := range cases {
		p, c, tot := TokenUsage(tc.in)
		if p != tc.p || c != tc.c || tot != tc.tot {
			t.Errorf("%s: got (%d,%d,%d), want (%d,%d,%d)", tc.name, p, c, tot, tc.p, tc.c, tc.tot)
		}
	}
}

// TestExtractCreditUnchanged 金额口径不受明细改动影响。
func TestExtractCreditUnchanged(t *testing.T) {
	if got := ExtractCredit(map[string]any{"credit": 0.33}); got != 0.33 {
		t.Fatalf("credit = %v, want 0.33", got)
	}
	if got := ExtractCredit(map[string]any{"credit": json.Number("1.25")}); got != 1.25 {
		t.Fatalf("credit(json.Number) = %v, want 1.25", got)
	}
	if got := ExtractCredit(nil); got != 0 {
		t.Fatalf("credit(nil) = %v, want 0", got)
	}
}

// TestDailyToday 锁定「今日使用额度」契约：Add 累计时同步按天账；
// Today 取本地日期当天，且不受环形缓冲条数限制（比明细求和更全）。
func TestDailyToday(t *testing.T) {
	tr := New(filepath.Join(t.TempDir(), "usage.json"), 500)
	tr.Add("u1", 0.5)
	tr.Add("u2", 1.5)
	snap := tr.Snapshot()
	if snap.Today != 2.0 {
		t.Fatalf("today = %v, want 2.0", snap.Today)
	}
	if snap.TodayDate != time.Now().Format("2006-01-02") {
		t.Fatalf("today_date = %q", snap.TodayDate)
	}
	// 只加明细（Record）不加金额（Add）不应计入今日
	tr.Record(Record{Model: "m", Credit: 99})
	if got := tr.Snapshot().Today; got != 2.0 {
		t.Fatalf("today after Record-only = %v, want 2.0（金额口径仍以 Add 为准）", got)
	}
}

// TestDailyPersists 按天账本持久化：容器重建后今日值不丢。
func TestDailyPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	tr := New(path, 500)
	tr.Add("u1", 3.25)
	again := New(path, 500)
	if got := again.Snapshot().Today; got != 3.25 {
		t.Fatalf("today after reload = %v, want 3.25", got)
	}
}

// TestDailyBackfillFromRecords 升级回填：老文件只有明细没有 daily 时，
// 用明细里的 credit 回填当天（尽力而为，之后由 Add 精确维护）。
func TestDailyBackfillFromRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	tr := New(path, 500)
	tr.Record(Record{Model: "m", Credit: 0.75}) // 时间取 now
	// 模拟旧版本写入的文件：清掉 daily 再重新加载
	tr.mu.Lock()
	tr.data.Daily = nil
	tr.save()
	tr.mu.Unlock()
	again := New(path, 500)
	if got := again.Snapshot().Today; got != 0.75 {
		t.Fatalf("backfilled today = %v, want 0.75", got)
	}
}

// TestTodayByAccount 锁定「点击今日已用 → 展开各账号当日消耗」契约：
// 按账号拆分、与 today 总额一致、持久化后不丢。
func TestTodayByAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	tr := New(path, 500)
	tr.Add("u1", 0.5)
	tr.Add("u2", 1.5)
	tr.Add("u1", 0.25)
	snap := tr.Snapshot()
	by := snap.TodayByAccount
	if len(by) != 2 || by["u1"] != 0.75 || by["u2"] != 1.5 {
		t.Fatalf("today_by_account = %v, want u1=0.75 u2=1.5", by)
	}
	var sum float64
	for _, v := range by {
		sum += v
	}
	if sum != snap.Today {
		t.Fatalf("sum(by_account) = %v != today = %v", sum, snap.Today)
	}
	// 持久化：重新加载后仍在
	again := New(path, 500)
	if got := again.Snapshot().TodayByAccount["u1"]; got != 0.75 {
		t.Fatalf("reloaded today_by_account[u1] = %v, want 0.75", got)
	}
}
