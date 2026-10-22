package lane

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

// **断流续写**。
// 上游的流不是**可靠**的，实测三种断法：截断（有 usage 没 finish_reason）、
// 静默停止（连接开着但一个字节都不来）、纯思考截断（只有 reasoning）。
// 三种都会让客户端拿到**残缺**的答案，而且它**不知道**答案是残缺的。
// 做法是用**工具调用协议**续写：伪造一次内部保留名的"工具调用"，
// 参数是**已经输出的完整文本**，再伪造一个**空**的"工具结果"。
// 上游看到"我刚才调用了这个工具，参数是这些"就知道这些是我已经说过的，
// 于是从那里继续。
// ⚠️ 朴素做法（把已输出文本当成新的 user 消息）有两个问题：
// 上游会**重新说一遍**（客户端看到重复内容），而且对话历史被污染。
// ⚠️ 这是整个设计里最精巧也最脆弱的一环 —— 它依赖上游"理解工具调用里的
//   文本就是我的输出"这个行为。所以它**有降级路径**：续写失败就如实报错。
// 详见 docs/design/00-overview.md §5.8。

// 续写用的内部工具名。
// ⚠️ 必须是一个**上游不会真的调用**的名字。
// 它用双下划线开头 —— 常规工具名不会这么起。
const continuationToolName = "__fmh_continue"

// 续写的最大次数。
// ⚠️ 刻意**小**。续写消耗真实额度，而且连着续写多次通常说明上游在反复截断 ——
// 继续试只是烧钱。
const maxContinuations = 2

// ContinuationOutcome 是一次续写的结果。
type ContinuationOutcome string

const (
	// ContinuationNotNeeded 表示不需要续写（流是完整的）。
	ContinuationNotNeeded ContinuationOutcome = "not_needed"
	// ContinuationOK 表示续写成功，答案补全了。
	ContinuationOK ContinuationOutcome = "ok"
	// ContinuationFailed 表示续写失败。
	ContinuationFailed ContinuationOutcome = "failed"
	// ContinuationTruncated 表示续写后**仍然**被截断。
	ContinuationTruncated ContinuationOutcome = "truncated"
)

// ContinuationReport 是一次续写的报告。
type ContinuationReport struct {
	Outcome  ContinuationOutcome
	Attempts int
	// Reason 是失败原因（如果有）。
	Reason string
	// RecoveredTokens 是续写补回来的 token 数。
	RecoveredTokens int
}

// continuationState 跟踪一轮推理的续写状态。
type ContinuationState struct {
	// text 是已经输出给客户端的可见文本。
	text strings.Builder
	// reasoning 是已经输出的思考。
	reasoning strings.Builder
	// toolCalls 是已经输出的工具调用（按 index）。
	toolCalls []completedToolCall
	// sawFinish 表示看到了终止帧。
	sawFinish bool
	sawUsage  bool
	// lastActivity 是最后一次收到数据的时间。
	lastActivity time.Time
	// attempts 是已经续写了几次。
	attempts int
}

// completedToolCall 是一个已完成的工具调用。
type completedToolCall struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

func NewContinuationState() *ContinuationState {
	return &ContinuationState{lastActivity: time.Now()}
}

func (s *ContinuationState) Observe(c Chunk) {
	s.lastActivity = time.Now()
	switch c.Kind {
	case ChunkTextDelta:
		s.text.WriteString(c.Delta)
	case ChunkReasonDelta:
		s.reasoning.WriteString(c.Delta)
	case ChunkToolCallDelta:
		s.observeToolCall(c)
	case ChunkBlockEnd:
		if c.BlockType == BlockToolCall {
			s.sawFinish = true
		}
	case ChunkFinish:
		s.sawFinish = true
	case ChunkUsage:
		s.sawUsage = true
	}
}

// observeToolCall 记录一个工具调用的增量。
func (s *ContinuationState) observeToolCall(c Chunk) {
	for i := range s.toolCalls {
		if s.toolCalls[i].Index == c.Index {
			if c.Name != "" {
				s.toolCalls[i].Name = c.Name
			}
			if c.ID != "" {
				s.toolCalls[i].ID = c.ID
			}
			s.toolCalls[i].Arguments += c.Arguments
			return
		}
	}
	s.toolCalls = append(s.toolCalls, completedToolCall{
		Index: c.Index, ID: c.ID, Name: c.Name, Arguments: c.Arguments,
	})
}

// complete 判断这一轮是否**完整**。
// ⚠️ 三种截断的判据不同：
//
//	① 截断      —— 看到了 usage 但没看到 finish
//	② 静默停止  —— 连接断了，什么终止帧都没有
//	③ 纯思考截断 —— 有思考但没有正文
//
// ⚠️ **纯思考**不是截断 —— 有些模型就是要先想很久；
// 真正的问题是"想了但一个字都没说"，那客户端拿不到任何答案。
func (s *ContinuationState) complete() bool {
	if s.sawFinish {
		return true
	}
	// 看到了 usage 说明上游**认为**这一轮结束了。
	if s.sawUsage && s.text.Len() > 0 {
		return true
	}
	// 有正文但没有终止帧 —— 可能是截断，也可能是流被静默掐断，交给超时判定。
	return false
}

// NeedsContinuation 判断是否需要续写。
// ⚠️ **纯思考截断**要单独处理：有思考、没正文 → 一定要续写
// （否则客户端拿不到任何东西）。
func (s *ContinuationState) NeedsContinuation() bool {
	if s.complete() {
		return false
	}
	// 没有任何输出 → 续写也没用（上游没给出任何东西）。
	if s.text.Len() == 0 && len(s.toolCalls) == 0 {
		return false
	}
	return true
}

