package usage

import (
	"encoding/json"
	"path/filepath"
	"testing"
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
