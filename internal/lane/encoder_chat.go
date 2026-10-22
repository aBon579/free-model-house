package lane

import (
	"fmt"
)

// 本文件把**统一事件流**投影成三种线上协议。
//   ChatEncoder     → OpenAI Chat Completions（Cline / Aider / OpenAI SDK）
//   ResponsesEncoder→ OpenAI Responses（Codex / 新 SDK）
//   MessagesEncoder → Anthropic Messages（Claude Code）
// 三个 encoder 共享同一套状态机（block 的开启/关闭、工具参数的"扣住"），
// 差别只在**线上的拼写**。
// 三个必须做对的细节：
//  1. **延迟提交**：在第一个"答案承载"的 delta 之前不写响应头。
//     这样上游的 429 + Retry-After 还能作为真正的 HTTP 429 返回。
//     一旦提交，失败只能以流内错误事件传递。
//  2. **思考不算提交**："想了一分钟然后撞上配额墙"恰恰是最该故障转移的情形。
//  3. **工具参数要扣住**：参数片段先到、名字后到是常见的。
//     不扣住就会把一段参数错配给别的调用。
// 详见 docs/design/00-overview.md §5.1 与 §5.10。

// Encoder 把统一事件流编码成一种线上协议。
type Encoder interface {
	// Write 把一个事件写出去。
	Write(c Chunk) error
	// Committed 报告是否已经"提交"（写出了第一个答案承载的 delta）。
	Committed() bool
	// Finish 收尾（写终止帧、flush）。
	Finish(finish FinishReason, usage *Usage) error
}

// EncoderStyle 指出用哪个 encoder。
type EncoderStyle string

const (
	StyleChat      EncoderStyle = "chat"
	StyleResponses EncoderStyle = "responses"
	StyleMessages  EncoderStyle = "messages"
)

// blockState 是一个已开启的块的状态。
type blockState struct {
	index int
	kind  BlockType
	text  string
	args  string
	id    string
	name  string
	// named 表示是否已经知道名字（工具块必须先有名字才能宣告）。
	named bool
	// held 是"扣住"的参数前缀（名字还没到）。
	held string
	// emitted 表示这个块是否已经对外宣告过。
	emitted bool
	// suppressed 表示这个块被压掉了（注入的占位工具）。
	suppressed bool
}

// encoderState 是三个 encoder 共享的状态机。
type encoderState struct {
	style EncoderStyle
	// blocks 是按 index 排的活动块。
	blocks map[int]*blockState
	order  []int
	// nextIndex 是下一个块序号。
	nextIndex int
	// committed 表示已经写出了第一个答案承载的 delta。
	committed bool
	// started 表示已经写过开场帧。
	started bool
	// finished 表示已经写过终止帧。
	finished bool
	// toolIndexMap 把内部块序号映射到线上的 tool_calls index。
	toolIndexMap map[int]int
	// nextToolIndex 是下一个线上 tool_calls index。
	nextToolIndex int
	usage         Usage
}

func newEncoderState(style EncoderStyle) *encoderState {
	return &encoderState{
		style:        style,
		blocks:       map[int]*blockState{},
		toolIndexMap: map[int]int{},
	}
}

// commit 标记"已提交"。
func (s *encoderState) commit() { s.committed = true }

// slot 取得（或开启）一个块。
func (s *encoderState) slot(index int, kind BlockType) *blockState {
	if b, ok := s.blocks[index]; ok {
		return b
	}
	b := &blockState{index: index, kind: kind}
	s.blocks[index] = b
	s.order = append(s.order, index)
	s.nextIndex++
	return b
}

// currentTextBlock 返回当前开启的文本/思考块（若有）。
func (s *encoderState) currentTextBlock() *blockState {
	for _, idx := range s.order {
		b := s.blocks[idx]
		if b.kind == BlockText || b.kind == BlockReasoning {
			return b
		}
	}
	return nil
}

// ChatEncoder 编码 OpenAI Chat Completions。
type ChatEncoder struct {
	baseEncoder
	// idSeq 是 Chat 的响应 id 序号。
	idSeq int
}

// NewChatEncoder 建一个 Chat encoder。
func NewChatEncoder(w Writer, requestID, modelName string, suppress, callerTools map[string]bool, rename map[string]string) *ChatEncoder {
	return &ChatEncoder{
		baseEncoder: baseEncoder{
			writer:      w,
			requestID:   requestID,
			modelName:   modelName,
			state:       newEncoderState(StyleChat),
			suppress:    suppress,
			callerTools: callerTools,
			rename:      rename,
		},
	}
}

