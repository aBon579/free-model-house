package lane

import (
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
)

// 这些测试锁住上游闸门的规格。
// 上游改规格时，**先改 zen_fingerprint.go，再改这里的期望值** ——
// 不要为了让测试变绿而放松断言。断言放松的那一刻，就是闸门静默失守的那一刻。
// 详见 docs/design/01-zen-fingerprint.md。

func TestZenClientUAShape(t *testing.T) {
	// 形状：opencode/<channel>/<version>/<client>
	parts := strings.Split(ZenClientUA, "/")
	if len(parts) != 4 {
		t.Fatalf("UA 形状不对: %q", ZenClientUA)
	}
	if parts[0] != "opencode" {
		t.Fatalf("UA 第一段必须是 opencode: %q", ZenClientUA)
	}
	major := parts[2]
	if !strings.HasPrefix(major, "2") && !strings.HasPrefix(major, "3") {
		t.Fatalf("UA 的 major 必须 ≥ 2（v1 已被上游按名字拒掉）: %q", ZenClientUA)
	}
}

func TestZenClientUAHasNoAffix(t *testing.T) {
	// 整串匹配：任何前后缀都会让它不再是"一个 v2 客户端"。
	if strings.Contains(ZenClientUA, " ") || strings.Contains(ZenClientUA, "(") {
		t.Fatalf("UA 不得带前后缀（v2 是整串匹配）: %q", ZenClientUA)
	}
	// 归属 token 会直接触发 403。
	for _, banned := range []string{"deepseek-harness", "free-model-house", "fmh"} {
		if strings.Contains(strings.ToLower(ZenClientUA), banned) {
			t.Fatalf("UA 不得包含 %q: %q", banned, ZenClientUA)
		}
	}
}

// 会话头

func TestZenHeadersHaveFourSessionIDs(t *testing.T) {
	h := ZenHeaders("ses_abc", true)
	session := "ses_abc"
	for _, name := range []string{
		"x-opencode-session",
		"x-opencode-session-id",
		"x-session-affinity",
		"x-session-id",
	} {
		got, ok := h[name]
		if !ok {
			t.Fatalf("缺少会话头 %q（缺一即 403）", name)
		}
		if got != session {
			t.Fatalf("会话头 %q 的值必须是 %q，实际 %q", name, session, got)
		}
	}
}

func TestZenHeadersNoV1RequestHeader(t *testing.T) {
	// x-opencode-request 是 v1 专用头，v2 收到就 403。
	h := ZenHeaders("ses_abc", false)
	if _, ok := h["x-opencode-request"]; ok {
		t.Fatal("绝不能发 x-opencode-request（v1 专用头，v2 收到即 403）")
	}
}

func TestZenHeadersCredential(t *testing.T) {
	h := ZenHeaders("ses_abc", false)
	if h["authorization"] != ZenCredential {
		t.Fatalf("免密车道的凭据必须是 %q，实际 %q", ZenCredential, h["authorization"])
	}
	// 流式与非流式的 Accept 不同。
	if ZenHeaders("ses_abc", true)["accept"] != "text/event-stream" {
		t.Fatal("流式请求的 accept 必须是 text/event-stream")
	}
	if ZenHeaders("ses_abc", false)["accept"] != "*/*" {
		t.Fatal("非流式请求的 accept 必须是 */*")
	}
}

// 会话 ID

func TestZenSessionIDShape(t *testing.T) {
	id := ZenMintSessionID(1760000000000)
	if !zenSessionRe.MatchString(id) {
		t.Fatalf("会话 ID 形状不对: %q", id)
	}
}

func TestZenSessionIDDescendingEncoding(t *testing.T) {
	// 那 12 位 hex 必须落在降序窗口内。
	id := ZenMintSessionID(1760000000000)
	part := id[4:16]
	if !zenSessionDescendingRe.MatchString(part) {
		t.Fatalf("会话 ID 的时间部分必须是降序编码（counter 1..63）: %q", part)
	}
}

func TestZenSessionIDCounterWindow(t *testing.T) {
	// 同一毫秒内连续铸造，必须都落在窗口内。
	now := int64(1760000000000)
	for i := 0; i < 200; i++ {
		id := ZenMintSessionID(now)
		if !zenSessionDescendingRe.MatchString(id[4:16]) {
			t.Fatalf("第 %d 次铸造越出了窗口: %q", i, id[4:16])
		}
	}
}

