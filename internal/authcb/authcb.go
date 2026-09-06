// Package authcb 实现 CodeBuddy 的 OAuth 设备授权登录流程。
//
// 流程与官方 CLI 一致（参考社区 codebuddy2api 的 codebuddy_auth_router.py）：
//
//	1) POST https://www.codebuddy.ai/v2/plugin/auth/state?platform=CLI&nonce=...
//	   → 返回 authUrl（用户在浏览器打开完成登录）与 state（轮询凭据）
//	2) GET  https://www.codebuddy.ai/v2/plugin/auth/token?state=...
//	   → code==11217 表示尚未完成（pending）；code==0 且 data.accessToken 存在即成功
//
// 该 token 属于 SaaS 域（www.codebuddy.ai），与 CN 控制台域（copilot.tencent.com）不同，
// 使用时需以对应 realm 的上游 base / 请求头访问。
package authcb

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Provider 一个授权域的配置（国际版 SaaS 或 中国版 CN）。
type Provider struct {
	Base    string // 授权与 token 接口 base，如 https://www.codebuddy.ai
	Host    string // 请求 Host / X-Domain，如 www.codebuddy.ai
	Product string // X-Product，如 SaaS
}

// SaaSProvider 国际版（www.codebuddy.ai）。
var SaaSProvider = Provider{
	Base:    "https://www.codebuddy.ai",
	Host:    "www.codebuddy.ai",
	Product: "SaaS",
}

// CNProvider 中国版（copilot.tencent.com）。
var CNProvider = Provider{
	Base:    "https://copilot.tencent.com",
	Host:    "copilot.tencent.com",
	Product: "SaaS",
}

// ProviderByRealm 按 realm 选择授权域；未知回落到国际版。
func ProviderByRealm(realm string) Provider {
	switch realm {
	case "cn", "cnc", "china", "copilot":
		return CNProvider
	default:
		return SaaSProvider
	}
}

const userAgent = "CLI/1.0.8 CodeBuddy/1.0.8"

// StartResult /api/auth/start 的返回。
type StartResult struct {
	AuthURL string `json:"auth_url"`
	State   string `json:"state"`
}

// TokenResult 授权成功后的 token 详情。
// 注意：上游 www.codebuddy.ai/v2/plugin/auth/token 返回的是 camelCase 字段
// （data.accessToken / data.refreshToken / data.expiresIn ...），务必与之一致。
type TokenResult struct {
	AccessToken  string `json:"accessToken"`
	TokenType    string `json:"tokenType"`
	ExpiresIn    int64  `json:"expiresIn"`
	RefreshToken string `json:"refreshToken"`
	SessionState string `json:"sessionState"`
	Scope        string `json:"scope"`
	Domain       string `json:"domain"`
}

// PollResult /api/auth/poll 的返回：Pending 表示用户尚未完成授权。
type PollResult struct {
	Pending bool
	Token   *TokenResult
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	const hx = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hx[v>>4]
		out[i*2+1] = hx[v&0x0f]
	}
	return string(out)
}

func authHeaders(p Provider) map[string]string {
	return map[string]string{
		"Host":               p.Host,
		"X-Domain":           p.Host,
		"User-Agent":         userAgent,
		"X-Product":          p.Product,
		"X-No-Authorization": "true",
		"X-No-User-Id":       "true",
		"Accept":             "application/json",
	}
}

func b3Headers() map[string]string {
	t := randHex(16)
	s := randHex(16)
	return map[string]string{
		"X-B3-TraceId": t,
		"X-B3-SpanId":  s,
		"X-B3-Sampled": "1",
	}
}

// StartLogin 向 CodeBuddy 申请一个设备授权会话，返回登录页 URL 与轮询 state。
func StartLogin(p Provider) (*StartResult, error) {
	nonce := randHex(8)
	u := p.Base + "/v2/plugin/auth/state?platform=CLI&nonce=" + nonce
	body, _ := json.Marshal(map[string]string{"nonce": nonce})

	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range authHeaders(p) {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("auth/state http %d: %s", resp.StatusCode, trunc(string(raw), 200))
	}

	var out struct {
		Code int `json:"code"`
		Data struct {
			AuthURL string `json:"authUrl"`
			State   string `json:"state"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("auth/state parse: %w", err)
	}
	if out.Data.AuthURL == "" || out.Data.State == "" {
		return nil, fmt.Errorf("auth/state: empty authUrl/state (%s)", trunc(string(raw), 200))
	}
	return &StartResult{AuthURL: out.Data.AuthURL, State: out.Data.State}, nil
}

// Poll 用 state 轮询授权结果。Pending=true 表示用户还未完成登录。
func Poll(p Provider, state string) (*PollResult, error) {
	if state == "" {
		return nil, fmt.Errorf("empty state")
	}
	u := p.Base + "/v2/plugin/auth/token?state=" + url.QueryEscape(state)

	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range authHeaders(p) {
		req.Header.Set(k, v)
	}
	for k, v := range b3Headers() {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("auth/token http %d: %s", resp.StatusCode, trunc(string(raw), 200))
	}

	var out struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("auth/token parse: %w", err)
	}
	if out.Code == 11217 {
		return &PollResult{Pending: true}, nil
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("auth/token code %d: %s", out.Code, trunc(string(raw), 200))
	}

	var tr TokenResult
	if err := json.Unmarshal(out.Data, &tr); err != nil {
		return nil, fmt.Errorf("token parse: %w", err)
	}
	if tr.AccessToken == "" {
		// 某些版本嵌套在 data.token
		var alt struct {
			Token string `json:"token"`
		}
		_ = json.Unmarshal(out.Data, &alt)
		tr.AccessToken = alt.Token
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("auth/token: empty accessToken (%s)", trunc(string(raw), 200))
	}
	if tr.TokenType == "" {
		tr.TokenType = "Bearer"
	}
	return &PollResult{Token: &tr}, nil
}

// UserIDFromJWT 从 JWT 的 payload 中提取用户标识（email / preferred_username / sub）。
func UserIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload := parts[1]
	if l := len(payload) % 4; l != 0 {
		payload += strings.Repeat("=", 4-l)
	}
	dec, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		if dec, err = base64.RawURLEncoding.DecodeString(parts[1]); err != nil {
			return ""
		}
	}
	var claims map[string]any
	if err := json.Unmarshal(dec, &claims); err != nil {
		return ""
	}
	for _, k := range []string{"email", "preferred_username", "sub"} {
		if v, ok := claims[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
