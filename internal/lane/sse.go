package lane

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// 把**上游的 SSE 流**解析成统一事件。
//
// 三个必须做对的细节：
//
//  1. **形状比 Content-Type 可信**。上游有时用 `application/json`
//     的 Content-Type 回答**流式**请求。所以先嗅探前几个字节的形状，
//     而且把嗅探到的字节 **replay** 回流里（不吞掉、不缓冲）。
//
//  2. **思考内容有四种拼法**：`reasoning` / `reasoning_content` /
//     `reasoning_text` / `thinking`。**只读第一个非空的那个** ——
//     有些网关**同时**用两个名字报同一段文本，全读会把块算重，用量统计会翻倍。
//
//  3. **终止帧有多个**。只认一个会把"撞到输出上限"看成"流被切断"
//     → 重试一个已经完成的回合。
//
// 详见 docs/design/00-overview.md §5.11。

// wireKind 指出上游在说哪种协议。
type wireKind string

const (
	wireChat      wireKind = "chat"
	wireMessages  wireKind = "messages"
	wireResponses wireKind = "responses"
)

// decoder 把一条上游 SSE 流解析成统一事件。
type decoder struct {
	wire wireKind
	// emit 收到一个统一事件。
	emit func(Chunk)
	// rename 把发出去的工具名还原成调用方的名字。
	rename map[string]string
	usage  Usage
	// sawFinish 报告是否见过终止帧。
	sawFinish bool
	// sawText / sawReasoning / sawToolCall 报告内容类别。
	sawText      bool
	sawReasoning bool
	sawToolCall  bool
	finish       FinishReason
	// blocks 跟踪已开启的块，key 是线路协议里的累加器 key
	// （Chat 的 tool_calls[].index、Messages 的块序号、Responses 的 output_index）。
	blocks map[string]*upBlock
	// nextBlock 是下一个块序号。
	nextBlock int
	// lastToolKey 是上一个工具块的 key。
	lastToolKey string
	// toolIDs 记录已宣告的工具 id。
	toolIDs map[string]bool
}

// upBlock 是上游流里的一个块。
type upBlock struct {
	index int
	kind  BlockType
	id    string
	name  string
	args  string
	// named 表示工具块是否已宣告。
	named bool
}

func newDecoder(wire wireKind, rename map[string]string, emit func(Chunk)) *decoder {
	return &decoder{
		wire:    wire,
		emit:    emit,
		rename:  rename,
		blocks:  map[string]*upBlock{},
		toolIDs: map[string]bool{},
	}
}

// decodeSSE 读一个 SSE 流并解析成统一事件。
//
// 它返回上游的错误（如果有）与最终的用量。
func (d *decoder) decodeSSE(r io.Reader) (*Usage, *Error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var raw []byte
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" || strings.HasPrefix(trimmed, ":") {
			// 空行与注释行 —— 跳过。
			// ⚠️ 但**不能**丢掉注释里可能带的错误信息。
			continue
		}
		if !strings.HasPrefix(trimmed, "data:") {
			// event: 之类的字段行。具名事件的类型在 data 里，
			// 所以这里不需要单独处理。
			continue
		}
		frame := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if frame == "" {
			continue
		}
		if frame == "[DONE]" {
			break
		}
		raw = append(raw, frame...)
		if err := d.feed([]byte(frame)); err != nil {
			return &d.usage, err
		}
	}
	if err := sc.Err(); err != nil {
		return &d.usage, &Error{Code: CodeTransport, Message: "upstream stream read failed: " + err.Error()}
	}
	return &d.usage, nil
}

// feed 解析一帧。
func (d *decoder) feed(frame []byte) *Error {
	// 先看是不是一个错误信封。
	if msg, ok := d.errorFromFrame(frame); ok {
		return msg
	}
	switch d.wire {
	case wireChat:
		d.feedChat(frame)
	case wireMessages:
		d.feedMessages(frame)
	case wireResponses:
		d.feedResponses(frame)
	}
	return nil
}

