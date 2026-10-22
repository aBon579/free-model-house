package lane

import (
	"encoding/json"
	"strings"
)

// **清单信封解析**。
// 兼容多种形状：上游改一次响应格式，解析不该崩一次。
// 实测形状（`data` / `models` / `items` / 裸数组，行是对象或纯字符串）：
//	{"data":[{"id":"..."}]}   ← OpenAI 形状（opencode / OpenRouter）
//	[{"id":"..."}]            ← 裸数组
// 详见 docs/design/00-overview.md §5.7。

// listingRow 是清单里的一行（归一化后）。
type listingRow struct {
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	Created int64  `json:"created"`
	// ContextWindow / MaxOutput 是上游声明的上下文窗口与最大输出（0 = 没声明）。
	ContextWindow int `json:"context_length"`
	MaxOutput     int `json:"max_output_tokens"`
	// Vision / Audio / File / Reasoning 是上游声明的模态。
	// ⚠️ 上游不声明时全部为 false。
	Vision    bool `json:"vision"`
	Audio     bool `json:"audio"`
	File      bool `json:"file"`
	Reasoning bool `json:"reasoning"`
	// InputDeclared / OutputDeclared 标记"上游是否声明过"——
	// 与"声明为 false"区分开。
	InputDeclared  bool `json:"inputDeclared,omitempty"`
	OutputDeclared bool `json:"outputDeclared,omitempty"`
}

// listingEnvelope 是清单的顶层信封。
// ⚠️ 三种可能的键都用 RawMessage 收下来，再按存在的那个解。
type listingEnvelope struct {
	Data   json.RawMessage `json:"data"`
	Models json.RawMessage `json:"models"`
	Items  json.RawMessage `json:"items"`
}

// jsonUnmarshal 是 json.Unmarshal 的薄包装（测试与解析共用）。
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// ParseListing 解析一个清单信封，返回归一化的行。
// 接受所有见过的形状：`{"data"/"models"/"items":[...]}` 或裸数组；
// 行可以是对象（取 `id`）或字符串（直接就是 id）。
func ParseListing(payload []byte) []listingRow {
	if len(payload) == 0 {
		return nil
	}
	if trimmed := strings.TrimLeft(string(payload), " \t\r\n"); strings.HasPrefix(trimmed, "[") {
		var rows []json.RawMessage
		if err := json.Unmarshal(payload, &rows); err != nil {
			return nil
		}
		return parseRows(rows)
	}

	var env listingEnvelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil
	}
	for _, raw := range []json.RawMessage{env.Data, env.Models, env.Items} {
		if len(raw) == 0 {
			continue
		}
		var rows []json.RawMessage
		if err := json.Unmarshal(raw, &rows); err != nil {
			// 不是一个数组 —— 继续试下一个键。
			continue
		}
		return parseRows(rows)
	}
	return nil
}

// parseRows 把原始行解析成归一化的行。
func parseRows(rows []json.RawMessage) []listingRow {
	out := make([]listingRow, 0, len(rows))
	for _, raw := range rows {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, listingRow{ID: s})
			}
			continue
		}
		var row listingRow
		if err := json.Unmarshal(raw, &row); err != nil {
			continue
		}
		if row.ID = strings.TrimSpace(row.ID); row.ID == "" {
			continue
		}
		out = append(out, row)
	}
	return out
}

// ParseListingIDs 从一个清单信封里抽出 id 列表。
func ParseListingIDs(payload []byte) []string {
	rows := ParseListing(payload)
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids
}
