package lane

import (
	"sort"
	"strings"
	"sync"
)

// LaneSwitches 管"哪条车道开不开"。
//
// ⚠️ **为什么需要它** ——
//
// 用户可能：
//   - 不想用 OpenRouter（不想让它碰自己的额度）→ 关掉 openrouter 车道；
//   - 免密车道被风控了，想临时只用 OpenRouter → 关掉 zen 车道；
//   - 只用某一条车道的模型做对照测试。
//
// 关掉一条车道的意思是：**它的模型不进候选链**。
// 不是"删掉模型"，也不是"删掉凭据" —— 重新打开就回来了。
//
// ⚠️ **关掉最后一条车道是不允许的** ——
//
//	那等于把网关变成一块砖。所以 `SetEnabled` 会拒绝它。
type LaneSwitches struct {
	mu sync.RWMutex
	// enabled 是车道 id → 开不开。
	enabled map[string]bool
	// known 是已知的车道 id（用来知道"用户没设置过"和"用户设成关"的区别）。
	known map[string]bool
}

// NewLaneSwitches 造一个开关组。
func NewLaneSwitches() *LaneSwitches {
	return &LaneSwitches{
		enabled: map[string]bool{},
		known:   map[string]bool{},
	}
}

// SetKnown 登记一条已知的车道（**默认开**）。
func (s *LaneSwitches) SetKnown(laneID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.known[laneID] = true
	if _, ok := s.enabled[laneID]; !ok {
		s.enabled[laneID] = true
	}
}

// Enabled 报告一条车道开不开。
//
// ⚠️ **没登记过的车道默认开** ——
//
//	否则新加一条车道会因为"没设置过"而莫名其妙是关的。
func (s *LaneSwitches) Enabled(laneID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.enabled[laneID]
	if !ok {
		return true
	}
	return v
}

// SetEnabled 开/关一条车道。
//
// ⚠️ **拒绝关掉最后一条开着的车道** ——
//
//	否则网关会变成一块砖（没有候选模型，所有请求都失败）。
//	返回 false 表示被拒绝。
func (s *LaneSwitches) SetEnabled(laneID string, on bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !on {
		// 数一下还有几条开着。
		open := 0
		for id, v := range s.enabled {
			if v && id != laneID {
				open++
			}
		}
		if open == 0 {
			return false
		}
	}

	s.enabled[laneID] = on
	s.known[laneID] = true
	return true
}

// Snapshot 返回全部车道开关。
func (s *LaneSwitches) Snapshot() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]bool, len(s.enabled))
	for k, v := range s.enabled {
		out[k] = v
	}
	return out
}

// Encode 把开关组编码成一个字符串（存进 settings 表）。
//
// 格式："zen:1,openrouter:0"
func (s *LaneSwitches) Encode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.enabled))
	for id := range s.enabled {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, id+":"+boolDigit(s.enabled[id]))
	}
	return strings.Join(parts, ",")
}

// Load 从编码的字符串恢复。
//
// ⚠️ **恢复不覆盖"没提到"的车道** ——
//
//	编码里没提到的意思是"没设置过"，不是"关"。
func (s *LaneSwitches) Load(encoded string) {
	if strings.TrimSpace(encoded) == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, part := range strings.Split(encoded, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, val, ok := strings.Cut(part, ":")
		if !ok {
			continue
		}
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		s.enabled[id] = val == "1" || strings.EqualFold(val, "true")
		s.known[id] = true
	}
}

// boolDigit 把 bool 变成 "1" / "0"。
func boolDigit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
