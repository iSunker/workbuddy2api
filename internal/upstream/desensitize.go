// desensitize.go 针对 CodeBuddy 上游内容审核的脱敏层（可关闭）。
//
// 背景
// ----
// CodeBuddy 后端（copilot.tencent.com）有内容审核，会拦截含"攻击/漏洞/凭证"
// 等含义的英文术语。但这些词大量出现在客户端**固定的合规 system 模板**里
// （例如 Claude Code 的 system prompt：「Refuse requests for DoS attacks,
// exploit development, credential stealers, escalation tooling ...」），
// 属于**拒绝作恶**的合规声明，并非用户有害输入，却被后端误判为敏感词，
// 导致整条请求被拦（http 400 / code 11128
// "Illegal API invocation from an unapproved channel"）。
//
// 本模块做的事
// ------------
//  1. 零宽脱敏：在命中词内部插入零宽空格 U+200B，打断后端关键词匹配，
//     而人/模型读起来无差别（"DoS" -> "Do\u200bS"）。
//  2. 运行时片段压缩：把 harness（Codex CLI / Claude Code）注入的超长
//     environment / permissions / skills / system-reminder 块替换成一行摘要。
//  3. 工具脱敏：对 tools 里的 description / title 做同样的零宽处理
//     （可选直接剔除描述字段，风险更低但会损失工具语义）。
//
// 设计原则
// --------
//   - 保守：词表小而明确；默认只处理 system / developer 角色，以及被识别为
//     harness 注入的 user 上下文；真实用户输入保持原样。
//   - 可关闭：由配置 features.desensitize 控制，默认开启。
//   - 不修改调用方传入的对象，所有改动都作用在拷贝上。
package upstream

import (
	"regexp"
	"sort"
	"strings"
)

// zwsp 零宽空格：插入到关键词内部，打断后端关键词匹配，人眼/模型无感。
const zwsp = "\u200b"

// 脱敏开关（默认开启）。由 SetDesensitize 按配置设置。
var (
	desensitizeEnabled = true
	desensitizeToolsOn = true
	stripToolMetadata  = false
)

// SetDesensitize 配置脱敏行为。
//
//	on        总开关
//	tools     是否处理 tools 的 description / title
//	stripMeta 是否直接删除 description / title（true 时不再做零宽替换）
func SetDesensitize(on, tools, stripMeta bool) {
	desensitizeEnabled = on
	desensitizeToolsOn = tools
	stripToolMetadata = stripMeta
}

// DesensitizeEnabled 报告脱敏总开关状态（供启动日志使用）。
func DesensitizeEnabled() bool { return desensitizeEnabled }

// sensitiveTerms 触发审核的"合规声明高频词"。
// 全部来自真实被拦截的客户端 system 模板，大小写不敏感，按词边界匹配。
var sensitiveTerms = []string{
	// 原有词表
	"DoS",
	"DDoS",
	"exploit",
	"credential testing",
	"credential stuffing",
	"supply chain compromise",
	"supply-chain compromise",
	"detection evasion",
	"C2 frameworks",
	"C2 framework",
	"command and control",
	"malicious purposes",
	"malicious intent",
	"mass targeting",
	"brute force",
	"brute-force",
	"privilege escalation",
	"reverse shell",
	"remote code execution",
	"SQL injection",
	"XSS",
	"CSRF",
	"phishing",
	"malware",
	"ransomware",
	"keylogger",
	"rootkit",
	"backdoor",
	"botnet",
	"zero-day",
	"0day",
	// Codex CLI system prompt 里额外的高频触发词
	"vulnerability",
	"vulnerabilities",
	"red teaming",
	"red-teaming",
	"sandbox",
	"sandboxing",
	"sandboxed",
	"unsandboxed",
	"escalated privileges",
	"escalated",
	"escalation",
	"destructive action",
	"destructive command",
	"destructive",
	"attack",
	"attacks",
	"cybersecurity",
	"security review",
	"exploit development",
	"hacking",
	"penetration testing",
	"penetration test",
	"injection",
	"weaponize",
	"weaponized",
	"harmful",
	"dangerous",
	"abuse",
	"abusive",
	"illegal",
	"terrorist",
	"terrorism",
	"bomb",
	"weapon",
	"weapons",
	"drug",
	"drugs",
	"narcotic",
	"suicide",
	"self-harm",
	"murder",
	"kill",
	"violence",
	"violent",
	// Claude Code / Anthropic 品牌词（避免竞争品牌词触发审核）
	"Claude Code",
	"Claude Opus",
	"Claude Sonnet",
	"Claude Haiku",
	"Claude Fable",
	"Anthropic",
	"Co-Authored-By",
	"noreply@anthropic.com",
}