func TestZenSessionIsCanonical(t *testing.T) {
	id := ZenMintSessionID(1760000000000)
	if !ZenIsCanonicalSession(id) {
		t.Fatalf("自己铸造的 id 必须被判定为 canonical: %q", id)
	}
	// 形状对但编码错的，必须被拒。
	bad := "ses_" + strings.Repeat("a", 12) + strings.Repeat("b", 14)
	if ZenIsCanonicalSession(bad) {
		t.Fatal("升序/随机的会话 ID 必须被判定为非 canonical")
	}
	if ZenIsCanonicalSession("not-a-session") {
		t.Fatal("垃圾输入必须被判定为非 canonical")
	}
}

func TestZenSessionIDsDiffer(t *testing.T) {
	// 不同会话必须拿到不同的 id。
	now := int64(1760000000000)
	a := ZenMintSessionID(now)
	b := ZenMintSessionID(now)
	if a == b {
		t.Fatal("两次铸造不得产生相同的 ID")
	}
}

// 工具指纹

func TestZenFingerprintDefinitionsUnchanged(t *testing.T) {
	// 这两个 description 与官方 CLI 逐字一致。
	// ⚠️ 实测上游**不**比对工具描述（乱写也 200）。
	// 这条回归测试锁的是"**别顺手改这些字面量**" —— 官方原文最稳。
	// 这个测试存在的意义就是：有人"顺手优化措辞"时 CI 会红。
	if !strings.HasPrefix(zenReadDescription, "Read the contents of a file or directory.") {
		t.Fatal("read 工具的 description 被改动了 —— 官方原文，不要随手改")
	}
	if !strings.HasPrefix(zenShellDescription, "Execute a shell command and return its output.") {
		t.Fatal("shell 工具的 description 被改动了 —— 官方原文，不要随手改")
	}
	// 参数 schema 的关键约束。
	read := zenFingerprintDefs["read"].Parameters
	if read["type"] != "object" {
		t.Fatal("read 的 parameters.type 必须是 object")
	}
	props, ok := read["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		t.Fatal("read 的 properties 不能为空（v1 时代的空 schema 已被拒）")
	}
	if read["additionalProperties"] != false {
		t.Fatal("read 的 additionalProperties 必须是 false")
	}
	shell := zenFingerprintDefs["shell"].Parameters
	if shell["additionalProperties"] != false {
		t.Fatal("shell 的 additionalProperties 必须是 false")
	}
}

func TestZenFingerprintInjectsMissingTools(t *testing.T) {
	body := map[string]any{"messages": []any{}}
	res := ZenApplyFingerprint(body, ZenStyleChat)
	if len(res.Injected) != 2 {
		t.Fatalf("应当注入 read 和 shell 两个工具，实际注入了 %v", res.Injected)
	}
	tools := zenToolsOf(body["tools"])
	if len(tools) != 2 {
		t.Fatalf("注入后应有 2 个工具，实际 %d", len(tools))
	}
	for _, tool := range tools {
		if zenFingerprintKey(zenToolName(tool)) == "" {
			t.Fatalf("注入的工具名不对: %q", zenToolName(tool))
		}
	}
}

func TestZenFingerprintKeepsCallerTools(t *testing.T) {
	// 调用方自己的工具必须原样保留。
	caller := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "my_tool",
			"description": "调用方自己的工具",
			"parameters":  map[string]any{"type": "object"},
		},
	}
	body := map[string]any{"tools": []any{caller}, "messages": []any{}}
	ZenApplyFingerprint(body, ZenStyleChat)

	tools := zenToolsOf(body["tools"])
	found := false
	for _, tool := range tools {
		if zenToolName(tool) == "my_tool" {
			found = true
		}
	}
	if !found {
		t.Fatal("调用方自己的工具被删掉了")
	}
}

func TestZenFingerprintCaseVariantPassesThrough(t *testing.T) {
	// `Read` / `Bash` 不是闸门工具，必须原样透传。
	// 按小写归并会把它变成 `read`，正好撞上"闸门只认小写精确名"的判定 → 403。
	caller := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "Read",
			"description": "调用方自己的 Read",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"p": map[string]any{"type": "string"}}},
		},
	}
	body := map[string]any{"tools": []any{caller}, "messages": []any{}}
	res := ZenApplyFingerprint(body, ZenStyleChat)

	if len(res.RenameMap) != 0 {
		t.Fatalf("大小写变体不得被改名: %v", res.RenameMap)
	}
	tools := zenToolsOf(body["tools"])
	found := false
	for _, tool := range tools {
		if zenToolName(tool) == "Read" {
			found = true
		}
	}
	if !found {
		t.Fatal("调用方的 Read 被改掉了")
	}
	// 官方定义仍然要补上。
	if len(res.Injected) != 2 {
		t.Fatalf("read/shell 仍需注入: %v", res.Injected)
	}
}

