package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// min_reasoning_effort：客户端档位低于下限时抬到下限，缺省补下限，高档保持。
func TestClampReasoningEffort(t *testing.T) {
	cases := []struct {
		floor, in, want string
	}{
		{"", "low", "low"},      // 未配置下限：原样透传
		{"", "", ""},            // 未配置下限且缺省：不注入
		{"high", "low", "high"}, // 低档抬到下限
		{"high", "", "high"},    // 缺省补下限
		{"high", "max", "max"},  // 高档保持
		{"high", "medium", "high"},
		{"max", "high", "max"},
		{"high", "garbage", "high"}, // 未知档位视为未提供
		{"low", "low", "low"},
	}
	for _, c := range cases {
		SetMinReasoningEffort(c.floor)
		if got := clampReasoningEffort(c.in); got != c.want {
			t.Errorf("floor=%q in=%q: got %q, want %q", c.floor, c.in, got, c.want)
		}
	}
	SetMinReasoningEffort("")
}

// BuildChatPayload 端到端：下限配置生效到最终上游请求体。
func TestBuildChatPayloadMinEffort(t *testing.T) {
	SetMinReasoningEffort("high")
	defer SetMinReasoningEffort("")

	clientReq := map[string]any{
		"model":            "glm-5.3-flash",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"reasoning_effort": "low",
	}
	raw, err := BuildChatPayload(clientReq, "glm-5.3-flash")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if v, _ := out["reasoning_effort"].(string); v != "high" {
		t.Errorf("reasoning_effort = %q, want %q", v, "high")
	}

	// 缺省时注入下限
	clientReq2 := map[string]any{
		"model":    "glm-5.3-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	raw, err = BuildChatPayload(clientReq2, "glm-5.3-flash")
	if err != nil {
		t.Fatal(err)
	}
	out = map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if v, _ := out["reasoning_effort"].(string); v != "high" {
		t.Errorf("reasoning_effort = %q, want injected %q", v, "high")
	}

	// 未配置下限时保持原样（不注入字段）
	SetMinReasoningEffort("")
	clientReq3 := map[string]any{
		"model":    "glm-5.3-flash",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	raw, err = BuildChatPayload(clientReq3, "glm-5.3-flash")
	if err != nil {
		t.Fatal(err)
	}
	out = map[string]any{}
	_ = json.Unmarshal(raw, &out)
	if _, exists := out["reasoning_effort"]; exists {
		t.Errorf("reasoning_effort should be absent without floor, got %v", out["reasoning_effort"])
	}
}

// 分片 content 归一化：把真实指令前置、提醒块后置，并拍扁成单字符串。
// 这是「Claude 桌面端回『没看到你的请求』」的根因修复。
func TestNormalizeMessageContents(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": "sys"},
		map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "text", "text": "<system-reminder>\nThe task tools haven't been used recently.\n</system-reminder>"},
			map[string]any{"type": "text", "text": "<system-reminder>\n<total_tokens>15000000 tokens left</total_tokens>\n</system-reminder>"},
			map[string]any{"type": "text", "text": "我让你看 https://github.com/MadsLorentzen/ai-job-search 这个项目啊"},
		}},
	}
	got := normalizeMessageContents(msgs)

	userMsg, _ := got[1].(map[string]any)
	content, _ := userMsg["content"].(string)
	if content == "" {
		t.Fatal("user content became empty")
	}
	// 真实指令必须在最前，提醒块在后
	if !strings.HasPrefix(content, "我让你看 https://github.com/MadsLorentzen/ai-job-search") {
		t.Errorf("real instruction not hoisted to front: %q", content)
	}
	if !strings.Contains(content, "<system-reminder>") {
		t.Errorf("reminder blocks should be preserved (appended), got %q", content)
	}

	// 字符串 content 不受影响
	msgs2 := []any{map[string]any{"role": "user", "content": "plain text"}}
	got2 := normalizeMessageContents(msgs2)
	if v, _ := got2[0].(map[string]any)["content"].(string); v != "plain text" {
		t.Errorf("plain string content mutated: %q", v)
	}
}
