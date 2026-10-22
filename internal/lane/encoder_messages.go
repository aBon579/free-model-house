package lane

// 本文件把统一事件流编码成 **Anthropic Messages** 协议。
// Claude Code 只说这个协议，所以它是 FMH 最重要的一条线。
// Anthropic 的块纪律（与另两条线不同）：
//  1. 文本/思考块**同一时刻只开一个**。来一个不同类型的 delta，
//     要先把前一个关掉。
//  2. **工具块是例外**：每个工具块由它自己的 block-end 关闭，
//     所以**并行工具调用可以同时开着**。
//     （关掉一个工具块去开另一个，会把一个调用的 JSON 拆成两个同 id 的块，
//     客户端会执行两次。）
//  3. usage 帧不带自己的帧：输入 token 在 message_start 里，
//     输出 token 在 message_delta 里。
//  4. 一个工具调用的参数片段要**扣住**直到调用报出名字 ——
//     Anthropic 的 tool_use 块没有名字就不能宣告。
// 5. 错误/取消的收尾只发 `event: error` + `message_stop`，
//     **不发** `message_delta` —— 一个 stop_reason 会声称这轮是正常结束的。
// 详见 docs/design/00-overview.md §5.1。

// MessagesEncoder 编码 Anthropic Messages。
type MessagesEncoder struct {
	baseEncoder
	// anthropicIndex 把内部块序号映射到 Anthropic 的块序号。
	anthropicIndex map[int]int
	// nextAnthropicIndex 是下一个 Anthropic 块序号。
	nextAnthropicIndex int
	// openKind 是当前开启的文本/思考块类型（"" 表示没有）。
	openKind BlockType
	// openIndex 是当前开启的块的内部序号。
	openIndex int
	// messageID 是这条消息的 id。
	messageID string
	// startedMessage 报告 message_start 是否已发。
	startedMessage bool
	// inputTokens 是输入 token 数（message_start 时就要给）。
	inputTokens int
}

// NewMessagesEncoder 建一个 Anthropic Messages encoder。
func NewMessagesEncoder(w Writer, requestID, modelName, messageID string, suppress, callerTools map[string]bool, rename map[string]string) *MessagesEncoder {
	return &MessagesEncoder{
		baseEncoder: baseEncoder{
			writer:      w,
			requestID:   requestID,
			modelName:   modelName,
			state:       newEncoderState(StyleMessages),
			suppress:    suppress,
			callerTools: callerTools,
			rename:      rename,
		},
		anthropicIndex: map[int]int{},
		messageID:      messageID,
	}
}

// anthropicIdx 给一个内部块分配 Anthropic 块序号。
func (e *MessagesEncoder) anthropicIdx(internal int) int {
	if idx, ok := e.anthropicIndex[internal]; ok {
		return idx
	}
	idx := e.nextAnthropicIndex
	e.nextAnthropicIndex++
	e.anthropicIndex[internal] = idx
	return idx
}

// ensureMessageStart 发出 message_start。
// ⚠️ 这是**延迟提交**的关键：message_start 不在第一个 delta 时立刻发，
// 而是等到真的有内容时才发。理由见 encoder_common.go。
func (e *MessagesEncoder) ensureMessageStart() error {
	if e.startedMessage {
		return nil
	}
	e.startedMessage = true
	e.state.started = true
	return e.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            e.messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         e.modelName,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  e.inputTokens,
				"output_tokens": 0,
			},
		},
	})
}

// closeOpenBlock 关闭当前开启的文本/思考块。
func (e *MessagesEncoder) closeOpenBlock() error {
	if e.openKind == "" {
		return nil
	}
	idx := e.anthropicIdx(e.openIndex)
	e.openKind = ""
	e.openIndex = 0
	return e.emit("content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": idx,
	})
}

// openTextBlock 开启一个文本/思考块。
func (e *MessagesEncoder) openTextBlock(internal int, kind BlockType) error {
	if e.openKind == kind && e.openIndex == internal {
		return nil
	}
	if err := e.closeOpenBlock(); err != nil {
		return err
	}
	idx := e.anthropicIdx(internal)
	e.openKind = kind
	e.openIndex = internal
	blockType := "text"
	if kind == BlockReasoning {
		blockType = "thinking"
	}
	return e.emit("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": idx,
		"content_block": map[string]any{
			"type": blockType,
			"text": "",
		},
	})
}

