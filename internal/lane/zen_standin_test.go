package lane

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 这是一个**本地替身上游**（stand-in），用来在没有真实网络的情况下
// 验证整条链路：指纹 → 请求体 → 流 → 统一事件。
//
// ⚠️ 替身**不能**证明上游会接受我们的指纹 —— 那只能靠真实上游证明。
// 替身能证明的是：我们的请求**长得对**，解析器**读得对**。
//
// 真实的验证在 zen_live_test.go（需要网络，默认跳过）。

// standIn 是一个模拟 OpenCode Zen 的测试服务器。
type standIn struct {
	// gotRequest 记录收到的请求。
	gotRequest *http.Request
	gotBody    map[string]any
	status     int
	// errorBody 是错误时要返回的 JSON 信封。
	errorBody string
	// jsonBody 是成功时要返回的 JSON（不设则走 SSE）。
	jsonBody string
	// frames 是要返回的 SSE 帧。
	frames []string
	// header 是额外的响应头。
	header map[string]string
	// contentType 是响应的 Content-Type。
	contentType string
}

// ServeHTTP 实现 http.Handler。
func (s *standIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.gotRequest = r
	raw, _ := readAll(r)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &s.gotBody)
	}

	if s.status != 0 && (s.status < 200 || s.status >= 300) {
		body := s.errorBody
		if body == "" {
			body = `{"error":{"type":"server_error","message":"upstream said no"}}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(body))
		return
	}
	ct := s.contentType
	if s.jsonBody != "" {
		ct = "application/json"
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(s.jsonBody))
		return
	}
	if ct == "" {
		ct = "text/event-stream"
	}
	w.Header().Set("Content-Type", ct)
	for k, v := range s.header {
		w.Header().Set(k, v)
	}
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, f := range s.frames {
		_, _ = fmt.Fprintf(w, "data: %s\n\n", f)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// readAll 读一个请求体。
func readAll(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	defer r.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			return buf, nil
		}
	}
}

// newStandInServer 起一个替身上游，并把 lane 指过去。
func newStandInServer(t *testing.T, s *standIn) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

// 请求体形状

func TestZenRequestCarriesFingerprint(t *testing.T) {
	s := &standIn{
		frames: []string{
			`{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
			`{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`,
		},
	}
	srv := newStandInServer(t, s)

	lane := NewZenLane(NewSessionMap())
	lane.base = srv.URL
	// 端点路径是绝对路径，替身服务器会接受任何路径。
	turn := &Turn{
		Model: "deepseek-v4.1-flash-free",
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{TextPart("hello")}},
		},
		Seed: "test-seed",
	}

	var chunks []Chunk
	usage, finish, uerr := lane.Do(context.Background(), turn, func(c Chunk) {
		chunks = append(chunks, c)
	})
	if uerr != nil {
		t.Fatalf("不该有错误: %v", uerr)
	}
	if finish != FinishStop {
		t.Fatalf("结束原因不对: %v", finish)
	}
	if usage == nil || usage.Input != 10 {
		t.Fatalf("用量不对: %+v", usage)
	}

	// ── 指纹：UA ──
	if got := s.gotRequest.Header.Get("User-Agent"); got != ZenClientUA {
		t.Fatalf("UA 不对: %q", got)
	}
	// ── 指纹：凭据 ──
	if got := s.gotRequest.Header.Get("Authorization"); got != ZenCredential {
		t.Fatalf("凭据不对: %q", got)
	}
	// ── 指纹：四个会话头 ──
	session := s.gotRequest.Header.Get("x-opencode-session")
	if session == "" {
		t.Fatal("缺少 x-opencode-session")
	}
	for _, h := range []string{"x-opencode-session-id", "x-session-affinity", "x-session-id"} {
		if got := s.gotRequest.Header.Get(h); got != session {
			t.Fatalf("%s 的值应为 %q，实际 %q", h, session, got)
		}
	}
	// ── 指纹：会话 id 的编码 ──
	if !ZenIsCanonicalSession(session) {
		t.Fatalf("会话 id 编码不合法: %q", session)
	}
	// ── 指纹：不能有 v1 的头 ──
	if s.gotRequest.Header.Get("x-opencode-request") != "" {
		t.Fatal("绝不能发 x-opencode-request")
	}
	// ── 指纹：工具 ──
	tools := zenToolsOf(s.gotBody["tools"])
	if len(tools) != 2 {
		t.Fatalf("应注入 2 个工具，实际 %d", len(tools))
	}
	for _, tool := range tools {
		name := zenToolName(tool)
		if name != "read" && name != "shell" {
			t.Fatalf("注入的工具名不对: %q", name)
		}
		if !zenCarriesOfficialDefinition(tool, name, ZenStyleChat) {
			t.Fatalf("注入的 %q 不是官方定义", name)
		}
	}
	// ── 指纹：占位提示 ──
	msgs, _ := s.gotBody["messages"].([]any)
	found := false
	for _, mm := range msgs {
		m, _ := mm.(map[string]any)
		if m["role"] == "system" {
			if txt, _ := m["content"].(string); strings.Contains(txt, "placeholder") {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("必须注入占位提示")
	}
	// ── 流：文本 ──
	var text strings.Builder
	for _, c := range chunks {
		if c.Kind == ChunkTextDelta {
			text.WriteString(c.Delta)
		}
	}
	if text.String() != "hi" {
		t.Fatalf("文本不对: %q", text.String())
	}
}

func TestZenRequestHasNoToolChoiceNone(t *testing.T) {
	s := &standIn{frames: []string{`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`}}
	srv := newStandInServer(t, s)
	lane := NewZenLane(NewSessionMap())
	lane.base = srv.URL

	turn := &Turn{
		Model:    "deepseek-v4.1-flash-free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}},
		Seed:     "s",
	}
	_, _, uerr := lane.Do(context.Background(), turn, func(Chunk) {})
	if uerr != nil {
		t.Fatalf("不该有错误: %v", uerr)
	}
	if tc, ok := s.gotBody["tool_choice"]; ok {
		if v, _ := tc.(string); v == "none" {
			t.Fatal("绝不能下发 tool_choice:none（那会让模型这一轮不调工具）")
		}
	}
}

// TestClassifyDistinguishesIdentityGateFromIPReputation 是**最关键的分类测试**。
//
// 上游对这两种情况返回**同样的** 403 + 同样的 type（FreeTierError），
// 只有 message 不同。混淆它们会导致"改了半天的指纹其实是对的"的误判。
func TestClassifyDistinguishesIdentityGateFromIPReputation(t *testing.T) {
	identityGate := "OpenCode's free tier can only be used from within OpenCode. Download it at https://opencode.ai and keep building."
	ipReputation := "OpenCode's free models aren't available from your current network because we've detected abusive traffic from this IP address. This isn't related to your account. If you're on a VPN or proxy, turn it off and try again, or subscribe to OpenCode Go at https://opencode.ai/go to keep building."

	e1 := ClassifyFailure(403, identityGate, 0)
	if e1.Code != CodeFreeTier {
		t.Errorf("身份闸门应为 %v，实际 %v", CodeFreeTier, e1.Code)
	}

	e2 := ClassifyFailure(403, ipReputation, 0)
	if e2.Code != CodeAbuseBlocked {
		t.Errorf("IP 风控应为 %v，实际 %v", CodeAbuseBlocked, e2.Code)
	}

	// 两者都不可原地重试。
	if e1.RetrySameModel() {
		t.Error("身份闸门不该原地重试")
	}
	if e2.RetrySameModel() {
		t.Error("IP 风控不该原地重试")
	}
	// 两者都可以换一个候选（换车道有意义）。
	if !e1.Switchable() || !e2.Switchable() {
		t.Error("两者都应该可切换")
	}
	// ⚠️ 关键：两者**不是**同一个码。
	if e1.Code == e2.Code {
		t.Error("身份闸门与 IP 风控必须是不同的错误码")
	}
}

func TestClassifyHTMLPageIsClientError(t *testing.T) {
	// WAF 的 HTML 页面是**前置代理**在说话，从来不是凭据在说话。
	html := "<!DOCTYPE html><html><body>403 Forbidden</body></html>"
	e := ClassifyFailure(403, html, 0)
	if e.Code != CodeClient {
		t.Fatalf("HTML 页面应为 %v，实际 %v", CodeClient, e.Code)
	}
	if e.Switchable() {
		t.Error("HTML 页面不该可切换（重放会拿到同样的页面）")
	}
}

func TestClassifyQuotaAndCredential(t *testing.T) {
	e := ClassifyFailure(429, `{"error":{"message":"rate limit exceeded"}}`, 0)
	if e.Code != CodeQuota {
		t.Errorf("应为 %v，实际 %v", CodeQuota, e.Code)
	}
	e = ClassifyFailure(401, `{"error":{"message":"No credentials found"}}`, 0)
	if e.Code != CodeCredential {
		t.Errorf("应为 %v，实际 %v", CodeCredential, e.Code)
	}
	// 凭据错了换模型也没用。
	if e.Switchable() {
		t.Error("凭据错误不该可切换")
	}
}

func TestClassifyContextWindowIsNotRetryable(t *testing.T) {
	e := ClassifyFailure(400, `{"error":{"message":"prompt is too long: exceeds context window"}}`, 0)
	if e.Switchable() {
		t.Error("上下文超限不该可切换（换个模型也一样超）")
	}
}

// TestZenListingFallsBackWhenUpstreamFails 验证"拉不到也有清单可用"。
//
// NAS 开机时网络可能还没通。清单空着 = 整个网关不可用。
func TestZenListingFallsBackWhenUpstreamFails(t *testing.T) {
	// 一个只会返回 500 的替身。
	s := &standIn{status: http.StatusInternalServerError}
	srv := newStandInServer(t, s)
	lane := NewZenLane(NewSessionMap())
	lane.base = srv.URL

	models, uerr := lane.ListModels(context.Background())
	if uerr == nil {
		t.Fatal("应该报告上游失败")
	}
	if len(models) == 0 {
		t.Fatal("拉不到时必须回落到快照 —— 清单空着等于整个网关不可用")
	}
	for _, m := range models {
		if !m.Free {
			t.Fatalf("回落清单里不该有付费模型: %q", m.ID)
		}
	}
}

func TestZenListingNeverReturnsEmptyOnSuccessfulButBlankResponse(t *testing.T) {
	// 上游返回 200 但清单是空的 —— 那说明上游出问题了，
	// 不说明"没有免费模型"。清单不能因此变空。
	s := &standIn{jsonBody: `{"object":"list","data":[]}`}
	srv := newStandInServer(t, s)
	lane := NewZenLane(NewSessionMap())
	lane.base = srv.URL

	models, uerr := lane.ListModels(context.Background())
	if uerr != nil {
		t.Fatalf("不该有错误: %v", uerr)
	}
	if len(models) == 0 {
		t.Fatal("空清单必须回落到快照")
	}
}

func TestZenListingOnlyReturnsFreeModels(t *testing.T) {
	// 上游清单混合了免费与付费 —— 必须过滤。
	// 付费 id 在没有用户凭据时会回 429/401，看起来像"车道坏了"。
	s := &standIn{
		frames: nil,
	}
	srv := newStandInServer(t, s)
	lane := NewZenLane(NewSessionMap())
	lane.base = srv.URL
	// 直接测过滤逻辑。
	if !IsFreeLane("deepseek-v4.1-flash-free") {
		t.Error("free 后缀应被识别")
	}
	if !IsFreeLane("big-pickle") {
		t.Error("alwaysFree 名单里的应被识别")
	}
	if IsFreeLane("gpt-5") {
		t.Error("付费模型不该被识别为免费")
	}
	if IsFreeLane("claude-sonnet-4-5") {
		t.Error("付费模型不该被识别为免费")
	}
}

// 会话稳定性

func TestZenSessionIsStablePerSeed(t *testing.T) {
	s := &standIn{frames: []string{`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`}}
	srv := newStandInServer(t, s)
	lane := NewZenLane(NewSessionMap())
	lane.base = srv.URL

	turn := &Turn{
		Model:    "deepseek-v4.1-flash-free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}},
		Seed:     "conversation-42",
	}
	for i := 0; i < 3; i++ {
		_, _, uerr := lane.Do(context.Background(), turn, func(Chunk) {})
		if uerr != nil {
			t.Fatalf("不该有错误: %v", uerr)
		}
	}
	// 同一个 seed 必须一直是同一个 session。
	session := s.gotRequest.Header.Get("x-opencode-session")
	if session == "" {
		t.Fatal("缺少会话头")
	}
	// 再打一次，确认还是它。
	_, _, _ = lane.Do(context.Background(), turn, func(Chunk) {})
	if got := s.gotRequest.Header.Get("x-opencode-session"); got != session {
		t.Fatalf("同一会话的 session id 变了: %q → %q", session, got)
	}
}

func TestZenDifferentSeedsGetDifferentSessions(t *testing.T) {
	m := NewSessionMap()
	a := m.For("seed-a")
	b := m.For("seed-b")
	if a == b {
		t.Fatal("不同的 seed 必须拿到不同的 session")
	}
	if !ZenIsCanonicalSession(a) || !ZenIsCanonicalSession(b) {
		t.Fatalf("session 编码不合法: %q %q", a, b)
	}
}

// 错误分类

func TestZenFreeTierErrorIsClassified(t *testing.T) {
	s := &standIn{
		status:    http.StatusForbidden,
		errorBody: `{"error":{"type":"FreeTierError","message":"free tier can only be used from within OpenCode"}}`,
	}
	srv := newStandInServer(t, s)
	lane := NewZenLane(NewSessionMap())
	lane.base = srv.URL

	turn := &Turn{
		Model:    "deepseek-v4.1-flash-free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}},
		Seed:     "s",
	}
	_, _, uerr := lane.Do(context.Background(), turn, func(Chunk) {})
	if uerr == nil {
		t.Fatal("应该有错误")
	}
	// ⚠️ 必须是 FREE_TIER_BLOCKED，不能是 INVALID_CREDENTIAL。
	// 归错码会让客户端以为"你的 key 错了"，并停掉故障转移。
	if uerr.Code != CodeFreeTier {
		t.Fatalf("错误码不对: %v（应为 %v）", uerr.Code, CodeFreeTier)
	}
	// 闸门对所有出口一视同仁 —— 原地重试只是白烧预算。
	// 但**换车道**是有意义的（免密被封 → OpenRouter 兜底）。
	if uerr.RetrySameModel() {
		t.Fatal("FREE_TIER_BLOCKED 不该重试同一个模型")
	}
	if !uerr.Switchable() {
		t.Fatal("FREE_TIER_BLOCKED 应该可以换一个候选（自动跨模型调用）")
	}
}

func TestZenRateLimitIsClassified(t *testing.T) {
	s := &standIn{
		status:    http.StatusTooManyRequests,
		errorBody: `{"error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`,
	}
	srv := newStandInServer(t, s)
	lane := NewZenLane(NewSessionMap())
	lane.base = srv.URL

	turn := &Turn{
		Model:    "deepseek-v4.1-flash-free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}},
		Seed:     "s",
	}
	_, _, uerr := lane.Do(context.Background(), turn, func(Chunk) {})
	if uerr == nil {
		t.Fatal("应该有错误")
	}
	if uerr.Code != CodeQuota {
		t.Fatalf("错误码不对: %v", uerr.Code)
	}
	if !uerr.Switchable() {
		t.Fatal("RATE_LIMIT 应该是可切换的")
	}
	// ⚠️ 配额按 session 记账 —— 重试是第二次烧钱。
	// 唯一有效的杠杆是**换模型**。
	if uerr.RetrySameModel() {
		t.Fatal("RATE_LIMIT 不该重试同一个模型")
	}
	// 没有 Retry-After 时要用默认的短冷却。
	if uerr.RetryAfterSec != DefaultQuotaCooldownSec {
		t.Fatalf("默认冷却应是 %d 秒，实际 %d", DefaultQuotaCooldownSec, uerr.RetryAfterSec)
	}
}

// 解码

func TestDecodeChatStream(t *testing.T) {
	var got []Chunk
	dec := newDecoder(wireChat, nil, func(c Chunk) { got = append(got, c) })
	frames := []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"He"}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"llo"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
	}
	for _, f := range frames {
		if err := dec.feed([]byte(f)); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
	}
	if !dec.sawText || !dec.sawFinish {
		t.Fatal("应该看到文本和结束标记")
	}
	if dec.usage.Input != 5 || dec.usage.Output != 3 {
		t.Fatalf("用量不对: %+v", dec.usage)
	}
	var text strings.Builder
	for _, c := range got {
		if c.Kind == ChunkTextDelta {
			text.WriteString(c.Delta)
		}
	}
	if text.String() != "Hello" {
		t.Fatalf("文本不对: %q", text.String())
	}
}

