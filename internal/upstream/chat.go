// chat.go 构造发往 CodeBuddy 的 OpenAI 兼容请求体。
//
// 上游与 OpenAI 格式基本一致，但需要两处适配：
//  1. 强制 stream=true —— 上游不支持非流式（会返回
//     "Non-stream chat request is currently not supported"），
//     非流式由本服务聚合 SSE 后再返回。
//  2. 清理上游不接受的字段 —— 非 function 类型的 tool、空 parameters、
//     以及 additionalProperties / strict（上游会报 schema 错误）。
package upstream

import (
	"encoding/json"
	"log"
	"strings"
)

// BuildChatPayload 把客户端请求体转成上游请求体。
// clientReq 是客户端原始 JSON（已解析为 map），model 是最终使用的模型名。
func BuildChatPayload(clientReq map[string]any, model string) ([]byte, error) {
	out := make(map[string]any, len(clientReq)+2)
	for k, v := range clientReq {
		out[k] = v
	}

	out["model"] = model
	out["stream"] = true

	// 思考深度下限：协议转换层（CC Switch chat 模式）会把客户端的思考设置
	// 映射成 reasoning_effort——桌面端未开扩展思考时会发 low，实测上游会
	// 直接关闭思考（reasoning_tokens=0）。这里按配置把低档抬到下限。
	if clamped := clampReasoningEffort(strField(out, "reasoning_effort")); clamped != "" {
		out["reasoning_effort"] = clamped
	}

	// CodeBuddy 上游硬性要求：对话首条消息必须是 system 角色，
	// 否则返回 11128 "first message is not system prompt"。
	// 若客户端未提供 system 消息，自动补一条默认 system。
	if msgs, ok := out["messages"].([]any); ok {
		if len(msgs) == 0 {
			out["messages"] = []any{defaultSystemMsg()}
		} else {
			if first, ok2 := msgs[0].(map[string]any); !ok2 || first["role"] != "system" {
				msgs = append([]any{defaultSystemMsg()}, msgs...)
			}
			// 分片 content 归一化：Claude 系 harness 把注入块与真实指令混排，
			// 直接透传会让上游读不到用户指令（见 normalizeMessageContents）。
			out["messages"] = normalizeMessageContents(msgs)
		}
	}

	// stream_options 上游不识别，且本服务自己控制聚合，直接去掉
	delete(out, "stream_options")

	// tools 清洗
	if raw, ok := out["tools"]; ok {
		cleaned, dropped := cleanTools(raw)
		if len(cleaned) == 0 {
			delete(out, "tools")
			delete(out, "tool_choice")
		} else {
			out["tools"] = cleaned
			if dropped > 0 {
				log.Printf("chat: dropped %d incompatible tool definition(s)", dropped)
			}
		}
	}

	// 内容审核脱敏（可关闭）：对敏感词插零宽空格，并压缩 harness 注入的
	// 运行时块。缓解 Claude Code / Codex CLI 的合规模板被后端误判为敏感词
	// 而整条请求被拦（http 400 / code 11128 "unapproved channel"）。
	out = applyDesensitize(out)

	return json.Marshal(out)
}

// defaultSystemMsg 当客户端未提供 system 消息时补的默认系统提示。
func defaultSystemMsg() map[string]any {
	return map[string]any{"role": "system", "content": "You are a helpful assistant."}
}

// normalizeMessageContents 把分片（数组）形式的 content 拍扁成单个字符串，
// 并把用户的真实输入（非 <system-reminder> 注入块）提到最前。
//
// 背景：Claude 系 harness（Claude Code / ZCode / CC Switch 转换后的 chat 请求）
// 会把工具提醒、token 预算、文件读取结果等以 <system-reminder>...</system-reminder>
// 包裹的多段 text 与用户的真实指令塞进同一条 user 消息的 parts 数组。
// 上游 CodeBuddy 是 OpenAI 兼容端点，实测对多段 content 容易只读前面几段、
// 忽略末尾的真实指令——表现为模型回「没看到你的请求」「just the setup context」。
// 这里统一拍扁成单字符串，并把非提醒块的正文前置，保证用户指令一定被读到。
func normalizeMessageContents(msgs []any) []any {
	out := make([]any, 0, len(msgs))
	for _, it := range msgs {
		m, ok := it.(map[string]any)
		if !ok {
			out = append(out, it)
			continue
		}
		parts, ok := m["content"].([]any)
		if !ok {
			out = append(out, it)
			continue
		}
		var plain, injected []string
		for _, p := range parts {
			switch pv := p.(type) {
			case string:
				plain = append(plain, pv)
			case map[string]any:
				txt, _ := pv["text"].(string)
				txt = strings.TrimRight(txt, "\n")
				if txt == "" {
					continue
				}
				if isReminderBlock(txt) {
					injected = append(injected, txt)
				} else {
					plain = append(plain, txt)
				}
			}
		}
		// 真实指令前置，注入块后置；空消息保持空串。
		merged := append(append([]string{}, plain...), injected...)
		m["content"] = strings.Join(merged, "\n\n")
		out = append(out, m)
	}
	return out
}