// Write 实现 Encoder。
func (e *MessagesEncoder) Write(c Chunk) error {
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

	case ChunkTextDelta, ChunkReasonDelta:
		if err := e.ensureMessageStart(); err != nil {
			return err
		}
		kind := BlockText
		deltaType := "text_delta"
		if c.Kind == ChunkReasonDelta {
			kind = BlockReasoning
			deltaType = "thinking_delta"
		}
		if err := e.openTextBlock(c.Index, kind); err != nil {
			return err
		}
		b := e.state.slot(c.Index, kind)
		b.text += c.Delta
		e.state.commit()
		return e.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": e.anthropicIdx(c.Index),
			"delta": map[string]any{
				"type": deltaType,
				"text": c.Delta,
			},
		})

	case ChunkToolCallDelta:
		if err := e.ensureMessageStart(); err != nil {
			return err
		}
		b := e.state.slot(c.Index, BlockToolCall)
		b.args += c.Arguments
		if c.Name != "" && !b.named {
			b.name = c.Name
			b.named = true
		}
		if c.ID != "" && b.id == "" {
			b.id = c.ID
		}
		// ⚠️ 压制的工具**不算提交**，也不对外宣告。
		if e.shouldSuppress(b.name) {
			b.suppressed = true
			return nil
		}
		e.state.commit()

		// ⚠️ 名字还没到 —— 扣住参数。
		// Anthropic 的 tool_use 块没有名字就不能宣告。
		if !b.named {
			b.held += c.Arguments
			return nil
		}
		// 第一次知道名字 —— 宣告这个块，并把扣住的参数一次性倒出来。
		if !b.emitted {
			b.emitted = true
			idx := e.anthropicIdx(c.Index)
			block := map[string]any{
				"type":  "tool_use",
				"id":    b.id,
				"name":  renameTools(e.rename, b.name),
				"input": map[string]any{},
			}
			if err := e.emit("content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         idx,
				"content_block": block,
			}); err != nil {
				return err
			}
			if b.held != "" {
				if err := e.emit("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]any{
						"type":         "input_json_delta",
						"partial_json": b.held,
					},
				}); err != nil {
					return err
				}
				b.held = ""
			}
		}
		return e.emit("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": e.anthropicIdx(c.Index),
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": c.Arguments,
			},
		})

	case ChunkUsage:
		if c.Usage != nil {
			e.state.usage = *c.Usage
			e.inputTokens = c.Usage.Input
		}
		return nil

	case ChunkBlockEnd:
		b := e.state.slot(c.Index, c.BlockType)
		if b.kind == BlockToolCall {
			if !b.named {
				// 到块结束还没名字 —— 坏掉的工具调用，丢掉。
				return nil
			}
			if b.emitted {
				// 工具块由它自己的 block-end 关闭 ——
				// 并行工具调用因此可以同时开着。
				return e.emit("content_block_stop", map[string]any{
					"type":  "content_block_stop",
					"index": e.anthropicIdx(c.Index),
				})
			}
			return nil
		}
		return nil
	}
	return nil
}

func (e *MessagesEncoder) Committed() bool { return e.state.committed }

func (e *MessagesEncoder) Finish(finish FinishReason, usage *Usage) error {
	if e.state.finished {
		return nil
	}
	e.state.finished = true
	if finish == FinishError || finish == FinishAborted {
		// ⚠️ 错误/取消不发 message_delta ——
		// 一个 stop_reason 会声称这轮是正常结束的。
		if err := e.emit("error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": "the turn did not complete",
			},
		}); err != nil {
			return err
		}
		return e.emit("message_stop", map[string]any{"type": "message_stop"})
	}
	if err := e.ensureMessageStart(); err != nil {
		return err
	}
	if err := e.closeOpenBlock(); err != nil {
		return err
	}
	u := e.state.usage
	if usage != nil {
		u = *usage
	}
	if err := e.emit("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   anthropicStopReason(finish),
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"input_tokens":  u.Input,
			"output_tokens": u.Output,
		},
	}); err != nil {
		return err
	}
	return e.emit("message_stop", map[string]any{"type": "message_stop"})
}

// anthropicStopReason 把统一结束原因映射成 Anthropic 的 stop_reason。
func anthropicStopReason(f FinishReason) string {
	switch f {
	case FinishToolCalls:
		return "tool_use"
	case FinishMaxTokens:
		return "max_tokens"
	default:
		return "end_turn"
	}
}
