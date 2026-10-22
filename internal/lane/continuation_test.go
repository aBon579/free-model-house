package lane

import (
	"context"
	"strings"
	"testing"
)

// 断流续写

// TestContinuationNotNeededWhenComplete 验证**完整的流不续写**。
func TestContinuationNotNeededWhenComplete(t *testing.T) {
	s := NewContinuationState()
	s.Observe(Chunk{Kind: ChunkTextDelta, Delta: "hello"})
	s.Observe(Chunk{Kind: ChunkFinish, Finish: FinishStop})

	if s.NeedsContinuation() {
		t.Fatal("完整的流不该续写")
	}
}

// TestContinuationNeededOnTruncation 验证**截断的流要续写**。
func TestContinuationNeededOnTruncation(t *testing.T) {
	s := NewContinuationState()
	s.Observe(Chunk{Kind: ChunkTextDelta, Delta: "hello wor"})
	// ⚠️ 没有 finish —— 截断。

	if !s.NeedsContinuation() {
		t.Fatal("截断的流该续写")
	}
}

// TestContinuationNotNeededWhenNothingProduced 验证**什么都没产出不续写**。
//
// 续写也要花钱，而"上游啥都没给"续写也救不回来。
func TestContinuationNotNeededWhenNothingProduced(t *testing.T) {
	s := NewContinuationState()
	s.Observe(Chunk{Kind: ChunkReasonDelta, Delta: "thinking..."})
	// 只有思考，没有正文，没有 finish。

	if s.NeedsContinuation() {
		t.Fatal("没有正文时不该续写（续写也救不回来）")
	}
}

// TestContinuationBuildsToolCallMessages 验证续写消息的**结构**。
//
// ⚠️ 这是续写的核心：把"已说过的文本"装进一次工具调用，
// 让上游知道"这些是我说过的"。
func TestContinuationBuildsToolCallMessages(t *testing.T) {
	s := NewContinuationState()
	s.Observe(Chunk{Kind: ChunkTextDelta, Delta: "The answer is 42."})

	base := []Message{
		{Role: RoleUser, Parts: []Part{TextPart("what is the answer?")}},
	}
	msgs := s.buildContinuationMessages(base)

	if len(msgs) != len(base)+2 {
		t.Fatalf("该有 %d 条消息，实际 %d", len(base)+2, len(msgs))
	}
	// ① 助手侧：一次工具调用。
	assistant := msgs[len(base)]
	if assistant.Role != RoleAssistant {
		t.Fatalf("第 %d 条该是 assistant", len(base))
	}
	if len(assistant.Parts) != 1 {
		t.Fatalf("该有 1 个 part，实际 %d", len(assistant.Parts))
	}
	part := assistant.Parts[0]
	if part.Type != PartToolCall {
		t.Fatalf("该是工具调用，实际 %v", part.Type)
	}
	if part.ToolName != continuationToolName {
		t.Fatalf("工具名不对: %q", part.ToolName)
	}
	// ⚠️ 参数里必须含**已输出的完整文本**。
	if !strings.Contains(part.Arguments, "The answer is 42.") {
		t.Fatalf("参数里该有已输出的文本: %q", part.Arguments)
	}
	// 参数必须是合法 JSON。
	if !isValidJSONObject(part.Arguments) {
		t.Fatalf("参数该是合法 JSON: %q", part.Arguments)
	}
	// ② 用户侧：空工具结果。
	toolMsg := msgs[len(base)+1]
	if toolMsg.Role != RoleTool {
		t.Fatalf("第 %d 条该是 tool", len(base)+1)
	}
}

// TestContinuationStripsRepeatedPrefix 验证**去掉上游重说的部分**。
//
// ⚠️ 这是续写里最容易出 bug 的地方 ——
//
//	上游可能把刚才的话**再吐一遍**，
//	不裁掉的话客户端会看到重复内容。
func TestContinuationStripsRepeatedPrefix(t *testing.T) {
	cases := []struct {
		name  string
		delta string
		known string
		want  string
	}{
		{"完全重复", "hello", "hello", ""},
		{"前缀+新增", "hello world", "hello", " world"},
		{"只是新增", "world", "hello", "world"},
		{"重复一部分", "world and more", "hello world", " and more"},
		{"不相关", "totally different", "hello", "totally different"},
	}
	for _, c := range cases {
		got := stripPrefix(c.delta, c.known)
		if got != c.want {
			t.Errorf("%s: stripPrefix(%q, %q) = %q，期望 %q",
				c.name, c.delta, c.known, got, c.want)
		}
	}
}

