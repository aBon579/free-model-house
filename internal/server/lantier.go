package server

import (
	"sync"
	"time"
)

// **访问档位** —— 两层限流模型。
//
// ## 网关级（gateway tier）：off / light / standard / deep
//
//	off    —— 网关不做限制，各 Key 按自己的档位限
//	light  —— 所有 Key 强制 10 次/分钟、2 并发
//	standard —— 所有 Key 强制 60 次/分钟、6 并发
//	deep   —— 所有 Key 不限
//
//	⚠️ 非 off 时单 Key 的档位**锁定不可改**（界面上禁用），
//	  但设置**保留** —— 回 off 后自动恢复生效。
//	  这样"临时锁一轮"不会把用户精心配的各设备档位洗掉。
//
// ## 单 Key 级（per-key tier）：light / standard / deep
//
//	只按**这把 Key** 记账 —— 笔记本 60 次/分钟不影响手机。
//	网关级为 off 时才生效。
//
// ⚠️ 限流是对**整个网关**的共享资源（免密车道的全局配额）的保护，
//   所以网关级的优先级**天然高于**单 Key 级 —— 不是"谁更严格听谁的"，
//   而是"网关说了算，网关不说话才轮到 Key"。

// LanTier 是访问档位。
//
// ⚠️ `off` **只对网关级合法** —— 单 Key 没有这个档
//
//	（一把不限制的 Key 等于没有这把 Key 的限流，那直接用 deep 就行了，
//	但"off"对单 Key 语义不明：是不限还是不启用？）。
type LanTier string

const (
	// LanTierOff 不限流（**只用于网关级**）—— 各 Key 按自己的档位限。
	LanTierOff LanTier = "off"
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
	LanTierOff:      3,
}

// LanTierRank 返回档位序数。
func LanTierRank(t LanTier) int {
	if v, ok := lanTierOrder[t]; ok {
		return v
	}
	return 3
}

// LanTierAll 是所有合法的**网关级**档位（给前端渲染卡片）。
var LanTierAll = []LanTier{LanTierOff, LanTierLight, LanTierStandard, LanTierDeep}

// KeyTierAll 是**单 Key** 的合法档位（没有 off）。
var KeyTierAll = []LanTier{LanTierLight, LanTierStandard, LanTierDeep}

// ParseLanTier 解析档位字符串。
//
// ⚠️ **旧的档位也要能读** ——
//
//	  用户升级后数据库里还存着 "limited" / "lan"。
//	  它们要映射到**语义最接近**的新档位，不能变成"未知"：
//
//		limited → light（原来是限流档）
//		lan     → deep（原来是不限流档）
func ParseLanTier(s string) LanTier {
	switch LanTier(s) {
	case LanTierOff:
		return LanTierOff
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
	default:
		// 认不出来 → **标准档**（不是最紧的）——
		// 免得升级后突然用不了了。
		return LanTierStandard
	}
}

// ParseKeyTier 解析单 Key 的档位 —— off 落到 deep（单 Key 没有 off）。
func ParseKeyTier(s string) LanTier {
	t := ParseLanTier(s)
	if t == LanTierOff {
		return LanTierDeep
	}
	return t
}

// TierLimits 返回一个档位对应的（每分钟请求数，并发数）。
//
// ⚠️ `0` 表示**不限**。
func TierLimits(t LanTier) (perMinute int, maxConcurrent int) {
	switch t {
	case LanTierLight:
		return 10, 2
	case LanTierStandard:
		return 60, 6
	case LanTierDeep:
		return 0, 0
	default:
		// off / 未知 —— 不限。
		return 0, 0
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
