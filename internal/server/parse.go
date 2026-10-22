package server

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/aBon579/free-model-house/internal/lane"
)

// 本文件是**入站请求的解析** —— 把三种线上协议翻译成统一形式。
// 详见 docs/design/00-overview.md §5.3。

// errJSON 是"请求体不是 JSON"。
var errJSON = errors.New("request body is not valid JSON")

// errNoModel 是"没给模型"。
var errNoModel = errors.New("model is required")

// jsonMarshal 是 json.Marshal 的包装。
func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// store_NewRequestID 铸一个请求 id。
func store_NewRequestID() string {
	return "req-" + randomHex(8)
}

// randomHex 生成 n 字节的随机 hex。
func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := randRead(b); err != nil {
		return "00000000"
	}
	return hexEncode(b)
}

// turnFromChat 解析一个 OpenAI Chat 请求。
func (g *Gateway) turnFromChat(body map[string]any) (*lane.Turn, bool, error) {
	model, _ := body["model"].(string)
	if strings.TrimSpace(model) == "" {
		return nil, false, errNoModel
	}
	stream, _ := body["stream"].(bool)

	turn := &lane.Turn{
		Model:  model,
		Stream: stream,
	}
	// 档位。
	if eff, ok := body["reasoning_effort"].(string); ok && eff != "" {
		turn.Effort = lane.NormalizeEffort(eff)
	}
	// 消息。
	msgs, err := parseChatMessages(body["messages"])
	if err != nil {
		return nil, false, err
	}
	turn.Messages = msgs
	// 工具。
	turn.Tools = parseChatTools(body["tools"])
	if mt, ok := body["max_tokens"].(float64); ok && mt > 0 {
		turn.MaxTokens = int(mt)
	}
	// 会话种子 —— 用于绑定稳定的**上游** session。
	turn.Seed = seedOf(model, msgs)
	turn.ClientID = "chat"
	return turn, stream, nil
}

// parseChatMessages 解析 Chat 线的消息。
func parseChatMessages(raw any) ([]lane.Message, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, nil
	}
	out := make([]lane.Message, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := m["role"].(string)
		msg := lane.Message{Role: lane.Role(role)}
		switch role {
		case "tool":
			// 工具结果。
			id, _ := m["tool_call_id"].(string)
			text := contentToText(m["content"])
			msg.Role = lane.RoleTool
			msg.Parts = []lane.Part{lane.ToolResultPart(id, text, false)}
		case "assistant":
			// 文本 + 工具调用。
			if t := contentToText(m["content"]); t != "" {
				msg.Parts = append(msg.Parts, lane.TextPart(t))
			}
			if calls, ok := m["tool_calls"].([]any); ok {
				for _, c := range calls {
					call, ok := c.(map[string]any)
					if !ok {
						continue
					}
					id, _ := call["id"].(string)
					fn, _ := call["function"].(map[string]any)
					name, _ := fn["name"].(string)
					args, _ := fn["arguments"].(string)
					msg.Parts = append(msg.Parts, lane.ToolCallPart(id, name, args))
				}
			}
		default:
			// 用户/系统消息：文本 + 多模态。
			msg.Parts = parseChatContent(m["content"])
			if role == "" {
				msg.Role = lane.RoleUser
			}
		}
		if len(msg.Parts) == 0 && role != "system" {
			continue
		}
		out = append(out, msg)
	}
	return out, nil
}

// parseChatContent 解析 Chat 线的内容（字符串或块数组）。
func parseChatContent(raw any) []lane.Part {
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []lane.Part{lane.TextPart(v)}
	case []any:
		var out []lane.Part
		for _, item := range v {
			blk, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch blk["type"] {
			case "text":
				if t, ok := blk["text"].(string); ok {
					out = append(out, lane.TextPart(t))
				}
			case "image_url":
				// {"type":"image_url","image_url":{"url":"data:..."}}
				iu, ok := blk["image_url"].(map[string]any)
				if !ok {
					continue
				}
				url, _ := iu["url"].(string)
				if url != "" {
					out = append(out, lane.ImagePart(url))
				}
			case "input_audio":
				// {"type":"input_audio","input_audio":{"data":"...","format":"wav"}}
				ia, ok := blk["input_audio"].(map[string]any)
				if !ok {
					continue
				}
				data, _ := ia["data"].(string)
				format, _ := ia["format"].(string)
				out = append(out, lane.Part{
					Type: lane.PartAudio, AudioBase64: data, AudioFormat: format,
				})
			case "file":
				// {"type":"file","file":{"filename":"x.pdf","file_data":"data:..."}}
				f, ok := blk["file"].(map[string]any)
				if !ok {
					continue
				}
				name, _ := f["filename"].(string)
				data, _ := f["file_data"].(string)
				out = append(out, lane.Part{
					Type: lane.PartFile, FileName: name, FileBase64: data,
				})
			}
		}
		return out
	}
	return nil
}

