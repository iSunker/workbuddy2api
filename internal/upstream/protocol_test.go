package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// parseEvents 把 SSE 输出拆成 (event 名, data JSON) 序列，便于断言事件顺序。
func parseEvents(t *testing.T, out string) [][2]string {
	t.Helper()
	var events [][2]string
	for _, block := range strings.Split(out, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var event, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				event = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if event != "" {
			events = append(events, [2]string{event, data})
		}
	}
	return events
}

// eventNames 提取事件名序列。
func eventNames(events [][2]string) []string {
	names := make([]string, 0, len(events))
	for _, e := range events {
		names = append(names, e[0])
	}
	return names
}

func decode(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("unmarshal %q: %v", raw, err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Anthropic：请求转换
// ---------------------------------------------------------------------------

func TestAnthropicToChat(t *testing.T) {
	req := map[string]any{
		"model": "claude-sonnet-4-20250514",
		"system": []any{
			map[string]any{"type": "text", "text": "你是助手"},
		},
		"max_tokens": float64(1024),
		"messages": []any{
			map[string]any{"role": "user", "content": "你好"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "text", "text": "我来查一下"},
				map[string]any{"type": "tool_use", "id": "tu_1", "name": "Read", "input": map[string]any{"path": "/a.go"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "tu_1", "content": "文件内容"},
			}},
		},
		"tools": []any{
			map[string]any{
				"name":        "Read",
				"description": "读文件",
				"input_schema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
				},
			},
		},
	}

	out, err := AnthropicToChat(req, "hy4-preview")
	if err != nil {
		t.Fatalf("AnthropicToChat: %v", err)
	}

	if out["model"] != "hy4-preview" {
		t.Fatalf("model = %v", out["model"])
	}
	if out["stream"] != true {
		t.Fatalf("stream must be forced true, got %v", out["stream"])
	}

	msgs, _ := out["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages (system + user + assistant(tool_use) + tool), got %d: %#v", len(msgs), msgs)
	}

	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "你是助手" {
		t.Fatalf("system message = %#v", first)
	}

	// assistant 消息应带 tool_calls
	assistant, _ := msgs[2].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("msg[2] role = %v", assistant["role"])
	}
	tcs, _ := assistant["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("assistant tool_calls = %#v", assistant["tool_calls"])
	}
	tc, _ := tcs[0].(map[string]any)
	if tc["id"] != "tu_1" || tc["type"] != "function" {
		t.Fatalf("tool_call = %#v", tc)
	}
	fn, _ := tc["function"].(map[string]any)
	if fn["name"] != "Read" {
		t.Fatalf("tool name = %v", fn["name"])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &args); err != nil {
		t.Fatalf("tool arguments not valid json: %v (%v)", err, fn["arguments"])
	}
	if args["path"] != "/a.go" {
		t.Fatalf("tool arguments = %v", args)
	}

	// tool_result → role=tool
	toolMsg, _ := msgs[3].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "tu_1" {
		t.Fatalf("tool message = %#v", toolMsg)
	}
	if toolMsg["content"] != "文件内容" {
		t.Fatalf("tool content = %v", toolMsg["content"])
	}

	// tools → OpenAI function 结构
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", out["tools"])
	}
	tl, _ := tools[0].(map[string]any)
	if tl["type"] != "function" {
		t.Fatalf("tool type = %v", tl["type"])
	}
	tfn, _ := tl["function"].(map[string]any)
	if tfn["name"] != "Read" {
		t.Fatalf("tool name = %v", tfn["name"])
	}
	if _, ok := tfn["parameters"].(map[string]any); !ok {
		t.Fatalf("input_schema must map to parameters, got %v", tfn["parameters"])
	}
}