// Write 实现 Encoder。
func (e *ChatEncoder) Write(c Chunk) error {
	switch c.Kind {
	case ChunkBlockStart:
		b := e.state.slot(c.Index, c.BlockType)
		if c.BlockType == BlockToolCall {
			if c.ID != "" {
				b.id = c.ID
			}
			if c.Name != "" {
				b.name = c.Name
				b.named = true
			}
		}
		return nil

	case ChunkTextDelta:
		b := e.state.slot(c.Index, BlockText)
		b.text += c.Delta
		e.state.commit()
		return e.writeChatChunk(map[string]any{
			"delta": map[string]any{"content": c.Delta},
		})

	case ChunkReasonDelta:
		b := e.state.slot(c.Index, BlockReasoning)
		b.text += c.Delta
		e.state.commit()
		// ⚠️ 思考内容要**双写** `reasoning` 和 `reasoning_content`。
		// 只写一个会让半个生态静默丢失思考内容。
		return e.writeChatChunk(map[string]any{
			"delta": map[string]any{
				"reasoning":         c.Delta,
				"reasoning_content": c.Delta,
			},
		})

	case ChunkToolCallDelta:
		b := e.state.slot(c.Index, BlockToolCall)
		b.args += c.Arguments
		if c.Name != "" && !b.named {
			b.name = c.Name
			b.named = true
		}
		if c.ID != "" && b.id == "" {
			b.id = c.ID
		}
		// ⚠️ 压制的工具**不算提交**。
		// 注入的占位工具是给上游看的，客户端不该看见它们。
		if e.shouldSuppress(b.name) {
			b.suppressed = true
			return nil
		}
		e.state.commit()
		// ⚠️ 名字还没到 —— 扣住参数，别错配给别的调用。
		if !b.named {
			b.held += c.Arguments
			return nil
		}
		return e.writeChatChunk(map[string]any{
			"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": e.toolIndexOf(b),
					"id":    b.id,
					"function": map[string]any{
						"name":      e.restoreName(b.name),
						"arguments": c.Arguments,
					},
				}},
			},
		})

	case ChunkUsage:
		if c.Usage != nil {
			e.state.usage = *c.Usage
		}
		return nil

	case ChunkBlockEnd:
		b := e.state.slot(c.Index, c.BlockType)
		if b.kind == BlockToolCall && !b.named {
			// 到块结束还没名字 —— 这个工具调用是坏的，丢掉。
			return nil
		}
		return nil
	}
	return nil
}

// toolIndexOf 给一个工具块分配（并记住）线上的 tool_calls index。
// 线上 index 要**从 0 重新编号** —— 被压制的占位工具不能占坑。
func (e *ChatEncoder) toolIndexOf(b *blockState) int {
	if idx, ok := e.state.toolIndexMap[b.index]; ok {
		return idx
	}
	idx := e.state.nextToolIndex
	e.state.nextToolIndex++
	e.state.toolIndexMap[b.index] = idx
	return idx
}

// restoreName 把发出去的工具名还原成调用方的名字。
func (e *ChatEncoder) restoreName(name string) string {
	if orig, ok := e.rename[name]; ok {
		return orig
	}
	return name
}

// writeChatChunk 写一个 Chat chunk。
func (e *ChatEncoder) writeChatChunk(payload map[string]any) error {
	// 首帧带 role。
	if !e.state.started {
		e.state.started = true
		role := map[string]any{
			"delta": map[string]any{"role": "assistant", "content": ""},
		}
		if err := e.emit("", role); err != nil {
			return err
		}
	}
	chunk := map[string]any{
		"id":      e.responseID(),
		"object":  "chat.completion.chunk",
		"created": nowUnix(),
		"model":   e.modelName,
		"choices": []any{map[string]any{
			"index": 0,
			"delta": payload["delta"],
		}},
	}
	return e.emit("", chunk)
}

// responseID 生成 Chat 的响应 id。
func (e *ChatEncoder) responseID() string {
	e.idSeq++
	return fmt.Sprintf("chatcmpl-fmh-%s-%d", e.requestID, e.idSeq)
}

func (e *ChatEncoder) Committed() bool { return e.state.committed }

func (e *ChatEncoder) Finish(finish FinishReason, usage *Usage) error {
	if e.state.finished {
		return nil
	}
	e.state.finished = true
	if !e.state.started {
		e.state.started = true
		if err := e.emit("", map[string]any{
			"delta": map[string]any{"role": "assistant", "content": ""},
		}); err != nil {
			return err
		}
	}
	reason := chatFinishReason(finish)
	payload := map[string]any{
		"id":      e.responseID(),
		"object":  "chat.completion.chunk",
		"created": nowUnix(),
		"model":   e.modelName,
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": reason,
		}},
	}
	if err := e.emit("", payload); err != nil {
		return err
	}
	// usage 单独一帧（choices 为空）。
	u := e.state.usage
	if usage != nil {
		u = *usage
	}
	// ⚠️ **思考 token 要带出去** ——
	// OpenAI 的形状是 `usage.completion_tokens_details.reasoning_tokens`
	// （嵌套，不是顶层）。
	// 不带的话，客户端**看不到**"思考占了多少 token" ——
	// 而思考 token 是算在 `completion_tokens` 里面的，
	// 不区分的话，"答案有多长"就被高估了。
	// 实测（2026-10-19，OpenRouter）：
	//	{"completion_tokens": 40, "completion_tokens_details": {"reasoning_tokens": 39}}
	details := map[string]any{}
	if u.Reasoning > 0 {
		details["reasoning_tokens"] = u.Reasoning
	}
	usageObj := map[string]any{
		"prompt_tokens":     u.Input,
		"completion_tokens": u.Output,
		"total_tokens":      u.Total,
	}
	if len(details) > 0 {
		usageObj["completion_tokens_details"] = details
	}
	return e.emit("", map[string]any{
		"id":      e.responseID(),
		"object":  "chat.completion.chunk",
		"created": nowUnix(),
		"model":   e.modelName,
		"choices": []any{},
		"usage":   usageObj,
	})
}

// chatFinishReason 把统一结束原因映射成 Chat 的 finish_reason。
func chatFinishReason(f FinishReason) string {
	switch f {
	case FinishToolCalls:
		return "tool_calls"
	case FinishMaxTokens:
		return "length"
	case FinishAborted:
		return "stop"
	case FinishError:
		return "stop"
	default:
		return "stop"
	}
}
