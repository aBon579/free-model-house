package lane

import (
	"encoding/json"
	"strings"
	"testing"
)

// testWriter 收集写出的内容。
type testWriter struct {
	buf    strings.Builder
	sent   bool
	status int
}

func (w *testWriter) WriteSSE(name string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	w.sent = true
	w.buf.WriteString("data: ")
	w.buf.Write(payload)
	w.buf.WriteString("\n\n")
	return nil
}

func (w *testWriter) WriteJSON(status int, payload any) error {
	w.sent = true
	w.status = status
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	w.buf.Write(b)
	return nil
}

func (w *testWriter) HeadersSent() bool { return w.sent }

// TestChatUsageCarriesReasoningTokens 验证 **usage 里有思考 token**。
//
// ⚠️ OpenAI 的形状是**嵌套**的：
// `usage.completion_tokens_details.reasoning_tokens`
//
// 不带的话，客户端看不到"思考占了多少 token" ——
// 而思考 token 是**算在** `completion_tokens` 里面的，
// 不区分的话"答案有多长"就被高估了。
func TestChatUsageCarriesReasoningTokens(t *testing.T) {
	w := &testWriter{}
	enc := NewChatEncoder(w, "req-1", "m", nil, nil, nil)
	// 思考 + 文本。
	if err := enc.Write(Chunk{Kind: ChunkReasonDelta, Index: 0, Delta: "thinking..."}); err != nil {
		t.Fatalf("写思考块失败: %v", err)
	}
	if err := enc.Write(Chunk{Kind: ChunkTextDelta, Index: 1, Delta: "answer"}); err != nil {
		t.Fatalf("写文本块失败: %v", err)
	}
	if err := enc.Write(Chunk{Kind: ChunkUsage, Usage: &Usage{
		Input: 100, Output: 40, Reasoning: 39, Total: 140,
	}}); err != nil {
		t.Fatalf("写用量失败: %v", err)
	}
	out := enc.Finish(FinishStop, nil)
	if out != nil {
		t.Fatalf("收尾失败: %v", out)
	}

	// 找到带 usage 的那一帧。
	var found bool
	for _, line := range strings.Split(w.buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue
		}
		usage, ok := frame["usage"].(map[string]any)
		if !ok {
			continue
		}
		found = true
		details, ok := usage["completion_tokens_details"].(map[string]any)
		if !ok {
			t.Fatalf("usage 缺 completion_tokens_details: %v", usage)
		}
		if details["reasoning_tokens"] != float64(39) {
			t.Fatalf("思考 token 不对: %v", details["reasoning_tokens"])
		}
	}
	if !found {
		t.Fatal("没找到带 usage 的帧")
	}
}

// TestChatUsageOmitsEmptyDetails 验证**没有思考时不塞空结构**。
//
// ⚠️ 塞一个空的 `completion_tokens_details: {}` 会让某些客户端
//
//	以为"思考 token 是 0"而不是"没测过"。
func TestChatUsageOmitsEmptyDetails(t *testing.T) {
	w := &testWriter{}
	enc := NewChatEncoder(w, "req-1", "m", nil, nil, nil)
	if err := enc.Write(Chunk{Kind: ChunkUsage, Usage: &Usage{
		Input: 10, Output: 5, Total: 15,
	}}); err != nil {
		t.Fatalf("写用量失败: %v", err)
	}
	enc.Finish(FinishStop, nil)
	if strings.Contains(w.buf.String(), "completion_tokens_details") {
		t.Fatalf("不该塞空的 completion_tokens_details: %s", w.buf.String())
	}
}

// TestDecodeOpenRouterNestedReasoningTokens 验证**能解出嵌套的思考 token**。
//
// ⚠️ 实测（2026-10-19）：OpenRouter 的 usage 形状是
//
//	{"prompt_tokens":206,"completion_tokens":40,"total_tokens":246,
//	 "completion_tokens_details":{"reasoning_tokens":39}}
//
// ⚠️ 顶层**没有** `reasoning_tokens` ——
//
//	只读顶层会永远得到 0。
func TestDecodeOpenRouterNestedReasoningTokens(t *testing.T) {
	frame := []byte(`{"choices":[],"usage":{"prompt_tokens":206,"completion_tokens":40,` +
		`"total_tokens":246,"completion_tokens_details":{"reasoning_tokens":39}}}`)

	dec := newDecoder(wireChat, nil, func(Chunk) {})
	dec.feedChat(frame)

	if dec.usage.Reasoning != 39 {
		t.Fatalf("思考 token 该是 39，实际 %d", dec.usage.Reasoning)
	}
	if dec.usage.Output != 40 {
		t.Fatalf("输出 token 该是 40，实际 %d", dec.usage.Output)
	}
}

// TestDecodeFlatReasoningTokens 验证**顶层的思考 token 也认**。
//
// 有些上游（不是 OpenRouter）用扁平形状 —— 两个都要支持。
func TestDecodeFlatReasoningTokens(t *testing.T) {
	frame := []byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,` +
		`"total_tokens":30,"reasoning_tokens":7}}`)

	dec := newDecoder(wireChat, nil, func(Chunk) {})
	dec.feedChat(frame)

	if dec.usage.Reasoning != 7 {
		t.Fatalf("思考 token 该是 7，实际 %d", dec.usage.Reasoning)
	}
}

// TestDecodeCostUSD 验证**费用能被解出来**。
//
// ⚠️ 只有 OpenRouter 报 `usage.cost` ——
//
//	免密车道的 usage 里没有这个字段（解出来是 0，这是对的）。
//
// ⚠️ **钱不能按整数记** ——
//
//	一次调用可能是 $0.0000043，
//	按整数截断会变成 0，看起来像"完全没花钱"，
//	而额度在**悄悄消耗**。
func TestDecodeCostUSD(t *testing.T) {
	frame := []byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,` +
		`"total_tokens":30,"cost":0.0000043}}`)

	dec := newDecoder(wireChat, nil, func(Chunk) {})
	dec.feedChat(frame)

	if dec.usage.CostUSD <= 0 {
		t.Fatalf("费用该 > 0，实际 %v", dec.usage.CostUSD)
	}
	// ⚠️ 小额不能被截断。
	if dec.usage.CostUSD == 0 {
		t.Fatal("小额费用被截断了")
	}
}

// TestDecodeCostMissingIsZero 验证**没有 cost 字段时是 0**（不是报错）。
func TestDecodeCostMissingIsZero(t *testing.T) {
	frame := []byte(`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":20,` +
		`"total_tokens":30}}`)

	dec := newDecoder(wireChat, nil, func(Chunk) {})
	dec.feedChat(frame)

	if dec.usage.CostUSD != 0 {
		t.Fatalf("没 cost 字段就该是 0，实际 %v", dec.usage.CostUSD)
	}
}

// TestDecodeCostIsSetNotAccumulated 验证**费用是"设置"不是"累加"**。
//
// ⚠️ OpenRouter 的**最后一帧**带完整的 usage。
//
//	如果中间帧也带 cost，累加就会**翻倍** ——
//	那是"报告了双倍花费"，比不报更糟。
func TestDecodeCostIsSetNotAccumulated(t *testing.T) {
	dec := newDecoder(wireChat, nil, func(Chunk) {})

	dec.feedChat([]byte(`{"choices":[],"usage":{"cost":0.01}}`))
	dec.feedChat([]byte(`{"choices":[],"usage":{"cost":0.01}}`))

	if dec.usage.CostUSD != 0.01 {
		t.Fatalf("费用该是 0.01（不累加），实际 %v", dec.usage.CostUSD)
	}
}