func TestAnthropicToChatPlainStringSystem(t *testing.T) {
	req := map[string]any{
		"model":    "claude-x",
		"system":   "直接用字符串",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out, err := AnthropicToChat(req, "auto")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if msgs[0].(map[string]any)["content"] != "直接用字符串" {
		t.Fatalf("system = %v", msgs[0])
	}
}

// ---------------------------------------------------------------------------
// Anthropic：流式事件序列
// ---------------------------------------------------------------------------

func TestStreamAsAnthropic(t *testing.T) {
	sse := "" +
		`data: {"id":"c1","choices":[{"delta":{"reasoning_content":"想"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"delta":{"reasoning_content":"一想"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"delta":{"content":"你好"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"delta":{"content":"世界"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	var buf bytes.Buffer
	if err := StreamAsAnthropic(&buf, strings.NewReader(sse), "deepseek-r1", nil, true, nil); err != nil {
		t.Fatalf("StreamAsAnthropic: %v", err)
	}

	events := parseEvents(t, buf.String())
	names := eventNames(events)
	want := []string{
		"message_start",
		"content_block_start", // thinking
		"content_block_delta", // thinking_delta（合并为一条）
		"content_block_stop",
		"content_block_start", // text
		"content_block_delta", // "你好"
		"content_block_delta", // "世界"
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	got := strings.Join(names, ",")
	wantJoined := strings.Join(want, ",")
	if got != wantJoined {
		t.Fatalf("event sequence mismatch:\n got: %s\nwant: %s", got, wantJoined)
	}

	// message_start 应带 model / role
	start := decode(t, events[0][1])
	msg, _ := start["message"].(map[string]any)
	if start["type"] != "message_start" || msg["role"] != "assistant" || msg["model"] != "deepseek-r1" {
		t.Fatalf("message_start = %#v", start)
	}

	// thinking 必须合并成一条 delta
	thinkingDelta := decode(t, events[2][1])
	d, _ := thinkingDelta["delta"].(map[string]any)
	if d["type"] != "thinking_delta" || d["thinking"] != "想一想" {
		t.Fatalf("thinking delta not coalesced: %#v", thinkingDelta)
	}

	// text delta 逐条下发
	td := decode(t, events[5][1])
	tdelta, _ := td["delta"].(map[string]any)
	if tdelta["type"] != "text_delta" || tdelta["text"] != "你好" {
		t.Fatalf("text delta = %#v", td)
	}

	// message_delta 带 stop_reason 与 output_tokens
	md := decode(t, events[len(events)-2][1])
	if md["type"] != "message_delta" {
		t.Fatalf("expected message_delta, got %v", md["type"])
	}
	delta, _ := md["delta"].(map[string]any)
	if delta["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", delta["stop_reason"])
	}
	usage, _ := md["usage"].(map[string]any)
	if numVal(usage["output_tokens"]) != 5 {
		t.Fatalf("output_tokens = %v", usage["output_tokens"])
	}
}

// 上游若把推理与正文交错返回（R→C→R），必须仍只产生一个 thinking 块，正文保持逐条回放。
func TestStreamAsAnthropic_InterleavedReasoning(t *testing.T) {
	sse := "" +
		`data: {"choices":[{"delta":{"reasoning_content":"想"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"先答"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"reasoning_content":"再想"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"后答"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	var buf bytes.Buffer
	if err := StreamAsAnthropic(&buf, strings.NewReader(sse), "deepseek-r1", nil, true, nil); err != nil {
		t.Fatalf("StreamAsAnthropic: %v", err)
	}
	events := parseEvents(t, buf.String())
	names := eventNames(events)
	// 期望：单 thinking 块 + 单 text 块（两段正文各自一个 delta）。
	want := []string{
		"message_start",
		"content_block_start", // thinking
		"content_block_delta", // thinking_delta（合并为一条）
		"content_block_stop",
		"content_block_start", // text
		"content_block_delta", // "先答"
		"content_block_delta", // "后答"
		"content_block_stop",
		"message_delta",
		"message_stop",
	}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("event sequence mismatch:\n got: %s\nwant: %s", strings.Join(names, ","), strings.Join(want, ","))
	}
	// 确认只有一个 thinking 块（content_block_start 里 type=thinking 仅一次）。
	var thinkingStarts int
	for _, ev := range events {
		if ev[0] != "content_block_start" {
			continue
		}
		m := decode(t, ev[1])
		if cb, _ := m["content_block"].(map[string]any); cb != nil && cb["type"] == "thinking" {
			thinkingStarts++
		}
	}
	if thinkingStarts != 1 {
		t.Fatalf("expected exactly 1 thinking block, got %d", thinkingStarts)
	}
	// 确认思考被合并为一条 delta。
	var thinkingText string
	for _, ev := range events {
		if ev[0] != "content_block_delta" {
			continue
		}
		m := decode(t, ev[1])
		d, _ := m["delta"].(map[string]any)
		if d["type"] == "thinking_delta" {
			thinkingText = fmt.Sprint(d["thinking"])
		}
	}
	if thinkingText != "想再想" {
		t.Fatalf("interleaved thinking not merged: %q", thinkingText)
	}
}

func TestAggregateAsAnthropic(t *testing.T) {
	resp := map[string]any{
		"id":     "c1",
		"model":  "deepseek-r1",
		"object": "chat.completion",
		"choices": []any{map[string]any{
			"index":         0,
			"finish_reason": "stop",
			"message": map[string]any{
				"role":              "assistant",
				"content":           "答案是 2",
				"reasoning_content": "算一下",
			},
		}},
		"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 7, "total_tokens": 27},
	}

	out := AggregateAsAnthropic(resp, "deepseek-r1", nil, true)
	if out["type"] != "message" || out["role"] != "assistant" {
		t.Fatalf("envelope = %#v", out)
	}
	if !strings.HasPrefix(out["id"].(string), "msg_") {
		t.Fatalf("id = %v", out["id"])
	}
	content, _ := out["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("want thinking + text blocks, got %#v", content)
	}
	if content[0].(map[string]any)["type"] != "thinking" {
		t.Fatalf("block0 = %#v", content[0])
	}
	if content[1].(map[string]any)["type"] != "text" || content[1].(map[string]any)["text"] != "答案是 2" {
		t.Fatalf("block1 = %#v", content[1])
	}
	if out["stop_reason"] != "end_turn" {
		t.Fatalf("stop_reason = %v", out["stop_reason"])
	}
	u, _ := out["usage"].(map[string]any)
	if u["input_tokens"] != 20 || u["output_tokens"] != 7 {
		t.Fatalf("usage = %#v", u)
	}
}

func TestCountTokens(t *testing.T) {
	req := map[string]any{
		"model":  "claude-x",
		"system": "你是一个助手",
		"messages": []any{
			map[string]any{"role": "user", "content": "你好，请问今天天气怎么样？"},
		},
	}
	n := CountTokens(req)
	if n <= 0 {
		t.Fatalf("CountTokens = %d, want > 0", n)
	}
	// 更长的内容应该估算出更多 token
	longer := map[string]any{
		"system":   strings.Repeat("系统提示词", 100),
		"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("问题内容", 100)}},
	}
	if CountTokens(longer) <= n {
		t.Fatalf("longer input should cost more tokens: %d vs %d", CountTokens(longer), n)
	}
}

// ---------------------------------------------------------------------------
// Responses：请求转换
// ---------------------------------------------------------------------------

func TestResponsesToChat(t *testing.T) {
	req := map[string]any{
		"model":             "gpt-5",
		"instructions":      "你是 Codex 助手",
		"max_output_tokens": float64(2048),
		"input": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "input_text", "text": "读一下文件"},
			}},
			map[string]any{"type": "function_call", "call_id": "call_1", "name": "shell", "arguments": `{"cmd":"ls"}`},
			map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "a.go"},
		},
		"tools": []any{
			map[string]any{
				"type":        "function",
				"name":        "shell",
				"description": "执行命令",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
			},
		},
	}

	out, err := ResponsesToChat(req, "glm-5.2")
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	if out["model"] != "glm-5.2" || out["stream"] != true {
		t.Fatalf("out = %#v", out)
	}
	if out["max_tokens"] != float64(2048) {
		t.Fatalf("max_output_tokens should map to max_tokens, got %v", out["max_tokens"])
	}

	msgs, _ := out["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages, got %d: %#v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" || msgs[0].(map[string]any)["content"] != "你是 Codex 助手" {
		t.Fatalf("system = %#v", msgs[0])
	}
	if msgs[1].(map[string]any)["content"] != "读一下文件" {
		t.Fatalf("user = %#v", msgs[1])
	}
	// function_call → assistant tool_calls
	assistant, _ := msgs[2].(map[string]any)
	tcs, _ := assistant["tool_calls"].([]any)
	if len(tcs) != 1 {
		t.Fatalf("function_call not converted: %#v", msgs[2])
	}
	// function_call_output → role=tool
	if msgs[3].(map[string]any)["role"] != "tool" {
		t.Fatalf("function_call_output not converted: %#v", msgs[3])
	}

	// tools 扁平结构 → function 嵌套结构
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", out["tools"])
	}
	tfn, _ := tools[0].(map[string]any)["function"].(map[string]any)
	if tfn["name"] != "shell" {
		t.Fatalf("tool name = %v", tfn["name"])
	}
}

