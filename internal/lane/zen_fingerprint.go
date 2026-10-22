// Package lane 负责与上游免费源对话。
//
// 本文件是整个项目最关键、也最易碎的一处：OpenCode Zen 的免密车道
// 不是"不要 key"，而是"只接受真正的 OpenCode 客户端"。上游会校验
// 端点路径、User-Agent、会话头、会话 ID 编码、流式协议与工具的存在性，
// 任何一条不满足都返回 403 FreeTierError。
//
// ⚠️ **实测（2026-10-22）确认的闸门条件** —— 必须全部满足：
//
//	User-Agent 形状对 · 四个会话头同值 · 会话 ID 形状对 ·
//	流式（stream: true）· 带 read + shell 两个工具
//
//	**不看**：accept、x-opencode-client、x-opencode-project、
//	system 提示、**工具描述的任何字**、tool_choice。
//
// 规格来源是 **OpenCode 官方客户端的源码**（MIT 授权，Copyright (c) 2025 opencode）：
//
//	https://github.com/sst/opencode
//	packages/core/src/tool/plugin/read.ts    → read 工具定义
//	packages/core/src/tool/plugin/shell.ts   → shell 工具定义
//	packages/core/src/session/model-request.ts → 会话头
//
// 另有 FMH 自己的**实测**（2026-10-19 ～ 22）确认了源码里读不出来的部分：
// 端点按模型名分流、会话 ID 的降序时间戳编码。
//
// 因此本文件必须满足两条纪律：
//  1. 所有闸门相关的字面量集中在这一处，上游改规格时只改这里；
//  2. zen_test.go 里的回归测试必须锁住这些字面量，防止被"顺手优化"掉。
//
// 详见 docs/design/01-zen-fingerprint.md 与
// docs/verification/upstream-conformance.md。
package lane

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// ZenBaseURL 是免密车道的根。
	ZenBaseURL = "https://opencode.ai"

	// ZenClientUA 是闸门指纹的 User-Agent。
	//
	// ⚠️ 形状是 opencode/<channel>/<version>/<client>，major 必须 ≥ 2。
	// ⚠️ 整串匹配：任何前后缀（包括本项目自己的归属信息）都会让它不再是
	//   "一个 v2 客户端"，直接换来 403 FreeTierError。归属信息请放到别的头。
	ZenClientUA = "opencode/latest/2.0.26/cli"

	// ZenCredential 是免密车道的共享凭据。
	// 它不是任何用户的私密 key —— 全网所有用户共用这一个。
	// 推论：这条车道的 429 反映的是"全局此刻拥挤"，不是"这个模型坏了"。
	ZenCredential = "Bearer public"
)

// zenFingerprintTools 是闸门要求必须声明的两个工具名。
// 大小写精确匹配：调用方的 `Read` / `Bash` 不算，原样透传。
var zenFingerprintTools = []string{"read", "shell"}

// **官方工具定义（用官方原文）**
//
// 来自 OpenCode 官方客户端（MIT 授权，Copyright (c) 2025 opencode）：
//
//	packages/core/src/tool/plugin/read.ts
//	packages/core/src/tool/plugin/shell.ts
//	https://github.com/sst/opencode
//
// ⚠️ **闸门到底比什么（实测，2026-10-22）**：
//
//	**比**：工具**名字** —— 必须是 `read` 和 `shell`（大小写精确）。
//	       改名（`read` → `read_file`）→ 403。
//	       **只给 `read` 不给 `shell`** → 403（两个都要）。
//	**不比**：description 的任何一个字、parameters / schema、
//	       多余的工具（加第三个照样过）、tool_choice。
//
//	所以下面的描述用官方原文，是**保守**（万一上游以后开始比对），
//	不是**必需**。
//
// 保留官方措辞还有第二个理由：它们描述的是真实语义（读文件、跑命令），
// 注入后如果模型真的去调，行为是可预期的。
//
// ⚠️ 名字才是关键 —— 调用方自己带 `read` / `shell` 时要**改名让位**
//	（见 zenApplyFingerprint 的别名逻辑），否则闸门看到重名会拒。