// TestStripPrefixKeepsNewText 验证**不误伤新内容**。
func TestStripPrefixKeepsNewText(t *testing.T) {
	// 已知 "abc"，增量 "xyz" —— 完全无关，必须原样保留。
	if got := stripPrefix("xyz", "abc"); got != "xyz" {
		t.Fatalf("不该裁掉无关内容: %q", got)
	}
	// 已知 "" —— 原样。
	if got := stripPrefix("xyz", ""); got != "xyz" {
		t.Fatalf("空已知不该裁: %q", got)
	}
}

// 续写的执行

// contLane 是一个可控的车道，用于测试续写。
type contLane struct {
	id string
	// responses 是每次调用返回的文本序列。
	responses []string
	// calls 记录每次调用的消息。
	calls [][]Message
	// failAt 是第几次调用开始失败（-1 = 不失败）。
	failAt int
}

func (c *contLane) ID() string  { return c.id }
func (c *contLane) Ready() bool { return true }
func (c *contLane) ListModels(ctx context.Context) ([]ModelInfo, *Error) {
	return nil, nil
}
func (c *contLane) Status() LaneStatus { return LaneStatus{ID: c.id, Ready: true} }
func (c *contLane) Probe(ctx context.Context, model string) ProbeResult {
	return ProbeResult{Model: model, State: StateAvailable}
}

func (c *contLane) Do(ctx context.Context, turn *Turn, emit func(Chunk)) (*Usage, FinishReason, *Error) {
	idx := len(c.calls)
	c.calls = append(c.calls, turn.Messages)
	if c.failAt >= 0 && idx >= c.failAt {
		return nil, FinishError, &Error{Code: CodeServer, Message: "upstream died"}
	}
	if idx < len(c.responses) {
		emit(Chunk{Kind: ChunkTextDelta, Delta: c.responses[idx]})
	}
	emit(Chunk{Kind: ChunkFinish, Finish: FinishStop})
	return &Usage{Output: 10}, FinishStop, nil
}

// TestContinueAppendsNewText 验证**续写把新内容接上**。
func TestContinueAppendsNewText(t *testing.T) {
	l := &contLane{id: "zen", responses: []string{" and more"}, failAt: -1}
	s := NewContinuationState()
	s.Observe(Chunk{Kind: ChunkTextDelta, Delta: "The answer"})

	var got string
	report, err := Continue(context.Background(), l, ContinuationRequest{
		Base:  []Message{{Role: RoleUser, Parts: []Part{TextPart("q")}}},
		Model: "m", Lane: "zen",
	}, s, func(c Chunk) {
		if c.Kind == ChunkTextDelta {
			got += c.Delta
		}
	})
	if err != nil {
		t.Fatalf("续写失败: %v", err)
	}
	if report.Outcome != ContinuationOK {
		t.Fatalf("续写该成功，实际 %v (%s)", report.Outcome, report.Reason)
	}
	if got != " and more" {
		t.Fatalf("续写内容不对: %q", got)
	}
}

// TestContinueDropsRepeatedText 验证**续写不重复已说过的内容**。
//
// ⚠️ 上游把"hello"又吐了一遍 —— 客户端不该看到第二遍。
func TestContinueDropsRepeatedText(t *testing.T) {
	l := &contLane{id: "zen", responses: []string{"hello world"}, failAt: -1}
	s := NewContinuationState()
	s.Observe(Chunk{Kind: ChunkTextDelta, Delta: "hello"})

	var got string
	report, _ := Continue(context.Background(), l, ContinuationRequest{
		Base:  []Message{{Role: RoleUser, Parts: []Part{TextPart("q")}}},
		Model: "m", Lane: "zen",
	}, s, func(c Chunk) {
		if c.Kind == ChunkTextDelta {
			got += c.Delta
		}
	})
	if report.Outcome != ContinuationOK {
		t.Fatalf("续写该成功，实际 %v", report.Outcome)
	}
	// ⚠️ "hello" 被裁掉，只剩 " world"。
	if got != " world" {
		t.Fatalf("重复内容没被裁掉: %q", got)
	}
}