// buildContinuationMessages 构造续写用的消息。
// ⚠️ 这是整个续写机制的核心。结构：
//
//	[原来的对话…]
//	assistant: [工具调用 __fmh_continue(参数 = 已输出的文本)]
//	user:      [工具结果: ""]
//
// 上游看到"我刚刚调用了这个工具，参数是这些文本"，
// 就知道那些是**它自己已经说过的** —— 于是从那里接着说。
func (s *ContinuationState) buildContinuationMessages(base []Message) []Message {
	out := make([]Message, 0, len(base)+2)
	out = append(out, base...)

	// ① 助手侧：把已输出的内容装进一次工具调用。
	assistant := Message{Role: RoleAssistant}
	// ⚠️ 工具调用参数是 JSON —— 所以要把文本放进一个对象里。
	args := `{"text":` + jsonQuote(s.text.String()) + `}`
	assistant.Parts = []Part{
		ToolCallPart("call_continuation", continuationToolName, args),
	}
	out = append(out, assistant)

	// ② 用户侧：工具结果为空。
	out = append(out, Message{
		Role:  RoleTool,
		Parts: []Part{ToolResultPart("call_continuation", "", false)},
	})
	return out
}

// ContinuationRequest 是一个续写请求。
type ContinuationRequest struct {
	Base   []Message
	Tools  []ToolDef
	Model  string
	Lane   string
	Effort string
	// MaxTokens 是原始请求的输出上限。
	MaxTokens int
	Seed      string
}

// Continue 尝试续写。
// ⚠️ 它**不会**把已输出的内容再发给客户端 —— 只把**新**的增量发出去，
// 已发过的部分由调用方保证不重复。
func Continue(
	ctx context.Context,
	laneImpl Lane,
	req ContinuationRequest,
	state *ContinuationState,
	emit func(Chunk),
) (ContinuationReport, error) {
	report := ContinuationReport{Outcome: ContinuationNotNeeded}

	if !state.NeedsContinuation() {
		return report, nil
	}
	if state.attempts >= maxContinuations {
		report.Outcome = ContinuationFailed
		report.Attempts = state.attempts
		report.Reason = "已达续写上限"
		return report, nil
	}

	state.attempts++
	report.Attempts = state.attempts

	// 构造续写请求。
	turn := &Turn{
		Lane:      req.Lane,
		Model:     req.Model,
		Messages:  state.buildContinuationMessages(req.Base),
		Tools:     req.Tools,
		Effort:    req.Effort,
		MaxTokens: req.MaxTokens,
		// ⚠️ **新的种子** —— 否则上游可能命中缓存，
		// 把刚才那截断的答案原样再吐一遍。
		Seed: req.Seed + "\x00cont" + itoa(state.attempts),
	}

	// ⚠️ 续写时**屏蔽**调用方自己的工具 ——
	//   我们只想让它把话说完，不想让它又去调工具。
	turn.SuppressTools = true

	before := state.text.Len()
	usage, finish, uerr := laneImpl.Do(ctx, turn, func(c Chunk) {
		// ⚠️ **只转发新的正文增量**。
		// 续写请求里带的是"已说过的文本"，上游可能会把它**再吐一遍**，
		// 所以这里要把它切掉。
		if c.Kind == ChunkTextDelta {
			// 上游这次说的内容 = 已说过的 + 新的。我们只发"新的"那部分。
			c.Delta = stripPrefix(c.Delta, state.text.String())
			if c.Delta == "" {
				return
			}
		}
		state.Observe(c)
		emit(c)
	})

	if uerr != nil {
		report.Outcome = ContinuationFailed
		report.Reason = uerr.Message
		return report, uerr
	}
	_ = finish
	if usage != nil {
		report.RecoveredTokens = usage.Output
	}

	// 续写后又截断了？
	if !state.complete() {
		report.Outcome = ContinuationTruncated
		report.Reason = "续写后仍然被截断"
		return report, nil
	}

	report.Outcome = ContinuationOK
	_ = before
	return report, nil
}

// stripPrefix 去掉 delta 开头的一段已知前缀。
// ⚠️ 上游在续写时**可能**把之前的话重说一遍，我们要的是"新增的那部分"。
// 但也不能**假设**它一定重说 —— 有些上游会正确地从断点继续，
// 所以只在前缀**确实匹配**时才裁掉。
func stripPrefix(delta, known string) string {
	if known == "" || delta == "" {
		return delta
	}
	// 增量本身就是已知前缀的一部分 → 全部裁掉。
	if strings.HasPrefix(known, delta) {
		return ""
	}
	// 增量以已知文本开头 → 裁掉开头。
	if strings.HasPrefix(delta, known) {
		return delta[len(known):]
	}
	// 情况更复杂：上游从中间某点重说。
	// 找已知文本的最长后缀出现在 delta 的开头。
	max := len(known)
	if max > len(delta) {
		max = len(delta)
	}
	for n := max; n > 0; n-- {
		if strings.HasPrefix(delta, known[len(known)-n:]) {
			return delta[n:]
		}
	}
	return delta
}

// jsonQuote 把一个字符串 JSON 编码（带引号）。
// ⚠️ 用 encoding/json 而不是手写转义 —— 手写一定会漏掉某些字符（控制字符、U+2028…）。
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
