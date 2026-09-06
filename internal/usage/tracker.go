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
}

// Snapshot 只读快照。
type Snapshot struct {
	Total     float64            `json:"total"`
	Limit     float64            `json:"limit"`
	Remaining float64            `json:"remaining"`
	Accounts  map[string]float64 `json:"accounts"`
	Updated   time.Time          `json:"updated"`
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

// Snapshot 返回当前快照。
func (t *Tracker) Snapshot() Snapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	accs := make(map[string]float64, len(t.data.Accounts))
	for k, v := range t.data.Accounts {
		accs[k] = v
	}
	return Snapshot{
		Total:     t.data.Total,
		Limit:     t.data.Limit,
		Remaining: t.data.Limit - t.data.Total,
		Accounts:  accs,
		Updated:   t.data.Updated,
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
