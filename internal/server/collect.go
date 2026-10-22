package server

import (
	"strings"

	"github.com/aBon579/free-model-house/internal/lane"
)

// 本文件是 **非流式的答案收集**。
//
// ⚠️ 一个容易搞错的事：**非流式的答案不能从 `turn.Messages` 里读。**
// `turn.Messages` 是**请求** —— 它装着用户问了什么、系统提示是什么；
// 模型的**回答**在响应的 chunk 流里。
//
// 所以非流式路径必须：① 跑一次推理，把 chunk 收下来
// ② 从 chunk 里**重建**一条 assistant 消息 ③ 用它构造响应。
//
// 早期版本偷懒从 turn.Messages 读 —— 结果永远返回空内容。
// （这个 bug 是被端到端测试抓出来的，单元测试测不出来。）

// assistantCollector 从 chunk 流里重建助手的回答。
type assistantCollector struct {
	turn *lane.Turn
	text strings.Builder
	// reasoning 是思考内容。
	reasoning strings.Builder
	// toolCalls 是工具调用（按 index 排）。
	toolCalls []collectedToolCall
	finish    lane.FinishReason
}

// collectedToolCall 是收集到的一个工具调用。
type collectedToolCall struct {
	Index int
	ID    string
	Name  string
	// Arguments 是参数（JSON 文本）。
	Arguments strings.Builder
}

// collectAssistant 建一个收集器。
func collectAssistant(turn *lane.Turn) *assistantCollector {
	return &assistantCollector{turn: turn, finish: lane.FinishStop}
}

// add 收一个 chunk。
func (c *assistantCollector) add(chunk lane.Chunk) {
	switch chunk.Kind {
	case lane.ChunkTextDelta:
		c.text.WriteString(chunk.Delta)
	case lane.ChunkReasonDelta:
		c.reasoning.WriteString(chunk.Delta)
	case lane.ChunkToolCallDelta:
		c.addToolCall(chunk)
	case lane.ChunkFinish:
		if chunk.Finish != "" {
			c.finish = chunk.Finish
		}
	}
}

// addToolCall 收一个工具调用的增量。
func (c *assistantCollector) addToolCall(chunk lane.Chunk) {
	// 按 index 找（上游可能先发 name 再发 arguments）。
	idx := chunk.Index
	for i := range c.toolCalls {
		if c.toolCalls[i].Index == idx {
			tc := &c.toolCalls[i]
			if chunk.Name != "" {
				tc.Name = chunk.Name
			}
			if chunk.ID != "" {
				tc.ID = chunk.ID
			}
			tc.Arguments.WriteString(chunk.Arguments)
			return
		}
	}
	// 新的。
	tc := collectedToolCall{Index: idx, Name: chunk.Name, ID: chunk.ID}
	tc.Arguments.WriteString(chunk.Arguments)
	c.toolCalls = append(c.toolCalls, tc)
}

// rebuild 重建出一个带助手回答的 Turn。
func (c *assistantCollector) rebuild() *lane.Turn {
	out := *c.turn
	// 复制消息切片（不要改到原来的）。
	msgs := make([]lane.Message, len(c.turn.Messages))
	copy(msgs, c.turn.Messages)

	// 加一条 assistant 消息。
	assistant := lane.Message{Role: lane.RoleAssistant}
	if c.reasoning.Len() > 0 {
		assistant.Parts = append(assistant.Parts, lane.Part{
			Type: lane.PartReasoning, Text: c.reasoning.String(),
		})
	}
	if c.text.Len() > 0 {
		assistant.Parts = append(assistant.Parts, lane.TextPart(c.text.String()))
	}
	for _, tc := range c.toolCalls {
		args := tc.Arguments.String()
		if args == "" {
			args = "{}"
		}
		assistant.Parts = append(assistant.Parts,
			lane.ToolCallPart(tc.ID, tc.Name, args))
	}
	if len(assistant.Parts) > 0 {
		msgs = append(msgs, assistant)
	}
	out.Messages = msgs
	return &out
}

// finishReason 返回收集到的结束原因。
func (c *assistantCollector) finishReason() lane.FinishReason { return c.finish }