func TestZenFingerprintAliasesConflictingTool(t *testing.T) {
	// 调用方正好用了 `read` 但定义不同 → 必须改名，让出闸门名。
	conflict := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "read",
			"description": "调用方自己的 read，定义不同",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
		},
	}
	body := map[string]any{"tools": []any{conflict}, "messages": []any{}}
	res := ZenApplyFingerprint(body, ZenStyleChat)

	alias, ok := res.RenameMap["read_client"]
	if !ok {
		t.Fatalf("冲突工具应被改名为 read_client，实际 %v", res.RenameMap)
	}
	if alias != "read" {
		t.Fatalf("renameMap 方向错了: %q", alias)
	}
	// 官方定义必须仍然被注入。
	if len(res.Injected) != 2 {
		t.Fatalf("官方 read/shell 仍需注入: %v", res.Injected)
	}
	// 注入的 read 必须是官方那份。
	tools := zenToolsOf(body["tools"])
	for _, tool := range tools {
		if zenToolName(tool) == "read" && !zenCarriesOfficialDefinition(tool, "read", ZenStyleChat) {
			t.Fatal("注入的 read 必须是官方定义")
		}
	}
}

func TestZenFingerprintRestoresName(t *testing.T) {
	rename := map[string]string{"read_client": "read"}
	if got := ZenRestoreToolName("read_client", rename); got != "read" {
		t.Fatalf("响应侧应还原成 read，实际 %q", got)
	}
	if got := ZenRestoreToolName("other", rename); got != "other" {
		t.Fatalf("未改名的工具应原样返回，实际 %q", got)
	}
}

func TestZenFingerprintAliasesHistoryToolCalls(t *testing.T) {
	// 历史里的 tool_call 必须一起改名，
	// 否则模型会把自己上一轮的 read 误认为注入的那个。
	conflict := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "read",
			"description": "调用方自己的 read",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
		},
	}
	body := map[string]any{
		"tools": []any{conflict},
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"tool_calls": []any{
					map[string]any{
						"id":       "call_1",
						"function": map[string]any{"name": "read", "arguments": "{}"},
					},
				},
			},
		},
	}
	ZenApplyFingerprint(body, ZenStyleChat)

	// ⚠️ 占位提示会作为一条 system 消息**前置**到 messages 里，
	// 所以不能假设 index 0 就是原来那条 assistant 消息。
	// 按内容找带 tool_calls 的那条。
	msgs, _ := body["messages"].([]any)
	var found bool
	for _, mm := range msgs {
		msg, ok := mm.(map[string]any)
		if !ok {
			continue
		}
		calls, ok := msg["tool_calls"].([]any)
		if !ok || len(calls) == 0 {
			continue
		}
		call, _ := calls[0].(map[string]any)
		fn, _ := call["function"].(map[string]any)
		if fn["name"] != "read_client" {
			t.Fatalf("历史里的 tool_call 必须一起改名，实际 %v", fn["name"])
		}
		found = true
	}
	if !found {
		t.Fatal("历史里的 tool_call 消息不见了")
	}
}

func TestZenFingerprintRetargetsToolChoice(t *testing.T) {
	conflict := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        "read",
			"description": "调用方自己的 read",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
		},
	}
	body := map[string]any{
		"tools":       []any{conflict},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "read"}},
		"messages":    []any{},
	}
	ZenApplyFingerprint(body, ZenStyleChat)

	choice, _ := body["tool_choice"].(map[string]any)
	fn, _ := choice["function"].(map[string]any)
	if fn["name"] != "read_client" {
		t.Fatalf("tool_choice 必须指向别名，实际 %v", fn["name"])
	}
}