func TestDecodeReasoningOnlyFirstSpelling(t *testing.T) {
	// 有些网关**同时**用两个名字报同一段文本。
	// 全读会把块算重，用量统计会翻倍。
	var got []Chunk
	dec := newDecoder(wireChat, nil, func(c Chunk) { got = append(got, c) })
	frame := `{"choices":[{"index":0,"delta":{"reasoning":"think","reasoning_content":"think"}}]}`
	if err := dec.feed([]byte(frame)); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	count := 0
	for _, c := range got {
		if c.Kind == ChunkReasonDelta {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("思考内容只应读一次，实际 %d 次", count)
	}
}

func TestDecodeToolCallWithoutIndexUsesCallID(t *testing.T) {
	// 没有 index 的（Claude 线常见）必须用 call id 区分，
	// 否则并行工具调用的 id 会互相覆盖，被合并成一个块。
	var got []Chunk
	dec := newDecoder(wireChat, nil, func(c Chunk) { got = append(got, c) })
	frame := `{"choices":[{"index":0,"delta":{"tool_calls":[
		{"id":"call_a","function":{"name":"alpha","arguments":"{}"}},
		{"id":"call_b","function":{"name":"beta","arguments":"{}"}}
	]}}]}`
	if err := dec.feed([]byte(frame)); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	indexes := map[int]bool{}
	for _, c := range got {
		if c.Kind == ChunkToolCallDelta {
			indexes[c.Index] = true
		}
	}
	if len(indexes) != 2 {
		t.Fatalf("两个并行工具调用应是两个块，实际 %d 个", len(indexes))
	}
}

func TestDecodeResponsesIncompleteIsTerminal(t *testing.T) {
	// ⚠️ `response.incomplete`（撞到输出上限的回合）也是**终止帧**。
	// 只认 completed 会让它看起来像被切断的流 → 重试一个已完成的回合。
	var got []Chunk
	dec := newDecoder(wireResponses, nil, func(c Chunk) { got = append(got, c) })
	frame := `{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1,"output_tokens":2}}}`
	if err := dec.feed([]byte(frame)); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !dec.sawFinish {
		t.Fatal("response.incomplete 必须是终止帧")
	}
	if dec.finish != FinishMaxTokens {
		t.Fatalf("结束原因不对: %v", dec.finish)
	}
}

func TestDecodeInStreamErrorIsDetected(t *testing.T) {
	// 200 的流里**也可能**带错误。
	var got []Chunk
	dec := newDecoder(wireChat, nil, func(c Chunk) { got = append(got, c) })
	frame := `{"error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`
	err := dec.feed([]byte(frame))
	if err == nil {
		t.Fatal("流内的错误必须被识别")
	}
	if err.Code != CodeQuota {
		t.Fatalf("错误码不对: %v", err.Code)
	}
}

func TestDecodeMessagesStream(t *testing.T) {
	var got []Chunk
	dec := newDecoder(wireMessages, nil, func(c Chunk) { got = append(got, c) })
	frames := []string{
		`{"type":"message_start","message":{"id":"m1","usage":{"input_tokens":7}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hi"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}`,
		`{"type":"message_stop"}`,
	}
	for _, f := range frames {
		if err := dec.feed([]byte(f)); err != nil {
			t.Fatalf("解析失败: %v", err)
		}
	}
	if !dec.sawText || !dec.sawFinish {
		t.Fatal("应该看到文本和结束标记")
	}
	if dec.usage.Input != 7 || dec.usage.Output != 4 {
		t.Fatalf("用量不对: %+v", dec.usage)
	}
}

// 形状嗅探

func TestSniffShape(t *testing.T) {
	cases := []struct {
		head string
		want bodyShape
	}{
		{"data: {\"a\":1}\n\n", shapeSSE},
		{": ping\n\n", shapeSSE},
		{"event: message\ndata: {}\n\n", shapeSSE},
		{"{\"data\":[]}", shapeJSON},
		{"[1,2,3]", shapeJSON},
		{"", shapeEmpty},
		{"   \n", shapeEmpty},
		{"garbage", shapeUnknown},
	}
	for _, c := range cases {
		if got := sniffShape([]byte(c.head)); got != c.want {
			t.Errorf("%q → %v，期望 %v", c.head, got, c.want)
		}
	}
}

func TestReplayReaderDoesNotLoseTheFirstFrame(t *testing.T) {
	// ⚠️ 嗅探用的是 **Read**（消费），不是 Peek（不消费）。
	// Peek 之后字节仍在缓冲区里，再 replay 一次就会**读两遍** ——
	// 客户端会收到重复的内容。
	raw := "data: {\"a\":1}\n\ndata: {\"b\":2}\n\n"
	br := bufio.NewReaderSize(strings.NewReader(raw), 1024)
	head := make([]byte, 10)
	n, _ := br.Read(head)
	head = head[:n]
	rr := &replayReader{head: head, body: br}
	out := make([]byte, len(raw))
	total := 0
	for total < len(raw) {
		m, err := rr.Read(out[total:])
		total += m
		if err != nil {
			break
		}
	}
	if string(out[:total]) != raw {
		t.Fatalf("replay 后内容不对:\n得到 %q\n期望 %q", out[:total], raw)
	}
	// 而且**不能**多出内容（Peek 会导致重复）。
	if total != len(raw) {
		t.Fatalf("长度不对: 得到 %d，期望 %d", total, len(raw))
	}
}
