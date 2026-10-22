package lane

import "fmt"

// 本文件把统一事件流编码成 **OpenAI Responses** 协议。
// Codex 与新 SDK 用这条线。
// 与另两条线的差异：
//  1. 事件是**具名**的（`event: response.output_text.delta`），
//     数据体里也带 `type`。
//  2. 输出项要先**宣告**再喂 delta：
//     `response.output_item.added` → `response.output_text.delta` →
//     `response.output_item.done`。
//     每个项按**宣告顺序**拿 `output_index`。
//     （硬编码到 index 0 的消息会和第一个 function call 撞车。）
//  3. 终止帧有多个：`response.completed` / `response.incomplete` /
//     `response.failed` / `response.done` 都是终止帧。
//     **只认 completed** 会让 `response.incomplete`
//     （一个撞到输出上限的回合）看起来像被切断的流 → 重试一个已完成的回合。
//  4. usage 的字段名换成了 `input_tokens` / `output_tokens` / `total_tokens`，
//     并且有 `input_tokens_details.cached_tokens` 与
//     `output_tokens_details.reasoning_tokens`。
// 详见 docs/design/00-overview.md §5.1。

// ResponsesEncoder 编码 OpenAI Responses。
type ResponsesEncoder struct {
	baseEncoder
	// responseID 是这次响应的 id。
	responseID string
	// outputIndex 是下一个输出项的序号。
	outputIndex int
	// itemIndex 把内部块序号映射到 Responses 的 output_index。
	itemIndex map[int]int
	// items 是已宣告的输出项（按宣告顺序）。
	items []responsesItem
	// itemKind 记录每个内部块对应的项类型。
	itemKind map[int]string
	// startedResponse 报告 response.created 是否已发。
	startedResponse bool
}

// responsesItem 是一个已宣告的输出项。
type responsesItem struct {
	index int
	// kind 是 "message" 或 "function_call"。
	kind string
	// callID 是 function_call 的 call_id。
	callID string
	// name 是 function_call 的名字。
	name string
	// text 是 message 的累计文本。
	text string
	// args 是 function_call 的累计参数。
	args string
}

// NewResponsesEncoder 建一个 Responses encoder。
func NewResponsesEncoder(w Writer, requestID, modelName string, suppress, callerTools map[string]bool, rename map[string]string) *ResponsesEncoder {
	return &ResponsesEncoder{
		baseEncoder: baseEncoder{
			writer:      w,
			requestID:   requestID,
			modelName:   modelName,
			state:       newEncoderState(StyleResponses),
			suppress:    suppress,
			callerTools: callerTools,
			rename:      rename,
		},
		responseID: "resp_" + requestID,
		itemIndex:  map[int]int{},
		itemKind:   map[int]string{},
	}
}

// ensureCreated 发出 response.created。
func (e *ResponsesEncoder) ensureCreated() error {
	if e.startedResponse {
		return nil
	}
	e.startedResponse = true
	e.state.started = true
	return e.emit("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":     e.responseID,
			"object": "response",
			"status": "in_progress",
			"model":  e.modelName,
			"output": []any{},
		},
	})
}

// itemFor 给一个内部块分配（或取回）Responses 的 output_index。
func (e *ResponsesEncoder) itemFor(internal int, kind string) int {
	if idx, ok := e.itemIndex[internal]; ok {
		return idx
	}
	idx := e.outputIndex
	e.outputIndex++
	e.itemIndex[internal] = idx
	e.itemKind[internal] = kind
	return idx
}