func TestZenFingerprintKeepsFirstDuplicate(t *testing.T) {
	// 上游按重复拒。同名重复声明只留第一个。
	// ⚠️ 只针对**闸门名**（read/shell）。调用方自己的工具重复声明是它的事，
	//   网关原样透传。
	dup := func(name string) map[string]any {
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": "重复声明",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}},
			},
		}
	}
	body := map[string]any{"tools": []any{dup("read"), dup("read")}, "messages": []any{}}
	res := ZenApplyFingerprint(body, ZenStyleChat)

	// 第一个被改成别名，第二个被丢弃（同名重复）。
	aliases := 0
	tools := zenToolsOf(body["tools"])
	for _, tool := range tools {
		switch zenToolName(tool) {
		case "read_client":
			aliases++
		case "read":
			// 只能是官方那一份
			if !zenCarriesOfficialDefinition(tool, "read", ZenStyleChat) {
				t.Fatal("留下的 read 必须是官方定义")
			}
		}
	}
	if aliases != 1 {
		t.Fatalf("同名重复的闸门工具应只处理一次，实际别名数 %d", aliases)
	}
	// 官方定义仍需注入。
	if len(res.Injected) != 2 {
		t.Fatalf("官方 read/shell 仍需注入: %v", res.Injected)
	}
}

func TestZenFingerprintAddsPlaceholderNote(t *testing.T) {
	// 注入的是真工具定义，不提示模型，模型就会真的去调。
	body := map[string]any{"messages": []any{}}
	ZenApplyFingerprint(body, ZenStyleChat)

	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("必须追加一条系统提示")
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("追加的必须是 system 消息，实际 %v", first["role"])
	}
	text, _ := first["content"].(string)
	if !strings.Contains(text, "placeholder") {
		t.Fatalf("系统提示必须说明这是占位符: %q", text)
	}
	// 措辞不能是"不可用" —— 那会让模型连调用方自己的工具也一起拒绝。
	if strings.Contains(text, "unavailable") || strings.Contains(text, "not available") {
		t.Fatalf("提示措辞不能是'不可用'（会误伤调用方自己的工具）: %q", text)
	}
}

func TestZenFingerprintNeverEmitsToolChoiceNone(t *testing.T) {
	// 兜底清除 tool_choice:"none"。
	body := map[string]any{"tool_choice": "none", "messages": []any{}}
	ZenApplyFingerprint(body, ZenStyleChat)
	if _, has := body["tool_choice"]; has {
		t.Fatal("绝不能下发 tool_choice:\"none\"")
	}

	body2 := map[string]any{"tool_choice": map[string]any{"type": "none"}, "messages": []any{}}
	ZenApplyFingerprint(body2, ZenStyleChat)
	if _, has := body2["tool_choice"]; has {
		t.Fatal("绝不能下发 tool_choice:{type:\"none\"}")
	}
}

func TestZenResponsesGetsAutoChoice(t *testing.T) {
	// Responses 线官方行为是补 auto；Chat 线不补。
	body := map[string]any{"messages": []any{}}
	ZenApplyFingerprint(body, ZenStyleFlat)
	if body["tool_choice"] != "auto" {
		t.Fatalf("Responses 线必须补 tool_choice:auto，实际 %v", body["tool_choice"])
	}

	body2 := map[string]any{"messages": []any{}}
	ZenApplyFingerprint(body2, ZenStyleChat)
	if _, has := body2["tool_choice"]; has {
		t.Fatal("Chat 线不得补 tool_choice")
	}
}

func TestZenFingerprintShapesPerWire(t *testing.T) {
	// 三种 wire 的工具形状不同。
	chat := map[string]any{"messages": []any{}}
	ZenApplyFingerprint(chat, ZenStyleChat)
	tool := zenToolsOf(chat["tools"])[0]
	if zenFunctionOf(tool) == nil {
		t.Fatal("Chat 线的工具必须包在 function 里")
	}

	flat := map[string]any{"messages": []any{}}
	ZenApplyFingerprint(flat, ZenStyleFlat)
	tool = zenToolsOf(flat["tools"])[0]
	if tool["strict"] != false {
		t.Fatal("Responses 线的工具必须带 strict:false")
	}

	claude := map[string]any{"messages": []any{}}
	ZenApplyFingerprint(claude, ZenStyleClaude)
	tool = zenToolsOf(claude["tools"])[0]
	if tool["input_schema"] == nil {
		t.Fatal("Claude 线的工具必须用 input_schema")
	}
}

// 端点选择

func TestZenEndpointSelection(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"deepseek-v4.1-flash-free", "/inference/v1/chat/completions"},
		{"muse-spark-1.3-contributor-free", "/inference/v1/responses"},
		{"union-alpha", "/inference/v1/messages"},
		{"muse_spark_1_2", "/inference/v1/responses"},
	}
	for _, c := range cases {
		if got := ZenEndpointFor(c.model); got != c.want {
			t.Errorf("%s → %s，期望 %s", c.model, got, c.want)
		}
	}
}