// TestContinueRespectsLimit 验证**续写次数有上限**。
func TestContinueRespectsLimit(t *testing.T) {
	l := &contLane{id: "zen", responses: []string{"x"}, failAt: -1}
	s := NewContinuationState()
	s.Observe(Chunk{Kind: ChunkTextDelta, Delta: "partial"})

	// 连试到超过上限。
	var last ContinuationReport
	for i := 0; i < maxContinuations+3; i++ {
		// 每次都让它"还没完" —— 不发 finish。
		l.responses = []string{"more"}
		s.sawFinish = false
		report, _ := Continue(context.Background(), l, ContinuationRequest{
			Base:  []Message{{Role: RoleUser, Parts: []Part{TextPart("q")}}},
			Model: "m", Lane: "zen",
		}, s, func(Chunk) {})
		last = report
	}
	if last.Attempts > maxContinuations {
		t.Fatalf("续写次数不该超过 %d，实际 %d", maxContinuations, last.Attempts)
	}
}

// TestContinueFailureIsReported 验证**续写失败会如实报告**。
//
// ⚠️ 绝不能假装成功。
func TestContinueFailureIsReported(t *testing.T) {
	l := &contLane{id: "zen", responses: nil, failAt: 0}
	s := NewContinuationState()
	s.Observe(Chunk{Kind: ChunkTextDelta, Delta: "partial"})

	report, err := Continue(context.Background(), l, ContinuationRequest{
		Base:  []Message{{Role: RoleUser, Parts: []Part{TextPart("q")}}},
		Model: "m", Lane: "zen",
	}, s, func(Chunk) {})
	if err == nil {
		t.Fatal("该返回错误")
	}
	if report.Outcome != ContinuationFailed {
		t.Fatalf("该报告失败，实际 %v", report.Outcome)
	}
	if report.Reason == "" {
		t.Fatal("该说明失败原因")
	}
}

// TestContinueUsesNewSeed 验证**续写用新的种子**。
//
// ⚠️ 否则上游可能命中缓存，把刚才那截断的答案原样再吐一遍。
func TestContinueUsesNewSeed(t *testing.T) {
	l := &contLane{id: "zen", responses: []string{"more"}, failAt: -1}
	s := NewContinuationState()
	s.Observe(Chunk{Kind: ChunkTextDelta, Delta: "partial"})

	var seenSeed string
	_, _ = Continue(context.Background(), &seedCaptureLane{contLane: l, seed: &seenSeed}, ContinuationRequest{
		Base:  []Message{{Role: RoleUser, Parts: []Part{TextPart("q")}}},
		Model: "m", Lane: "zen", Seed: "orig-seed",
	}, s, func(Chunk) {})

	if seenSeed == "orig-seed" {
		t.Fatal("续写不该用原种子")
	}
	if !strings.Contains(seenSeed, "orig-seed") {
		t.Fatalf("续写种子该包含原种子: %q", seenSeed)
	}
}

// seedCaptureLane 捕获续写用的种子。
type seedCaptureLane struct {
	*contLane
	seed *string
}

func (s *seedCaptureLane) Do(ctx context.Context, turn *Turn, emit func(Chunk)) (*Usage, FinishReason, *Error) {
	*s.seed = turn.Seed
	return s.contLane.Do(ctx, turn, emit)
}

// TestContinueSuppressesCallerTools 验证**续写要求抑制调用方的工具**。
//
// ⚠️ 注意：抑制是在 **lane.Do 里**做的（看 turn.SuppressTools），
//
//	不是在构造 Turn 时清空 Tools ——
//	因为清空 Tools 会让我们**看不到**调用方原本要了什么。
func TestContinueSuppressesCallerTools(t *testing.T) {
	l := &contLane{id: "zen", responses: []string{"more"}, failAt: -1}
	s := NewContinuationState()
	s.Observe(Chunk{Kind: ChunkTextDelta, Delta: "partial"})

	var sawSuppress bool
	_, _ = Continue(context.Background(), &toolCaptureLane{contLane: l, tools: &sawSuppress}, ContinuationRequest{
		Base:  []Message{{Role: RoleUser, Parts: []Part{TextPart("q")}}},
		Model: "m", Lane: "zen",
		Tools: []ToolDef{{Name: "read", Description: "x"}},
	}, s, func(Chunk) {})

	if !sawSuppress {
		t.Fatal("续写必须设置 SuppressTools")
	}
}

// toolCaptureLane 捕获续写时是否要求抑制工具。
type toolCaptureLane struct {
	*contLane
	tools *bool
}

func (t *toolCaptureLane) Do(ctx context.Context, turn *Turn, emit func(Chunk)) (*Usage, FinishReason, *Error) {
	*t.tools = turn.SuppressTools
	return t.contLane.Do(ctx, turn, emit)
}
