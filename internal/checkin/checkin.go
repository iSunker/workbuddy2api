// Package checkin —— CodeBuddy 每日签到（官方接口）。
// 仅 OAuth 登录型（KindToken）账号可签；ck_ 静态 API Key 会被官方拒绝（403），标记 Skipped。
//   POST https://www.workbuddy.cn/billing/meter/daily-checkin
//   Authorization: Bearer <登录 token>
//   code==0 → 签到成功（data.credit / data.streak_days）
//   code==10001 → 今天已签到（幂等）
// 服务器与调度器共用本包，避免两套签到逻辑。
package checkin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"codebuddy2api/internal/cred"
)

// Endpoint 官方每日签到接口。
const Endpoint = "https://www.workbuddy.cn/billing/meter/daily-checkin"

// Item 单账号签到结果。
type Item struct {
	UID        string `json:"uid"`
	Nickname   string `json:"nickname,omitempty"`
	Realm      string `json:"realm,omitempty"`
	Kind       string `json:"kind,omitempty"`
	OK         bool   `json:"ok"`
	Code       int    `json:"code"`
	Credit     int64  `json:"credit,omitempty"`
	StreakDays int    `json:"streak_days,omitempty"`
	Skipped    bool   `json:"skipped,omitempty"`
	Msg        string `json:"msg,omitempty"`
	// AuthDead 表示请求被上游以 401/403 拒绝（token 失效）。调度器据此自动禁用账号；不外发 JSON。
	AuthDead bool `json:"-"`
}

// All 对 authDir 下所有 OAuth 登录账号各签一次并聚合。
// skipDisabled 为非 nil 时，被禁用的账号（按 uid）直接跳过（调度器用）。
func All(authDir string, skipDisabled map[string]bool) ([]Item, error) {
	creds, err := cred.LoadDir(authDir)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(creds))
	for _, c := range creds {
		if c == nil {
			continue
		}
		if c.Kind != cred.KindToken || c.Token == "" {
			items = append(items, Item{
				UID: c.UID, Kind: string(c.Kind), Realm: c.Realm,
				Skipped: true, Msg: "仅 OAuth 登录账号可签到（ck_ 静态 API Key 不支持）",
			})
			continue
		}
		if skipDisabled != nil && skipDisabled[c.UID] {
			continue
		}
		it := doOne(c.Token)
		it.UID = c.UID
		it.Nickname = c.Nickname
		it.Realm = c.Realm
		it.Kind = string(c.Kind)
		items = append(items, it)
	}
	return items, nil
}

// One 对单个 OAuth 账号签到。authDir 下找不到或非登录型时 ok=false。
func One(authDir, uid string) (Item, bool) {
	creds, err := cred.LoadDir(authDir)
	if err != nil {
		return Item{}, false
	}
	for _, c := range creds {
		if c == nil || c.UID != uid || c.Kind != cred.KindToken || c.Token == "" {
			continue
		}
		it := doOne(c.Token)
		it.UID = c.UID
		it.Nickname = c.Nickname
		it.Realm = c.Realm
		it.Kind = string(c.Kind)
		return it, true
	}
	return Item{}, false
}

// ---------- 当天签到状态（持久化到 <authDir>/../data/checkin_state.json） ----------

// AccountState 单个账号某天的签到记录。
type AccountState struct {
	UID        string `json:"uid"`
	Signed     bool   `json:"signed"`
	Credit     int64  `json:"credit,omitempty"`
	StreakDays int    `json:"streak_days,omitempty"`
}

// State 每日签到记录文件内容。
type State struct {
	Date     string                  `json:"date"` // YYYY-MM-DD（本地）
	Accounts map[string]AccountState `json:"accounts"`
}

var stateMu sync.Mutex

func statePath(authDir string) string {
	return filepath.Join(filepath.Dir(authDir), "data", "checkin_state.json")
}

func loadState(p string) State {
	raw, err := os.ReadFile(p)
	if err != nil {
		return State{}
	}
	var st State
	_ = json.Unmarshal(raw, &st)
	if st.Accounts == nil {
		st.Accounts = map[string]AccountState{}
	}
	return st
}

func saveState(p string, st State) {
	raw, _ := json.MarshalIndent(st, "", "  ")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, raw, 0o644)
}

// Record 记录某账号今天已签到（或确认今天已由别处签到）。
func Record(authDir, uid string, signed bool, credit int64, streak int) {
	if uid == "" {
		return
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	today := time.Now().Format("2006-01-02")
	p := statePath(authDir)
	st := loadState(p)
	if st.Date != today {
		st.Date = today
		st.Accounts = map[string]AccountState{}
	}
	acct := st.Accounts[uid]
	if signed {
		acct.Signed = true
		if credit > 0 || !acct.Signed {
			acct.Credit = credit
			acct.StreakDays = streak
		}
	}
	acct.UID = uid
	st.Accounts[uid] = acct
	saveState(p, st)
}

// Today 返回当天日期与该日各账号签到记录。
// 每日 00:00（本地时区）重置：记录日期不是今天时视为“新的一天”，返回空记录，
// 避免把昨天遗留的“已签”误显示成今天已签（等到当天签到后再写入新记录）。
func Today(authDir string) (string, map[string]AccountState) {
	p := statePath(authDir)
	st := loadState(p)
	today := time.Now().Format("2006-01-02")
	if st.Date != today {
		return today, map[string]AccountState{}
	}
	return st.Date, st.Accounts
}

func doOne(token string) Item {
	req, err := http.NewRequest(http.MethodPost, Endpoint, bytes.NewReader([]byte("{}")))
	if err != nil {
		return Item{Msg: "构造请求失败: " + err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "CodeBuddy/1.0.8 CLI")

	cl := &http.Client{Timeout: 25 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return Item{Msg: "请求失败: " + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Credit     int64 `json:"credit"`
			StreakDays int   `json:"streak_days"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Item{Msg: "解析响应失败(HTTP " + resp.Status + "): " + string(body)}
	}
	out := Item{Code: payload.Code, Credit: payload.Data.Credit, StreakDays: payload.Data.StreakDays}
	switch {
	case resp.StatusCode == http.StatusOK && payload.Code == 0:
		out.OK = true
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		out.AuthDead = true
		out.Msg = "凭证被拒绝(HTTP " + resp.Status + ")，需重新登录"
	default:
		if payload.Msg != "" {
			out.Msg = payload.Msg
		} else {
			out.Msg = "HTTP " + resp.Status
		}
	}
	return out
}
