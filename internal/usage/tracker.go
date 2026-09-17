// Package usage 提供本地额度累计器。
// CodeBuddy 上游没有公开剩余额度 API，但每个对话响应会返回 usage.credit，
// 本包据此累计各账号及全局已用额度，并持久化到 JSON 文件。
package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const defaultLimit = 500.0

// maxRecords 消费明细环形缓冲容量：只保留最近这么多次真实请求，
// 再多就丢最旧的（明细是「看最近烧了什么」，不是账本）。
const maxRecords = 200

// Tracker 线程安全的额度累计器。
type Tracker struct {
	mu    sync.RWMutex
	path  string
	data  persisted
}

type persisted struct {
	Total    float64            `json:"total"`
	Limit    float64            `json:"limit"`
	Accounts map[string]float64 `json:"accounts"`
	Updated  time.Time          `json:"updated"`
	Records  []Record           `json:"records,omitempty"`
}

// Record 一次真实请求的消费明细（供看板「积分消费情况」表展示）。
type Record struct {
	At               time.Time `json:"at"`
	UID              string    `json:"uid,omitempty"`
	Model            string    `json:"model,omitempty"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	Credit           float64   `json:"credit"`
	LatencyMS        int64     `json:"latency_ms"`
	Stream           bool      `json:"stream,omitempty"`
}

// Snapshot 只读快照。
type Snapshot struct {
	Total     float64            `json:"total"`
	Limit     float64            `json:"limit"`
	Remaining float64            `json:"remaining"`
	Accounts  map[string]float64 `json:"accounts"`
	Updated   time.Time          `json:"updated"`
	Records   []Record           `json:"records,omitempty"`
}

// New 创建/加载 Tracker。limit 为 0 时使用默认 500。
func New(path string, limit float64) *Tracker {
	if path == "" {
		path = "usage.json"
	}
	if !filepath.IsAbs(path) {
		if abs, err := filepath.Abs(path); err == nil {
			path = abs
		}
	}
	t := &Tracker{path: path}
	if limit <= 0 {
		limit = defaultLimit
	}
	t.data = persisted{Limit: limit, Accounts: map[string]float64{}}
	t.load()
	if t.data.Limit <= 0 {
		t.data.Limit = limit
	}
	if t.data.Accounts == nil {
		t.data.Accounts = map[string]float64{}
	}
	return t
}

// Add 增加一次消耗。uid 为空时只累计全局。
func (t *Tracker) Add(uid string, credit float64) {
	if credit <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data.Total += credit
	if uid != "" {
		t.data.Accounts[uid] += credit
	}
	t.data.Updated = time.Now()
	t.save()
}

// Record 追加一条消费明细（不累计金额——金额仍由 Add 负责，
// 两条路径分开，避免改明细时动到账本口径）。
func (t *Tracker) Record(rec Record) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if rec.At.IsZero() {
		rec.At = time.Now()
	}
	t.data.Records = append(t.data.Records, rec)
	if len(t.data.Records) > maxRecords {
		// 环形裁剪：保留最近 maxRecords 条
		t.data.Records = append([]Record(nil), t.data.Records[len(t.data.Records)-maxRecords:]...)
	}
	t.save()
}

// Records 返回消费明细副本，最新在前。
func (t *Tracker) Records() []Record {
	t.mu.RLock()
	defer t.mu.RUnlock()
	n := len(t.data.Records)
	out := make([]Record, 0, n)
	for i := n - 1; i >= 0; i-- { // 倒序：最新在前
		out = append(out, t.data.Records[i])
	}
	return out
}

// Snapshot 返回当前快照。
func (t *Tracker) Snapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	accs := make(map[string]float64, len(t.data.Accounts))
	for k, v := range t.data.Accounts {
		accs[k] = v
	}
	// 明细倒序内联构造，不复用 Records()——那会二次取 RLock，
	// 存在写者等待时读锁重入的死锁风险（Go RWMutex 不可递归读）。
	n := len(t.data.Records)
	recs := make([]Record, 0, n)
	for i := n - 1; i >= 0; i-- {
		recs = append(recs, t.data.Records[i])
	}
	return Snapshot{
		Total:     t.data.Total,
		Limit:     t.data.Limit,
		Remaining: t.data.Limit - t.data.Total,
		Accounts:  accs,
		Updated:   t.data.Updated,
		Records:   recs,
	}
}

// SetLimit 重新设置额度上限。
func (t *Tracker) SetLimit(limit float64) {
	if limit <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data.Limit = limit
	t.data.Updated = time.Now()
	t.save()
}

// ExtractCredit 从上游 usage 对象里提取 credit（额度消耗）。
func ExtractCredit(usage map[string]any) float64 {
	if usage == nil {
		return 0
	}
	switch v := usage["credit"].(type) {
	case float64:
		return v
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return f
		}
	}
	return 0
}

// TokenUsage 从上游 usage 对象里提取 token 统计。
// 上游同一响应里可能同时给 prompt_tokens/total_tokens 与
// cache_* / completion_thinking_tokens 等扩展字段；这里只取三项通用值，
// total 缺失时用 prompt+completion 兜底。
func TokenUsage(usage map[string]any) (prompt, completion, total int) {
	if usage == nil {
		return 0, 0, 0
	}
	prompt = intField(usage, "prompt_tokens")
	completion = intField(usage, "completion_tokens")
	total = intField(usage, "total_tokens")
	if total == 0 && (prompt > 0 || completion > 0) {
		total = prompt + completion
	}
	return
}

// intField 宽松读取整数字段（上游可能给 float64 / json.Number / 字符串）。
func intField(m map[string]any, key string) int {
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n)
		}
	case int:
		return v
	case int64:
		return int(v)
	}
	return 0
}

func (t *Tracker) load() {
	b, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &t.data)
}

func (t *Tracker) save() {
	_ = os.MkdirAll(filepath.Dir(t.path), 0o755)
	b, _ := json.MarshalIndent(t.data, "", "  ")
	_ = os.WriteFile(t.path, b, 0o644)
}
