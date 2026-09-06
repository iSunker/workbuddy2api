// responses.go OpenAI Responses 协议（Codex CLI 走 /v1/responses）与上游
// OpenAI 兼容 chat 协议之间的双向转换。
//
// 方向：
//   - 请求：Responses {instructions, input(字符串或 item 数组), tools(扁平 function)}
//     → chat {messages, tools[type=function]}
//   - 响应：上游 SSE / 聚合结果 → Responses SSE 事件 / response 对象
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// ────────────────────────── 请求：Responses → chat ──────────────────────────

// ResponsesToChat 把 /v1/responses 的请求体转成发给上游的 chat 请求体。
func ResponsesToChat(req map[string]any, model string) (map[string]any, error) {
	out := map[string]any{
		"model":  model,
		"stream": true, // 上游只支持流式
	}

	if v, ok := req["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}

	msgs := make([]any, 0, 8)

	// instructions 相当于 system prompt（Codex CLI 会塞很长的运行时提示）
	if inst := responsesText(req["instructions"]); inst != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": inst})
	}

	// input 可能是纯字符串，也可能是 message / function_call / function_call_output 数组
	switch in := req["input"].(type) {
	case string:
		if in != "" {
			msgs = append(msgs, map[string]any{"role": "user", "content": in})
		}
	case []any:
		for _, raw := range in {
			item, ok := raw.(map[string]any)
			if !ok {
				if s, ok2 := raw.(string); ok2 && s != "" {
					msgs = append(msgs, map[string]any{"role": "user", "content": s})
				}
				continue
			}
			msgs = append(msgs, responsesItemToChat(item)...)
		}
	}
	out["messages"] = msgs

	// tools：Responses 的 function 是扁平的（name/parameters 在顶层）
	if raw, ok := req["tools"]; ok {
		if arr, ok2 := raw.([]any); ok2 {
			if tools := responsesToolsToChat(arr); len(tools) > 0 {
				out["tools"] = tools
			}
		}
	}
	if v, ok := req["tool_choice"]; ok {
		out["tool_choice"] = v
	}

	return out, nil
}

// responsesText 归一化 Responses 的文本字段：
// 字符串，或 [{type:"input_text"|"output_text"|"text", text:"..."}]。
func responsesText(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		var parts []string
		for _, it := range t {
			switch e := it.(type) {
			case string:
				parts = append(parts, e)
			case map[string]any:
				if s, ok := e["text"].(string); ok {
					parts = append(parts, s)
				}
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// responsesItemToChat 把一个 Responses input item 转成 0..n 条 chat 消息。
func responsesItemToChat(item map[string]any) []any {
	switch item["type"] {
	case "function_call":
		id, _ := item["call_id"].(string)
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		if args == "" {
			args = "{}"
		}
		return []any{map[string]any{
			"role":    "assistant",
			"content": "",
			"tool_calls": []any{map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": args,
				},
			}},
		}}
	case "function_call_output":
		id, _ := item["call_id"].(string)
		return []any{map[string]any{
			"role":         "tool",
			"tool_call_id": id,
			"content":     responsesText(item["output"]),
		}}
	}

	role, _ := item["role"].(string)
	switch role {
	case "system", "developer":
		role = "system"
	case "assistant":
	case "user":
	default:
		role = "user"
	}

	text := responsesText(item["content"])
	if text == "" && role != "assistant" {
		return nil
	}
	return []any{map[string]any{"role": role, "content": text}}
}

// responsesToolsToChat 把 Responses 的扁平 function 工具转成 chat 的
// {type:"function", function:{...}} 结构；已经是 chat 结构的原样保留。
func responsesToolsToChat(arr []any) []any {
	out := make([]any, 0, len(arr))
	for _, it := range arr {
		t, _ := it.(map[string]any)
		if t == nil {
			continue
		}
		if _, ok := t["function"].(map[string]any); ok {
			out = append(out, t)
			continue
		}
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		params, _ := t["parameters"].(map[string]any)
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": t["description"],
				"parameters":  params,
			},
		})
	}
	return out
}

// ────────────────────────── 响应：chat → Responses ──────────────────────────

func newResponseID() string { return fmt.Sprintf("resp_%d", time.Now().UnixNano()) }

