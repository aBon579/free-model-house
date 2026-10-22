package lane

import (
	"encoding/json"
	"sync"
	"time"
)

// **限流账本**。
//
// 核心认识：免费车道的凭据是**全局共享**的（`Bearer public`），
// 所以 429 反映的是"此刻拥挤"，**不是**"这个模型坏了"。
//
// 实测 92 次限流事件里最短的一次只持续 0.6 秒，抽样的两个模型都在两分钟内重新 200，
// 所以无 Retry-After 时的默认冷却是 **10 秒**而不是 60 秒 ——
// 冷却过长会让面板显示"不可用"而模型其实好好的（候选链会跳过被限流的模型）。
//
// 而**重试一个被拒的请求不消耗配额** —— 便宜的方向就是短冷却。
//
// 详见 docs/design/00-overview.md §5.6。

const (
	// DefaultQuotaCooldownSec 是没有 Retry-After 时的默认冷却秒数。
	// 刻意**短**。理由见文件头。
	DefaultQuotaCooldownSec = 10

	// MaxQuotaCooldownSec 是冷却的上限（2 小时）。
	// 探测会重新验证，所以不需要更长的封禁。
	MaxQuotaCooldownSec = 2 * 3600

	// throttleEpisodeTTL 是一次限流窗口的保留时长。
	throttleEpisodeTTL = 14 * 24 * time.Hour

	// throttleMaxEpisodes 是每个模型保留的限流窗口数。
	throttleMaxEpisodes = 12
)

// Episode 是一次限流窗口。
type Episode struct {
	// Start 是窗口开始（epoch ms）。
	Start int64 `json:"start"`
	// End 是窗口结束（epoch ms）；0 表示窗口还开着。
	End int64 `json:"end"`
}

// Note 是一个模型的限流状态。
type Note struct {
	// ThrottledAt 是最近一次被拒的时间。
	ThrottledAt int64 `json:"throttledAt,omitempty"`
	// CooldownUntil 是冷却结束时间。
	CooldownUntil int64 `json:"cooldownUntil,omitempty"`
	// LastOK 是最近一次成功的时间。
	LastOK int64 `json:"lastOk,omitempty"`
	// Episodes 是最近的限流窗口。
	Episodes []Episode `json:"episodes,omitempty"`
}

// Book 是限流账本。
//
// 所有方法都并发安全。OnUpdate 在每次变更后触发（用于持久化 + UI 刷新）。
type Book struct {
	mu    sync.Mutex
	notes map[string]*Note
	// OnUpdate 在某个模型的记录变化后触发。
	OnUpdate func(model string, note Note)
}

func NewBook() *Book {
	return &Book{notes: map[string]*Note{}}
}

// Load 用持久化的记录播种（保留跨重启的窗口历史）。
func (b *Book) Load(notes map[string]Note) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for m, n := range notes {
		cp := n
		b.notes[m] = &cp
	}
}

// Snapshot 导出全部记录。
func (b *Book) Snapshot() map[string]Note {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]Note, len(b.notes))
	for m, n := range b.notes {
		out[m] = *n
	}
	return out
}

// ThrottleRow 是限流账本的一行（可持久化的形状）。
type ThrottleRow struct {
	Model string `json:"model"`
	// ThrottledAt 是最近一次被拒的时间（epoch ms）。
	ThrottledAt int64 `json:"throttledAt,omitempty"`
	// CooldownUntil 是冷却结束时间（epoch ms）。
	CooldownUntil int64 `json:"cooldownUntil,omitempty"`
	// LastOK 是最近一次成功的时间（epoch ms）。
	LastOK int64 `json:"lastOk,omitempty"`
	// Episodes 是限流窗口的历史（JSON 数组）。
	Episodes string `json:"episodes,omitempty"`
}

