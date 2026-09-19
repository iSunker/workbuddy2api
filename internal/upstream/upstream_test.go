package upstream

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// SSE 解析与聚合
// ---------------------------------------------------------------------------

// TestIsModelChannelRejected 验证「模型/通道不被批准」的 4xx 识别逻辑：
// 这类错误应被 forwardStream 当作可重试（换账号/realm），而非立刻失败。
func TestIsModelChannelRejected(t *testing.T) {
	cases := []struct {
		name string
		code int
		msg  string
		want bool
	}{
		{"11128 unapproved channel", 11128, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, true},
		{"11102 model not supported", 11102, `{"code":11102,"msg":"model not supported"}`, true},
		{"keyword unapproved", 0, "upstream 400 client body={\"msg\":\"request from an unapproved channel\"}", true},
		{"keyword illegal invocation", 0, "Illegal API invocation from an unapproved channel", true},
		{"first msg not system (also 11128)", 11128, `{"code":11128,"msg":"first message is not system prompt"}`, true},
		{"plain 400 bad param", 400, `{"error":"invalid max_tokens"}`, false},
		{"hard credit", 402, `{"msg":"quota exceeded"}`, false},
		{"nil-like", 0, "", false},
	}
	for _, c := range cases {
		e := &Error{Code: c.code, Msg: c.msg}
		if got := IsModelChannelRejected(e); got != c.want {
			t.Errorf("%s: IsModelChannelRejected(%d,%q)=%v, want %v", c.name, c.code, c.msg, got, c.want)
		}
	}
}

func TestExtractCode(t *testing.T) {
	if got := extractCode([]byte(`{"code":11128,"msg":"x"}`)); got != 11128 {
		t.Errorf("extractCode = %d, want 11128", got)
	}
	if got := extractCode([]byte(`not json`)); got != 0 {
		t.Errorf("extractCode(non-json) = %d, want 0", got)
	}
	if got := extractCode(nil); got != 0 {
		t.Errorf("extractCode(nil) = %d, want 0", got)
	}
}

