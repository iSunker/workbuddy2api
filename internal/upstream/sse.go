// sse.go 处理 CodeBuddy 的标准 OpenAI SSE：
// 每行 data:{...}，data:[DONE] 表示结束。
package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"time"
)

// ParseSSE 逐行解析标准 OpenAI SSE，每个有效 chunk 调 onChunk。
//   - "data: [DONE]" → 正常结束
//   - 注释行（: 开头）、event: 行、空行 → 忽略
//   - 解析失败的行 → 跳过（不致命）
func ParseSSE(r io.Reader, onChunk func(map[string]any) error) error {
	br := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" {
				// 空 data 行，忽略
			} else if payload == "[DONE]" {
				return nil
			} else if payload[0] == '{' {
				var chunk map[string]any
				if json.Unmarshal([]byte(payload), &chunk) == nil {
					if err := onChunk(chunk); err != nil {
						return err
					}
				}
			}
		}
		if err == io.EOF {
			return nil
		}
	}
}

// Aggregate 聚合完整 SSE 为单个 OpenAI chat.completion 响应。
// model 用于覆盖响应中的 model 字段（上游可能回自己的内部名，客户端要看到请求时的名字）。
// tool_calls 以流式 delta 形式到达，按 index 合并：首片带 id/type/name，后续只带 arguments 片段。
func Aggregate(r io.Reader, model string) (map[string]any, error) {
	var (
		id           string
		created      float64
		content      strings.Builder
		reasoning    strings.Builder
		role         = "assistant"
		finishReason = "stop"
		usage        map[string]any
		toolCalls    = map[int]map[string]any{}
		toolOrder    []int
	)
	err := ParseSSE(r, func(chunk map[string]any) error {
		if v, ok := chunk["id"].(string); ok && id == "" {
			id = v
		}
		if v, ok := chunk["created"].(float64); ok && created == 0 {
			created = v
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			// 上游最后一个 chunk 才给 usage（通常全 0）；保留最后一次非空结果
			usage = mergeUsage(usage, u)
		}
		ch, _ := chunk["choices"].([]any)
		for _, ci := range ch {
			c, _ := ci.(map[string]any)
			if c == nil {
				continue
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			if delta, ok := c["delta"].(map[string]any); ok {
				mergeDelta(delta, &role, &content, &reasoning, toolCalls, &toolOrder)
			}
			// 少数实现把内容放在 message 而非 delta
			if msg, ok := c["message"].(map[string]any); ok {
				mergeDelta(msg, &role, &content, &reasoning, toolCalls, &toolOrder)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if id == "" {
		id = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	message := map[string]any{
		"role":    role,
		"content": content.String(),
	}
	if reasoning.Len() > 0 {
		message["reasoning_content"] = reasoning.String()
	}
	if len(toolOrder) > 0 {
		sort.Ints(toolOrder)
		calls := make([]map[string]any, 0, len(toolOrder))
		for _, idx := range toolOrder {
			call := toolCalls[idx]
			if id, ok := call["id"].(string); ok && id != "" {
				call["id"] = renameToolCallID(id)
			}
			calls = append(calls, call)
		}
		message["tool_calls"] = calls
	}
	if content.Len() == 0 && reasoning.Len() == 0 && len(toolOrder) == 0 {
		message["content"] = ""
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{
			map[string]any{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}
	if usage != nil {
		resp["usage"] = usage
	}
	return resp, nil
}

// hideReasoningInStream 流式响应是否剥离 reasoning_content（DeepSeek 系私有扩展，
// 非标准 OpenAI 字段）。默认隐藏：响应保持 100% 标准 OpenAI 流；纯 OpenAI 客户端
// （zcode 等）想看思考内容可关闭——关闭后按帧透传，不引入任何缓冲。
var hideReasoningInStream = true

// SetHideReasoningStream 配置流式响应是否剥离 reasoning_content。
func SetHideReasoningStream(hide bool) { hideReasoningInStream = hide }

// StreamAsOpenAI 把上游 SSE 转写为标准 OpenAI SSE 给客户端。
// 每个 chunk 重写 model 字段为客户端请求的模型名；末尾补 data: [DONE]。
// 全程逐帧规范化透传（与 workbuddy2api 的 Stream 同构，TTFB 与上游一致），
// 唯一分歧是 reasoning_content：hide=true 剥除，hide=false 按帧透传。
func StreamAsOpenAI(w io.Writer, r io.Reader, model string, flush func(), onUsage ...func(map[string]any)) error {
	return streamOpenAIFrames(w, r, model, flush, onUsage)
}

// streamOpenAIFrames 逐帧读取上游 SSE，按 OpenAI 流式规范白名单重建后立即下发。
// 与 workbuddy2api 的 upstream.Stream 同构：
//   - 逐帧转发 + flush，TTFB 与上游一致（不整段缓冲）；
//   - finish_reason:"" → null，缺失 → null；
//   - 空 delta 键一律省略，空 content/refusal、空 tool_calls 列表、空占位
//     function_call 剥除，顶层未知字段剔除；
//   - usage 缺失 → null，object 缺失 → "chat.completion.chunk"；
//   - tool_call id 规范化为 toolu_ 前缀；reasoning_content 剥除（本模式即隐藏模式）。
//
// 上游显式 [DONE] 后停止读取；末尾保证恰好一个 [DONE]。零有效帧时返回错误供
// 调用方记录（[DONE] 已补发，客户端可正常收尾）。
func streamOpenAIFrames(w io.Writer, r io.Reader, model string, flush func(), onUsage []func(map[string]any)) error {
	br := bufio.NewReaderSize(r, 256*1024)
	validFrames := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				// 上游显式结束：统一在循环结束后补发，保证恰好一个 [DONE]。
				break
			}
			if payload != "" {
				var obj map[string]any
				if json.Unmarshal([]byte(payload), &obj) == nil {
					validFrames++
					if u, ok := obj["usage"].(map[string]any); ok && u != nil && len(onUsage) > 0 {
						onUsage[0](u)
					}
					frame := normalizeOpenAIFrame(obj, model)
					raw, _ := json.Marshal(frame)
					if _, werr := fmt.Fprintf(w, "data: %s\n\n", raw); werr != nil {
						return werr
					}
					if flush != nil {
						flush()
					}
				}
			}
		}
		if err == io.EOF {
			break
		}
	}
	if _, werr := io.WriteString(w, "data: [DONE]\n\n"); werr != nil {
		return werr
	}
	if flush != nil {
		flush()
	}
	log.Printf("chat stream done (frame-passthrough): model=%s frames=%d", model, validFrames)
	if validFrames == 0 {
		return fmt.Errorf("upstream stream contained no valid data events")
	}
	return nil
}

// normalizeOpenAIFrame 以 OpenAI 流式规范白名单重建一帧（与 workbuddy2api 的
// normalizeFrame 同构）：仅保留标准字段，剔除上游噪声——finish_reason:"" → null、
// 空 content/refusal、空 tool_calls 列表、空占位 function_call、顶层未知字段；
// 空 delta 键一律省略；usage 缺失 → null；object 缺失 → "chat.completion.chunk"；
// model 覆盖为客户端请求名；tool_call id 规范化为 toolu_ 前缀。
func normalizeOpenAIFrame(obj map[string]any, model string) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"id", "object", "created", "system_fingerprint", "service_tier"} {
		if v, ok := obj[k]; ok && v != nil {
			out[k] = v
		}
	}
	out["object"] = "chat.completion.chunk"
	out["model"] = model
	if chs, ok := obj["choices"].([]any); ok {
		nchs := make([]any, 0, len(chs))
		for _, ci := range chs {
			c, ok := ci.(map[string]any)
			if !ok {
				continue
			}
			nc := map[string]any{}
			if idx, ok := c["index"]; ok {
				nc["index"] = idx
			} else {
				nc["index"] = 0
			}
			delta := map[string]any{}
			if d, ok := c["delta"].(map[string]any); ok {
				friendlyToolCallIDsInDelta(d)
				if v, ok := d["role"].(string); ok && v != "" {
					delta["role"] = v
				}
				if v, ok := d["content"].(string); ok && v != "" {
					delta["content"] = v
				}
				if !hideReasoningInStream {
					if v, ok := d["reasoning_content"].(string); ok && v != "" {
						delta["reasoning_content"] = v
					}
				}
				if v, ok := d["refusal"].(string); ok && v != "" {
					delta["refusal"] = v
				}
				if tcs, ok := d["tool_calls"].([]any); ok && len(tcs) > 0 {
					delta["tool_calls"] = tcs
				}
				if fc, ok := d["function_call"]; ok && fc != nil {
					// 空占位 function_call（name/arguments 全空）视为噪声剔除
					keep := false
					if fcm, ok2 := fc.(map[string]any); ok2 {
						n, _ := fcm["name"].(string)
						a, _ := fcm["arguments"].(string)
						keep = n != "" || a != ""
					} else {
						keep = true
					}
					if keep {
						delta["function_call"] = fc
					}
				}
			}
			nc["delta"] = delta
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				nc["finish_reason"] = fr
			} else {
				nc["finish_reason"] = nil
			}
			nchs = append(nchs, nc)
		}
		out["choices"] = nchs
	}
	if u, ok := obj["usage"]; ok {
		out["usage"] = u
	} else {
		out["usage"] = nil
	}
	return out
}

// renameToolCallID 规范化工具调用 id 为 toolu_ 前缀。
//
// 下游若存在 OpenAI→Anthropic 协议转换层（CC Switch 本地路由等），带 call_
// 前缀的 id 会被 Claude 客户端判为无法解析（"tool call could not be parsed"），
// 进而在工具执行完成后清空整轮回复。统一为 toolu_ 前缀对纯 OpenAI 客户端
// 无副作用（id 仅作回传关联，上游不校验格式）。
func renameToolCallID(id string) string {
	id = strings.TrimPrefix(strings.TrimSpace(id), "call_")
	if id == "" {
		id = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if !strings.HasPrefix(id, "toolu_") {
		id = "toolu_" + id
	}
	return id
}

// friendlyToolCallIDsInDelta 原地规范化一个 delta 内 tool_calls 的非空 id。
func friendlyToolCallIDsInDelta(delta map[string]any) {
	tcs, ok := delta["tool_calls"].([]any)
	if !ok {
		return
	}
	for _, tc := range tcs {
		call, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		if id, ok := call["id"].(string); ok && id != "" {
			call["id"] = renameToolCallID(id)
		}
	}
}

// mergeDelta 把一个 delta/message 合并进累计状态。
func mergeDelta(delta map[string]any, role *string, content, reasoning *strings.Builder, toolCalls map[int]map[string]any, toolOrder *[]int) {
	if r, ok := delta["role"].(string); ok && r != "" {
		*role = r
	}
	if txt, ok := delta["content"].(string); ok && txt != "" {
		content.WriteString(txt)
	} else if v, ok := delta["content"].(map[string]any); ok {
		// 少数上游把 content 包成 {"type":"text","text":"..."}
		if txt, ok := v["text"].(string); ok {
			content.WriteString(txt)
		}
	}
	if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
		reasoning.WriteString(rc)
	}
	tcs, _ := delta["tool_calls"].([]any)
	for _, tc := range tcs {
		call, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		idx := 0
		if v, ok := call["index"].(float64); ok {
			idx = int(v)
		}
		merged, seen := toolCalls[idx]
		if !seen {
			merged = map[string]any{"index": idx}
			toolCalls[idx] = merged
			*toolOrder = append(*toolOrder, idx)
		}
		mergeToolCallDelta(merged, call)
	}
}

// mergeToolCallDelta 把流式 tool_call 片段合并到累计对象：
// id/type/function.name 直覆盖（后续分片通常缺省），function.arguments 拼接。
func mergeToolCallDelta(merged, delta map[string]any) {
	if v, ok := delta["id"].(string); ok && v != "" {
		merged["id"] = v
	}
	if v, ok := delta["type"].(string); ok && v != "" {
		merged["type"] = v
	}
	df, _ := delta["function"].(map[string]any)
	if df == nil {
		return
	}
	mf, _ := merged["function"].(map[string]any)
	if mf == nil {
		mf = map[string]any{}
		merged["function"] = mf
	}
	if v, ok := df["name"].(string); ok && v != "" {
		mf["name"] = v
	}
	if v, ok := df["arguments"].(string); ok && v != "" {
		if prev, _ := mf["arguments"].(string); prev != "" {
			mf["arguments"] = prev + v
		} else {
			mf["arguments"] = v
		}
	}
}

// mergeUsage 取两次 usage 中较"完整"的一份（非全 0 优先）。
func mergeUsage(prev, cur map[string]any) map[string]any {
	if prev == nil {
		return cur
	}
	if usageSum(cur) > usageSum(prev) {
		return cur
	}
	return prev
}

// IsEmptyToolResponse 报告 resp 是否为「偶发空工具响应」：
// finish_reason=tool_calls，但 content / reasoning_content / tool_calls 全为空。
//
// 上游偶发返回这种响应（尤其带 tools 的请求）。若原样下发：
//   - OpenAI 客户端看到空消息；
//   - Anthropic 客户端（Claude Code）在 stop_reason=tool_use 且无 tool_use 块时
//     报 "The model's tool call could not be parsed"。
//
// 调用方应据此换号重试。
func IsEmptyToolResponse(resp map[string]any) bool {
	ch, _ := resp["choices"].([]any)
	if len(ch) == 0 {
		return false // 结构异常交给上层错误处理
	}
	c, _ := ch[0].(map[string]any)
	if c == nil {
		return false
	}
	if fr, _ := c["finish_reason"].(string); fr != "tool_calls" {
		return false
	}
	m, _ := c["message"].(map[string]any)
	if m == nil {
		return true
	}
	if txt, _ := m["content"].(string); strings.TrimSpace(txt) != "" {
		return false
	}
	if rc, _ := m["reasoning_content"].(string); rc != "" {
		return false
	}
	if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
		return false
	}
	return true
}

// numVal 把 JSON 数字转成 int。
// json.Unmarshal 解出的是 float64，但代码里手工构造的 map 可能是 int/int64，
// 这里统一兼容，避免 usage 读取时因类型断言失败而静默变成 0。
func numVal(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case float32:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		if f, err := n.Float64(); err == nil {
			return int(f)
		}
	}
	return 0
}

func usageSum(u map[string]any) int64 {
	var total int64
	if v, ok := u["total_tokens"].(float64); ok {
		total += int64(v)
	}
	if v, ok := u["prompt_tokens"].(float64); ok {
		total += int64(v)
	}
	if v, ok := u["completion_tokens"].(float64); ok {
		total += int64(v)
	}
	return total
}