// Rows 导出限流账本（供持久化）。
func (b *Book) Rows() []ThrottleRow {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]ThrottleRow, 0, len(b.notes))
	for m, n := range b.notes {
		row := ThrottleRow{
			Model:         m,
			ThrottledAt:   n.ThrottledAt,
			CooldownUntil: n.CooldownUntil,
			LastOK:        n.LastOK,
		}
		if len(n.Episodes) > 0 {
			if data, err := json.Marshal(n.Episodes); err == nil {
				row.Episodes = string(data)
			}
		}
		out = append(out, row)
	}
	return out
}

// noteLocked 取得（必要时建立）一个模型的记录。
func (b *Book) noteLocked(model string) *Note {
	n, ok := b.notes[model]
	if !ok {
		n = &Note{}
		b.notes[model] = n
	}
	return n
}

func (b *Book) fire(model string, n *Note) {
	if b.OnUpdate != nil {
		b.OnUpdate(model, *n)
	}
}

// MarkThrottled 记一次实时的配额拒绝。
//
// 第一次调用开启一个窗口；重复调用只延长冷却。
func (b *Book) MarkThrottled(model string, retryAfterSec int) {
	now := time.Now().UnixMilli()
	cd := int64(retryAfterSec) * 1000
	if cd <= 0 {
		cd = DefaultQuotaCooldownSec * 1000
	}
	if cd > MaxQuotaCooldownSec*1000 {
		cd = MaxQuotaCooldownSec * 1000
	}

	b.mu.Lock()
	n := b.noteLocked(model)
	if len(n.Episodes) == 0 || n.Episodes[len(n.Episodes)-1].End != 0 {
		// 开一个新窗口。
		n.Episodes = append(n.Episodes, Episode{Start: now})
		if len(n.Episodes) > throttleMaxEpisodes {
			n.Episodes = n.Episodes[len(n.Episodes)-throttleMaxEpisodes:]
		}
	}
	n.ThrottledAt = now
	n.CooldownUntil = now + cd
	b.fire(model, n)
	b.mu.Unlock()
}

// MarkOK 记一次成功。
//
// 成功会**立刻**清掉限流状态 —— 真实流量是比任何探测样本都硬的证据。
func (b *Book) MarkOK(model string) {
	now := time.Now().UnixMilli()
	b.mu.Lock()
	n := b.noteLocked(model)
	n.LastOK = now
	if n.CooldownUntil > 0 && n.CooldownUntil <= now {
		n.CooldownUntil = 0
	}
	// 关闭还开着的窗口。
	for i := len(n.Episodes) - 1; i >= 0; i-- {
		if n.Episodes[i].End == 0 {
			n.Episodes[i].End = now
			break
		}
	}
	b.fire(model, n)
	b.mu.Unlock()
}

// Throttled 报告一个模型当前是否在冷却中。
func (b *Book) Throttled(model string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.notes[model]
	if !ok {
		return false
	}
	return n.CooldownUntil > time.Now().UnixMilli()
}

// CooldownRemaining 返回剩余冷却秒数（0 表示不在冷却）。
func (b *Book) CooldownRemaining(model string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.notes[model]
	if !ok {
		return 0
	}
	left := n.CooldownUntil - time.Now().UnixMilli()
	if left <= 0 {
		return 0
	}
	return int(left / 1000)
}

// RecoveryETA 估算恢复时间。
//
// 用**已关闭窗口**的中位时长 —— 开着的那次说明还没恢复。
func (b *Book) RecoveryETA(model string) (int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n, ok := b.notes[model]
	if !ok || len(n.Episodes) == 0 {
		return 0, false
	}
	var durations []int64
	for _, e := range n.Episodes {
		if e.End > e.Start {
			durations = append(durations, e.End-e.Start)
		}
	}
	if len(durations) == 0 {
		return 0, false
	}
	sortInt64(durations)
	return durations[len(durations)/2], true
}

// sortInt64 排序一个 int64 切片（升序）。
func sortInt64(a []int64) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}
