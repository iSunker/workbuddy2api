// client.go CodeBuddy 官方 API 客户端。
//
// 上游是 OpenAI 兼容的：POST {base}/v2/chat/completions，Bearer/X-Api-Key 认证，
// 响应为标准 OpenAI SSE。与 QoderWork 相比没有 COSY 签名、没有 body 编码，
// 唯一要注意的是：上游只接受 stream=true，非流式由本服务聚合后返回。
package upstream

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// 端点路径。
const (
	// ChatPath 对话端点（拼在 Base 后）。
	ChatPath = "/v2/chat/completions"
	// ModelsPathV2 OpenAI 风格模型列表。
	ModelsPathV2 = "/v2/models"
	// ConfigPathV3 CodeBuddy 配置端点，匿名访问时 data.models 为空，带凭证才返回模型。
	ConfigPathV3 = "/v3/config"
)

// Client 上游业务 API 客户端。
type Client struct {
	HTTP *http.Client
	Base string // https://copilot.tencent.com
}

// New 默认客户端（180s 超时）。
func New() *Client { return NewWithTimeout(180 * time.Second) }

// NewWithTimeout 指定上游 HTTP 超时（config.upstream.timeout_seconds 注入）。
// 强制 HTTP/1.1：CodeBuddy 网关对 HTTP/2 长流不友好（易 INTERNAL_ERROR）。
func NewWithTimeout(timeout time.Duration) *Client {
	tr := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	return &Client{
		HTTP: &http.Client{Timeout: timeout, Transport: tr},
		Base: "https://copilot.tencent.com",
	}
}

// NewWithBase 测试用：覆盖 base。
func NewWithBase(base string) *Client {
	c := New()
	if base != "" {
		c.Base = trimSlash(base)
	}
	return c
}

// ChatResult 上游流式响应的句柄。
type ChatResult struct {
	Body io.ReadCloser
}

// doReq 带认证头发起请求；hdr 为 realm 额外的请求头（如 SaaS 的 X-Domain 等）。
func (c *Client) doReq(ctx context.Context, method, fullURL, token string, body []byte, hdr map[string]string) (*http.Response, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, rdr)
	if err != nil {
		return nil, err
	}
	applyAuth(req, token, body)
	for k, v := range hdr {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	return c.HTTP.Do(req)
}

// rawPOST 带认证头发 JSON POST，返回原始响应（不关 body，由调用方决定）。
func (c *Client) rawPOST(ctx context.Context, path, token string, body []byte) (*http.Response, error) {
	return c.doReq(ctx, http.MethodPost, c.Base+path, token, body, nil)
}

// rawGET 带认证头发 GET。
func (c *Client) rawGET(ctx context.Context, path, token string) (*http.Response, error) {
	return c.doReq(ctx, http.MethodGet, c.Base+path, token, nil, nil)
}

// QuotaInfo 上游账户/额度信息（来自 billing/meter 接口）。
type QuotaInfo struct {
	PaymentType string         // trial / free / pro / ultimate / standard
	Upgrade     map[string]any // get-free-user-upgrade 返回的 data（含额度/升级信息，可能为空）
	Raw         map[string]any // get-payment-type 返回的 data
	Account     map[string]any // /v2/accounts 返回的账户资料（nickname/uin/type 等，可能为空）
	Resource    *ResourceInfo  // get-user-resource 返回的真实额度明细（可能为空）
}

// ResourceAccount 单个资源包的额度明细（来自 get-user-resource）。
// 注意：上游时间字段（CycleStartTime/CycleEndTime/DeductionEndTime）有时是字符串、
// 有时是数字，故用 any 容错。
type ResourceAccount struct {
	PackageName         string  `json:"PackageName"`
	CapacityUnit        string  `json:"CapacityUnit"`
	CapacitySize        float64 `json:"CapacitySize"`
	CapacityUsed        float64 `json:"CapacityUsed"`
	CapacityRemain      float64 `json:"CapacityRemain"`
	CycleCapacitySize   float64 `json:"CycleCapacitySize"`
	CycleCapacityUsed   float64 `json:"CycleCapacityUsed"`
	CycleCapacityRemain float64 `json:"CycleCapacityRemain"`
	CycleStartTime      any     `json:"CycleStartTime"`
	CycleEndTime        any     `json:"CycleEndTime"`
	DeductionEndTime    any     `json:"DeductionEndTime"`
	Status              float64 `json:"Status"`
}