// isReminderBlock 判断一段文本是否为 harness 注入的 <system-reminder> 块。
func isReminderBlock(s string) bool {
	return strings.HasPrefix(strings.TrimSpace(s), "<system-reminder>")
}

// reasoningEffortRank 给 reasoning_effort 档位排序，数值越大思考越深。
// minimal/off/none 视为关闭思考；未知档位按 -1 处理（视为未提供）。
var reasoningEffortRank = map[string]int{
	"off": 0, "none": 0, "minimal": 0,
	"low": 1, "medium": 2, "moderate": 2, "high": 3, "max": 4,
}

// minReasoningEffort 思考深度下限（空串表示不干预，原样透传客户端档位）。
var minReasoningEffort string

// SetMinReasoningEffort 配置思考深度下限（low/medium/high/max）。
// 客户端（经协议转换层）发来的 reasoning_effort 低于下限或缺省时，抬到下限。
func SetMinReasoningEffort(effort string) {
	minReasoningEffort = strings.ToLower(strings.TrimSpace(effort))
}

// clampReasoningEffort 把客户端档位钳制到不低于下限。
// 下限未配置时原样返回；缺省/未知/低档返回下限；高档保持客户端值。
func clampReasoningEffort(in string) string {
	floor := minReasoningEffort
	if floor == "" {
		return in
	}
	fr, ok := reasoningEffortRank[floor]
	if !ok || fr == 0 {
		return in // 下限非法或为"关闭思考"档，不干预
	}
	cr, ok := reasoningEffortRank[strings.ToLower(strings.TrimSpace(in))]
	if !ok || cr < fr {
		return floor
	}
	return strings.TrimSpace(in)
}

// strField 从 map 取字符串字段（缺失或类型不符返回空串）。
func strField(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// cleanTools 过滤/清洗 tools 数组：
//   - 丢弃非 function 类型（web_search 等上游不支持）
//   - 丢弃 function.parameters 为空或缺 type 的定义
//   - 递归移除 additionalProperties 与 strict 字段
func cleanTools(raw any) ([]any, int) {
	arr, ok := raw.([]any)
	if !ok {
		return nil, 0
	}
	out := make([]any, 0, len(arr))
	dropped := 0
	for _, it := range arr {
		tool, ok := it.(map[string]any)
		if !ok {
			dropped++
			continue
		}
		if t, _ := tool["type"].(string); t != "" && t != "function" {
			dropped++
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			dropped++
			continue
		}
		params, hasParams := fn["parameters"].(map[string]any)
		if hasParams {
			if len(params) == 0 {
				dropped++
				continue
			}
			if _, hasType := params["type"]; !hasType {
				dropped++
				continue
			}
			fn["parameters"] = stripSchemaKeys(params)
		}
		delete(fn, "strict")
		tool["type"] = "function"
		out = append(out, tool)
	}
	return out, dropped
}

// stripSchemaKeys 递归删除上游不支持的 JSON Schema 关键字。
func stripSchemaKeys(node map[string]any) map[string]any {
	delete(node, "additionalProperties")
	delete(node, "strict")
	for _, key := range []string{"properties", "$defs", "definitions"} {
		if sub, ok := node[key].(map[string]any); ok {
			for k, v := range sub {
				if m, ok := v.(map[string]any); ok {
					sub[k] = stripSchemaKeys(m)
				}
			}
		}
	}
	for _, key := range []string{"items", "anyOf", "oneOf", "allOf"} {
		switch sub := node[key].(type) {
		case map[string]any:
			node[key] = stripSchemaKeys(sub)
		case []any:
			for i, v := range sub {
				if m, ok := v.(map[string]any); ok {
					sub[i] = stripSchemaKeys(m)
				}
			}
		}
	}
	return node
}