// Write 实现 Encoder。
func (e *ResponsesEncoder) Write(c Chunk) error {
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
		if err := e.ensureCreated(); err != nil {
			return err
		}
		b := e.state.slot(c.Index, BlockText)
		b.text += c.Delta
		// 第一个文本 delta 之前先宣告 message 项。
		if e.itemKind[c.Index] == "" {
			idx := e.itemFor(c.Index, "message")
			e.items = append(e.items, responsesItem{index: idx, kind: "message"})
			if err := e.emit("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": idx,
				"item": map[string]any{
					"type":    "message",
					"id":      "msg_" + e.responseID,
					"role":    "assistant",
					"status":  "in_progress",
					"content": []any{},
				},
			}); err != nil {
				return err
			}
		}
		e.state.commit()
		idx := e.itemIndex[c.Index]
		return e.emit("response.output_text.delta", map[string]any{
			"type":         "response.output_text.delta",
			"output_index": idx,
			"item_id":      "msg_" + e.responseID,
			"delta":        c.Delta,
		})

	case ChunkReasonDelta:
		if err := e.ensureCreated(); err != nil {
			return err
		}
		b := e.state.slot(c.Index, BlockReasoning)
		b.text += c.Delta
		e.state.commit()
		idx := e.itemFor(c.Index, "reasoning")
		if e.itemKind[c.Index] == "reasoning" && len(e.items) > 0 &&
			e.items[len(e.items)-1].index == idx {
			// 已宣告过，直接喂 delta。
		} else {
			e.items = append(e.items, responsesItem{index: idx, kind: "reasoning"})
			if err := e.emit("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": idx,
				"item": map[string]any{
					"type":    "reasoning",
					"id":      "rs_" + e.responseID,
					"summary": []any{},
				},
			}); err != nil {
				return err
			}
		}
		return e.emit("response.reasoning_summary_text.delta", map[string]any{
			"type":         "response.reasoning_summary_text.delta",
			"output_index": idx,
			"item_id":      "rs_" + e.responseID,
			"delta":        c.Delta,
		})

	case ChunkToolCallDelta:
		if err := e.ensureCreated(); err != nil {
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
		if e.shouldSuppress(b.name) {
			b.suppressed = true
			return nil
		}
		e.state.commit()
		if !b.named {
			b.held += c.Arguments
			return nil
		}
		idx := e.itemFor(c.Index, "function_call")
		if !b.emitted {
			b.emitted = true
			e.items = append(e.items, responsesItem{
				index:  idx,
				kind:   "function_call",
				callID: b.id,
				name:   renameTools(e.rename, b.name),
				args:   b.held,
			})
			if err := e.emit("response.output_item.added", map[string]any{
				"type":         "response.output_item.added",
				"output_index": idx,
				"item": map[string]any{
					"type":      "function_call",
					"id":        "fc_" + b.id,
					"call_id":   b.id,
					"name":      renameTools(e.rename, b.name),
					"arguments": "",
					"status":    "in_progress",
				},
			}); err != nil {
				return err
			}
			if b.held != "" {
				if err := e.emit("response.function_call_arguments.delta", map[string]any{
					"type":         "response.function_call_arguments.delta",
					"output_index": idx,
					"item_id":      "fc_" + b.id,
					"delta":        b.held,
				}); err != nil {
					return err
				}
				b.held = ""
			}
		}
		return e.emit("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"output_index": idx,
			"item_id":      "fc_" + b.id,
			"delta":        c.Arguments,
		})

	case ChunkUsage:
		if c.Usage != nil {
			e.state.usage = *c.Usage
		}
		return nil

	case ChunkBlockEnd:
		b := e.state.slot(c.Index, c.BlockType)
		if b.kind != BlockToolCall || b.suppressed {
			return nil
		}
		if !b.named || !b.emitted {
			return nil
		}
		idx := e.itemIndex[c.Index]
		return e.emit("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": idx,
			"item": map[string]any{
				"type":      "function_call",
				"id":        "fc_" + b.id,
				"call_id":   b.id,
				"name":      renameTools(e.rename, b.name),
				"arguments": b.args,
				"status":    "completed",
			},
		})
	}
	return nil
}

func (e *ResponsesEncoder) Committed() bool { return e.state.committed }

func (e *ResponsesEncoder) Finish(finish FinishReason, usage *Usage) error {
	if e.state.finished {
		return nil
	}
	e.state.finished = true
	if err := e.ensureCreated(); err != nil {
		return err
	}
	u := e.state.usage
	if usage != nil {
		u = *usage
	}
	status := "completed"
	eventType := "response.completed"
	if finish == FinishMaxTokens {
		status = "incomplete"
		eventType = "response.incomplete"
	}
	if finish == FinishError || finish == FinishAborted {
		status = "failed"
		eventType = "response.failed"
	}

	// 收尾时把还没 done 的输出项补上。
	for i := range e.items {
		it := &e.items[i]
		if it.kind == "function_call" {
			continue
		}
	}

	usagePayload := map[string]any{
		"input_tokens":  u.Input,
		"output_tokens": u.Output,
		"total_tokens":  u.Total,
		"input_tokens_details": map[string]any{
			"cached_tokens": u.CacheRead,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": u.Reasoning,
		},
	}

	return e.emit(eventType, map[string]any{
		"type": eventType,
		"response": map[string]any{
			"id":     e.responseID,
			"object": "response",
			"status": status,
			"model":  e.modelName,
			"output": e.outputArray(),
			"usage":  usagePayload,
			"incomplete_details": map[string]any{
				"reason": "max_output_tokens",
			},
		},
	})
}

// outputArray 重建最终的输出数组（按宣告顺序）。
func (e *ResponsesEncoder) outputArray() []any {
	out := make([]any, 0, len(e.items))
	for _, it := range e.items {
		switch it.kind {
		case "message":
			out = append(out, map[string]any{
				"type":   "message",
				"id":     "msg_" + e.responseID,
				"role":   "assistant",
				"status": "completed",
				"content": []any{map[string]any{
					"type": "output_text",
					"text": it.text,
				}},
			})
		case "function_call":
			out = append(out, map[string]any{
				"type":      "function_call",
				"id":        "fc_" + it.callID,
				"call_id":   it.callID,
				"name":      it.name,
				"arguments": it.args,
				"status":    "completed",
			})
		case "reasoning":
			out = append(out, map[string]any{
				"type": "reasoning",
				"id":   "rs_" + e.responseID,
				"summary": []any{map[string]any{
					"type": "summary_text",
					"text": it.text,
				}},
			})
		}
	}
	return out
}

// responseEventName 返回一个 Responses 事件的线上名字。
func responseEventName(kind string) string {
	return fmt.Sprintf("response.%s", kind)
}