// responsesUsage 把 OpenAI usage 转成 Responses 的 usage（带 total_tokens）。
func responsesUsage(u map[string]any) map[string]any {
	in, out := numVal(u["prompt_tokens"]), numVal(u["completion_tokens"])
	return map[string]any{
		"input_tokens":  in,
		"output_tokens": out,
		"total_tokens":  in + out,
	}
}

// responsesEnvelope 构造 Responses 响应外壳。
func responsesEnvelope(id string, createdAt int64, model, status string, output []any, usage map[string]any, outputText string) map[string]any {
	if output == nil {
		output = []any{}
	}
	if usage == nil {
		usage = responsesUsage(nil)
	}
	return map[string]any{
		"id":                   id,
		"object":               "response",
		"created_at":           createdAt,
		"status":               status,
		"model":                model,
		"output":               output,
		"output_text":          outputText,
		"usage":                usage,
		"error":                nil,
		"incomplete_details":   nil,
		"instructions":         nil,
		"metadata":             map[string]any{},
		"parallel_tool_calls":  true,
		"tool_choice":          "auto",
		"tools":                []any{},
	}
}

// writeResponsesEvent 写一个 Responses SSE 事件。
func writeResponsesEvent(w io.Writer, event string, data map[string]any, flush func()) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, raw); err != nil {
		return err
	}
	if flush != nil {
		flush()
	}
	return nil
}

// StreamAsResponses 把上游 chat SSE 转写成 Responses SSE 事件流。
// 事件序列：response.created → response.in_progress → [reasoning item] →
// output_item.added → content_part.added → output_text.delta* → output_text.done →
// content_part.done → output_item.done → response.completed
func StreamAsResponses(w io.Writer, r io.Reader, model string, flush func(), onUsage ...func(map[string]any)) error {
	respID := newResponseID()
	msgID := newMessageID()
	reasonID := "rs_" + respID[5:]
	createdAt := time.Now().Unix()

	emit := func(event string, data map[string]any) {
		_ = writeResponsesEvent(w, event, data, flush)
	}

	emit("response.created", map[string]any{
		"type":     "response.created",
		"response": responsesEnvelope(respID, createdAt, model, "in_progress", []any{}, nil, ""),
	})
	emit("response.in_progress", map[string]any{
		"type":     "response.in_progress",
		"response": responsesEnvelope(respID, createdAt, model, "in_progress", []any{}, nil, ""),
	})

	var (
		finalUsage       map[string]any
		pendingReasoning strings.Builder
		outputIdx        = 0
		messageAdded     bool
		fullText         strings.Builder
	)

	// 推理先于正文到达，攒满后在正文之前作为独立的 reasoning output item 发出
	flushReasoning := func() {
		if pendingReasoning.Len() == 0 {
			return
		}
		reasonItem := map[string]any{
			"type":    "reasoning",
			"id":      reasonID,
			"summary": []any{map[string]any{"type": "summary_text", "text": pendingReasoning.String()}},
			"status":  "completed",
		}
		emit("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": outputIdx,
			"item":        reasonItem,
		})
		emit("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": outputIdx,
			"item":        reasonItem,
		})
		pendingReasoning.Reset()
		outputIdx++
	}

	err := ParseSSE(r, func(chunk map[string]any) error {
		if u, ok := chunk["usage"].(map[string]any); ok {
			finalUsage = mergeUsage(finalUsage, u)
		}
		ch, _ := chunk["choices"].([]any)
		if len(ch) == 0 {
			return nil
		}
		c, _ := ch[0].(map[string]any)
		if c == nil {
			return nil
		}
		delta, _ := c["delta"].(map[string]any)
		if delta == nil {
			return nil
		}

		if rc, ok := delta["reasoning_content"].(string); ok && rc != "" {
			pendingReasoning.WriteString(rc)
			return nil
		}

		txt, ok := delta["content"].(string)
		if !ok || txt == "" {
			return nil
		}

		// 正文开始：先收尾 reasoning 项，再挂上 message 项
		if !messageAdded {
			flushReasoning()
			messageAdded = true
			emit("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": outputIdx,
				"item": map[string]any{
					"type":    "message",
					"id":      msgID,
					"role":    "assistant",
					"status":  "in_progress",
					"content": []any{},
				},
			})
			emit("response.content_part.added", map[string]any{
				"type":          "response.content_part.added",
				"item_id":       msgID,
				"output_index":  outputIdx,
				"content_index": 0,
				"part":          map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
			})
		}

		fullText.WriteString(txt)
		emit("response.output_text.delta", map[string]any{
			"type":          "response.output_text.delta",
			"item_id":       msgID,
			"output_index":  outputIdx,
			"content_index": 0,
			"delta":         txt,
		})
		return nil
	})
	if err != nil {
		return err
	}

	// 只有推理没有正文的边缘情况：把推理作为唯一 output 项发出
	if !messageAdded {
		flushReasoning()
	}

	if messageAdded {
		emit("response.output_text.done", map[string]any{
			"type":          "response.output_text.done",
			"item_id":       msgID,
			"output_index":  outputIdx,
			"content_index": 0,
			"text":         fullText.String(),
		})
		emit("response.content_part.done", map[string]any{
			"type":          "response.content_part.done",
			"item_id":       msgID,
			"output_index":  outputIdx,
			"content_index": 0,
			"part":          map[string]any{"type": "output_text", "text": fullText.String(), "annotations": []any{}},
		})
		emit("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": outputIdx,
			"item": map[string]any{
				"type":    "message",
				"id":      msgID,
				"role":    "assistant",
				"status":  "completed",
				"content": []any{map[string]any{"type": "output_text", "text": fullText.String(), "annotations": []any{}}},
			},
		})
	}

	emit("response.completed", map[string]any{
		"type":     "response.completed",
		"response": responsesEnvelope(respID, createdAt, model, "completed", AggregateAsResponsesOutput(fullText.String(), pendingReasoning.String()), responsesUsage(finalUsage), fullText.String()),
	})

	if len(onUsage) > 0 && finalUsage != nil {
		onUsage[0](finalUsage)
	}
	return nil
}