const zenReadDescription = "Read the contents of a file or directory. Supports text files, images, and PDFs. Images and PDFs are presented directly to the model. Each text line is prefixed by its 1-based line number as <line>: <content>. The prefix is for reference and is not part of the file content. Directory entries are returned one per line. Use offset and limit to read large files or directories in sections. Prefer one larger read over many small slices, and use grep to find specific content in large files."

const zenShellDescription = "Execute a shell command and return its output. Commands run on Linux using bash. Quote file paths containing spaces or special characters. Prefer dedicated tools over shell commands when possible. When output is large, the full result is saved to a file and a truncated preview is returned. Rely on automatic truncation unless filtering the output is more useful. Commands accept an optional timeout, background commands have no timeout by default. Background commands return immediately, and you will be notified when they complete."

// zenToolDefinition 是一个闸门工具的完整定义。
type zenToolDefinition struct {
	Description string
	Parameters  map[string]any
}

// zenFingerprintDefs 是闸门工具的官方定义表。
// 每次调用都会深拷贝一份再返回，避免调用方改坏共享的 schema。
var zenFingerprintDefs = map[string]zenToolDefinition{
	"read": {
		Description: zenReadDescription,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "File or directory to read"},
				"offset": map[string]any{"type": "integer", "minimum": 0, "description": "The line or directory entry to start reading from (1-based)"},
				"limit":  map[string]any{"type": "integer", "minimum": 0, "description": "The maximum number of lines or directory entries to read (defaults to and capped at 2000)"},
			},
			"required":             []any{"path"},
			"additionalProperties": false,
		},
	},
	"shell": {
		Description: zenShellDescription,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":    map[string]any{"type": "string", "description": "Shell command string to execute"},
				"workdir":    map[string]any{"type": "string", "description": "Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead."},
				"timeout":    map[string]any{"type": "integer", "minimum": 0, "description": "Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands. Background commands have no timeout by default."},
				"background": map[string]any{"type": "boolean", "description": "Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use '&' at the end of the command when using this parameter. You will be notified when it completes. DO NOT poll for completion."},
			},
			"required":             []any{"command"},
			"additionalProperties": false,
		},
	},
}

const zenBase62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// zenSessionRe 是会话 ID 的形状。
var zenSessionRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

// zenSessionDescendingRe 是那 12 位 hex 的降序编码窗口。
//
// 上游会解码这个前缀，验证它解出一个新鲜的时间戳。counter 必须在 1..63，
// 于是低 12 位落在 0xfc0-0xfff（前 9 位任意，第 10 位 c-f，后两位任意）。
// sha256 摘要、随机 hex、升序 ts<<12 —— 即使 ses_ 形状完全正确，也一律 403。
var zenSessionDescendingRe = regexp.MustCompile(`^[0-9a-f]{9}f[c-f][0-9a-f]$`)

// zenSessionRemintAfter 是会话重铸周期。
//
// 上游容忍约一周的新鲜度窗口；六天后重铸留出余量，
// 这样从不重启的会话也不会滑出窗口。
const zenSessionRemintAfter = 6 * 24 * time.Hour

// zenSessionCounterMax 是上游接受的 counter 上界。
const zenSessionCounterMax = 63

var (
	zenLastStamp atomic.Int64
	zenSeq       atomic.Int64
)

func zenBase62From(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = zenBase62[int(c)%62]
	}
	return string(out)
}

