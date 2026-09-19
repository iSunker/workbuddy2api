// classify.go 错误分类：驱动 pool 冷却状态机。
package upstream

import (
	"errors"
	"fmt"
	"strings"
)

// ErrKind 错误类别。
type ErrKind int

const (
	ErrNone         ErrKind = iota
	ErrHardCredit           // 余额/配额不足（402 / 关键词）
	ErrSoftRate             // 429 软限流
	ErrTokenExpired         // 401 token 过期 → 触发 refresh 重试
	ErrSessionDead          // 凭证失效 → 禁用（需重新登录）
	ErrNotFound             // 404 → 短冷却，不累计 errCount（防雪崩）
	ErrServer               // 5xx
	ErrClient               // 其他 4xx
	ErrContentRejected      // 内容审核拒绝（11140 request illegal）— 用户侧问题，不冷却账号
	ErrBrokenToolSeq        // 工具调用序列断裂（11148）— 会话历史问题，不冷却账号、换号无意义
)

func (k ErrKind) String() string {
	switch k {
	case ErrHardCredit:
		return "hard_credit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrTokenExpired:
		return "token_expired"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrContentRejected:
		return "content_rejected"
	case ErrBrokenToolSeq:
		return "broken_tool_sequence"
	default:
		return "none"
	}
}

// Error 实现 error 接口，让 ErrKind 可作 errors.Is 的 target。
func (k ErrKind) Error() string { return k.String() }

// Error 带分类的上游错误。
type Error struct {
	Kind   ErrKind
	Status int
	Code   int // 上游业务码（如 11128 模型/通道不被批准、11102 模型不支持），解析不到为 0
	Msg    string
}

func (e *Error) Error() string {
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}

// Is 让 errors.Is(err, ErrKind) 可用。
func (e *Error) Is(target error) bool {
	if k, ok := target.(ErrKind); ok {
		return e.Kind == k
	}
	return false
}

// hardMarkers 余额/配额不足关键词（大小写不敏感）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit", "isquotaexceeded\":true", "insufficient balance",
	"balance is not enough", "exceeded the quota",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
	"配额不足", "配额已用完", "用量已超限",
}

// deadMarkers 凭证本身不可用（不是余额问题）→ 禁用账号。
var deadMarkers = []string{
	"invalid api key", "invalid_api_key", "api key not found", "apikey invalid",
	"invalid_format", "invalid apikey", "apikey not exist", "api key revoked",
	"token expired", "token invalid", "invalid token", "unauthorized",
	"forbidden", "authentication required", "account disabled", "account banned",
	"invalid signature", "signature invalid",
	"apikey无效", "凭证无效", "账号已被禁用", "未授权",
}