// contentToText 把内容转成文本。
func contentToText(raw any) string {
	switch v := raw.(type) {
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, item := range v {
			blk, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if t, ok := blk["text"].(string); ok {
				sb.WriteString(t)
			}
		}
		return sb.String()
	}
	return ""
}

// parseChatTools 解析 Chat 线的工具。
func parseChatTools(raw any) []lane.ToolDef {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []lane.ToolDef
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		def := lane.ToolDef{}
		// 两种形状：flat 与 {"function":{...}}。
		if fn, ok := m["function"].(map[string]any); ok {
			m = fn
		}
		def.Name, _ = m["name"].(string)
		def.Description, _ = m["description"].(string)
		if def.Name == "" {
			continue
		}
		if params, ok := m["parameters"].(map[string]any); ok {
			def.Parameters = params
		} else {
			def.Parameters = map[string]any{"type": "object"}
		}
		out = append(out, def)
	}
	return out
}

// turnFromResponses 解析一个 OpenAI Responses 请求。
func (g *Gateway) turnFromResponses(body map[string]any) (*lane.Turn, bool, error) {
	model, _ := body["model"].(string)
	if strings.TrimSpace(model) == "" {
		return nil, false, errNoModel
	}
	stream, _ := body["stream"].(bool)
	turn := &lane.Turn{
		Model:  model,
		Stream: stream,
	}
	if eff, ok := body["reasoning_effort"].(string); ok && eff != "" {
		turn.Effort = lane.NormalizeEffort(eff)
	}
	// instructions → 系统提示。
	var msgs []lane.Message
	if instr, ok := body["instructions"].(string); ok && instr != "" {
		msgs = append(msgs, lane.Message{
			Role: lane.RoleSystem, Parts: []lane.Part{lane.TextPart(instr)},
		})
	}
	// input 数组。
	if items, ok := body["input"].([]any); ok {
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				// input 也可以是一个纯字符串。
				if s, ok := item.(string); ok {
					msgs = append(msgs, lane.Message{
						Role: lane.RoleUser, Parts: []lane.Part{lane.TextPart(s)},
					})
				}
				continue
			}
			switch m["type"] {
			case "function_call":
				id, _ := m["call_id"].(string)
				name, _ := m["name"].(string)
				args, _ := m["arguments"].(string)
				msgs = append(msgs, lane.Message{
					Role:  lane.RoleAssistant,
					Parts: []lane.Part{lane.ToolCallPart(id, name, args)},
				})
			case "function_call_output":
				id, _ := m["call_id"].(string)
				out, _ := m["output"].(string)
				msgs = append(msgs, lane.Message{
					Role:  lane.RoleTool,
					Parts: []lane.Part{lane.ToolResultPart(id, out, false)},
				})
			case "message":
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				msgs = append(msgs, lane.Message{
					Role:  lane.Role(role),
					Parts: parseResponsesContent(m["content"]),
				})
			default:
				// 没有 type 字段 —— 当普通消息。
				role, _ := m["role"].(string)
				if role == "" {
					role = "user"
				}
				msgs = append(msgs, lane.Message{
					Role:  lane.Role(role),
					Parts: parseResponsesContent(m["content"]),
				})
			}
		}
	}
	turn.Messages = msgs
	turn.Tools = parseChatTools(body["tools"])
	if mt, ok := body["max_output_tokens"].(float64); ok && mt > 0 {
		turn.MaxTokens = int(mt)
	}
	turn.Seed = seedOf(model, msgs)
	turn.ClientID = "responses"
	return turn, stream, nil
}

// parseResponsesContent 解析 Responses 线的内容。
func parseResponsesContent(raw any) []lane.Part {
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []lane.Part{lane.TextPart(v)}
	case []any:
		var out []lane.Part
		for _, item := range v {
			blk, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch blk["type"] {
			case "input_text", "output_text", "text":
				if t, ok := blk["text"].(string); ok {
					out = append(out, lane.TextPart(t))
				}
			case "input_image":
				url, _ := blk["image_url"].(string)
				if url == "" {
					// 也可能是 {"image_url":{"url":...}}
					if iu, ok := blk["image_url"].(map[string]any); ok {
						url, _ = iu["url"].(string)
					}
				}
				if url != "" {
					out = append(out, lane.ImagePart(url))
				}
			case "input_file":
				name, _ := blk["filename"].(string)
				data, _ := blk["file_data"].(string)
				out = append(out, lane.Part{
					Type: lane.PartFile, FileName: name, FileBase64: data,
				})
			}
		}
		return out
	}
	return nil
}