func TestParseSSE(t *testing.T) {
	raw := "data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"A\"}}]}\n\n" +
		": keep-alive\n\n" +
		"event: ping\n\n" +
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"B\"}}]}\n\n" +
		"data: [DONE]\n\n" +
		"data: {\"id\":\"1\",\"choices\":[{\"delta\":{\"content\":\"C\"}}]}\n\n" // DONE 之后应被忽略

	var got []string
	err := ParseSSE(strings.NewReader(raw), func(chunk map[string]any) error {
		choices, _ := chunk["choices"].([]any)
		for _, c := range choices {
			m, _ := c.(map[string]any)
			d, _ := m["delta"].(map[string]any)
			if s, ok := d["content"].(string); ok {
				got = append(got, s)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("ParseSSE: %v", err)
	}
	if strings.Join(got, "") != "AB" {
		t.Fatalf("want AB, got %q", strings.Join(got, ""))
	}
}

func TestAggregateTextAndReasoning(t *testing.T) {
	sse := "" +
		`data: {"id":"c1","created":1700000000,"choices":[{"delta":{"role":"assistant","content":"你好"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"delta":{"content":"世界"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"delta":{"reasoning_content":"先想一想"}}]}` + "\n\n" +
		`data: {"id":"c1","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	resp, err := Aggregate(strings.NewReader(sse), "glm-5.2")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if resp["model"] != "glm-5.2" {
		t.Fatalf("model not overridden: %v", resp["model"])
	}
	if resp["object"] != "chat.completion" {
		t.Fatalf("object: %v", resp["object"])
	}
	choices, _ := resp["choices"].([]any)
	if len(choices) != 1 {
		t.Fatalf("choices len = %d", len(choices))
	}
	c, _ := choices[0].(map[string]any)
	msg, _ := c["message"].(map[string]any)
	if msg["content"] != "你好世界" {
		t.Fatalf("content = %v", msg["content"])
	}
	if msg["reasoning_content"] != "先想一想" {
		t.Fatalf("reasoning = %v", msg["reasoning_content"])
	}
	if msg["role"] != "assistant" {
		t.Fatalf("role = %v", msg["role"])
	}
	if c["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", c["finish_reason"])
	}
	if resp["usage"] == nil {
		t.Fatal("usage missing")
	}
	if resp["id"] != "c1" {
		t.Fatalf("id = %v", resp["id"])
	}
}

func TestAggregateToolCalls(t *testing.T) {
	sse := "" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"北京\"}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"get_time","arguments":"{}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	resp, err := Aggregate(strings.NewReader(sse), "deepseek-v4-pro")
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	choices, _ := resp["choices"].([]any)
	msg, _ := choices[0].(map[string]any)["message"].(map[string]any)
	calls, _ := msg["tool_calls"].([]map[string]any)
	if len(calls) != 2 {
		t.Fatalf("tool_calls len = %d", len(calls))
	}
	// id 会被规范化为 toolu_ 前缀（Anthropic 客户端友好，见 sse.go renameToolCallID）
	if calls[0]["id"] != "toolu_1" || calls[0]["type"] != "function" {
		t.Fatalf("call_0 = %v", calls[0])
	}
	fn0, _ := calls[0]["function"].(map[string]any)
	if fn0["name"] != "get_weather" {
		t.Fatalf("name = %v", fn0["name"])
	}
	if fn0["arguments"] != `{"city":"北京"}` {
		t.Fatalf("arguments = %v", fn0["arguments"])
	}
	if calls[1]["id"] != "toolu_2" {
		t.Fatalf("call_1 = %v", calls[1])
	}
	if choices[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choices[0].(map[string]any)["finish_reason"])
	}
}

func TestStreamAsOpenAI(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n" + `data: [DONE]` + "\n\n"
	var buf bytes.Buffer
	if err := StreamAsOpenAI(&buf, strings.NewReader(sse), "glm-5.2", nil); err != nil {
		t.Fatalf("StreamAsOpenAI: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `"model":"glm-5.2"`) {
		t.Fatalf("model not overridden: %s", out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("missing [DONE] terminator: %q", out)
	}
	if !strings.Contains(out, "data: {\"choices\"") {
		t.Fatalf("unexpected chunk format: %s", out)
	}
}

// hide=false 时 reasoning_content 必须逐帧透传（与 workbuddy 同构）：
// 不缓冲、不合并、不改写正文 delta 边界，思考与正文按上游原始顺序流出。
func TestStreamAsOpenAI_ReasoningPassthrough(t *testing.T) {
	SetHideReasoningStream(false)
	defer SetHideReasoningStream(true)
	sse := "" +
		`data: {"id":"c1","created":1,"choices":[{"delta":{"reasoning_content":"is"}}]}` + "\n\n" +
		`data: {"id":"c1","created":1,"choices":[{"delta":{"reasoning_content":" using"}}]}` + "\n\n" +
		`data: {"id":"c1","created":1,"choices":[{"delta":{"reasoning_content":", then"}}]}` + "\n\n" +
		`data: {"id":"c1","created":1,"choices":[{"delta":{"content":"offer "}}]}` + "\n\n" +
		`data: {"id":"c1","created":1,"choices":[{"delta":{"content":"to help"}}]}` + "\n\n" +
		`data: {"id":"c1","created":1,"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	var buf bytes.Buffer
	if err := StreamAsOpenAI(&buf, strings.NewReader(sse), "deepseek-r1", nil); err != nil {
		t.Fatalf("StreamAsOpenAI: %v", err)
	}
	out := buf.String()

	// 3 条 reasoning delta 按帧原样透传。
	rcCount := strings.Count(out, `"reasoning_content"`)
	if rcCount != 3 {
		t.Fatalf("expected 3 reasoning_content deltas (frame passthrough), got %d. out=%s", rcCount, out)
	}
	for _, want := range []string{`"reasoning_content":"is"`, `"reasoning_content":" using"`, `"reasoning_content":", then"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing reasoning delta %s. out=%s", want, out)
		}
	}
	// 两条 content delta 须原样转发。
	if !strings.Contains(out, `"content":"offer "`) || !strings.Contains(out, `"content":"to help"`) {
		t.Fatalf("content deltas lost: %s", out)
	}
	// 推理先于正文（保持上游顺序）。
	rcIdx := strings.Index(out, `"reasoning_content"`)
	ctIdx := strings.Index(out, `"content":"offer`)
	if rcIdx < 0 || ctIdx < 0 || rcIdx > ctIdx {
		t.Fatalf("reasoning must be flushed before content. rc=%d content=%d out=%s", rcIdx, ctIdx, out)
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("missing [DONE] terminator: %q", out)
	}
}

// 上游若把推理与正文交错返回（R→C→R），透传必须保持原始交错顺序。
func TestStreamAsOpenAI_InterleavedReasoning(t *testing.T) {
	SetHideReasoningStream(false)
	defer SetHideReasoningStream(true)
	sse := "" +
		`data: {"choices":[{"delta":{"reasoning_content":"想"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"先答"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"reasoning_content":"再想"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{"content":"后答"}}]}` + "\n\n" +
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"

	var buf bytes.Buffer
	if err := StreamAsOpenAI(&buf, strings.NewReader(sse), "deepseek-r1", nil); err != nil {
		t.Fatalf("StreamAsOpenAI: %v", err)
	}
	out := buf.String()
	if strings.Count(out, `"reasoning_content"`) != 2 {
		t.Fatalf("interleaved reasoning deltas must pass through frame-by-frame. out=%s", out)
	}
	// 顺序：想 → 先答 → 再想 → 后答
	idx := func(s string) int { return strings.Index(out, s) }
	if !(idx(`"reasoning_content":"想"`) < idx(`"content":"先答"`) &&
		idx(`"content":"先答"`) < idx(`"reasoning_content":"再想"`) &&
		idx(`"reasoning_content":"再想"`) < idx(`"content":"后答"`)) {
		t.Fatalf("interleaved order broken: %s", out)
	}
}

// ---------------------------------------------------------------------------
// 请求体构造
// ---------------------------------------------------------------------------

func TestBuildChatPayloadForcesStream(t *testing.T) {
	req := map[string]any{
		"model":    "glm-5.2",
		"stream":   false,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}
	raw, err := BuildChatPayload(req, "glm-5.2")
	if err != nil {
		t.Fatalf("BuildChatPayload: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 上游只接受流式，必须被强制为 true
	if out["stream"] != true {
		t.Fatalf("stream = %v, want true", out["stream"])
	}
	if _, ok := out["stream_options"]; ok {
		t.Fatal("stream_options should be stripped")
	}
	if out["model"] != "glm-5.2" {
		t.Fatalf("model = %v", out["model"])
	}
	// 上游硬性要求首条消息必须是 system，客户端没给时要自动补一条
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("want default system prepended (2 messages), got %d: %#v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatalf("first message must be system, got %#v", msgs[0])
	}
}

// 客户端已经提供 system 时不应重复补一条。
func TestBuildChatPayloadKeepsExistingSystem(t *testing.T) {
	req := map[string]any{
		"model":    "glm-5.2",
		"messages": []any{map[string]any{"role": "system", "content": "自定义提示词"}},
	}
	raw, err := BuildChatPayload(req, "glm-5.2")
	if err != nil {
		t.Fatalf("BuildChatPayload: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("system should not be duplicated, got %d: %#v", len(msgs), msgs)
	}
	if msgs[0].(map[string]any)["content"] != "自定义提示词" {
		t.Fatalf("system content = %#v", msgs[0])
	}
}

// TestProbePayloadHasSystemFirst 锁定保活/重测探针的首消息必须是 system：
// 上游硬性要求（否则 11128 "first message is not system prompt"），而探针
// 绕过了 BuildChatPayload 的自动补齐。缺 system 会让「模型列表拉不到」的
// 账号（如 saas 域）在保活里被误判为连接失败。
func TestProbePayloadHasSystemFirst(t *testing.T) {
	p := probePayload()
	if p["stream"] != true {
		t.Fatalf("probe must be streaming, got %v", p["stream"])
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) < 2 {
		t.Fatalf("want >=2 messages (system+user), got %d", len(msgs))
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("first message role = %v, want system", first["role"])
	}
	if c, _ := first["content"].(string); c == "" {
		t.Fatal("system content must not be empty")
	}
}

func TestBuildChatPayloadCleansTools(t *testing.T) {
	req := map[string]any{
		"model":    "glm-5.2",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			// 合法
			map[string]any{"type": "function", "function": map[string]any{
				"name": "ok", "parameters": map[string]any{
					"type": "object", "properties": map[string]any{
						"a": map[string]any{"type": "string", "additionalProperties": false},
					},
					"additionalProperties": false,
				},
				"strict": true,
			}},
			// 非 function 类型 → 丢弃
			map[string]any{"type": "web_search", "function": map[string]any{"name": "ws"}},
			// 空 parameters → 丢弃
			map[string]any{"type": "function", "function": map[string]any{"name": "empty", "parameters": map[string]any{}}},
			// parameters 缺 type → 丢弃
			map[string]any{"type": "function", "function": map[string]any{
				"name": "notype", "parameters": map[string]any{"properties": map[string]any{}},
			}},
		},
	}
	raw, err := BuildChatPayload(req, "glm-5.2")
	if err != nil {
		t.Fatalf("BuildChatPayload: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	if fn["name"] != "ok" {
		t.Fatalf("kept wrong tool: %v", fn["name"])
	}
	if _, ok := fn["strict"]; ok {
		t.Fatal("strict should be stripped")
	}
	params, _ := fn["parameters"].(map[string]any)
	if _, ok := params["additionalProperties"]; ok {
		t.Fatal("additionalProperties should be stripped")
	}
	props, _ := params["properties"].(map[string]any)
	propA, _ := props["a"].(map[string]any)
	if _, ok := propA["additionalProperties"]; ok {
		t.Fatal("nested additionalProperties should be stripped")
	}
}

func TestBuildChatPayloadDropsToolChoiceWhenNoTools(t *testing.T) {
	req := map[string]any{
		"model":       "glm-5.2",
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":       []any{map[string]any{"type": "web_search"}},
		"tool_choice": "auto",
	}
	raw, err := BuildChatPayload(req, "glm-5.2")
	if err != nil {
		t.Fatalf("BuildChatPayload: %v", err)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if _, ok := out["tools"]; ok {
		t.Fatal("tools should be removed when all definitions are dropped")
	}
	if _, ok := out["tool_choice"]; ok {
		t.Fatal("tool_choice should be removed along with tools")
	}
}

// ---------------------------------------------------------------------------
// 错误分类
// ---------------------------------------------------------------------------

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		code int
		body string
		want ErrKind
	}{
		{"402", 402, "", ErrHardCredit},
		{"quota words", 400, `{"message":"quota exceeded"}`, ErrHardCredit},
		{"cn quota", 200, `{"message":"积分不足"}`, ErrHardCredit},
		{"401 dead", 401, `{"message":"invalid_format"}`, ErrSessionDead},
		{"401 expired", 401, `{"message":"please login"}`, ErrTokenExpired},
		{"403 dead", 403, `{"message":"forbidden"}`, ErrSessionDead},
		{"429", 429, "", ErrSoftRate},
		{"404", 404, "", ErrNotFound},
		{"500", 500, "", ErrServer},
		{"418", 418, "", ErrClient},

		// 11148 工具调用序列断裂：实测返回 400 + code 11148，必须优先于
		// hardMarkers 判定，否则会被 "do not match" 之类措辞误判成配额不足。
		{
			"11148 broken tool sequence", 400,
			`{"code":11148,"msg":"tool calls and tool results do not match, please start a new conversation and retry",` +
				`"extError":{"code":"tool_call_sequence_broken","message":"tool calls and tool results do not match"}}`,
			ErrBrokenToolSeq,
		},
		{"11148 by marker only", 400, `{"msg":"tool_call_sequence_broken"}`, ErrBrokenToolSeq},
	}
	for _, c := range cases {
		if got := Classify(c.code, c.body); got != c.want {
			t.Errorf("%s: Classify(%d,%q) = %v, want %v", c.name, c.code, c.body, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 模型列表解析
// ---------------------------------------------------------------------------

func TestParseModelList(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			"openai style",
			`{"object":"list","data":[{"id":"glm-5.2","owned_by":"zhipu"},{"id":"deepseek-v4-pro"}]}`,
			[]string{"deepseek-v4-pro", "glm-5.2"},
		},
		{
			"v3 config array",
			`{"code":0,"data":{"models":[{"id":"DeepSeek-V4-Pro"},{"id":"GLM-5.2"}]}}`,
			[]string{"deepseek-v4-pro", "glm-5.2"},
		},
		{
			"v3 config map",
			`{"code":0,"data":{"models":{"glm-5.2":{"display_name":"GLM-5.2"},"auto":{}}}}`,
			[]string{"auto", "glm-5.2"},
		},
		{
			"v3 config grouped by scene",
			`{"code":0,"data":{"models":{"chat":[{"id":"glm-5.2"}],"vision":[{"id":"glm-5.2"},{"id":"auto"}]}}}`,
			[]string{"auto", "glm-5.2"},
		},
		{
			"bare array",
			`[{"model":"glm-5.2","display_name":"GLM-5.2"}]`,
			[]string{"glm-5.2"},
		},
		{
			"empty config",
			`{"code":0,"msg":"OK","data":{"models":null}}`,
			nil,
		},
	}
	for _, c := range cases {
		got := parseModelList([]byte(c.in))
		var ids []string
		for _, m := range got {
			ids = append(ids, m.ID)
		}
		if len(ids) != len(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, ids, c.want)
			continue
		}
		for i := range ids {
			if ids[i] != c.want[i] {
				t.Errorf("%s: got %v, want %v", c.name, ids, c.want)
				break
			}
		}
	}
}

func TestNormalizeModelName(t *testing.T) {
	cases := map[string]string{
		"DeepSeek-V4-Pro": "deepseek-v4-pro",
		"GLM 5.2":         "glm-5.2",
		"kimi_k2.7":       "kimi-k2.7",
		"  Auto  ":        "auto",
	}
	for in, want := range cases {
		if got := NormalizeModelName(in); got != want {
			t.Errorf("NormalizeModelName(%q) = %q, want %q", in, got, want)
		}
	}
}