// Classify 按 HTTP 状态码 + body 判定错误类别。
func Classify(status int, body string) ErrKind {
	if status == 402 {
		return ErrHardCredit
	}
	lower := strings.ToLower(body)
	// 11148 = 工具调用序列断裂（tool_call_sequence_broken）。
	//
	// 会话历史里存在「孤儿 tool_calls」或「孤儿 tool 结果」（assistant 发起了调用但
	// 没有配对的 tool 消息，或反之），上游强校验直接拒绝整条请求。
	// 这是**请求体自身**的问题，与账号/配额无关：
	//   - 冷却或禁用账号毫无意义（换号也一样 400，因为断链在 body 里）；
	//   - 必须优先于 hardMarkers 判定——错误文案里带 "do not match"，
	//     若被 hardMarkers 抢先会误判成「余额不足」而把账号冷却 12h。
	// 调用方据此返回明确文案，提示用户新建会话。
	if extractCode([]byte(body)) == brokenToolSeqCode || strings.Contains(lower, "tool_call_sequence_broken") {
		return ErrBrokenToolSeq
	}
	for _, m := range hardMarkers {
		if strings.Contains(lower, strings.ToLower(m)) || strings.Contains(body, m) {
			return ErrHardCredit
		}
	}
	if status == 401 {
		for _, m := range deadMarkers {
			if strings.Contains(lower, m) {
				return ErrSessionDead
			}
		}
		return ErrTokenExpired
	}
	if status == 403 {
		// 11140 = 上游内容审核拒绝（"内容未通过安全审核，请调整后重试"）。
		// 此前 403 无差别判 ErrHardCredit，导致：① 客户端收到假 429「余额不足」误导排查；
		// ② 账号被误冷却 12h。内容审核是用户侧问题，与配额无关，必须优先识别。
		if extractCode([]byte(body)) == contentRejectedCode {
			return ErrContentRejected
		}
		for _, m := range deadMarkers {
			if strings.Contains(lower, m) {
				return ErrSessionDead
			}
		}
		return ErrHardCredit
	}
	if status == 429 {
		return ErrSoftRate
	}
	if status == 404 {
		return ErrNotFound
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 {
		for _, m := range deadMarkers {
			if strings.Contains(lower, m) {
				return ErrSessionDead
			}
		}
		return ErrClient
	}
	return ErrNone
}

// IsKind 工具函数。
func IsKind(err error, kind ErrKind) bool {
	var ue *Error
	if errors.As(err, &ue) {
		return ue.Kind == kind
	}
	return false
}

// contentRejectedCode 上游内容审核拒绝的业务码（403 + 11140）。
const contentRejectedCode = 11140

// brokenToolSeqCode 上游「工具调用序列断裂」的业务码。
//
// 实测（2026-09）返回形态为 HTTP 400 + code 11148：
//
//	{"code":11148,"msg":"tool calls and tool results do not match, please start a new conversation and retry",
//	 "extError":{"code":"tool_call_sequence_broken","message":"tool calls and tool results do not match, ..."}}
//
// 注意这是 400（不是 422/409），且属于**用户会话侧**问题——重试、换号、冷却都不解决。
const brokenToolSeqCode = 11148

// modelChannelBlockedCodes 上游用这些业务码表达「该账号/通道不批准此模型或调用」。
// 典型如 11128（Illegal API invocation from an unapproved channel / first message is not system），
// 以及 11102（模型不支持）。这类错误换成另一个 realm/账号重试通常能成功，不应立刻判失败。
var modelChannelBlockedCodes = map[int]bool{
	11128: true,
	11102: true,
}

// transientChannelBlockMarkers 上游「未批准渠道」类安全拦截的关键词。
// 这类 11128（Illegal API invocation from an unapproved channel）通常是网关侧
// 偶发的风控/限流抖动，稍后重试即可恢复，不应当作永久的渠道/模型拒绝，
// 也不应立刻把单账号判定为不可用（否则所有账号都中招时直接 503）。
// 注意与「first message is not system prompt」「model not supported」区分——后者是真错误。
var transientChannelBlockMarkers = []string{
	"unapproved channel",
	"illegal api invocation",
}

// IsTransientChannelBlock 判断上游错误是否为「可重试的瞬时渠道安全拦截」。
// 仅命中 "unapproved channel" / "illegal api invocation" 这类间歇性问题才返回 true；
// 模型不支持、首条消息非 system 等稳定错误返回 false（应走换账号/直接报错分支）。
func IsTransientChannelBlock(e *Error) bool {
	if e == nil {
		return false
	}
	lower := strings.ToLower(e.Msg)
	for _, m := range transientChannelBlockMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// IsModelChannelRejected 判断上游 4xx 是否为「模型/通道不被该账号批准」。
// 命中时调用方应换账号/realm 重试，而不是把 400 直接甩给客户端。
func IsModelChannelRejected(e *Error) bool {
	if e == nil {
		return false
	}
	if modelChannelBlockedCodes[e.Code] {
		return true
	}
	lower := strings.ToLower(e.Msg)
	for _, m := range []string{
		"unapproved channel", "not supported by model", "not approved",
		"illegal api invocation", "model not support", "model is not supported",
	} {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// freeLimitMarkers 免费模型 / 当日限额类措辞。命中表示该账号对这个模型的当日免费额度已用尽：
// 换一个账号重试通常还能继续用，不应把整个账号长时间冷却（会误伤该账号的其它模型）。
var freeLimitMarkers = []string{
	"daily limit", "daily quota", "day limit", "per-day limit", "per day",
	"reach daily", "free quota", "free tier", "free limit", "free model",
	"usage limit reached", "frequency limit", "rate limit for free",
	"已达到当日上限", "达到当日上限", "超过当日上限", "当日额度", "当日用量",
	"每日额度", "每日限额", "今日额度已用完", "今日已用完", "今天已用完",
	"免费额度已用完", "免费额度用完", "免费模型", "免费次数", "次数已达上限",
	"调用过于频繁", "请求过于频繁", "当前时段已限流", "今日限流",
}

// IsDailyFreeLimit 判断上游错误是否为「模型当日免费额度/频率用尽」。
// 命中时调度方应仅对本请求换账号重试，不要对整号做长时间冷却。
func IsDailyFreeLimit(e *Error) bool {
	if e == nil {
		return false
	}
	lower := strings.ToLower(e.Msg)
	for _, m := range freeLimitMarkers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}