// AggregateAsResponsesOutput 构造 response.completed 里的 output 数组。
func AggregateAsResponsesOutput(text, reasoning string) []any {
	out := make([]any, 0, 2)
	if reasoning != "" {
		out = append(out, map[string]any{
			"type":    "reasoning",
			"id":      "rs_" + fmt.Sprint(time.Now().UnixNano()),
			"summary": []any{map[string]any{"type": "summary_text", "text": reasoning}},
			"status":  "completed",
		})
	}
	out = append(out, map[string]any{
		"type":   "message",
		"id":     newMessageID(),
		"role":   "assistant",
		"status": "completed",
		"content": []any{
			map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
		},
	})
	return out
}

// AggregateAsResponses 把聚合后的 chat 响应转成 Responses 响应对象（非流式）。
func AggregateAsResponses(resp map[string]any, model string) map[string]any {
	text, reasoning := "", ""
	var toolCalls []any

	ch, _ := resp["choices"].([]any)
	if len(ch) > 0 {
		if c, ok := ch[0].(map[string]any); ok {
			if m, ok := c["message"].(map[string]any); ok {
				text, _ = m["content"].(string)
				reasoning, _ = m["reasoning_content"].(string)
				toolCalls, _ = m["tool_calls"].([]any)
			}
		}
	}

	output := AggregateAsResponsesOutput(text, reasoning)

	// 工具调用：Responses 里是 function_call 类型的 output 项
	for _, it := range toolCalls {
		tc, _ := it.(map[string]any)
		if tc == nil {
			continue
		}
		fn, _ := tc["function"].(map[string]any)
		name, args := "", "{}"
		if fn != nil {
			name, _ = fn["name"].(string)
			if a, ok := fn["arguments"].(string); ok && a != "" {
				args = a
			}
		}
		output = append(output, map[string]any{
			"type":      "function_call",
			"id":        tc["id"],
			"call_id":   tc["id"],
			"name":      name,
			"arguments": args,
			"status":    "completed",
		})
	}

	var usage map[string]any
	if u, ok := resp["usage"].(map[string]any); ok {
		usage = responsesUsage(u)
	}

	return responsesEnvelope(newResponseID(), time.Now().Unix(), model, "completed", output, usage, text)
}
