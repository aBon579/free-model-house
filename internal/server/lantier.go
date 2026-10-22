package server

import (
	"sync"
	"time"
)

// **局域网访问档位**。
//
// ⚠️ 之前只有一个 `lan_enabled` 布尔开关 —— 粒度太粗：
//   要么局域网全开（任何人都能用你的免费额度），
//   要么全关。
// 但实际需求是**分级**的：家里的 NAS 可能只让自家几台设备用，
// 而公司/合租环境需要**限量**。
//
// 三档（从松到紧）：
//
//	light   —— 轻量：少量调用（给偶尔查一下的 agent）
//	standard—— 标准：正常用（家里的主力档）
//	deep    —— 深度：不限量（自己独占，或者出口很干净）
//
// ⚠️ **没有"完全关闭"这一档** ——
//   因为那等于"局域网里的 Agent 完全用不了"，
//   而这个网关**就是**给局域网里的 agent 用的。
//   要限制局域网接入，用**换 Key**（轮换 LAN Key）或**防火墙**，
//   不要用"关掉整个功能"这种钝器。
//
// ⚠️ 三档的限制都是**对整个网关**生效的，不是对单个 Key ——
//   因为免密车道的配额是**全局共享**的，
//   限制单个 Key 等于没限制（多拿几把 Key 就绕过了）。
//
// ⚠️ 本机 Key（管理台自己）**永远不受限** ——
//   否则管理台自己的轮询会被自己的限流打掉。

// LanTier 是局域网访问档位。
type LanTier string

const (
	// LanTierLight 轻量档 —— 少量调用。
	LanTierLight LanTier = "light"
	// LanTierStandard 标准档 —— 正常用。
	LanTierStandard LanTier = "standard"
	// LanTierDeep 深度档 —— 不限量。
	LanTierDeep LanTier = "deep"
)

// lanTierOrder 是档位的严格程度（用于设置页排序：松 → 紧）。
var lanTierOrder = map[LanTier]int{
	LanTierDeep:     0,
	LanTierStandard: 1,
	LanTierLight:    2,
}

// LanTierRank 返回档位序数。
func LanTierRank(t LanTier) int {
	if v, ok := lanTierOrder[t]; ok {
		return v
	}
	return 3
}

// LanTierAll 是所有合法档位（给前端渲染下拉框）。
var LanTierAll = []LanTier{LanTierLight, LanTierStandard, LanTierDeep}

// ParseLanTier 解析档位字符串。
//
// ⚠️ **旧的档位也要能读** ——
//
//	  用户升级后数据库里还存着 "off" / "limited" / "lan"。
//	  它们要映射到**语义最接近**的新档位，不能变成"未知"：
//
//		off     → light（最紧的一档）
//		limited → light（原来是限流档）
//		lan     → deep（原来是不限流档）
func ParseLanTier(s string) LanTier {
	switch LanTier(s) {
	case LanTierLight:
		return LanTierLight
	case LanTierStandard:
		return LanTierStandard
	case LanTierDeep:
		return LanTierDeep
	// 旧档位 → 新档位。
	case "limited":
		return LanTierLight
	case "lan":
		return LanTierDeep
	case "off":
		return LanTierLight
	default:
		// 认不出来 → **标准档**（不是最紧的）——
		// 免得升级后局域网突然用不了了。
		return LanTierStandard
	}
}

// TierLimits 返回一个档位对应的（每分钟请求数，并发数）。
//
// ⚠️ `0` 表示**不限**。
//
// ⚠️ 数值是**默认值**，用户在设置页可以改 ——
//
//	这里是"合理的起点"，不是硬编码的限制。
func TierLimits(t LanTier) (perMinute int, maxConcurrent int) {
	switch t {
	case LanTierLight:
		return 10, 2
	case LanTierStandard:
		return 60, 6
	case LanTierDeep:
		return 0, 0
	default:
		return 60, 6
	}
}

// **限流器** —— 令牌桶。
//
// ⚠️ 自己写而不用 golang.org/x/time/rate ——
//
//	那个包会多一个依赖（还要进 vendor/，多几百 KB），
//	而这里的需求很简单：一个每分钟 N 次的桶 + 一个并发上限。
type limiter struct {
	mu sync.Mutex
	// tokens 是当前可用令牌。
	tokens float64
	// capacity 是桶容量（= 每分钟配额）。
	capacity float64
	// refillPerSec 是每秒补充速率。
	refillPerSec float64
	// last 是上次补充的时间。
	last time.Time
	// inFlight 是当前正在处理的请求数。
	inFlight int
	// maxInFlight 是并发上限。
	maxInFlight int
}

// newLimiter 造一个限流器。
//
// ⚠️ `perMinute` <= 0 表示**不限流**（桶无限大）。
func newLimiter(perMinute int, maxConcurrent int) *limiter {
	l := &limiter{
		capacity:     float64(perMinute),
		refillPerSec: float64(perMinute) / 60.0,
		last:         time.Now(),
		maxInFlight:  maxConcurrent,
	}
	if perMinute <= 0 {
		// 不限流 → 桶无限大，速率设成 0（不消耗令牌）。
		l.capacity = 0
		l.refillPerSec = 0
	}
	// ⚠️ **桶初始是满的** ——
	//   不填满的话，刚切到 limited 档时第一个请求就被拒
	//   （tokens=0 < 1），用户会看到"刚设置就 429"。
	l.tokens = l.capacity
	return l
}

// allow 报告这个请求能不能进，并占用一个名额。
//
// ⚠️ **先扣令牌再查并发** —— 反过来的话，
//
//	大量并发请求会先占满 inFlight，然后才被令牌桶拒，
//	于是"限流"实际上变成了"限并发"，令牌桶形同虚设。
func (l *limiter) allow() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()

	// 补充令牌。
	if l.capacity > 0 {
		elapsed := now.Sub(l.last).Seconds()
		if elapsed > 0 {
			l.tokens += elapsed * l.refillPerSec
			if l.tokens > l.capacity {
				l.tokens = l.capacity
			}
			l.last = now
		}
		if l.tokens < 1 {
			return false
		}
		l.tokens--
	}

	// 并发上限。
	if l.maxInFlight > 0 && l.inFlight >= l.maxInFlight {
		return false
	}
	l.inFlight++
	return true
}

// release 释放一个名额。
func (l *limiter) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight > 0 {
		l.inFlight--
	}
}

// stats 返回当前状态（给管理台显示）。
func (l *limiter) stats() (tokens float64, inFlight int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.tokens, l.inFlight
}
