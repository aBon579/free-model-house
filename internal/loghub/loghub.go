// Package loghub 是**运行日志中心**。
//
// ⚠️ **它解决什么问题** ——
//
//	管理台原来只有 stdout 日志（容器重建就没了），
//	用户想知道"刚才那次调用到底发生了什么"只能去翻日志文件。
//
//	所以这里把关键事件**按类目**落到数据库里，
//	管理台的「日志」页可以直接看。
//
// ⚠️ **每个类目都有开关** ——
//
//	关掉某个类目 = **不再记录**这一类的新日志，
//	日志页也**不再显示**这一类的内容。
//
//	⚠️ **历史数据不删** —— 关掉开关只是"不再记、也不显示"，
//	库里那些行还在（用户随时可以重新打开开关看到它们）。
//	真要清空，日志页上有「清空日志」按钮。
package loghub

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/aBon579/free-model-house/internal/store"
)

// Category 是一个日志类目。
type Category string

// 日志类目。
//
// ⚠️ **这些是**`setting log_categories` **里的键** ——
//
//	改名会让老用户的开关设置失效，所以**不要随便改**。
const (
	// CategoryRequest 是**调用请求**（每次推理一行）。
	CategoryRequest Category = "request"
	// CategoryProbe 是**探测**（可用性/延迟结论）。
	CategoryProbe Category = "probe"
	// CategoryKey 是**调用 Key 与认证**（Key Hub 的增删改、认证失败）。
	CategoryKey Category = "key"
	// CategoryLane 是**上游车道**（清单刷新、上游错误）。
	CategoryLane Category = "lane"
	// CategorySystem 是**系统与设置**（启动、设置变更、管理操作）。
	CategorySystem Category = "system"
)

// SettingLogCategories 是"类目 → 是否开启"的存储键。
const SettingLogCategories = "log_categories"

// categoryLabel 是类目在界面上的中文名。
var categoryLabel = map[Category]string{
	CategoryRequest: "调用请求",
	CategoryProbe:   "探测",
	CategoryKey:     "密钥与认证",
	CategoryLane:    "上游车道",
	CategorySystem:  "系统与设置",
}

// Categories 返回全部类目（固定顺序）。
func Categories() []Category {
	return []Category{
		CategoryRequest,
		CategoryProbe,
		CategoryKey,
		CategoryLane,
		CategorySystem,
	}
}

// Label 返回类目的中文名。
func (c Category) Label() string {
	if s, ok := categoryLabel[c]; ok {
		return s
	}
	return string(c)
}

// maxKept 是日志表保留的最大条数。
//
// ⚠️ **为什么要截断** —— 日志表会长到**无限大**，
//
//	  一天几万条调用日志很常见。
//
//		保留最近 5000 条：足够排障，又不会把数据库撑爆。
const maxKept = 5000

// Hub 是日志中心。
type Hub struct {
	db *store.DB

	mu sync.RWMutex
	// enabled 是"类目 → 是否记录"。
	enabled map[Category]bool
	// dirty 表示开关有变化还没落盘。
	dirty bool
	// keepCount 是裁剪节流计数。
	keepCount int
}

// New 建一个日志中心，并从数据库恢复开关状态。
func New(db *store.DB) *Hub {
	h := &Hub{
		db:      db,
		enabled: map[Category]bool{},
	}
	h.load()
	return h
}

// load 恢复开关设置。
//
// ⚠️ **老数据一律全开** ——
//
//	没写过这个键说明是第一次用日志功能，全部类目都该开着。
func (h *Hub) load() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range Categories() {
		h.enabled[c] = true
	}
	raw, ok, err := h.db.GetSetting(SettingLogCategories)
	if err != nil || !ok || raw == "" {
		return
	}
	var saved map[string]bool
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		return
	}
	for _, c := range Categories() {
		if v, ok := saved[string(c)]; ok {
			h.enabled[c] = v
		}
	}
}

// Enabled 报告某类日志当前是否记录。
func (h *Hub) Enabled(c Category) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.enabled[c]
}

// SetEnabled 打开/关闭某类日志，并落盘。
func (h *Hub) SetEnabled(c Category, on bool) error {
	h.mu.Lock()
	h.enabled[c] = on
	h.dirty = true
	h.mu.Unlock()
	return h.persist()
}

// SetAll 一次性设置全部类目。
func (h *Hub) SetAll(on bool) error {
	h.mu.Lock()
	for _, c := range Categories() {
		h.enabled[c] = on
	}
	h.dirty = true
	h.mu.Unlock()
	return h.persist()
}

// Snapshot 返回当前开关状态。
func (h *Hub) Snapshot() map[Category]bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[Category]bool, len(h.enabled))
	for k, v := range h.enabled {
		out[k] = v
	}
	return out
}

// EnabledCategories 返回当前**开启**的类目（给查询用）。
func (h *Hub) EnabledCategories() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.enabled))
	for _, c := range Categories() {
		if h.enabled[c] {
			out = append(out, string(c))
		}
	}
	return out
}

// persist 把开关状态写回数据库。
func (h *Hub) persist() error {
	h.mu.Lock()
	if !h.dirty {
		h.mu.Unlock()
		return nil
	}
	saved := make(map[string]bool, len(h.enabled))
	for k, v := range h.enabled {
		saved[string(k)] = v
	}
	h.dirty = false
	h.mu.Unlock()
	return h.db.PutJSON(SettingLogCategories, saved)
}

// Log 记一条日志。
//
// ⚠️ **类目关着就直接返回** —— 开关是**记录前**判断的，
//
//	所以关掉之后这一类**一条都不会再写进库**。
//
// ⚠️ **写失败不 panic** —— 日志是辅助信息，
//
//	记日志失败不该让业务请求失败。
func (h *Hub) Log(c Category, format string, args ...any) {
	if !h.Enabled(c) {
		return
	}
	msg := format
	if len(args) > 0 {
		msg = fmt.Sprintf(format, args...)
	}
	_ = h.db.AppendLog(time.Now().UnixMilli(), string(c), msg)
	// ⚠️ **顺手截断** —— 每写一条都检查太贵，
	//   所以按条数粗略地截：写满一批后裁一次。
	h.trimIfNeeded()
}

// trimIfNeeded 偶尔裁一下日志表。
func (h *Hub) trimIfNeeded() {
	h.mu.Lock()
	// 用一个简单的计数做节流 —— 每 500 条裁一次。
	h.keepCount++
	need := h.keepCount >= 500
	if need {
		h.keepCount = 0
	}
	h.mu.Unlock()
	if need {
		_ = h.db.TrimLogs(maxKept)
	}
}