// ZenMintSessionID 铸造一个上游接受的会话 ID。
//
// 编码是 ~(ts_ms << 12 | counter) 的降序形式：取反让低 12 位落在
// 0xfff - counter。计数器每毫秒归零，所以即使同一毫秒内突发铸造多次，
// 也不会跑出上游接受的 1..63 窗口。
//
// 参见 docs/design/01-zen-fingerprint.md §4。
func ZenMintSessionID(nowMS int64) string {
	if nowMS <= 0 {
		nowMS = time.Now().UnixMilli()
	}
	prev := zenLastStamp.Swap(nowMS)
	if prev != nowMS {
		zenSeq.Store(0)
	}
	seq := zenSeq.Add(1)
	if seq > zenSessionCounterMax {
		// 极端情况：同一毫秒内超过 63 次铸造。极罕见，但越界就是 403。
		// 退到下一个毫秒，宁可慢一点也不要被拒。
		nowMS++
		zenLastStamp.Store(nowMS)
		seq = 1
		zenSeq.Store(1)
	}

	value := ^(uint64(nowMS)<<12 | uint64(seq&0xFFF))
	var raw [6]byte
	for i := 0; i < 6; i++ {
		raw[i] = byte((value >> (40 - 8*i)) & 0xFF)
	}
	rnd := make([]byte, 14)
	if _, err := rand.Read(rnd); err != nil {
		// 随机源失败时也要给出一个形状正确的 id：随机位退化为时间派生，
		// 形状与编码仍然合法。
		for i := range rnd {
			rnd[i] = byte(nowMS >> (8 * (i % 8)))
		}
	}
	return "ses_" + hex.EncodeToString(raw[:]) + zenBase62From(rnd)
}

// ZenIsCanonicalSession 报告一个 id 是否已经是上游接受的编码。
// 下游若自带了一个"看起来对"但编码不对的 id，直接转发会 403。
func ZenIsCanonicalSession(id string) bool {
	id = strings.TrimSpace(id)
	if !zenSessionRe.MatchString(id) {
		return false
	}
	return zenSessionDescendingRe.MatchString(id[4:16])
}

// ZenHeaders 构造闸门要求的请求头。
//
// ⚠️ 四个会话头必须同时存在且同值，缺一即 403。
// ⚠️ 绝不发 x-opencode-request —— 那是 v1 专用头，v2 收到就 403。
func ZenHeaders(session string, stream bool) map[string]string {
	accept := "*/*"
	if stream {
		accept = "text/event-stream"
	}
	return map[string]string{
		"content-type":          "application/json",
		"authorization":         ZenCredential,
		"user-agent":            ZenClientUA,
		"x-opencode-client":     "cli",
		"x-opencode-session":    session,
		"x-opencode-session-id": session,
		"x-session-affinity":    session,
		"x-session-id":          session,
		"x-opencode-project":    "global",
		"accept":                accept,
	}
}

var (
	zenMuseSparkRe = regexp.MustCompile(`(?i)^muse[-_]?spark(?:$|[-_:.\s])`)
)

// ZenEndpointFor 返回该模型该走的端点。
//
// 免费车道在 /inference/v1 下；旧的 /zen/v1 对免费 id 一律 403。
// 注意：/zen/v1/models 与 /inference/v1/models 都还能拿到清单 ——
// "清单能取到"不代表"对话能通"。
//
// ⚠️ 端点是**按模型名**选的，不是按清单里的声明 ——
// 上游清单不声明任何 wire 信息（实测 2026-10-19：只有 id/object/created/owned_by）。
//
// 实测的线分配（2026-10-19，清单里的真实 id）：
//
//	muse-spark-1.2 / muse-spark-1.3  → responses 线
//	其余免费模型                        → chat 线
func ZenEndpointFor(modelID string) string {
	base := ZenBaseModelID(modelID)
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	switch {
	case zenMuseSparkRe.MatchString(base):
		return "/inference/v1/responses"
	case base == "union-alpha":
		return "/inference/v1/messages"
	default:
		return "/inference/v1/chat/completions"
	}
}

// ZenWireFor 返回该模型说的线上协议。
func ZenWireFor(modelID string) string {
	switch ZenEndpointFor(modelID) {
	case "/inference/v1/responses":
		return "responses"
	case "/inference/v1/messages":
		return "messages"
	default:
		return "chat"
	}
}