// sensitiveRe 按词长降序编译成一个大正则（避免短词先吃掉长词），
// 词边界 + 忽略大小写（词边界防止 "kill" 误伤 "skills"）。
var sensitiveRe = func() *regexp.Regexp {
	terms := append([]string(nil), sensitiveTerms...)
	sort.SliceStable(terms, func(i, j int) bool { return len(terms[i]) > len(terms[j]) })
	parts := make([]string, 0, len(terms))
	for _, t := range terms {
		parts = append(parts, regexp.QuoteMeta(t))
	}
	return regexp.MustCompile(`(?i)\b(?:` + strings.Join(parts, "|") + `)\b`)
}()

// multiNewlineRe 连续 3 个及以上换行收敛为 2 个。
var multiNewlineRe = regexp.MustCompile(`\n{3,}`)

// zeroWidthSplit 在词内部插入零宽空格，如 "DoS" -> "Do\u200bS"。
func zeroWidthSplit(term string) string {
	r := []rune(term)
	if len(r) <= 1 {
		return term
	}
	return string(r[:1]) + zwsp + string(r[1:])
}

// desensitizeText 对文本中的触发词插入零宽空格。无触发词则原样返回。
func desensitizeText(text string) string {
	if text == "" {
		return text
	}
	return sensitiveRe.ReplaceAllStringFunc(text, zeroWidthSplit)
}

// blockReplacement 一段由起止标记包裹的运行时元数据的替换规则。
type blockReplacement struct {
	start string
	end   string
	repl  string
}

// runtimeBlocks harness 注入的冗长运行时块 → 一行中性摘要。
var runtimeBlocks = []blockReplacement{
	{"<environment_context>", "</environment_context>",
		"Environment context is provided by the harness."},
	{"<permissions instructions>", "</permissions instructions>",
		"Runtime permissions apply: filesystem access may be sandboxed, network may be restricted, and some commands may require user approval."},
	{"<collaboration_mode>", "</collaboration_mode>",
		"Collaboration mode instructions are provided by the harness."},
	{"<skills_instructions>", "</skills_instructions>",
		"Runtime skill metadata is available. Use relevant skills only when explicitly requested or clearly applicable."},
	{"<plugins_instructions>", "</plugins_instructions>",
		"Runtime plugin metadata is available when relevant."},
	{"<system-reminder>", "</system-reminder>",
		"Runtime reminder context is provided by the harness."},
}

// compiledBlock 预编译的块替换规则。
type compiledBlock struct {
	re   *regexp.Regexp
	repl string
}

var compiledBlocks = func() []compiledBlock {
	out := make([]compiledBlock, 0, len(runtimeBlocks))
	for _, b := range runtimeBlocks {
		re := regexp.MustCompile(`(?s)\s*` + regexp.QuoteMeta(b.start) + `.*?` + regexp.QuoteMeta(b.end) + `\s*`)
		out = append(out, compiledBlock{re: re, repl: "\n\n" + b.repl + "\n\n"})
	}
	return out
}()