func TestResponsesToChatPlainStringInput(t *testing.T) {
	req := map[string]any{"model": "gpt-5", "input": "直接一句话"}
	out, err := ResponsesToChat(req, "auto")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["content"] != "直接一句话" {
		t.Fatalf("msgs = %#v", msgs)
	}
}

// ---------------------------------------------------------------------------
// Responses：流式事件序列
// ---------------------------------------------------------------------------

func TestStreamAsResponses(t *testing.T) {
	sse := "" +
		`data: {"id":"c1","choices":[{"delta":{"reasoning_content":"先想"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"delta":{"reasoning_content":"一下"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"delta":{"content":"你好"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"delta":{"content":"世界"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	var buf bytes.Buffer
	if err := StreamAsResponses(&buf, strings.NewReader(sse), "glm-5.2", nil); err != nil {
		t.Fatalf("StreamAsResponses: %v", err)
	}

	events := parseEvents(t, buf.String())
	names := eventNames(events)
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added", // reasoning
		"response.output_item.done",  // reasoning
		"response.output_item.added", // message
		"response.content_part.added",
		"response.output_text.delta", // 你好
		"response.output_text.delta", // 世界
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	got := strings.Join(names, ",")
	if got != strings.Join(want, ",") {
		t.Fatalf("event sequence mismatch:\n got: %s\nwant: %s", got, strings.Join(want, ","))
	}

	// reasoning 项应为合并后的文本
	reasonAdded := decode(t, events[2][1])
	item, _ := reasonAdded["item"].(map[string]any)
	if item["type"] != "reasoning" {
		t.Fatalf("expected reasoning item, got %#v", item)
	}
	summary, _ := item["summary"].([]any)
	if len(summary) != 1 || summary[0].(map[string]any)["text"] != "先想一下" {
		t.Fatalf("reasoning not coalesced: %#v", item["summary"])
	}

	// completed 响应要带 output_text 与 usage
	completed := decode(t, events[len(events)-1][1])
	if completed["type"] != "response.completed" {
		t.Fatalf("last event = %v", completed["type"])
	}
	respObj, _ := completed["response"].(map[string]any)
	if respObj["status"] != "completed" {
		t.Fatalf("status = %v", respObj["status"])
	}
	if respObj["output_text"] != "你好世界" {
		t.Fatalf("output_text = %v", respObj["output_text"])
	}
	u, _ := respObj["usage"].(map[string]any)
	if numVal(u["input_tokens"]) != 10 || numVal(u["output_tokens"]) != 5 || numVal(u["total_tokens"]) != 15 {
		t.Fatalf("usage = %#v", u)
	}
}

// ---------------------------------------------------------------------------
// OpenAI 兼容：流式 reasoning 合并
// ---------------------------------------------------------------------------

// TestStreamAsOpenAIReasoningPassthrough 验证：hide=false 时上游 delta 中的
// reasoning_content 逐帧透传，同时规范化剥除噪声——空 content=""、空
// tool_calls=[]、extra_fields 等未知字段、空占位 function_call 均不出现；
// role 保留；finish_reason 缺省帧为 null、末帧为 "stop"；usage 保留。
func TestStreamAsOpenAIReasoningPassthrough(t *testing.T) {
	SetHideReasoningStream(false)
	defer SetHideReasoningStream(true)
	sse := "" +
		`data: {"id":"c1","model":"glm-5.3-flash","choices":[{"delta":{"content":"","extra_fields":null,"function_call":null,"reasoning_content":"The","refusal":"","role":"assistant","tool_calls":[]}}]}` + "\n\n" +
		`data: {"id":"c1","model":"glm-5.3-flash","choices":[{"delta":{"content":"","reasoning_content":" user","role":"assistant","tool_calls":[]}}]}` + "\n\n" +
		`data: {"id":"c1","model":"glm-5.3-flash","choices":[{"delta":{"content":"","reasoning_content":" asks","role":"assistant","tool_calls":[]}}]}` + "\n\n" +
		`data: {"id":"c1","model":"glm-5.3-flash","choices":[{"delta":{"content":"Hi","role":"assistant","tool_calls":[]}}]}` + "\n\n" +
		`data: {"id":"c1","model":"glm-5.3-flash","choices":[{"delta":{"content":" there","role":"assistant","tool_calls":[]}}]}` + "\n\n" +
		`data: {"id":"c1","model":"glm-5.3-flash","choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	var buf bytes.Buffer
	if err := StreamAsOpenAI(&buf, strings.NewReader(sse), "glm-5.3-flash", nil); err != nil {
		t.Fatalf("StreamAsOpenAI: %v", err)
	}
	out := buf.String()

	// 拆成标准 OpenAI chunk（每行 data:）
	var chunks []map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		var c map[string]any
		if err := json.Unmarshal([]byte(payload), &c); err != nil {
			t.Fatalf("unmarshal chunk %q: %v", payload, err)
		}
		chunks = append(chunks, c)
	}

	// reasoning 逐帧透传：3 条，文本依次为 The / " user" / " asks"
	var rcTexts []string
	var contentTexts []string
	for _, c := range chunks {
		ch, _ := c["choices"].([]any)
		if len(ch) == 0 {
			continue
		}
		d, _ := ch[0].(map[string]any)["delta"].(map[string]any)
		if d == nil {
			continue
		}
		if s, ok := d["reasoning_content"].(string); ok && s != "" {
			rcTexts = append(rcTexts, s)
		}
		if s, ok := d["content"].(string); ok && s != "" {
			contentTexts = append(contentTexts, s)
		}
	}
	wantRC := []string{"The", " user", " asks"}
	if len(rcTexts) != len(wantRC) {
		t.Fatalf("reasoning chunks = %d (%v), want %d", len(rcTexts), rcTexts, len(wantRC))
	}
	for i, w := range wantRC {
		if rcTexts[i] != w {
			t.Fatalf("reasoning delta %d = %q, want %q", i, rcTexts[i], w)
		}
	}
	if len(contentTexts) != 2 || contentTexts[0] != "Hi" || contentTexts[1] != " there" {
		t.Fatalf("content chunks = %v, want [Hi  there]", contentTexts)
	}
	// 噪声剥除 + 规范化
	if strings.Contains(out, "extra_fields") || strings.Contains(out, `"content":""`) || strings.Contains(out, `"tool_calls":[]`) {
		t.Fatalf("noise leaked into normalized stream: %s", out)
	}
	if !strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Fatalf("object field missing: %s", out)
	}
	last := chunks[len(chunks)-1]
	ch0, _ := last["choices"].([]any)
	if len(ch0) == 0 || ch0[0].(map[string]any)["finish_reason"] != "stop" {
		t.Fatalf("last chunk finish_reason = %v, want stop", ch0)
	}
	if u, ok := last["usage"].(map[string]any); !ok || numVal(u["prompt_tokens"]) != 10 {
		t.Fatalf("usage missing/wrong on last chunk: %v", last["usage"])
	}
}

func TestAggregateAsResponses(t *testing.T) {
	resp := map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message": map[string]any{
				"role":              "assistant",
				"content":           "完成",
				"reasoning_content": "思考",
			},
		}},
		"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 4},
	}
	out := AggregateAsResponses(resp, "glm-5.2")
	if out["object"] != "response" || out["status"] != "completed" {
		t.Fatalf("envelope = %#v", out)
	}
	if !strings.HasPrefix(out["id"].(string), "resp_") {
		t.Fatalf("id = %v", out["id"])
	}
	if out["output_text"] != "完成" {
		t.Fatalf("output_text = %v", out["output_text"])
	}
	output, _ := out["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("want reasoning + message outputs, got %#v", output)
	}
	if output[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("output[0] = %#v", output[0])
	}
	if output[1].(map[string]any)["type"] != "message" {
		t.Fatalf("output[1] = %#v", output[1])
	}
	u, _ := out["usage"].(map[string]any)
	if numVal(u["total_tokens"]) != 7 {
		t.Fatalf("usage = %#v", u)
	}
}