// zenEndpointForWire 按线名给出端点。
//
// ⚠️ **诊断用** —— 正常路径走 `ZenEndpointFor`（按模型名选）。
func zenEndpointForWire(wire string) string {
	switch wire {
	case "responses":
		return "/inference/v1/responses"
	case "messages":
		return "/inference/v1/messages"
	default:
		return "/inference/v1/chat/completions"
	}
}

// ZenBaseModelID 剥掉档位后缀，让查找命中基础 id。
func ZenBaseModelID(model string) string {
	s := strings.TrimSpace(model)
	if len(s) > 1 && s[len(s)-1] == ')' {
		for i := len(s) - 2; i >= 0; i-- {
			if s[i] == '(' {
				return strings.TrimSpace(s[:i])
			}
		}
	}
	return s
}

// ZenEffortOf 取出档位后缀（若模型用 "(level)" 的形式声明）。
func ZenEffortOf(model string) string {
	s := strings.TrimSpace(model)
	if len(s) > 1 && s[len(s)-1] == ')' {
		for i := len(s) - 2; i >= 0; i-- {
			if s[i] == '(' {
				inner := strings.ToLower(strings.TrimSpace(s[i+1 : len(s)-1]))
				switch inner {
				case "light", "balanced", "deep", "minimal", "low", "medium", "high", "xhigh", "max":
					return inner
				}
				return ""
			}
		}
	}
	return ""
}

// ZenWireStyle 描述当前请求体所处的线上协议形状。
type ZenWireStyle int

const (
	// ZenStyleChat 是 OpenAI Chat：工具包在 function 里，system 在 messages[0]。
	ZenStyleChat ZenWireStyle = iota
	// ZenStyleFlat 是 OpenAI Responses：工具是 flat 的，system 在 instructions。
	ZenStyleFlat
	// ZenStyleClaude 是 Anthropic Messages：工具用 input_schema，system 是独立字段。
	ZenStyleClaude
)

// zenToolName 读出一个工具声明的名字，兼容两种形状。
func zenToolName(tool map[string]any) string {
	if tool == nil {
		return ""
	}
	if n, ok := tool["name"].(string); ok && strings.TrimSpace(n) != "" {
		return strings.TrimSpace(n)
	}
	if fn, ok := tool["function"].(map[string]any); ok {
		if n, ok := fn["name"].(string); ok && strings.TrimSpace(n) != "" {
			return strings.TrimSpace(n)
		}
	}
	return ""
}

// zenFunctionOf 返回 Chat 形状的 function 包装。
func zenFunctionOf(tool map[string]any) map[string]any {
	fn, _ := tool["function"].(map[string]any)
	return fn
}

// zenFingerprintKey 返回工具名对应的闸门名；不是闸门名则返回 ""。
//
// ⚠️ 大小写变体（`Read` / `Bash`）**不算**闸门工具：上游接受它们与注入的
// `read`/`shell` 并存（实测）。按小写归并反而会把调用方的 `Read` 改成
// `read`，正好撞上"闸门只认小写精确名"的判定 → 403。
func zenFingerprintKey(name string) string {
	trimmed := strings.TrimSpace(name)
	for _, t := range zenFingerprintTools {
		if t == trimmed {
			return trimmed
		}
	}
	return ""
}

// zenDefinitionOf 返回一个工具的定义所在的 map。
func zenDefinitionOf(tool map[string]any, style ZenWireStyle) map[string]any {
	if style == ZenStyleClaude || style == ZenStyleFlat {
		return tool
	}
	return zenFunctionOf(tool)
}

// zenCarriesOfficialDefinition 报告这个工具是否已经带着官方的定义。
func zenCarriesOfficialDefinition(tool map[string]any, name string, style ZenWireStyle) bool {
	def := zenDefinitionOf(tool, style)
	if def == nil {
		return false
	}
	want, ok := zenFingerprintDefs[name]
	if !ok {
		return false
	}
	got, _ := def["description"].(string)
	if got != want.Description {
		return false
	}
	return zenJSONEqual(def["parameters"], want.Parameters)
}