func TestZenWireSelection(t *testing.T) {
	if ZenWireFor("muse-spark-1.3-contributor-free") != "responses" {
		t.Error("muse-spark 走 responses 线")
	}
	if ZenWireFor("union-alpha") != "messages" {
		t.Error("union-alpha 走 messages 线")
	}
	if ZenWireFor("deepseek-v4.1-flash-free") != "chat" {
		t.Error("默认走 chat 线")
	}
}

func TestZenBaseModelIDStripsEffort(t *testing.T) {
	if got := ZenBaseModelID("deepseek-v4.1-flash-free (deep)"); got != "deepseek-v4.1-flash-free" {
		t.Fatalf("档位后缀应被剥掉，实际 %q", got)
	}
	if got := ZenBaseModelID("deepseek-v4.1-flash-free"); got != "deepseek-v4.1-flash-free" {
		t.Fatalf("无后缀时不应改动，实际 %q", got)
	}
}

// 一个"整体形状"的冒烟测试

func TestZenEndToEndShape(t *testing.T) {
	// 模拟一个真实请求：调用方有 read（定义不同）+ 一个自己的工具。
	body := map[string]any{
		"model":    "deepseek-v4.1-flash-free",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name": "read", "description": "调用方的",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"p": map[string]any{"type": "string"}}},
			}},
			map[string]any{"type": "function", "function": map[string]any{
				"name": "grep", "description": "调用方的",
				"parameters": map[string]any{"type": "object", "properties": map[string]any{"p": map[string]any{"type": "string"}}},
			}},
		},
	}

	session := ZenMintSessionID(0)
	if !ZenIsCanonicalSession(session) {
		t.Fatalf("铸造的会话 ID 不合法: %q", session)
	}
	headers := ZenHeaders(session, true)
	if len(headers) < 8 {
		t.Fatal("请求头不完整")
	}

	res := ZenApplyFingerprint(body, ZenStyleChat)
	if len(res.RenameMap) != 1 {
		t.Fatalf("应当只改名一个工具，实际 %v", res.RenameMap)
	}
	if len(res.Injected) != 2 {
		t.Fatalf("应当注入两个官方工具，实际 %v", res.Injected)
	}
	// 结果里必须有 read、shell、grep、read_client 四个工具。
	names := map[string]bool{}
	for _, tool := range zenToolsOf(body["tools"]) {
		names[zenToolName(tool)] = true
	}
	for _, want := range []string{"read", "shell", "grep", "read_client"} {
		if !names[want] {
			t.Fatalf("工具集合缺少 %q，实际 %v", want, names)
		}
	}
}

func TestZenSessionDescendingRegexMatchesSpec(t *testing.T) {
	// 窗口的算术：
	//   low12 = (~counter) & 0xFFF
	//   counter=1  → 0xffe   （接受）
	//   counter=63 → 0xfc0   （接受）
	//   counter=64 → 0xfbf   （越界）
	//   counter=100→ 0xf9b   （越界）
	// 12 位 hex 的**最后 3 位**就是 low12，
	// 所以模式是 9 位任意 + 'f' + [c-f] + [0-9a-f]。
	re := regexp.MustCompile(`^[0-9a-f]{9}f[c-f][0-9a-f]$`)

	if !re.MatchString("012345678ffe") { // counter=1
		t.Error("counter=1 的时间部分应被接受")
	}
	if !re.MatchString("012345678fc0") { // counter=63
		t.Error("counter=63 的时间部分应被接受")
	}
	if re.MatchString("012345678fbf") { // counter=64
		t.Error("counter=64 越界，不应被接受")
	}
	if re.MatchString("012345678f9b") { // counter=100
		t.Error("counter=100 越界，不应被接受")
	}
	// 升序写法（~(ms<<12) 少了 counter 的那一位）落在 0x000-0xfff，不在窗口内。
	if re.MatchString("012345678000") {
		t.Error("升序时间戳不应被接受")
	}
}

func TestZenMintSessionIDHexIsTwelveDigits(t *testing.T) {
	id := ZenMintSessionID(1760000000000)
	part := strings.TrimPrefix(id, "ses_")
	raw, err := hex.DecodeString(part[:12])
	if err != nil {
		t.Fatalf("时间部分必须是合法 hex: %v", err)
	}
	if len(raw) != 6 {
		t.Fatalf("时间部分必须是 6 字节，实际 %d", len(raw))
	}
}
