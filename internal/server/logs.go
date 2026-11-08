package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/aBon579/free-model-house/internal/loghub"
)

// handleLogs 返回日志页需要的全部数据。
//
// ⚠️ **已关闭的类目不返回内容** ——
//
//	关闭只是"不再记录 + 不再显示"，**历史行仍然在库里**。
//	重新打开开关，它们会再次出现。
func (a *Admin) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的方法"})
		return
	}
	if a.loghub == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "日志功能未启用"})
		return
	}
	limit := 300
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	// ⚠️ 传了 category 就只看这一类；但**关着的类目直接回空**。
	filter := []string{}
	if c := r.URL.Query().Get("category"); c != "" {
		if !a.loghub.Enabled(loghub.Category(c)) {
			writeJSON(w, http.StatusOK, map[string]any{
				"logs":       []any{},
				"categories": a.logCategories(),
				"counts":     a.logCounts(),
			})
			return
		}
		filter = []string{c}
	} else {
		filter = a.loghub.EnabledCategories()
	}
	rows, err := a.db.ListLogs(filter, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"logs":       rows,
		"categories": a.logCategories(),
		"counts":     a.logCounts(),
	})
}

// logCategories 返回类目清单与开关状态。
func (a *Admin) logCategories() []map[string]any {
	snap := a.loghub.Snapshot()
	out := make([]map[string]any, 0, len(snap))
	for _, c := range loghub.Categories() {
		out = append(out, map[string]any{
			"id":      string(c),
			"label":   c.Label(),
			"enabled": snap[c],
		})
	}
	return out
}

// logCounts 返回各类目累计条数。
func (a *Admin) logCounts() map[string]int {
	counts, err := a.db.CountLogsByCategory()
	if err != nil {
		return map[string]int{}
	}
	return counts
}

// handleLogConfig 改日志类目开关。
//
// 两种写法：
//
//	{"category": "request", "enabled": false}   —— 改单个类目
//	{"enabledAll": true}                        —— 全开/全关
func (a *Admin) handleLogConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的方法"})
		return
	}
	if a.loghub == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "日志功能未启用"})
		return
	}
	var body struct {
		Category   string `json:"category"`
		Enabled    *bool  `json:"enabled"`
		EnabledAll *bool  `json:"enabledAll"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是 JSON"})
		return
	}
	if body.EnabledAll != nil {
		if err := a.loghub.SetAll(*body.EnabledAll); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "categories": a.logCategories()})
		return
	}
	if body.Category == "" || body.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 category / enabled"})
		return
	}
	// ⚠️ 只允许已知类目 —— 否则开关状态里会混进垃圾键。
	known := false
	for _, c := range loghub.Categories() {
		if string(c) == body.Category {
			known = true
			break
		}
	}
	if !known {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知日志类目"})
		return
	}
	if err := a.loghub.SetEnabled(loghub.Category(body.Category), *body.Enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "categories": a.logCategories()})
}

// handleLogClear 清空日志（可只清一个类目）。
func (a *Admin) handleLogClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的方法"})
		return
	}
	var body struct {
		Category string `json:"category"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if err := a.db.ClearLogs(body.Category); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