// zenJSONEqual 比较两个任意值的 JSON 序列化结果。
func zenJSONEqual(a, b any) bool {
	return zenMarshal(a) == zenMarshal(b)
}

// zenMarshal 是 encoding/json.Marshal 的薄包装。
// 参数 schema 里可能含 map[string]any，序列化顺序由 Go 的 map 排序保证稳定，
// 所以逐字比较是可靠的。
func zenMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// zenWithToolName 克隆一个工具并换掉它的名字。
func zenWithToolName(tool map[string]any, name string) map[string]any {
	if fn := zenFunctionOf(tool); fn != nil {
		clone := zenCloneMap(tool)
		inner := zenCloneMap(fn)
		inner["name"] = name
		clone["function"] = inner
		return clone
	}
	clone := zenCloneMap(tool)
	clone["name"] = name
	return clone
}

// zenCloneMap 浅拷贝一个 map。
func zenCloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// zenOfficialTool 生成闸门工具在该 wire 形状下的声明。
func zenOfficialTool(name string, style ZenWireStyle) map[string]any {
	def := zenFingerprintDefs[name]
	switch style {
	case ZenStyleClaude:
		return map[string]any{
			"name":         name,
			"description":  def.Description,
			"input_schema": def.Parameters,
		}
	case ZenStyleFlat:
		return map[string]any{
			"type":        "function",
			"name":        name,
			"description": def.Description,
			"parameters":  def.Parameters,
			"strict":      false,
		}
	default:
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": def.Description,
				"parameters":  def.Parameters,
			},
		}
	}
}

// zenPlaceholderNote 是注入占位工具后必须附带的提示。
//
// ⚠️ v2 注入的是**真工具定义**（v1 的"must not be used"桩描述已失效），
// 模型会真的去调它 —— 实测拿 read 回答"看看 /etc 里有什么文件"。
// 不加提示，模型就真的会调。
//
// ⚠️ 措辞有讲究：写成"不可用"会让模型连**调用方自己的** Read/Bash 也一起
// 拒绝；"占位符、请用别的工具"才既压住注入项、又不误伤调用方的工具。
func zenPlaceholderNote(names []string) string {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "`" + n + "`"
	}
	if len(names) == 1 {
		return "Do not call the " + quoted[0] +
			" tool; it is a placeholder that does nothing. Use the other tools you have been given instead."
	}
	return "Do not call the " + strings.Join(quoted, " or ") +
		" tools; they are placeholders that do nothing. Use the other tools you have been given instead."
}

// ZenFingerprintResult 是一次指纹处理的结果。
type ZenFingerprintResult struct {
	// RenameMap 是"发出去的名字 → 调用方的名字"。
	// 响应侧用它把名字还原回去。
	RenameMap map[string]string
	// Injected 列出本次注入的占位工具名。
	Injected []string
}