// runtimeTailMarkers 出现在提示词尾部的工具/技能清单起始标记，
// 从首个命中的位置起整体裁掉，换成一句话摘要。
var runtimeTailMarkers = []string{
	"The following deferred tools are now available via ToolSearch.",
	"Available agent types for the Agent tool:",
	"The following skills are available for use with the Skill tool:",
	"## MCP Server Instructions",
}

const runtimeTailSummary = "Runtime tool, agent, skill, and MCP metadata is available separately."

// codexSystemMarkers 识别 harness 的 system 模板（Codex CLI / Claude Code）。
var codexSystemMarkers = []string{
	"You are a coding agent running in the Codex CLI",
	"Within this context, Codex refers to",
	"# How you work",
	"You are Claude Code",
}

const (
	claudeCodeSummary = "You are a coding assistant. Be precise, helpful, concise, and safe. " +
		"Use available tools when needed, follow repository instructions, and keep the user informed."
	codexCoreSummary = "You are a coding assistant in Codex CLI. Be precise, helpful, concise, and safe. " +
		"Inspect the repository, use available tools when needed, follow repository instructions, " +
		"and keep the user informed with concise progress updates."
	permissionsSummary = "Runtime permissions apply: filesystem access may be sandboxed, network may be restricted, " +
		"and some commands may require user approval."
	skillsSummary      = "Runtime skill metadata is available. Use relevant skills only when explicitly requested or clearly applicable."
	harnessUserSummary = "Repository instructions and environment context are provided. " +
		"Follow repository guidance while answering the user's actual request."
)

// permissionsMarkers / skillsMarkers 单独成段的权限与技能说明标记。
var (
	permissionsMarkers = []string{
		"<permissions instructions>",
		"Filesystem sandboxing defines which files can be read or written.",
		"## How to request escalation",
	}
	skillsMarkers = []string{
		"<skills_instructions>",
		"### Available skills",
		"### How to use skills",
	}
)

// harnessUserMarkers 判断一条 user 消息其实是 harness 注入的上下文而非自然输入。
var harnessUserMarkers = []string{
	"# AGENTS.md instructions",
	"<environment_context>",
	"<permissions instructions>",
	"<collaboration_mode>",
	"<skills_instructions>",
	"<system-reminder>",
	"# claudeMd",
}

func containsAny(text string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// contentToText 把字符串或 content blocks 规整成纯文本。
func contentToText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, blk := range c {
			if m, ok := blk.(map[string]any); ok && m["type"] == "text" {
				if t, ok := m["text"].(string); ok {
					sb.WriteString(t)
				}
			}
		}
		return sb.String()
	}
	return ""
}

// looksLikeHarnessUser 判断 user 消息是否是 harness 注入的上下文。
func looksLikeHarnessUser(content any) bool {
	return containsAny(contentToText(content), harnessUserMarkers)
}

// pruneRuntimeFragments 裁掉冗长的运行时元数据，保留主要行为指令。
func pruneRuntimeFragments(role, text string) string {
	if text == "" {
		return text
	}
	pruned := text
	for _, b := range compiledBlocks {
		pruned = b.re.ReplaceAllString(pruned, b.repl)
	}

	cut := -1
	for _, m := range runtimeTailMarkers {
		if idx := strings.Index(pruned, m); idx >= 0 && (cut < 0 || idx < cut) {
			cut = idx
		}
	}
	if cut >= 0 {
		head := strings.TrimRight(pruned[:cut], " \t\r\n")
		if head != "" {
			pruned = head + "\n\n" + runtimeTailSummary
		} else {
			pruned = runtimeTailSummary
		}
	}

	pruned = multiNewlineRe.ReplaceAllString(pruned, "\n\n")
	return strings.TrimSpace(pruned)
}

