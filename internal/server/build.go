package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/aBon579/free-model-house/internal/lane"
)

// randRead 读随机字节。
func randRead(b []byte) (int, error) { return rand.Read(b) }

// hexEncode 编码成 hex。
func hexEncode(b []byte) string { return hex.EncodeToString(b) }

// BuildChatCompletion 构造一个 OpenAI Chat 的非流式响应。
func BuildChatCompletion(turn *lane.Turn, usage *lane.Usage, finish lane.FinishReason, reqID string) map[string]any {
	content := ""
	toolCalls := []any{}
	for _, m := range turn.Messages {
		if m.Role != lane.RoleAssistant {
			continue
		}
		for _, p := range m.Parts {
			switch p.Type {
			case lane.PartText:
				content += p.Text
			case lane.PartToolCall:
				toolCalls = append(toolCalls, map[string]any{
					"id":   p.ToolCallID,
					"type": "function",
					"function": map[string]any{
						"name":      p.ToolName,
						"arguments": p.Arguments,
					},
				})
			}
		}
	}
	message := map[string]any{"role": "assistant", "content": content}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	choice := map[string]any{
		"index":         0,
		"message":       message,
		"finish_reason": chatFinish(finish),
	}
	// ⚠️ **思考 token 要带出去** ——
	//
	// OpenAI 的形状是 `usage.completion_tokens_details.reasoning_tokens`。
	// 思考 token 是**算在** `completion_tokens` 里面的 ——
	// 不区分的话，"答案有多长"会被高估。
	usageObj := map[string]any{
		"prompt_tokens":     usage.Input,
		"completion_tokens": usage.Output,
		"total_tokens":      usage.Total,
	}
	if usage.Reasoning > 0 {
		usageObj["completion_tokens_details"] = map[string]any{
			"reasoning_tokens": usage.Reasoning,
		}
	}
	return map[string]any{
		"id":      "chatcmpl-" + reqID,
		"object":  "chat.completion",
		"created": nowUnix(),
		"model":   turn.Model,
		"choices": []any{choice},
		"usage":   usageObj,
	}
}

// chatFinish 把统一结束原因映射成 Chat 的 finish_reason。
func chatFinish(f lane.FinishReason) string {
	switch f {
	case lane.FinishToolCalls:
		return "tool_calls"
	case lane.FinishMaxTokens:
		return "length"
	default:
		return "stop"
	}
}

// BuildAnthropicMessage 构造一个 Anthropic Messages 的非流式响应。
//
// ⚠️ 内容顺序遵循 Anthropic 的规则：思考、然后文本、然后每个调用一个 tool_use。
//
// ⚠️ 思考块**不带 signature** ——
// 空字符串会是一个"重放的客户端无法验证的谎言"。
func BuildAnthropicMessage(turn *lane.Turn, usage *lane.Usage, finish lane.FinishReason, reqID string) map[string]any {
	var content []any
	for _, m := range turn.Messages {
		if m.Role != lane.RoleAssistant {
			continue
		}
		for _, p := range m.Parts {
			switch p.Type {
			case lane.PartReasoning:
				content = append(content, map[string]any{
					"type": "thinking", "thinking": p.Text,
				})
			case lane.PartText:
				content = append(content, map[string]any{
					"type": "text", "text": p.Text,
				})
			case lane.PartToolCall:
				input := map[string]any{}
				if p.Arguments != "" {
					_ = jsonUnmarshal([]byte(p.Arguments), &input)
				}
				content = append(content, map[string]any{
					"type": "tool_use", "id": p.ToolCallID,
					"name": p.ToolName, "input": input,
				})
			}
		}
	}
	if content == nil {
		content = []any{}
	}
	// ⚠️ 撞到上限时，stop_reason 是 max_tokens ——
	// 即使这一轮里有工具调用。
	stopReason := "end_turn"
	switch finish {
	case lane.FinishToolCalls:
		stopReason = "tool_use"
	case lane.FinishMaxTokens:
		stopReason = "max_tokens"
	}
	return map[string]any{
		"id":            "msg_" + reqID,
		"type":          "message",
		"role":          "assistant",
		"model":         turn.Model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  usage.Input,
			"output_tokens": usage.Output,
		},
	}
}

// BuildResponsesObject 构造一个 OpenAI Responses 的非流式响应。
func BuildResponsesObject(turn *lane.Turn, usage *lane.Usage, finish lane.FinishReason, reqID string) map[string]any {
	var output []any
	for _, m := range turn.Messages {
		if m.Role != lane.RoleAssistant {
			continue
		}
		for _, p := range m.Parts {
			switch p.Type {
			case lane.PartText:
				output = append(output, map[string]any{
					"type":   "message",
					"id":     "msg_" + reqID,
					"role":   "assistant",
					"status": "completed",
					"content": []any{map[string]any{
						"type": "output_text", "text": p.Text,
					}},
				})
			case lane.PartToolCall:
				output = append(output, map[string]any{
					"type":      "function_call",
					"id":        "fc_" + p.ToolCallID,
					"call_id":   p.ToolCallID,
					"name":      p.ToolName,
					"arguments": p.Arguments,
					"status":    "completed",
				})
			}
		}
	}
	if output == nil {
		output = []any{}
	}
	status := "completed"
	if finish == lane.FinishMaxTokens {
		status = "incomplete"
	}
	resp := map[string]any{
		"id":     "resp_" + reqID,
		"object": "response",
		"status": status,
		"model":  turn.Model,
		"output": output,
		"usage": map[string]any{
			"input_tokens":  usage.Input,
			"output_tokens": usage.Output,
			"total_tokens":  usage.Total,
			"input_tokens_details": map[string]any{
				"cached_tokens": usage.CacheRead,
			},
			"output_tokens_details": map[string]any{
				"reasoning_tokens": usage.Reasoning,
			},
		},
	}
	if status == "incomplete" {
		resp["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	return resp
}

// jsonUnmarshal 是 json.Unmarshal 的包装。
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// nowUnix 返回当前秒。
func nowUnix() int64 { return time.Now().Unix() }

// trimSpaceString 去空白。
func trimSpaceString(s string) string { return strings.TrimSpace(s) }