// turnFromMessages 解析一个 Anthropic Messages 请求。
func (g *Gateway) turnFromMessages(body map[string]any) (*lane.Turn, bool, error) {
	model, _ := body["model"].(string)
	if strings.TrimSpace(model) == "" {
		return nil, false, errNoModel
	}
	stream, _ := body["stream"].(bool)
	turn := &lane.Turn{
		Model:  model,
		Stream: stream,
	}
	// ⚠️ Anthropic 的思考预算是 `thinking.budget_tokens` —— 要**分桶**到统一档位。
	if th, ok := body["thinking"].(map[string]any); ok {
		// ⚠️ `type: "disabled"` 要**先**判 ——
		// 否则一个带 budget_tokens 的 disabled 请求会被当成"有思考"。
		if t, _ := th["type"].(string); t == "disabled" {
			turn.Effort = lane.EffortOff
		} else if budget, ok := th["budget_tokens"].(float64); ok {
			turn.Effort = effortFromBudget(int(budget))
		}
	}
	var msgs []lane.Message
	// system 是独立字段。
	if sys, ok := body["system"].(string); ok && sys != "" {
		msgs = append(msgs, lane.Message{
			Role: lane.RoleSystem, Parts: []lane.Part{lane.TextPart(sys)},
		})
	}
	if items, ok := body["messages"].([]any); ok {
		for _, item := range items {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			parts := parseAnthropicContent(m["content"])
			if len(parts) == 0 {
				continue
			}
			msgs = append(msgs, lane.Message{
				Role: lane.Role(role), Parts: parts,
			})
		}
	}
	turn.Messages = msgs
	turn.Tools = parseAnthropicTools(body["tools"])
	if mt, ok := body["max_tokens"].(float64); ok && mt > 0 {
		turn.MaxTokens = int(mt)
	}
	turn.Seed = seedOf(model, msgs)
	turn.ClientID = "messages"
	return turn, stream, nil
}

// effortFromBudget 把 Anthropic 的思考预算分桶到统一档位。
func effortFromBudget(budget int) string {
	switch {
	case budget <= 0:
		// ⚠️ Anthropic 的 `budget_tokens: 0` 表示**不思考** ——
		// 它是"关掉思考"的语义，不是"最轻的思考"。
		return lane.EffortOff
	case budget >= 16384:
		return "deep"
	case budget >= 4096:
		return "balanced"
	default:
		return "light"
	}
}

// parseAnthropicContent 解析 Anthropic 的内容块。
func parseAnthropicContent(raw any) []lane.Part {
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []lane.Part{lane.TextPart(v)}
	case []any:
		var out []lane.Part
		for _, item := range v {
			blk, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch blk["type"] {
			case "text":
				if t, ok := blk["text"].(string); ok {
					out = append(out, lane.TextPart(t))
				}
			case "thinking":
				if t, ok := blk["thinking"].(string); ok {
					out = append(out, lane.Part{Type: lane.PartReasoning, Text: t})
				}
			case "image":
				src, ok := blk["source"].(map[string]any)
				if !ok {
					continue
				}
				mediaType, _ := src["media_type"].(string)
				data, _ := src["data"].(string)
				url := "data:" + mediaType + ";base64," + data
				out = append(out, lane.ImagePart(url))
			case "document":
				src, ok := blk["source"].(map[string]any)
				if !ok {
					continue
				}
				mediaType, _ := src["media_type"].(string)
				data, _ := src["data"].(string)
				out = append(out, lane.Part{
					Type: lane.PartFile, MediaType: mediaType, FileBase64: data,
				})
			case "tool_use":
				id, _ := blk["id"].(string)
				name, _ := blk["name"].(string)
				args := "{}"
				if input, ok := blk["input"].(map[string]any); ok {
					if b, err := json.Marshal(input); err == nil {
						args = string(b)
					}
				}
				out = append(out, lane.ToolCallPart(id, name, args))
			case "tool_result":
				id, _ := blk["tool_use_id"].(string)
				text := contentToText(blk["content"])
				out = append(out, lane.ToolResultPart(id, text, false))
			case "redacted_thinking":
				// ⚠️ 不透明的、账号绑定的密文 —— 丢掉。
				continue
			}
		}
		return out
	}
	return nil
}

// parseAnthropicTools 解析 Anthropic 的工具。
func parseAnthropicTools(raw any) []lane.ToolDef {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	var out []lane.ToolDef
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		def := lane.ToolDef{}
		def.Name, _ = m["name"].(string)
		def.Description, _ = m["description"].(string)
		if def.Name == "" {
			continue
		}
		if schema, ok := m["input_schema"].(map[string]any); ok {
			def.Parameters = schema
		} else {
			def.Parameters = map[string]any{"type": "object"}
		}
		out = append(out, def)
	}
	return out
}

// seedOf 算一个会话种子。
// ⚠️ 取**首条用户消息**的前 200 字符 ——
// 这样同一话题能复用同一个上游 session（命中 prompt cache），
// 而不同话题不会挤在一起。
func seedOf(model string, msgs []lane.Message) string {
	first := ""
	for _, m := range msgs {
		if m.Role != lane.RoleUser {
			continue
		}
		first = m.TextOf()
		break
	}
	if len(first) > 200 {
		first = first[:200]
	}
	return model + "\x00" + first
}
