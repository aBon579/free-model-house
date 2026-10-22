package lane

import (
	"encoding/json"
	"strings"
)

// **工具调用的配对修复** —— 不是可选的清理工作，而是一道**防线**。
//
// 被中断的回合会在**持久化的历史**里留下悬空的 tool_call，
// 重放它 → 上游 400 拒绝 → 而且这个 400 会**污染整个会话**：
// 之后每一轮都失败，因为那个坏记录还住在历史里。
//
// 要删掉三类东西：
//  1. 悬空的 tool_call —— 模型调了，但历史里没有对应结果
//  2. 孤立的 tool_result —— 有结果，但历史里没有对应调用
//  3. 名字为空的 tool_call —— 到达宿主时是 `unknown tool ""`，
//     一个**已经被回答过**的毒丸（配对修复会把它永远留在历史里）
//
// 详见 docs/design/00-overview.md §5.3.1。

// RepairToolPairing 删掉悬空的工具调用、孤立的工具结果与无名工具调用。
//
// 它**就地修改** msgs 并返回它，方便链式调用。
func RepairToolPairing(msgs []Message) []Message {
	if len(msgs) == 0 {
		return msgs
	}

	// 第一遍：收集还"活着"的调用。
	// 有结果的工具调用（tool_result 就在后面）也算活着 ——
	// 它是被回答过的，只是结果在下一条消息里。
	answered := map[string]bool{}
	present := map[string]bool{}
	for _, m := range msgs {
		for _, p := range m.Parts {
			if p.Type == PartToolCall && p.ToolCallID != "" && p.ToolName != "" {
				present[p.ToolCallID] = true
			}
			if p.Type == PartToolResult && p.ToolCallID != "" {
				answered[p.ToolCallID] = true
			}
		}
	}

	// 第二遍：重建消息，删掉该删的。
	out := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != RoleAssistant {
			out = append(out, m)
			continue
		}
		kept := make([]Part, 0, len(m.Parts))
		for _, p := range m.Parts {
			switch p.Type {
			case PartToolCall:
				// 无名调用是毒丸，删。
				if p.ToolName == "" {
					continue
				}
				// 悬空调用 —— 没有结果。删。
				if !answered[p.ToolCallID] {
					continue
				}
				kept = append(kept, p)
			case PartToolResult:
				// 孤立结果 —— 没有对应调用。删。
				if !present[p.ToolCallID] {
					continue
				}
				kept = append(kept, p)
			default:
				kept = append(kept, p)
			}
		}
		// 整条消息被清空了就丢掉它 —— 留着一条空 assistant 消息
		// 也会让某些上游困惑。
		if len(kept) == 0 {
			continue
		}
		m.Parts = kept
		out = append(out, m)
	}
	return out
}

// toolResultText 把一条工具结果渲染成文本。
func toolResultText(p Part) string {
	if p.IsError {
		return "error: " + p.Text
	}
	return p.Text
}

// isValidJSONObject 判断一段文本是不是合法的 JSON 对象。
//
// 用途：工具参数被截断时（max_tokens 到了）它不是合法 JSON，要标记出来 ——
// "finish_reason 说是 tool_calls"并不可信，上游可能在 JSON 中间被切断了。
func isValidJSONObject(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	var v map[string]any
	return json.Unmarshal([]byte(s), &v) == nil
}