// ResourceInfo get-user-resource 的真实额度（按资源包聚合）。
type ResourceInfo struct {
	TotalCount  float64           `json:"TotalCount"`
	TotalDosage float64           `json:"TotalDosage"`
	Accounts    []ResourceAccount `json:"Accounts"`
}

// postJSONMap 带认证头发空体 POST 并解析为 map（用于 billing/meter 等 JSON 接口）。
func (c *Client) postJSONMap(ctx context.Context, fullURL, token string, hdr map[string]string) (map[string]any, error) {
	resp, err := c.doReq(ctx, http.MethodPost, fullURL, token, []byte("{}"), hdr)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := readBody(resp)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	raw, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return m, nil
}

// getJSONMap 带认证头发 GET 并解析为 map（用于 /v2/accounts 等 JSON 接口）。
func (c *Client) getJSONMap(ctx context.Context, fullURL, token string) (map[string]any, error) {
	resp, err := c.doReq(ctx, http.MethodGet, fullURL, token, nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := readBody(resp)
		return nil, fmt.Errorf("upstream %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	raw, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return m, nil
}

// FetchQuota 拉取上游账户的套餐与额度信息。
// 依次查询 get-payment-type（套餐类型，SaaS 域可用）与 get-free-user-upgrade
// （免费用户额度/升级信息，部分域不存在则忽略）。两者都没有时返回错误。
func (c *Client) FetchQuota(ctx context.Context, base, token string, hdr map[string]string) (*QuotaInfo, error) {
	info := &QuotaInfo{}
	if m, err := c.postJSONMap(ctx, trimSlash(base)+"/v2/billing/meter/get-payment-type", token, hdr); err == nil {
		if code, _ := m["code"].(float64); int(code) == 0 {
			if d, ok := m["data"].(map[string]any); ok {
				if v, _ := d["paymentType"].(string); v != "" {
					info.PaymentType = v
				}
				info.Raw = d
			}
		}
	}
	if m, err := c.postJSONMap(ctx, trimSlash(base)+"/v2/billing/meter/get-free-user-upgrade", token, hdr); err == nil {
		if code, _ := m["code"].(float64); int(code) == 0 {
			if d, ok := m["data"].(map[string]any); ok && len(d) > 0 {
				info.Upgrade = d
			}
		}
	}
	// 账户资料（SaaS 域用 Bearer JWT 可用；失败则忽略）
	if m, err := c.getJSONMap(ctx, trimSlash(base)+"/v2/accounts", token); err == nil {
		if code, _ := m["code"].(float64); int(code) == 0 {
			if d, ok := m["data"].(map[string]any); ok {
				if accts, ok := d["accounts"].([]any); ok && len(accts) > 0 {
					if a, ok := accts[0].(map[string]any); ok {
						info.Account = a
					}
				}
			}
		}
	}
	// 真实额度明细（get-user-resource），失败则忽略（部分域/账号无此接口）
	if res, err := c.FetchResource(ctx, base, token, hdr); err == nil && res != nil {
		info.Resource = res
	}
	if info.PaymentType == "" && info.Upgrade == nil && info.Account == nil && info.Resource == nil {
		return info, fmt.Errorf("upstream returned no quota info (account may not expose billing)")
	}
	return info, nil
}

// FetchResource 拉取上游真实额度明细（POST /v2/billing/meter/get-user-resource）。
// 返回每个资源包的容量（总量/已用/剩余）与当前周期用量，是权威余额来源。
func (c *Client) FetchResource(ctx context.Context, base, token string, hdr map[string]string) (*ResourceInfo, error) {
	m, err := c.postJSONMap(ctx, trimSlash(base)+"/v2/billing/meter/get-user-resource", token, hdr)
	if err != nil {
		return nil, err
	}
	if code, _ := m["code"].(float64); int(code) != 0 {
		return nil, fmt.Errorf("upstream code %d: %v", int(code), m["msg"])
	}
	data, ok := m["data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("no data field")
	}
	resp, ok := data["Response"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("no Response field")
	}
	inner, ok := resp["Data"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("no Data field")
	}
	b, _ := json.Marshal(inner)
	var info ResourceInfo
	if err := json.Unmarshal(b, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// applyAuth 设置认证头：Bearer 必带；X-Api-Key 仅 ck_ 静态 Key 才双写
// （OAuth 得到的 JWT 不应带 X-Api-Key，否则 SaaS 域可能拒绝）。
func applyAuth(req *http.Request, token string, body []byte) {
	req.Header.Set("Authorization", "Bearer "+token)
	if strings.HasPrefix(token, "ck_") {
		req.Header.Set("X-Api-Key", token)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	// 上游 /v3/config 等端点会校验 User-Agent 中的 “Coding Copilot” 版本号，
	// 缺失或无法解析会返回 12403 "check ua, get coding copilot version error"。
	// 因此必须带真实客户端形态的 UA（与已安装 CodeBuddy CN 1.106.1 对齐）。
	req.Header.Set("User-Agent", "Coding Copilot/1.106.1 CodeBuddy/1.106.1")
	_ = body
}

// readBody 限长读取并关闭。
func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// extractCode 从上游错误 JSON 体里取业务码 code（如 11128）。解析不到返回 0。
func extractCode(raw []byte) int {
	var probe struct {
		Code int `json:"code"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil {
		return probe.Code
	}
	return 0
}

// ChatForward 把 OpenAI 请求体转发到上游（默认 base + ChatPath）。
// 上游只支持流式，因此这里恒以 stream=true 发送；非流式由调用方聚合。
// 成功返回 SSE body 流（调用方负责 Close）；失败返回带分类的 *Error。
func (c *Client) ChatForward(ctx context.Context, token string, payload []byte) (io.ReadCloser, error) {
	return c.ChatForwardRealm(ctx, c.Base, ChatPath, token, payload, nil)
}

// ChatForwardRealm 指定 base / chat 路径 / 额外头转发（按账号 realm 选择上游）。
func (c *Client) ChatForwardRealm(ctx context.Context, base, chatPath, token string, payload []byte, hdr map[string]string) (io.ReadCloser, error) {
	resp, err := c.doReq(ctx, http.MethodPost, base+chatPath, token, payload, hdr)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := readBody(resp)
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("chat_forward: upstream %d %s body=%s", resp.StatusCode, kind, truncate(string(raw), 200))
		// 调试：把本次真实发出的请求（URL/头/body）落盘，便于排查 11128 等持续性错误。
		dumpUpstreamRequest("POST", base+chatPath, token, payload, hdr, resp.StatusCode, raw)
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Code: extractCode(raw), Msg: truncate(string(raw), 300)}
	}
	return resp.Body, nil
}

// dumpUpstreamRequest 把一次上游请求的诊断信息写到 bin/last_upstream_req.txt（调试用，不影响主流程）。
func dumpUpstreamRequest(method, fullURL, token string, payload []byte, hdr map[string]string, status int, respBody []byte) {
	defer func() { _ = recover() }()
	masked := token
	if len(masked) > 12 {
		masked = masked[:6] + "***" + masked[len(masked)-4:]
	}
	hdrLines := []string{
		"Authorization: Bearer " + masked,
	}
	for k, v := range hdr {
		if v == "" {
			continue
		}
		if k == "X-Api-Key" {
			v = "***"
		}
		hdrLines = append(hdrLines, k+": "+v)
	}
	// 从 applyAuth 复制出的固定头（与 doReq 一致）
	hdrLines = append(hdrLines, "Accept: text/event-stream", "Content-Type: application/json", "Accept-Encoding: identity", "User-Agent: Coding Copilot/1.106.1 CodeBuddy/1.106.1")

	out := strings.Builder{}
	out.WriteString("=== upstream request dump ===\n")
	out.WriteString(fmt.Sprintf("time : %s\n", time.Now().Format(time.RFC3339)))
	out.WriteString(fmt.Sprintf("method: %s\n", method))
	out.WriteString(fmt.Sprintf("url   : %s\n", fullURL))
	out.WriteString(fmt.Sprintf("status: %d\n", status))
	out.WriteString("--- request headers ---\n")
	out.WriteString(strings.Join(hdrLines, "\n"))
	out.WriteString("\n--- request body (sent to upstream) ---\n")
	pb := payload
	if len(pb) > 8192 {
		pb = pb[:8192]
	}
	out.WriteString(string(pb))
	out.WriteString("\n--- upstream response body ---\n")
	rb := respBody
	if len(rb) > 1024 {
		rb = rb[:1024]
	}
	out.WriteString(string(rb))
	out.WriteString("\n=== end dump ===\n")
	// 写到工作目录下的 bin/，失败忽略
	if werr := writeFileSafe("bin/last_upstream_req.txt", []byte(out.String())); werr != nil {
		log.Printf("dumpUpstreamRequest: write failed: %v", werr)
	}
}

func writeFileSafe(path string, data []byte) error {
	// 若 bin/ 不存在则尝试创建
	if i := strings.LastIndex(path, "/"); i > 0 {
		_ = os.MkdirAll(path[:i], 0o755)
	}
	return os.WriteFile(path, data, 0o644)
}

// Probe 用最小请求探测凭证是否有效（用于保活与健康检查）。
// 只建立连接并读少量字节，不消费完整生成过程。
func (c *Client) Probe(ctx context.Context, token string) error {
	payload, err := json.Marshal(probePayload())
	if err != nil {
		return err
	}
	rc, err := c.ChatForward(ctx, token, payload)
	if err != nil {
		return err
	}
	defer rc.Close()
	// 读一点验证流确实通了即可
	buf := make([]byte, 512)
	if _, err := rc.Read(buf); err != nil && err != io.EOF {
		return fmt.Errorf("probe read: %w", err)
	}
	return nil
}

// probePayload 保活/重测探针的最小请求体。
//
// 必须自带 system 首消息：上游硬性要求首条为 system 角色，否则回
// 11128 "first message is not system prompt"（聊天主路径由 BuildChatPayload
// 自动补齐，探针绕过了它，此处必须显式给）。缺了会让「模型列表拉不到」的
// 账号（如 saas 域）在保活/重测里一律显示连接失败，且失败原因具有误导性。
func probePayload() map[string]any {
	return map[string]any{
		"model": "auto",
		"messages": []map[string]string{
			{"role": "system", "content": "You are a helpful assistant."},
			{"role": "user", "content": "hi"},
		},
		"stream":     true,
		"max_tokens": 1,
	}
}

// ProbeRealm 按 base / chat 路径 / 额外头探测凭证有效性（用于保活与健康检查）。
func (c *Client) ProbeRealm(ctx context.Context, base, chatPath, token string, hdr map[string]string) error {
	payload, err := json.Marshal(probePayload())
	if err != nil {
		return err
	}
	rc, err := c.ChatForwardRealm(ctx, base, chatPath, token, payload, hdr)
	if err != nil {
		return err
	}
	defer rc.Close()
	buf := make([]byte, 512)
	if _, err := rc.Read(buf); err != nil && err != io.EOF {
		return fmt.Errorf("probe read: %w", err)
	}
	return nil
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