// ZenApplyFingerprint 让请求体满足闸门。
//
// 处理顺序（每一步都对应一次实测到的 403）：
//
//  1. 调用方的工具原样保留（含 Read/Bash 这类大小写变体）；
//  2. 名字正好是 read/shell 但定义与官方不同的，改名为 <name>_client[N]；
//  3. 同名重复声明只留第一个（上游按重复拒）；
//  4. 注入缺失的官方定义；
//  5. 同步改写历史里的 tool_call 名与 tool_choice 的指向；
//  6. 追加占位提示；
//  7. 绝不下发 tool_choice:"none"（那会让模型这一轮不调工具）。
//
// body 会被就地修改。
func ZenApplyFingerprint(body map[string]any, style ZenWireStyle) ZenFingerprintResult {
	result := ZenFingerprintResult{RenameMap: map[string]string{}}

	tools := zenToolsOf(body["tools"])
	taken := map[string]bool{}
	for _, t := range tools {
		if n := zenToolName(t); n != "" {
			taken[n] = true
		}
	}

	// seen 记录已经出现过的**闸门名** —— 上游按重复拒，同名只留第一个。
	// present 记录**真正带着官方定义留下来**的闸门名 ——
	// 注入判据必须看后者，否则"调用方的 read 被改成 read_client"
	// 会被误判成"read 已经声明"，官方定义反而不会补上（闸门仍然 403）。
	//
	// ⚠️ 非闸门工具（调用方自己的）**不去重**：它们的重复声明是调用方的事，
	//   网关原样透传。
	seen := map[string]bool{}
	present := map[string]bool{}
	out := make([]any, 0, len(tools)+len(zenFingerprintTools))

	for _, tool := range tools {
		name := zenToolName(tool)
		key := zenFingerprintKey(name)
		if key == "" {
			out = append(out, tool)
			continue
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		if zenCarriesOfficialDefinition(tool, key, style) {
			out = append(out, tool)
			present[key] = true
			continue
		}
		// 冲突工具改名，让出闸门名。
		alias := key + "_client"
		for i := 2; taken[alias]; i++ {
			alias = key + "_client" + strconv.Itoa(i)
		}
		taken[alias] = true
		result.RenameMap[alias] = name
		out = append(out, zenWithToolName(tool, alias))
	}

	for _, name := range zenFingerprintTools {
		if present[name] {
			continue
		}
		out = append(out, zenOfficialTool(name, style))
		result.Injected = append(result.Injected, name)
	}

	body["tools"] = out

	zenRetargetToolChoice(body, result.RenameMap)
	zenAliasHistoryToolCalls(body, result.RenameMap)
	if len(result.Injected) > 0 {
		zenNoteInjectedTools(body, style, result.Injected)
	}

	// ⚠️ **兜底清除 `tool_choice: "none"`**。
	//
	// ⚠️ **实测（2026-10-22）：上游并不拒绝 `none`** ——
	//   直接打上游，`tool_choice: "none"` 照样返回 200。
	//   所以这段**不是**为了过闸门，是**为了正确性**：
	//
	//	调用方（尤其是断流续写路径）可能留下 `none`，
	//	那会让模型**这一轮不调工具** —— 而我们明明把工具给它了。
	//	压制"这轮别调工具"的正确做法是在续写指令正文里写
	//	"Do not call tools"，而不是设 `tool_choice: "none"`。
	switch choice := body["tool_choice"].(type) {
	case string:
		if choice == "none" {
			delete(body, "tool_choice")
		}
	case map[string]any:
		if t, _ := choice["type"].(string); t == "none" {
			delete(body, "tool_choice")
		}
	}

	// Responses 线官方行为是补 auto；其他线不发。
	if style == ZenStyleFlat {
		if _, has := body["tool_choice"]; !has {
			body["tool_choice"] = "auto"
		}
	}

	return result
}

// zenToolsOf 归一化 body.tools，兼容 []any 与 []map[string]any。
func zenToolsOf(v any) []map[string]any {
	var out []map[string]any
	switch t := v.(type) {
	case []any:
		for _, item := range t {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
	case []map[string]any:
		out = append(out, t...)
	}
	return out
}

// zenRetargetToolChoice 把显式指定的 tool_choice 指向改名后的别名。
func zenRetargetToolChoice(body map[string]any, rename map[string]string) {
	if len(rename) == 0 {
		return
	}
	aliases := map[string]string{}
	for alias, orig := range rename {
		aliases[orig] = alias
	}
	choice, ok := body["tool_choice"].(map[string]any)
	if !ok {
		return
	}
	if n, ok := choice["name"].(string); ok {
		if alias, hit := aliases[strings.TrimSpace(n)]; hit {
			choice["name"] = alias
		}
		return
	}
	if fn, ok := choice["function"].(map[string]any); ok {
		if n, ok := fn["name"].(string); ok {
			if alias, hit := aliases[strings.TrimSpace(n)]; hit {
				fn["name"] = alias
			}
		}
	}
}

// zenAliasHistoryToolCalls 改写历史里的 tool_call 名。
//
// 不改的话，模型会把自己上一轮的 read 误认为注入的那个，
// 而两个定义并不一致 —— 于是它会困惑或重复调用。
func zenAliasHistoryToolCalls(body map[string]any, rename map[string]string) {
	if len(rename) == 0 {
		return
	}
	aliases := map[string]string{}
	for alias, orig := range rename {
		aliases[orig] = alias
	}
	renameName := func(name string) string {
		if a, ok := aliases[name]; ok {
			return a
		}
		return name
	}

	// Responses 线的 input 数组
	if items, ok := body["input"].([]any); ok {
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if t, _ := m["type"].(string); t == "function_call" {
				if n, ok := m["name"].(string); ok {
					m["name"] = renameName(n)
				}
			}
		}
		body["input"] = items
	}

	// Chat 线与 Claude 线的 messages
	msgs, ok := body["messages"].([]any)
	if !ok {
		return
	}
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		// Claude 线的 tool_use 在 content 块里
		if content, ok := msg["content"].([]any); ok {
			for _, cc := range content {
				blk, ok := cc.(map[string]any)
				if !ok {
					continue
				}
				if t, _ := blk["type"].(string); t == "tool_use" {
					if n, ok := blk["name"].(string); ok {
						blk["name"] = renameName(n)
					}
				}
			}
			msg["content"] = content
		}
		// Chat 线的 tool_calls 在 message.tool_calls
		if calls, ok := msg["tool_calls"].([]any); ok {
			for _, cc := range calls {
				call, ok := cc.(map[string]any)
				if !ok {
					continue
				}
				if fn, ok := call["function"].(map[string]any); ok {
					if n, ok := fn["name"].(string); ok {
						fn["name"] = renameName(n)
					}
				}
			}
		}
	}
	body["messages"] = msgs
}

