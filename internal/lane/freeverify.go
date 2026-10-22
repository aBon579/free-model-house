package lane

import (
	"context"
	"regexp"
	"sync"
	"time"
)

// 解决一个关键问题：**"我拉到的模型，真的都是免费的吗？"**
//
// 答案：**名字匹配只是一个快速过滤，不构成保证**。
// 真正的保证来自**运行时探测** —— 上游对非免费模型的反应是决定性的。
//
//	名字过滤  →  快，但不保证（可能漏、可能多）
//	运行时探测 →  慢（消耗额度），但**权威**
//
// 所以生产路径是"名字过滤 → 探测 → 只把验证过的暴露给客户端"。

// alwaysFree 是**名字里没有 free 但确实免费**的模型。
//
// ⚠️ 这份名单是**实测**出来的，会过期。
// 所以它只是"快速过滤"的一部分 —— 真正的保证来自探测。
var alwaysFree = map[string]bool{
	"union-alpha":      true,
	"space-bunny-free": true,
	"big-pickle":       true,
}

// freeLaneRe 匹配免费车道的命名模式。
//
// 上游的清单是**免费与付费混在一起**的 —— 只有这些 id 能在没有用户凭据的情况下应答。
//
// ⚠️ 82/85 的 id **不**匹配这个模式（实测 2026-10-19），
// 所以这个过滤能把 85 缩到 ~10。
var freeLaneRe = regexp.MustCompile(`(?:^|[-_])free(?:$|[-_.])`)

// IsFreeLane 报告一个 id 是否**看起来**在免费车道上。
//
// ⚠️ 名字说"是"**不等于**真的是。
// 见 FreeVerifier —— 那是权威答案。
func IsFreeLane(modelID string) bool {
	base := baseModelID(modelID)
	if alwaysFree[base] {
		return true
	}
	return freeLaneRe.MatchString(base)
}

// FreeVerdict 是一个模型的"是否真的免费"的结论。
type FreeVerdict string

const (
	// VerdictUnknown 还没探过。
	VerdictUnknown FreeVerdict = "unknown"
	// VerdictFree 探过了，免密凭据能应答。
	VerdictFree FreeVerdict = "free"
	// VerdictNotFree 探过了，**确定**不能免密应答。
	VerdictNotFree FreeVerdict = "not-free"
)

// FreeVerifier 记录"哪些模型真的在免费车道上"。
//
// 它的价值：名字过滤会**漏**（比如 `test` 这种不含 free 但确实能用的），
// 也会**多**（名字匹配但实际已下线）。探测把两者都解决掉。
type FreeVerifier struct {
	mu      sync.RWMutex
	verdict map[string]FreeVerdict
	at      map[string]int64
	// probe 发起一次探测。
	probe func(ctx context.Context, model string) *Error
	// now 可注入以便测试。
	now func() time.Time
}

func NewFreeVerifier(probe func(ctx context.Context, model string) *Error) *FreeVerifier {
	return &FreeVerifier{
		verdict: map[string]FreeVerdict{},
		at:      map[string]int64{},
		probe:   probe,
		now:     time.Now,
	}
}

// Verdict 返回一个模型当前的结论。
func (v *FreeVerifier) Verdict(model string) FreeVerdict {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if verdict, ok := v.verdict[model]; ok {
		return verdict
	}
	return VerdictUnknown
}

// Set 记录一个结论。
func (v *FreeVerifier) Set(model string, verdict FreeVerdict) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.verdict[model] = verdict
	v.at[model] = v.now().UnixMilli()
}

// Verify 探一个模型，确认它是否真的免费。
//
// ⚠️ 判定依据是**上游的反应**，不是名字：
//
//	401 "Missing API key"  → **确定不是**免费车道
//	                         （即使带着 Bearer public）
//	200 / 429 / 403 闸门  → 在免费车道上
//	                         （429=限流，403=闸门/IP 风控）
//
// 401 是决定性的：它意味着上游在查凭据时发现 `Bearer public`
// **不适用于这个模型** —— 也就是说这个模型需要用户自己的 key。
func (v *FreeVerifier) Verify(ctx context.Context, model string) FreeVerdict {
	if v.probe == nil {
		return VerdictUnknown
	}
	uerr := v.probe(ctx, model)
	verdict := VerdictFree
	if uerr != nil {
		switch uerr.Code {
		case CodeCredential:
			// ⚠️ 决定性：免密凭据不适用于这个模型。
			verdict = VerdictNotFree
		case CodeClient, CodeAborted:
			// 请求自己的错 —— 不能据此下结论。
			return VerdictUnknown
		default:
			// 限流、闸门、IP 风控、传输故障 ——
			// 都说明**它确实在免密车道上**（否则会是 401）。
			verdict = VerdictFree
		}
	}
	v.Set(model, verdict)
	return verdict
}

// Filter 过滤掉确定不免费的模型。
//
// ⚠️ **未知的一律保留** —— 探测是有成本的，
// 而一个"还没探过"的模型不该被静默藏起来。
func (v *FreeVerifier) Filter(models []ModelInfo) []ModelInfo {
	out := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		if v.Verdict(m.ID) == VerdictNotFree {
			continue
		}
		out = append(out, m)
	}
	return out
}

// Snapshot 导出全部结论（用于持久化与展示）。
func (v *FreeVerifier) Snapshot() map[string]FreeVerdict {
	v.mu.RLock()
	defer v.mu.RUnlock()
	out := make(map[string]FreeVerdict, len(v.verdict))
	for k, v2 := range v.verdict {
		out[k] = v2
	}
	return out
}

// Load 用持久化的结论播种。
func (v *FreeVerifier) Load(verdicts map[string]FreeVerdict) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for k, verdict := range verdicts {
		v.verdict[k] = verdict
		v.at[k] = v.now().UnixMilli()
	}
}

// missingAPIKeyRe 匹配上游"缺 API key"的拒绝。
//
// ⚠️ 这个措辞是**决定性**的：上游在查凭据时发现 `Bearer public`
// **不适用于这个模型** —— 也就是这个模型需要用户自己的 key。
//
// 别的措辞（"free tier can only be used from within"）不是这个意思。
var missingAPIKeyRe = regexp.MustCompile(`(?i)missing api key|no api key|api key required|invalid api key|authentication required`)

// IsMissingAPIKey 报告一个错误是不是"这个模型要自己的 key"。
func IsMissingAPIKey(uerr *Error) bool {
	if uerr == nil {
		return false
	}
	if uerr.Code != CodeCredential {
		return false
	}
	return missingAPIKeyRe.MatchString(uerr.Message)
}