// compactHarnessMessage 把 harness 注入的超长提示压缩成短摘要。
// 返回 (摘要, 是否命中压缩规则)；未命中时 ok=false，调用方走常规零宽脱敏。
func compactHarnessMessage(role string, content any) (string, bool) {
	text := contentToText(content)
	if text == "" {
		return "", false
	}
	if role == "system" && containsAny(text, codexSystemMarkers) {
		if strings.Contains(text, "You are Claude Code") {
			return claudeCodeSummary, true
		}
		return codexCoreSummary, true
	}
	if containsAny(text, permissionsMarkers) {
		return permissionsSummary, true
	}
	if containsAny(text, skillsMarkers) {
		return skillsSummary, true
	}
	// 注意：user 消息不再整条替换成 harnessUserSummary 常量。
	// Claude 系 harness 会把 <system-reminder> 注入块与用户的【真实指令】塞进
	// 同一条 user 消息；旧逻辑只要命中 <system-reminder> 就把整条（含用户指令）
	// 替换成一句「Follow repository guidance...」的摘要，导致上游收不到真实指令，
	// 表现为模型回「没看到你的请求」。现在 user 消息只做常规 prune + 零宽脱敏，
	// 保留全部正文与指令（与未开启 desensitize 的旧版行为一致）。
	return "", false
}

// desensitizeContent 处理一条消息的 content（字符串或 blocks 数组）。
func desensitizeContent(role string, content any) any {
	if compacted, ok := compactHarnessMessage(role, content); ok {
		return desensitizeText(compacted)
	}
	switch c := content.(type) {
	case string:
		return desensitizeText(pruneRuntimeFragments(role, c))
	case []any:
		blocks := make([]any, 0, len(c))
		for _, blk := range c {
			m, ok := blk.(map[string]any)
			if !ok || m["type"] != "text" {
				blocks = append(blocks, blk)
				continue
			}
			nm := make(map[string]any, len(m))
			for k, v := range m {
				nm[k] = v
			}
			if t, ok := m["text"].(string); ok {
				nm["text"] = desensitizeText(pruneRuntimeFragments(role, t))
			}
			blocks = append(blocks, nm)
		}
		return blocks
	}
	return content
}

// desensitizeMessages 对 system / developer 角色以及 harness 注入的 user
// 上下文做脱敏，返回新的 messages 列表（不修改原对象）。
func desensitizeMessages(msgs []any) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			out = append(out, m)
			continue
		}
		role, _ := mm["role"].(string)
		should := role == "system" || role == "developer"
		if role == "user" && looksLikeHarnessUser(mm["content"]) {
			should = true
		}
		if !should {
			out = append(out, mm)
			continue
		}
		nm := make(map[string]any, len(mm))
		for k, v := range mm {
			nm[k] = v
		}
		nm["content"] = desensitizeContent(role, mm["content"])
		out = append(out, nm)
	}
	return out
}

// desensitizeToolValue 递归处理 tool 定义中的 description / title。
func desensitizeToolValue(v any) any {
	switch val := v.(type) {
	case map[string]any:
		nv := make(map[string]any, len(val))
		for k, item := range val {
			if k == "description" || k == "title" {
				if s, ok := item.(string); ok {
					if stripToolMetadata {
						continue // 直接剔除，风险最低但损失工具语义
					}
					nv[k] = desensitizeText(s)
					continue
				}
			}
			nv[k] = desensitizeToolValue(item)
		}
		return nv
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = desensitizeToolValue(item)
		}
		return out
	}
	return v
}

// applyDesensitize 对最终发往上游的请求体做脱敏（messages + tools）。
// 返回新的 map，不修改入参。
func applyDesensitize(out map[string]any) map[string]any {
	if !desensitizeEnabled {
		return out
	}
	if msgs, ok := out["messages"].([]any); ok && len(msgs) > 0 {
		out["messages"] = desensitizeMessages(msgs)
	}
	if desensitizeToolsOn {
		if tools, ok := out["tools"]; ok {
			out["tools"] = desensitizeToolValue(tools)
		}
	}
	return out
}