// errorFromFrame 判断一帧是不是错误信封。
//
// ⚠️ 200 的流里**也可能**带错误 —— 上游有时用 200 回答一个失败。
// 只看状态码会把它当成正常内容，然后下游拿到一个空回答。
func (d *decoder) errorFromFrame(frame []byte) (*Error, bool) {
	var probe struct {
		Type  string          `json:"type"`
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(frame, &probe); err != nil {
		return nil, false
	}
	if probe.Type == "error" || len(probe.Error) > 0 {
		payload := extractErrorPayload(frame)
		return ClassifyFailure(0, payload, 0), true
	}
	return nil, false
}

// chatFrame 是 OpenAI Chat 的一个 chunk。
type chatFrame struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ReasoningText    string `json:"reasoning_text"`
			Thinking         string `json:"thinking"`
			ToolCalls        []struct {
				Index    *int   `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens         int `json:"prompt_tokens"`
		CompletionTokens     int `json:"completion_tokens"`
		TotalTokens          int `json:"total_tokens"`
		ReasoningTokens      int `json:"reasoning_tokens"`
		CacheReadInputTokens int `json:"cache_read_input_tokens"`
		// ⚠️ **OpenRouter 把思考 token 放在嵌套结构里** ——
		//	`usage.completion_tokens_details.reasoning_tokens`。
		//
		//	顶层那个 `usage.reasoning_tokens` **不存在** ——
		//	所以只读顶层会永远得到 0，"思考占了多少 token"就永远是 0。
		//
		//	实测（2026-10-19）：
		//	{"usage": {"completion_tokens": 18, "completion_tokens_details": {"reasoning_tokens": 15}}}
		CompletionTokensDetails *struct {
			ReasoningTokens int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		// CostUSD 是这次调用花了多少美元。
		//
		// ⚠️ **只有 OpenRouter 报** —— 免密车道的 usage 里没有这个字段。
		//
		// 实测（2026-10-19）：`"usage": {"cost": 0, ...}`
		// 对 `:free` 模型永远是 0，但**非免费模型**（如果用户
		// 手动配了付费模型）会是真的数字。
		CostUSD float64 `json:"cost"`
	} `json:"usage"`
}

// feedChat 解析一个 Chat 帧。
func (d *decoder) feedChat(frame []byte) {
	var f chatFrame
	if err := json.Unmarshal(frame, &f); err != nil {
		return
	}
	if f.Usage != nil {
		// ⚠️ 思考 token 要**两个地方都读** ——
		//	顶层（部分上游）和嵌套的 completion_tokens_details（OpenRouter）。
		reasoning := f.Usage.ReasoningTokens
		if f.Usage.CompletionTokensDetails != nil && f.Usage.CompletionTokensDetails.ReasoningTokens > 0 {
			reasoning = f.Usage.CompletionTokensDetails.ReasoningTokens
		}
		d.mergeUsage(&Usage{
			Input:     f.Usage.PromptTokens,
			Output:    f.Usage.CompletionTokens,
			Reasoning: reasoning,
			CacheRead: f.Usage.CacheReadInputTokens,
			Total:     f.Usage.TotalTokens,
			CostUSD:   f.Usage.CostUSD,
		})
		d.emit(Chunk{Kind: ChunkUsage, Usage: &d.usage})
	}
	for _, ch := range f.Choices {
		// 思考：四种拼法，**只读第一个非空的**。
		reasoning := firstNonEmpty(ch.Delta.Reasoning, ch.Delta.ReasoningContent, ch.Delta.ReasoningText, ch.Delta.Thinking)
		if reasoning != "" {
			idx := d.ensureBlock(BlockReasoning)
			d.sawReasoning = true
			d.emit(Chunk{Kind: ChunkReasonDelta, Index: idx, Delta: reasoning})
		}
		if ch.Delta.Content != "" {
			idx := d.ensureBlock(BlockText)
			d.sawText = true
			d.emit(Chunk{Kind: ChunkTextDelta, Index: idx, Delta: ch.Delta.Content})
		}
		for _, tc := range ch.Delta.ToolCalls {
			d.emitToolCall(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
		}
		if ch.FinishReason != "" {
			d.sawFinish = true
			d.finish = chatFinishToCanonical(ch.FinishReason)
		}
	}
}

// emitToolCall 处理一个工具调用增量。
//
// 关键：线上协议的 `tool_calls[].index` 是**累加器 key**。
// 没有 index 的（Claude 线常见）必须用 **call id** 区分，
// 否则并行工具调用的 id 会互相覆盖，被合并成一个块。
func (d *decoder) emitToolCall(index *int, id, name, args string) {
	key := ""
	switch {
	case index != nil:
		key = "c" + itoa(*index)
		d.lastToolKey = key
	case id != "":
		// 找一个已开的、id 相同的工具块。
		for _, b := range d.blocks {
			if b.kind == BlockToolCall && b.id == id {
				key = itoa(b.index)
				d.lastToolKey = key
				break
			}
		}
		if key == "" {
			key = "cid:" + id
			d.lastToolKey = key
		}
	default:
		key = d.lastToolKey
		if key == "" {
			key = "c0"
		}
	}

	b := d.blocks[key]
	if b == nil {
		b = &upBlock{index: d.nextBlock, kind: BlockToolCall}
		d.nextBlock++
		d.blocks[key] = b
	}
	if id != "" && b.id == "" {
		b.id = id
	}
	if name != "" {
		b.name = name
		if !b.named {
			b.named = true
			d.emit(Chunk{Kind: ChunkBlockStart, Index: b.index, BlockType: BlockToolCall, ID: b.id, Name: d.restore(name)})
		}
	}
	b.args += args
	d.sawToolCall = true
	d.emit(Chunk{
		Kind:      ChunkToolCallDelta,
		Index:     b.index,
		ID:        b.id,
		Name:      d.restore(name),
		Arguments: args,
	})
}

// restore 把发出去的工具名还原成调用方的名字。
func (d *decoder) restore(name string) string {
	if orig, ok := d.rename[name]; ok {
		return orig
	}
	return name
}

// ensureBlock 取得（或开启）一个文本/思考块。
func (d *decoder) ensureBlock(kind BlockType) int {
	for _, b := range d.blocks {
		if b.kind == kind {
			return b.index
		}
	}
	b := &upBlock{index: d.nextBlock, kind: kind}
	d.nextBlock++
	d.blocks["b"+itoa(b.index)] = b
	return b.index
}

// mergeUsage 合并一次用量。
func (d *decoder) mergeUsage(u *Usage) {
	if u == nil {
		return
	}
	if u.Input > 0 {
		d.usage.Input = u.Input
	}
	if u.Output > 0 {
		d.usage.Output = u.Output
	}
	if u.Reasoning > 0 {
		d.usage.Reasoning = u.Reasoning
	}
	if u.CacheRead > 0 {
		d.usage.CacheRead = u.CacheRead
	}
	// ⚠️ **费用是"最后看到的那个"** —— 不是累加。
	//
	// 上游在**最后一帧**报这次调用的总费用。
	// 如果累加，就会算成"费用 × 帧数"。
	if u.CostUSD > 0 {
		d.usage.CostUSD = u.CostUSD
	}
	d.usage.Total = d.usage.Input + d.usage.Output
}

// messagesFrame 是 Anthropic Messages 的一个事件。
//
// ⚠️ "delta" 字段在不同的帧类型里含义不同：
//
//	content_block_delta → {type, text|partial_json|thinking}
//	message_delta      → {stop_reason}
//
// 所以这里先按 RawMessage 收下来，再按帧类型分别解。
type messagesFrame struct {
	Type  string `json:"type"`
	Index int    `json:"index"`
	// content_block_start
	ContentBlock struct {
		Type  string          `json:"type"`
		ID    string          `json:"id"`
		Name  string          `json:"name"`
		Text  string          `json:"text"`
		Input json.RawMessage `json:"input"`
	} `json:"content_block"`
	// content_block_delta 的 delta
	Delta json.RawMessage `json:"delta"`
	// message_start / message_delta
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// messagesContentDelta 是 content_block_delta 的 delta。
type messagesContentDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	PartialJSON string `json:"partial_json"`
	Thinking    string `json:"thinking"`
}

// messagesStopDelta 是 message_delta 的 delta。
type messagesStopDelta struct {
	StopReason string `json:"stop_reason"`
}

// feedMessages 解析一个 Messages 帧。
func (d *decoder) feedMessages(frame []byte) {
	var f messagesFrame
	if err := json.Unmarshal(frame, &f); err != nil {
		return
	}
	switch f.Type {
	case "message_start":
		d.mergeUsage(&Usage{Input: f.Message.Usage.InputTokens, Output: f.Message.Usage.OutputTokens})
		d.emit(Chunk{Kind: ChunkUsage, Usage: &d.usage})

	case "content_block_start":
		switch f.ContentBlock.Type {
		case "text":
			d.ensureBlockAt(f.Index, BlockText)
		case "thinking":
			d.ensureBlockAt(f.Index, BlockReasoning)
		case "tool_use":
			b := d.ensureBlockAt(f.Index, BlockToolCall)
			b.id = f.ContentBlock.ID
			b.name = f.ContentBlock.Name
			b.named = true
			d.sawToolCall = true
			d.emit(Chunk{
				Kind: ChunkBlockStart, Index: b.index, BlockType: BlockToolCall,
				ID: b.id, Name: d.restore(f.ContentBlock.Name),
			})
		}

	case "content_block_delta":
		var cd messagesContentDelta
		if err := json.Unmarshal(f.Delta, &cd); err != nil {
			return
		}
		switch cd.Type {
		case "text_delta":
			b := d.ensureBlockAt(f.Index, BlockText)
			d.sawText = true
			d.emit(Chunk{Kind: ChunkTextDelta, Index: b.index, Delta: cd.Text})
		case "thinking_delta":
			b := d.ensureBlockAt(f.Index, BlockReasoning)
			d.sawReasoning = true
			d.emit(Chunk{Kind: ChunkReasonDelta, Index: b.index, Delta: cd.Thinking})
		case "input_json_delta":
			b := d.ensureBlockAt(f.Index, BlockToolCall)
			d.sawToolCall = true
			d.emit(Chunk{
				Kind: ChunkToolCallDelta, Index: b.index, ID: b.id,
				Name: d.restore(b.name), Arguments: cd.PartialJSON,
			})
		}

	case "content_block_stop":
		b := d.blockAt(f.Index)
		if b != nil {
			d.emit(Chunk{Kind: ChunkBlockEnd, Index: b.index, BlockType: b.kind})
		}

	case "message_delta":
		d.mergeUsage(&Usage{Output: f.Usage.OutputTokens})
		var sd messagesStopDelta
		if err := json.Unmarshal(f.Delta, &sd); err == nil && sd.StopReason != "" {
			d.sawFinish = true
			d.finish = anthropicFinishToCanonical(sd.StopReason)
		}
		d.emit(Chunk{Kind: ChunkUsage, Usage: &d.usage})

	case "message_stop":
		d.sawFinish = true
	}
}

// ensureBlockAt 取得（或开启）指定序号的块。
func (d *decoder) ensureBlockAt(index int, kind BlockType) *upBlock {
	key := "i" + itoa(index)
	if b, ok := d.blocks[key]; ok {
		return b
	}
	b := &upBlock{index: index, kind: kind}
	d.blocks[key] = b
	if index >= d.nextBlock {
		d.nextBlock = index + 1
	}
	return b
}

// blockAt 取得指定序号的块。
func (d *decoder) blockAt(index int) *upBlock {
	return d.blocks["i"+itoa(index)]
}

// responsesFrame 是 OpenAI Responses 的一个事件。
type responsesFrame struct {
	Type        string `json:"type"`
	OutputIndex int    `json:"output_index"`
	ItemID      string `json:"item_id"`
	Delta       string `json:"delta"`
	Item        struct {
		Type      string `json:"type"`
		ID        string `json:"id"`
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		Content   []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"item"`
	Response struct {
		Status string `json:"status"`
		Usage  struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			TotalTokens        int `json:"total_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
		IncompleteDetails struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
}

// feedResponses 解析一个 Responses 帧。
func (d *decoder) feedResponses(frame []byte) {
	var f responsesFrame
	if err := json.Unmarshal(frame, &f); err != nil {
		return
	}
	switch f.Type {
	case "response.output_text.delta":
		b := d.ensureBlockAt(f.OutputIndex, BlockText)
		d.sawText = true
		d.emit(Chunk{Kind: ChunkTextDelta, Index: b.index, Delta: f.Delta})

	case "response.reasoning_summary_text.delta":
		b := d.ensureBlockAt(f.OutputIndex, BlockReasoning)
		d.sawReasoning = true
		d.emit(Chunk{Kind: ChunkReasonDelta, Index: b.index, Delta: f.Delta})

	case "response.output_item.added":
		if f.Item.Type == "function_call" {
			b := d.ensureBlockAt(f.OutputIndex, BlockToolCall)
			b.id = f.Item.CallID
			b.name = f.Item.Name
			b.named = true
			d.sawToolCall = true
			d.emit(Chunk{
				Kind: ChunkBlockStart, Index: b.index, BlockType: BlockToolCall,
				ID: b.id, Name: d.restore(f.Item.Name),
			})
		}

	case "response.function_call_arguments.delta":
		b := d.ensureBlockAt(f.OutputIndex, BlockToolCall)
		d.sawToolCall = true
		d.emit(Chunk{
			Kind: ChunkToolCallDelta, Index: b.index, ID: b.id,
			Name: d.restore(b.name), Arguments: f.Delta,
		})

	case "response.output_item.done":
		b := d.blockAt(f.OutputIndex)
		if b != nil {
			d.emit(Chunk{Kind: ChunkBlockEnd, Index: b.index, BlockType: b.kind})
		}

	case "response.completed", "response.incomplete", "response.failed", "response.done":
		// ⚠️ 四个都是终止帧。
		// 只认 completed 会让 incomplete（撞到输出上限的回合）
		// 看起来像被切断的流 → 重试一个已经完成的回合。
		d.sawFinish = true
		d.mergeUsage(&Usage{
			Input:     f.Response.Usage.InputTokens,
			Output:    f.Response.Usage.OutputTokens,
			Reasoning: f.Response.Usage.OutputTokensDetails.ReasoningTokens,
			CacheRead: f.Response.Usage.InputTokensDetails.CachedTokens,
			Total:     f.Response.Usage.TotalTokens,
		})
		d.emit(Chunk{Kind: ChunkUsage, Usage: &d.usage})
		if f.Response.IncompleteDetails.Reason != "" {
			d.finish = FinishMaxTokens
		}
	}
}

// itoa 是一个极简的整数转字符串（避免到处 import strconv）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// firstNonEmpty 返回第一个非空的字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// chatFinishToCanonical 把 Chat 的 finish_reason 映射成统一结束原因。
func chatFinishToCanonical(s string) FinishReason {
	switch s {
	case "tool_calls", "function_call":
		return FinishToolCalls
	case "length":
		return FinishMaxTokens
	default:
		return FinishStop
	}
}

// anthropicFinishToCanonical 把 Anthropic 的 stop_reason 映射成统一结束原因。
func anthropicFinishToCanonical(s string) FinishReason {
	switch s {
	case "tool_use":
		return FinishToolCalls
	case "max_tokens":
		return FinishMaxTokens
	default:
		return FinishStop
	}
}