// zenNoteInjectedTools 把占位提示塞进该 wire 的系统提示位置。
func zenNoteInjectedTools(body map[string]any, style ZenWireStyle, names []string) {
	note := zenPlaceholderNote(names)
	switch style {
	case ZenStyleFlat:
		if s, ok := body["instructions"].(string); ok && strings.TrimSpace(s) != "" {
			body["instructions"] = s + "\n\n" + note
		} else {
			body["instructions"] = note
		}
	case ZenStyleClaude:
		if s, ok := body["system"].(string); ok && s != "" {
			body["system"] = s + "\n\n" + note
		} else {
			body["system"] = note
		}
	default:
		msgs, ok := body["messages"].([]any)
		if !ok || len(msgs) == 0 {
			// 没有消息可挂 —— 单独建一条 system 消息。
			// ⚠️ 不能就这么返回：没有提示，模型就会真的去调注入的占位工具。
			body["messages"] = []any{map[string]any{"role": "system", "content": note}}
			return
		}
		if body["system"] != nil {
			// 上游已经有一个独立的 system 字段，不重复发送。
			return
		}
		if first, ok := msgs[0].(map[string]any); ok {
			if r, _ := first["role"].(string); r == "system" {
				if c, ok := first["content"].(string); ok {
					clone := zenCloneMap(first)
					clone["content"] = c + "\n\n" + note
					body["messages"] = append([]any{clone}, msgs[1:]...)
					return
				}
			}
		}
		body["messages"] = append([]any{map[string]any{"role": "system", "content": note}}, msgs...)
	}
}

// ZenRestoreToolName 把发出去的名字还原成调用方的名字。
func ZenRestoreToolName(name string, rename map[string]string) string {
	if orig, ok := rename[name]; ok {
		return orig
	}
	return name
}

// ZenDeclaredToolNames 列出一个请求体声明的工具名。
func ZenDeclaredToolNames(body map[string]any) map[string]bool {
	names := map[string]bool{}
	for _, m := range zenToolsOf(body["tools"]) {
		if n := zenToolName(m); n != "" {
			names[n] = true
		}
	}
	return names
}
