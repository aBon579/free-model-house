package lane

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// **候选链构造**与**故障转移**。
//
// 三条铁律（每条都对应一类真实事故）：
//
//  1. **429 绝不重试同一模型** —— 配额按 session 记账，重试 = 再烧一次。
//     **换模型**是唯一诚实的杠杆。
//
//  2. **一旦已向客户端吐过内容，绝不换模型** —— 换了会内容重复/拼接错乱。
//     ⚠️ 而且"思考"**不算**吐过内容 —— "想了一分钟然后撞上配额墙"
//     恰恰是最值得故障转移的情形。
//
//  3. **请求自身的错（4xx）绝不换** —— 重放同样的 body 会重现同样的拒绝。
//
// 详见 docs/design/00-overview.md §5.4。

// Router 在多个候选之间做故障转移。
type Router struct {
	catalog  *Catalog
	lanes    *Registry
	throttle *Book
	// maxFailover 是最多换几个候选。
	maxFailover int
	// quotaBudgetMS 是"配额类拒绝"的总重试预算。
	//
	// ⚠️ 按**失败类别**分配预算，不是一个全局预算。
	// 一个全局预算会让重试静默退化成"只试一次" ——
	// 93 秒后报 502，而健康的候选闲置着。
	quotaBudgetMS int64
	// slowBudgetMS 是"慢故障"（传输/5xx）的总预算。
	slowBudgetMS int64
	// maxSameModelRetries 是**同一个模型**最多重试几次。
	//
	// ⚠️ 默认 2 —— 抖动类失败重试两次通常就好了，
	//   再多就是浪费用户的等待时间。
	maxSameModelRetries int
	// sameModelRetryDelay 是重试同模型前等多久（毫秒）。
	//
	// ⚠️ 要给上游一点喘息 —— 立刻重试往往撞同一个错。
	sameModelRetryDelay time.Duration
	// allowModelSwitch 允许**自动跨模型调用**。
	//
	// ⚠️ 默认**关闭** —— 关闭时**只重试当前模型**，
	//   重试仍失败就报错，**绝不**换成别的模型。
	//
	// ⚠️ 这个开关现在是**模型级别**的（不是车道级别）：
	//   开着 → 同一个模型失败后可以换**任意**可用模型；
	//   关着 → 一个模型都不换。
	allowModelSwitch bool
	// switches 管"哪条车道开不开"。
	//
	// ⚠️ **关掉的车道的模型不进候选链** —— 那条车道的模型
	//   仍然显示在清单里（用户能看到它存在），但不会被路由选中。
	switches *LaneSwitches
	// planModels 是**计划性调用列表**（最多 3 个槽位，按优先级排列）。
	//
	// ⚠️ 只在**自动跨模型回落**开启时生效：
	//   请求的模型失败后，按这里的槽位顺序依次回落；
	//   三个槽位都不可用时，才回到原先的"模型池"回落规则。
	planModels []string
	// mu 保护可变设置。
	mu sync.RWMutex
}

func NewRouter(catalog *Catalog, lanes *Registry, throttle *Book) *Router {
	return &Router{
		catalog:          catalog,
		lanes:            lanes,
		throttle:         throttle,
		maxFailover:      6,
		quotaBudgetMS:    40_000,
		slowBudgetMS:     210_000,
		allowModelSwitch: false,
		// ⚠️ 重试同模型 2 次、间隔 250ms ——
		//   抖动类失败（传输层断开 / 5xx）通常一次就好，
		//   两次是保险，再多就是让用户干等。
		maxSameModelRetries: 2,
		sameModelRetryDelay: 250 * time.Millisecond,
		switches:            NewLaneSwitches(),
	}
}

// SetLaneSwitches 装上车道开关。
func (r *Router) SetLaneSwitches(s *LaneSwitches) { r.switches = s }

// LaneEnabled 报告一条车道开不开。
//
// ⚠️ 给推理面的 `/v1/models` 用 —— 关闭的车道连清单都不该出现。
// nil 开关组 = 全开（还没初始化时的兜底）。
func (r *Router) LaneEnabled(laneID string) bool {
	if r.switches == nil {
		return true
	}
	return r.switches.Enabled(laneID)
}

// SetAutoModelSwitch 开关「自动跨模型调用」。
func (r *Router) SetAutoModelSwitch(allow bool) { r.allowModelSwitch = allow }

// SetPlanModels 设置**计划性调用列表**（按优先级排列，最多 3 个）。
//
// ⚠️ 传空 = 不用计划列表，回落到原有模型池规则。
func (r *Router) SetPlanModels(models []string) {
	out := make([]string, 0, len(models))
	for _, m := range models {
		if strings.TrimSpace(m) == "" {
			continue
		}
		out = append(out, m)
	}
	r.mu.Lock()
	r.planModels = out
	r.mu.Unlock()
}

// planModelList 返回计划列表副本。
func (r *Router) planModelList() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.planModels...)
}

// ThrottleRows 导出限流账本（供持久化）。
func (r *Router) ThrottleRows() []ThrottleRow {
	if r.throttle == nil {
		return nil
	}
	return r.throttle.Rows()
}

// candidate 是一个候选。
type candidate struct {
	Lane string
	// Model 是**裸名**（上游只认这个）。
	Model string
	// Full 是**带车道前缀**的 id（`zen/mimo-v2.6-flash-free`）——
	// 用它当限流账本、探测表、证据的键。
	Full string
	Info ModelInfo
	// score 是排序键（越小越优先）。
	score candidateScore
	// tried 标记已经试过。
	tried bool
}

// candidateScore 是候选的排序键。
//
// ⚠️ **可用性永远是最主要的项** —— 延迟只在同一可用性档位内排序。
// 一个探测可用但慢的模型，永远好过一个未探测但可能很快的模型。
type candidateScore struct {
	// avail 是可用性档位（0 最好）。
	avail int
	known int
	// ttft 是首字延迟（毫秒）。
	ttft int64
	// capConfidence 是能力结论的可信度（0 最好）。
	//
	// ⚠️ 它排在可用性与延迟**之后** ——
	// "这个模型能不能看图"不如"它现在能不能用"重要。
	// 但在同等条件下，已知能力的应该优先。
	capConfidence int
	// index 是目录顺序（最后的平局裁决）。
	index int
}

// less 比两个候选。
func less(a, b candidateScore) bool {
	if a.avail != b.avail {
		return a.avail < b.avail
	}
	if a.known != b.known {
		return a.known < b.known
	}
	if a.ttft != b.ttft {
		return a.ttft < b.ttft
	}
	if a.capConfidence != b.capConfidence {
		return a.capConfidence < b.capConfidence
	}
	return a.index < b.index
}

// BuildCandidates 构造一次请求的候选链。
//
// ⚠️ 请求的模型**永远排第一** —— 故障转移只在它"拒绝且尚未吐字"时启用。
func (r *Router) BuildCandidates(requestedLane, requestedModel string, needs Needs) []candidate {
	models := r.catalog.Models()

	// 解析请求的模型。
	// ⚠️ **解析规则只在这里定义一次** —— `Complete` 判"原车道"时也用它，
	//   否则两处口径不一致就会出现"候选被排到别的车道，原车道判定却还怪着"的错乱。
	laneID, upstream, strict := r.resolveTarget(requestedLane, requestedModel)

	out := make([]candidate, 0, len(models))
	for i, m := range models {
		if !m.Enabled {
			continue
		}
		// ⚠️ **关掉的车道不进候选链** ——
		//   模型仍然显示在清单里（用户能看到它存在），
		//   但路由不会选它。
		if r.switches != nil && !r.switches.Enabled(m.Lane) {
			continue
		}
		// ⚠️ `m.ID` **已经带车道前缀** —— 上游只认裸名，要剥掉。
		_, bare := ParseModelID(m.ID)
		cand := candidate{Lane: m.Lane, Model: bare, Full: m.ID, Info: m}
		cand.score = r.scoreFor(m, i)
		out = append(out, cand)
	}

	// 能力过滤（硬约束）：图片请求不能落到纯文本模型上。
	//
	// ⚠️ "未知能力"**通过** —— 它会在排序时排在已知能力之后。
	// 一条**已知**的判决才是硬过滤。
	filtered := out[:0]
	for _, c := range out {
		if SatisfiesNeeds(c.Info, needs) {
			filtered = append(filtered, c)
		}
	}
	out = filtered

	// 上下文长度过滤（软）。
	//
	// ⚠️ 用 1.5 倍余量 —— 实测容量是**下界**。
	// 候选被静默丢弃的代价是**整个请求失败**；
	// 让略微超标的 prompt 真的发出去，上游才是裁判。
	//
	// ⚠️ 过滤**只移除明显不够大的**候选，
	// 绝不因为"可能有更合适的"就把剩下的也清空 ——
	// 全被滤光时宁可超限也不失败。
	if needs.PromptTokens > 0 {
		soft := make([]candidate, 0, len(out))
		for _, c := range out {
			// 容量未知（0）→ 保留。
			if c.Info.ContextWindow <= 0 {
				soft = append(soft, c)
				continue
			}
			if needs.PromptTokens > ApplyHeadroom(c.Info.ContextWindow) {
				continue
			}
			soft = append(soft, c)
		}
		// ⚠️ 全被滤光了 —— 那就宁可超限也不失败。
		if len(soft) > 0 {
			out = soft
		}
	}

	// 排序。
	sortCandidates(out)

	// ⚠️ **「自动跨模型调用」开启时的车道优先级**：
	//
	//	① **Kilo** 与 **Zen** 之间互相切换（都是免密的，不花任何人的钱）
	//	② 两条免密车道都没有可用模型了 → **OpenRouter**
	//
	// 排序只在这**同一优先级组内**生效（`sortCandidates` 排的是可用性/延迟）。
	//
	// ⚠️ 为什么 Kilo 与 Zen **并列**（而不是谁优先谁）：
	//   两条免密车道的可用性**跟着出口 IP 走** ——
	//   IP 被风控时整条车道失效。
	//   它们是**互相的备胎**，没有固定的先后。
	//
	// ⚠️ 但**请求的那个模型永远排第一** ——
	//   换模型是"它不行之后"的事，不是"一开始就换"。
	if r.allowModelSwitch {
		out = orderLanesForSwitch(out, laneID)
		// ⚠️ **计划性调用列表优先** —— 三个槽位按优先级排在池子前面；
		//   三个都不可用时保持原有顺序（即"回落到原规则"）。
		out = r.orderPlanModels(out)
	}

	// 请求的模型提到最前。
	//
	// ⚠️⚠️ **必须"先精确全名，再同车道同名，最后才跨车道同名"** ——
	//   只用裸名匹配（`Model == upstream`）会在**同名模型存在于多条车道**时
	//   把排序更靠前的**别的车道**提到第一位，于是请求被静默路由到错误的车道；
	//   关闭「自动跨模型调用」时又会因为它不在"原车道"而直接被拒。
	if idx := pickRequested(out, laneID, upstream); idx > 0 {
		c := out[idx]
		copy(out[1:idx+1], out[0:idx])
		out[0] = c
	}

	// ⚠️⚠️ **「自动跨模型调用」关闭时 —— 候选链只剩请求的模型。**
	//
	// 这是这个开关的**核心语义**：
	//
	//	关闭 → 只重试**当前模型**。重试仍失败就**直接报错**，
	//	        一个模型都不换。
	//	开启 → 允许换模型（Kilo ↔ Zen → OpenRouter）。
	//
	// ⚠️ 陷阱在于候选链和开关管的范围**不是一件事**：
	//   若候选链永远是整个模型池，而开关只管"能不能用花额度的车道"，
	//   免密车道之间就会一直偷偷换模型 —— 关掉开关也拦不住。
	//
	//   要求是"指名了哪个模型就用哪个；它不行就明说不行"。
	// ⚠️⚠️ **前缀/显式指定命中了真实车道 → 只在这条车道的候选里挑。**
	//
	//   同名的其他车道模型必须清掉 —— 这正是"kilo/xxx 被静默送到 zen"的路径：
	//   Kilo 清单里没有这个模型（或它被能力/容量过滤掉）时，
	//   跨车道的同名模型会顶上来，成为默认落点。
	//
	//   关闭自动跨模型 → 该车道没有候选就明确失败（不再静默换车道）；
	//   开启 → 允许把其他车道作为**用户已授权的**回落。
	if strict {
		if !r.allowModelSwitch {
			return restrictToRequestedStrict(out, laneID, upstream)
		}
		// 开启时保留全部候选 —— 请求的模型已被 pickRequested 提到第一位。
		return out
	}

	if !r.allowModelSwitch {
		return restrictToRequested(out, laneID, upstream)
	}
	return out
}

// lanePriorityForSwitch 给「自动跨模型调用」定车道优先级。
//
//	0 —— **请求原本的那条车道**（换模型是"它不行之后"的事）
//	1 —— 另一条**免密**车道（Kilo ↔ Zen）：**不花任何人的钱**
//	2 —— **OpenRouter**：用**用户自己的**额度，最后才考虑
//
// ⚠️ 组内的相对顺序**保持 sortCandidates 的结果** ——
//
//	那是按可用性/延迟排的，比"哪个车道优先"更重要。
func lanePriorityForSwitch(laneID, originLane string) int {
	if laneID == originLane {
		return 0
	}
	switch laneID {
	case ZenLaneID, KiloLaneID:
		return 1
	case OpenRouterLaneID:
		return 2
	default:
		// ⚠️ 未知车道 —— 当成"要用用户额度"处理（保守）。
		return 2
	}
}

// orderLanesForSwitch 按车道优先级重排候选链。
//
// ⚠️ **稳定排序** —— 同组内保持原有相对顺序。
func orderLanesForSwitch(cands []candidate, originLane string) []candidate {
	if len(cands) < 2 {
		return cands
	}
	sort.SliceStable(cands, func(i, j int) bool {
		return lanePriorityForSwitch(cands[i].Lane, originLane) <
			lanePriorityForSwitch(cands[j].Lane, originLane)
	})
	return cands
}

// orderPlanModels 把**计划性调用列表**里的可用模型按槽位顺序提到前面。
//
// ⚠️ **"不可用"的口径** —— 与路由实际会跳过的口径一致：
//   - 车道没就绪（没有凭据/关闭）→ 不可用
//   - 探测结论是**新鲜的负面结论**（限流/区域封锁/不可用）→ 不可用
//   - 限流账本里正在冷却 → 不可用
//
// ⚠️ 三个槽位都不在候选链里（或都不可用）→ **原样返回**，
//
//	落回原有模型池顺序。这保证"计划列表"永远不会把池子缩成空。
func (r *Router) orderPlanModels(cands []candidate) []candidate {
	plan := r.planModelList()
	if len(plan) == 0 || len(cands) < 2 {
		return cands
	}
	byFull := map[string]candidate{}
	for _, c := range cands {
		byFull[c.Full] = c
	}
	planned := make([]candidate, 0, len(plan))
	used := map[string]bool{}
	for _, full := range plan {
		c, ok := byFull[full]
		if !ok || used[c.Full] || !r.planCandidateUsable(c) {
			continue
		}
		used[c.Full] = true
		planned = append(planned, c)
	}
	if len(planned) == 0 {
		return cands
	}
	out := make([]candidate, 0, len(cands))
	out = append(out, planned...)
	for _, c := range cands {
		if !used[c.Full] {
			out = append(out, c)
		}
	}
	return out
}

// planCandidateUsable 报告一个计划槽位的模型当前可不可用。
func (r *Router) planCandidateUsable(c candidate) bool {
	// ① 车道就绪。
	if r.lanes != nil {
		if l, ok := r.lanes.Get(c.Lane); ok && !l.Ready() {
			return false
		}
	}
	// ② 限流账本冷却中。
	if r.throttle != nil && r.throttle.Throttled(c.Full) {
		return false
	}
	// ③ 新鲜的负面探测结论。
	if p := r.catalog.Probe(c.Full); p.Fresh(ProbeTTL) {
		switch p.State {
		case StateAvailable, StateUnknown:
			return true
		default:
			return false
		}
	}
	return true
}

// sleepCtx 等一段时间，但**能被 ctx 取消**。
//
// ⚠️ 必须用 ctx 版 —— 用户取消请求时，重试等待要立刻结束。
//
//	用 time.Sleep 的话，取消后还会白等 250ms。
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// restrictToRequested 把候选链裁剪成**只剩请求的模型**。
//
// ⚠️ 找不到请求的模型时**返回空链** ——
//
//	那样路由器会报"没有可用的模型"，而不是**偷偷换一个**。
//	这正是"关闭 = 不换模型"的含义。
//
// ⚠️ 匹配顺序与 `pickRequested` 一致：**全名 → 同车道同名 → 跨车道同名**。
//
//	只有前两级都落空时，才退到跨车道同名（目录刷新导致前缀变化时的兜底）。
func restrictToRequested(cands []candidate, laneID, upstream string) []candidate {
	if i := pickRequested(cands, laneID, upstream); i >= 0 {
		return cands[i : i+1]
	}
	return nil
}

// restrictToRequestedStrict 是**前缀锁定车道**时的候选裁剪。
//
// ⚠️ 与 `restrictToRequested` 的唯一差别：**没有第 ③ 步跨车道同名兜底**。
//
//	用户点名 `kilo/xxx` 时，zen 的同名模型不允许顶上来 ——
//	那种"兜底"正是把请求静默送到 zen 的根因。
//
// 没有候选 → 返回 nil，上层会给出"车道 kilo 不可用/模型不存在"的明确错误。
func restrictToRequestedStrict(cands []candidate, laneID, upstream string) []candidate {
	if laneID == "" {
		return restrictToRequested(cands, laneID, upstream)
	}
	requested := laneID + "/" + upstream
	for i := range cands {
		if cands[i].Lane == laneID && cands[i].Full == requested {
			return cands[i : i+1]
		}
	}
	for i := range cands {
		if cands[i].Lane == laneID && cands[i].Model == upstream {
			return cands[i : i+1]
		}
	}
	return nil
}

// pickRequested 在候选链里定位**请求的那个模型**。
//
// ⚠️ 为什么不能只比裸名 ——
//
//	`zen/mimo-v2.6-flash-free` 与 `kilo/mimo-v2.6-flash-free` 的裸名相同，
//	只比裸名时，"排序靠前的那个车道"会抢走请求，
//	于是用户点名的模型被**静默路由到另一条车道**（同名不同上游，行为/额度都不同）。
//
//	所以优先级是：
//	  ① 全名完全一致（`车道/模型`）
//	  ② 同一车道 + 同名
//	  ③ 其他车道的同名（仅在目录里查不到原车道时兜底）
func pickRequested(cands []candidate, laneID, upstream string) int {
	requested := laneID + "/" + upstream
	for i := range cands {
		if cands[i].Full == requested {
			return i
		}
	}
	for i := range cands {
		if cands[i].Lane == laneID && cands[i].Model == upstream {
			return i
		}
	}
	for i := range cands {
		if cands[i].Model == upstream {
			return i
		}
	}
	return -1
}

// scoreFor 给一个模型算排序键。
func (r *Router) scoreFor(m ModelInfo, index int) candidateScore {
	score := candidateScore{avail: 1, known: 1, index: index}

	// 探测状态。
	//
	// ⚠️ `m.ID` **已经带车道前缀** —— 直接用它查，
	//   不同车道的同名模型不会撞。
	probe := r.catalog.Probe(m.ID)
	if probe.Fresh(ProbeTTL) {
		switch probe.State {
		case StateAvailable:
			score.avail = 0
		case StateUnknown:
			score.avail = 1
		case StateThrottled:
			score.avail = 2
		case StateRegionBlock:
			score.avail = 3
		case StateAbuseBlocked:
			score.avail = 4
		case StateUnavailable:
			score.avail = 5
		}
	}

	// 限流账本 —— 冷却中的模型要往后排。
	if r.throttle != nil && r.throttle.Throttled(m.ID) {
		score.avail += 1
	}

	// 延迟。
	if m.TTFTMs > 0 {
		score.known = 0
		score.ttft = m.TTFTMs
	}

	// 能力结论的可信度。
	score.capConfidence = capabilityConfidence(m.Capability)
	return score
}

// capabilityConfidence 把能力来源映射成可信度（0 最好）。
//
// ⚠️ 顺序：实测 > 上游声明 > 第三方目录 > 规则表 > 未知。
func capabilityConfidence(source CapabilitySource) int {
	switch source {
	case CapabilitySourceProbe:
		return 0
	case CapabilitySourceListing:
		return 1
	case CapabilitySourceHeuristic:
		return 2
	default:
		return 3
	}
}

// sortCandidates 按排序键排（稳定）。
func sortCandidates(cands []candidate) {
	// 用插入排序 —— 候选数不大（几十个），而且要稳定。
	for i := 1; i < len(cands); i++ {
		for j := i; j > 0 && less(cands[j].score, cands[j-1].score); j-- {
			cands[j], cands[j-1] = cands[j-1], cands[j]
		}
	}
}

// TurnResult 是一次成功推理的结果。
type TurnResult struct {
	// Usage 是 token 计量。
	Usage  *Usage
	Finish FinishReason
	Lane   string
	Model  string
	// Attempts 是物理尝试次数。
	Attempts int
	// Failed 记录每次失败的候选与错误。
	Failed []AttemptFailure
	// Resolution 是本次请求的**解码路径**（前缀锁定 / 目录匹配 / 未命中）。
	//
	// ⚠️ 排障时必须能看到"这条请求是怎么被解析到车道的" ——
	//   否则同名模型跨车道的问题只能靠猜。
	Resolution string
	// Attempted 是**每一次物理尝试**的落点与结果（成功 / 失败都记）。
	//
	// ⚠️ 失败请求也要能回答"到底试了哪条车道、哪个上游模型" ——
	//   只靠 `Failed` 时，日志里只剩"实际= 车道=?"，用户没法核对。
	Attempted []AttemptLog
}

// AttemptLog 记录一次物理尝试。
type AttemptLog struct {
	Lane  string
	Model string // 上游裸 id（`kilo/xxx` 里的 `xxx`）
	Full  string // 对外全名（`车道/上游`）
	// Result 是这一试的结果摘要：`成功` 或 `失败(错误码): 上游原文`。
	Result string
}

// AttemptFailure 记录一次失败的尝试。
type AttemptFailure struct {
	Lane  string
	Model string
	Code  Code
	Msg   string
	// RetryAfterSec 是上游给的退避秒数（0 = 没给）。
	//
	// ⚠️ 必须一路保留到最终错误 ——
	// 客户端靠它决定等多久再重试。
	RetryAfterSec int
}

// Complete 在候选链上跑一轮推理。
//
// emit 收到的是**统一事件**。⚠️ 关键：一旦 emit 出去过内容，就**不能**再换候选 ——
// 所以换候选之前必须确认还没吐过东西。
//
// commit 回调报告"是否已经吐过内容"。它由上层提供，
// 因为"什么算吐过内容"取决于 encoder 的提交语义
// （思考不算，文本与工具调用算）。
func (r *Router) Complete(ctx context.Context, turn *Turn, emit func(Chunk), committed func() bool) (*TurnResult, *Error) {
	needs := SummarizeNeeds(turn.Messages, len(turn.Tools) > 0)
	cands := r.BuildCandidates(turn.Lane, turn.Model, needs)

	// ⚠️ **请求原本要的车道** —— 判断"要不要换"的基准。
	//   必须与 `BuildCandidates` 用**同一个解析**，
	//   否则同名模型跨车道时会出现"候选排到了 A、许可却认为原车道是 B"。
	originLane, upstream, strict := r.resolveTarget(turn.Lane, turn.Model)
	// 裸名走目录匹配时，落点由目录决定 —— 用候选链首项定"原车道"。
	if !strict && originLane == "" && len(cands) > 0 {
		originLane = cands[0].Lane
	}

	resolution := decodePath(strict, originLane, upstream)
	// ⚠️ 前缀锁定但该车道没有这个模型时，解码行必须**当场说清** ——
	//   否则日志显示"前缀锁定 → kilo/xxx"，实际却由别的车道应答。
	if strict && originLane != "" {
		switch {
		case len(cands) == 0:
			resolution += "（该车道没有可用候选）"
		case cands[0].Lane != originLane:
			resolution += "（该车道无此模型，已回落 " + cands[0].Lane + "）"
		}
	}
	result := &TurnResult{Attempts: 0, Resolution: resolution}

	// ⚠️ 每一次物理尝试都要留证据（成功 / 失败都记）。
	noteAttempt := func(c *candidate, e *Error) {
		a := AttemptLog{Lane: c.Lane, Model: c.Model, Full: c.Full, Result: "成功"}
		if e != nil {
			a.Result = "失败(" + string(e.Code) + "): " + e.Message
		}
		result.Attempted = append(result.Attempted, a)
	}

	start := time.Now()
	slowBudgetUsed := int64(0)
	quotaBudgetUsed := int64(0)

	// ⚠️ **同一个模型的重试次数**。
	//
	// 「自动跨模型调用」关闭时，候选链只剩**一个**候选 ——
	// 这个循环就是"重试当前模型"的全部机制。
	//
	// 开启时，它仍然有意义：某个模型可能是**抖动**（传输层断开、
	// 5xx），重试一下就成了，不该为此换模型。
	//
	// ⚠️ **429 不重试**（配额按 session 记账，重试 = 再烧一次）。
	//   这由 `Error.RetrySameModel()` 决定。
	sameModelRetries := 0

	for i := range cands {
		cand := &cands[i]
		if cand.tried {
			continue
		}
		// ⚠️ **最多试几个候选** ——
		//
		//   之前 `maxFailover` 只声明了从来没被用过，
		//   于是"试多少个"完全取决于候选链有多长。
		//
		//   「自动跨模型调用」扩到**整个模型池**之后，这会变成真问题：
		//   池子里有 40 个模型，逐个试 = 一轮请求打满所有上游的额度。
		//
		//   6 个是实测出来的甜点 ——
		//   够穿过"一条车道整体不可用"（1 次）+ 几个模型级失败，
		//   又不至于把用户的所有上游都试一遍。
		if result.Attempts >= r.maxFailover {
			break
		}
		cand.tried = true

		// ⚠️ **「自动跨模型调用」的许可检查 —— 必须在尝试之前**。
		//
		// ⚠️ **关闭时，候选链里压根就没有别的模型** ——
		//   这个分支根本不会走到（`BuildCandidates` 已经裁剪过了）。
		//   保留它是为了**双保险**：万一将来有别的路径塞进候选，
		//   这里还能拦住。
		//
		// ⚠️ **开启时**，规则是：
		//
		//	免密车道之间（Kilo ↔ Zen）→ **随便换**，不花任何人的钱
		//	换到 **OpenRouter**（用用户自己的 Key）→ **花用户自己的额度**
		//
		// 开着这个开关就意味着用户**同意**用他的额度兜底，
		// 所以这里不再拦 OpenRouter。
		if !r.allowModelSwitch && cand.Lane != originLane {
			// 「自动跨模型调用」是关的 —— 不许换模型。
			continue
		}

		// 车道没就绪 → 跳过（不浪费一次尝试）。
		l, ok := r.lanes.Get(cand.Lane)
		if !ok || !l.Ready() {
			continue
		}

		// 已经吐过内容 → 不能再换。
		if committed != nil && committed() {
			break
		}
		// 预算检查。
		elapsed := time.Since(start).Milliseconds()
		if slowBudgetUsed >= r.slowBudgetMS || quotaBudgetUsed >= r.quotaBudgetMS {
			break
		}

		// 构造这一轮的 Turn。
		attempt := *turn
		attempt.Lane = cand.Lane
		attempt.Model = cand.Model
		attempt.Seed = turn.Seed + "\x00" + cand.Lane

		result.Attempts++
		usage, finish, uerr := l.Do(ctx, &attempt, emit)
		noteAttempt(cand, uerr)
		if uerr == nil {
			result.Usage = usage
			result.Finish = finish
			result.Lane = cand.Lane
			// ⚠️ **对外报全名**（带车道前缀）——
			//   用户看到的是"这个模型"，不是"某个裸 id"。
			result.Model = cand.Full
			// 成功 → 清掉限流状态。
			//
			// ⚠️ 真实流量是比任何探测样本都硬的证据。
			//
			// ⚠️ 限流账本和证据都用**全名**当键 ——
			//   不同车道的同名模型不能共用一条记录。
			if r.throttle != nil {
				r.throttle.MarkOK(cand.Full)
			}
			// ⚠️ **留一份成功证据** —— 让下一次探测写结论时对质。
			//   免密车道的配额按出口 IP 记账、以秒级波动，
			//   所以探测的一次拒绝可能只是"这个出口在这一刻"的快照。
			//   没有这份证据，面板会跟事实脱节
			//   （显示"被风控"但实际一直能调）。
			if r.catalog != nil {
				r.catalog.NoteEvidence(cand.Full)
			}
			return result, nil
		}

		result.Failed = append(result.Failed, AttemptFailure{
			Lane:          cand.Lane,
			Model:         cand.Full,
			Code:          uerr.Code,
			Msg:           uerr.Message,
			RetryAfterSec: uerr.RetryAfterSec,
		})

		// 已经吐过内容 → 不能换，只能把错误交出去。
		if committed != nil && committed() {
			// ⚠️ 失败也要留下落点 —— 日志的 `实际=`/`车道=` 靠它。
			result.Lane, result.Model = cand.Lane, cand.Full
			return result, uerr
		}

		// ⚠️ **先问"要不要原地重试同一个模型"** ——
		//
		//   这优先于"换下一个候选"。
		//
		//   「自动跨模型调用」**关闭**时，候选链只有一个 ——
		//   重试就是唯一的选择（重试还不行就报错）。
		//
		//   开启时也一样：抖动类失败（传输/5xx）重试一下常常就成了，
		//   换模型反而是**浪费**（换了个更慢的模型，还多烧一份额度）。
		//
		//   ⚠️ 但**不能无限重试** —— 要受预算和时间约束。
		//
		// ⚠️ **这里必须用内层循环，不能用 `continue`** ——
		//   `continue` 会跳到**下一个候选**，而我们要的是**再试这一个**。
		for uerr.RetrySameModel() && sameModelRetries < r.maxSameModelRetries {
			elapsed := time.Since(start).Milliseconds()
			// 预算检查 —— 跟换候选用同一套预算。
			if elapsed >= r.quotaBudgetMS || elapsed >= r.slowBudgetMS {
				break
			}
			sameModelRetries++
			// ⚠️ 记录这次失败（用户能看到"试了两次"）。
			result.Failed = append(result.Failed, AttemptFailure{
				Lane:          cand.Lane,
				Model:         cand.Full,
				Code:          uerr.Code,
				Msg:           uerr.Message,
				RetryAfterSec: uerr.RetryAfterSec,
			})
			// 等一小会儿再试 —— 立刻重试往往还是同样的错。
			if err := sleepCtx(ctx, r.sameModelRetryDelay); err != nil {
				result.Lane, result.Model = cand.Lane, cand.Full
				return result, uerr
			}
			// ⚠️ **再试同一个候选**。
			result.Attempts++
			usage, finish, rerr := l.Do(ctx, &attempt, emit)
			noteAttempt(cand, rerr)
			if rerr == nil {
				result.Usage = usage
				result.Finish = finish
				result.Lane = cand.Lane
				result.Model = cand.Full
				if r.throttle != nil {
					r.throttle.MarkOK(cand.Full)
				}
				if r.catalog != nil {
					r.catalog.NoteEvidence(cand.Full)
				}
				return result, nil
			}
			uerr = rerr
		}

		// 不可切换 → 立即返回。
		if !uerr.Switchable() {
			result.Lane, result.Model = cand.Lane, cand.Full
			return result, uerr
		}

		// 记账。
		switch uerr.Code {
		case CodeQuota:
			if r.throttle != nil {
				r.throttle.MarkThrottled(cand.Full, uerr.RetryAfterSec)
			}
			quotaBudgetUsed = elapsed
			// ⚠️ **账号级配额** → 同车道的其他模型**也一样**会被拒。
			//
			//	OpenCode Zen 的 "Free usage limit reached" 是
			//	**整个账号**的配额，不是某一个模型的。
			//
			//	不跳过的话，候选链会把 6 次尝试（maxFailover）
			//	**全部烧在同一条车道上** ——
			//	结果就是"开着自动跨模型调用，却永远轮不到 Kilo"。
			//
			//	这正是实测踩到的：zen 配额耗尽 → 429 → 6 次都是 429
			//	→ 尝试预算耗尽 → 返回 429，**根本没试过 Kilo**。
			//
			// ⚠️ 但**模型级**配额（"这个模型没额度了"）
			//   **不能**跳车道 —— 那会让同车道的其他模型白白受牵连。
			if isAccountQuota(uerr.Message) {
				skipLane(cands, i, cand.Lane)
			}
		case CodeServer, CodeTimeout, CodeTransport, CodeReset, CodeEmpty, CodeUnknown:
			slowBudgetUsed = elapsed
		case CodeFreeTier, CodeAbuseBlocked:
			// 车道级不可用 —— 换车道有意义。
			//
			// ⚠️ 但**同一车道内**换模型没用：
			// 闸门/风控对所有模型一视同仁，
			// 所以要把同车道的其余候选也跳过。
			skipLane(cands, i, cand.Lane)
		}

		// ⚠️ 许可检查在**尝试之前**做了
		//   （见循环开头的 `allowModelSwitch` 判断），这里不再重复判断。
	}

	// 候选链走完了。
	// ⚠️ 失败也要留下"最后落在哪" —— 日志的 `实际=`/`车道=` 靠它。
	if result.Lane == "" {
		switch {
		case len(result.Failed) > 0:
			last := result.Failed[len(result.Failed)-1]
			result.Lane, result.Model = last.Lane, last.Model
		case originLane != "":
			result.Lane = originLane
			result.Model = FormatModelID(originLane, upstream)
		default:
			result.Model = upstream
		}
	}
	if len(result.Failed) == 0 {
		var msg string
		switch {
		case strict:
			// ⚠️ 必须说清**为什么**：车道不存在 / 未就绪 / 模型不在池里。
			laneKnown, laneReady := false, false
			if r.lanes != nil {
				if l, ok := r.lanes.Get(originLane); ok {
					laneKnown, laneReady = true, l.Ready()
				}
			}
			switch {
			case !laneKnown:
				msg = fmt.Sprintf("前缀锁定到车道 %s，但该车道不存在 —— 模型 %s 无法调用", originLane, upstream)
			case !laneReady:
				msg = fmt.Sprintf("前缀锁定到车道 %s，但该车道未配置或未就绪（没有凭据/已关闭）—— 模型 %s 无法调用，不会换到其他车道", originLane, upstream)
			default:
				msg = fmt.Sprintf("前缀锁定到车道 %s，但模型 %s 不在该车道的可用模型池（可能不是免费模型、被过滤或清单尚未刷新）—— 不会改用其他车道的同名模型", originLane, upstream)
			}
		case originLane != "":
			msg = fmt.Sprintf("模型 %s 当前不可用（目录把它解析到车道 %s，但该车道未就绪/已关闭，或不在模型池中）", upstream, originLane)
		default:
			msg = fmt.Sprintf("模型 %s 无法解析 —— 模型名没有车道前缀（形如 kilo/模型名），目录里也没有这个模型", upstream)
		}
		if !r.allowModelSwitch {
			msg += "；未开启「自动跨模型调用」，不会回落到其他模型"
		}
		return result, &Error{
			Code:    CodeServer,
			Message: msg,
		}
	}
	// 报最后一个错误（它最接近真相）。
	last := result.Failed[len(result.Failed)-1]
	// ⚠️ **必须带上 RetryAfterSec** —— 客户端的退避逻辑靠它决定等多久。
	// 丢了它，客户端就只能瞎猜（通常是一秒后重试 → 再被拒）。
	final := &Error{
		Code:      last.Code,
		Message:   last.Msg,
		Exhausted: true,
	}
	if last.RetryAfterSec > 0 {
		final.RetryAfterSec = last.RetryAfterSec
	}
	// ⚠️ **关闭换模型时，错误必须说明"没有下文"** ——
	//   否则用户看到"出口被风控"，会以为网关会自动换个模型再试。
	if !r.allowModelSwitch {
		final.Message += "（未开启「自动跨模型调用」，不会切换到其他模型）"
	}
	return result, final
}

// candidateNeedsCredential 报告候选所在的车道是否要花**用户自己的**额度。
func (r *Router) candidateNeedsCredential(laneID string) bool {
	if r.lanes == nil {
		return false
	}
	l, ok := r.lanes.Get(laneID)
	if !ok {
		return false
	}
	return laneNeedsCredential(l)
}

// isAccountQuota 报告这个配额错误是不是**账号级**的。
//
// ⚠️ **为什么要分两种** ——
//
//	账号级（"Free usage limit reached"）：
//	  配额是**整个账号**共享的，同车道的其他模型**一样**会被拒。
//	  → 跳过同车道，直接去另一条免密车道。
//
//	模型级（"This model has no quota" 之类）：
//	  只有**这一个模型**没额度，同车道的其他模型还好着。
//	  → **不能**跳车道，否则白白浪费一堆可用模型。
//
// ⚠️ 判据用**消息文本** —— 上游没给结构化的错误码，
//
//	只能认字。匹配不到就当成**模型级**（保守：不跳车道）。
func isAccountQuota(msg string) bool {
	m := strings.ToLower(msg)
	for _, needle := range []string{
		"free usage limit reached",
		"usage limit reached",
		"daily limit reached",
		"monthly limit reached",
		"quota exhausted",
		"too many requests",
	} {
		if strings.Contains(m, needle) {
			return true
		}
	}
	return false
}

// skipLane 把同车道的其余候选标记为已试。
//
// ⚠️ 闸门拒绝与 IP 风控对**所有模型**一视同仁 ——
// 同车道内换模型是白费功夫。
//
// ⚠️ **只跳同车道，不跳别的车道** ——
//
//	zen 被风控不该连累 Kilo（那是**另一个**上游，出口 IP 都可能不同）。
func skipLane(cands []candidate, from int, laneID string) {
	for j := from + 1; j < len(cands); j++ {
		if cands[j].Lane == laneID {
			cands[j].tried = true
		}
	}
}

// resolveTarget 解析**请求指向的（车道, 裸模型名）**。
//
// ⚠️ 优先级：
//
//	① 模型 id 里带的车道前缀，**且该前缀确实是已知车道**（`kilo/...`）
//	② 客户端显式指定的车道（`turn.Lane`）
//	③ 目录里**第一条**含该裸名的车道（`ResolveLaneForModel`，顺序稳定）
//	④ **没有兜底** —— 解析不出来就返回空车道，由上层报错
//
// ⚠️ 返回值 `strict` = 前缀/显式指定**命中了一条真实车道** ——
//
//	此时**只能在这条车道的候选里挑**；同名模型绝不能抢走它。
//
// ⚠️ 前缀的归一化：去首尾空白 + 忽略大小写 ——
//
//	`KILO/x` 与 `kilo /x` 都指向 kilo 车道（用户手输模型名时很常见）。
//
// ⚠️ 这个结果同时用于：
//
//	· 把请求的候选提到候选链最前（`BuildCandidates`）
//	· 判定"原车道"（`Complete` 的换车道许可）
//
// 两处**必须**来自同一个解析，否则"候选排到了 A，许可却认为原车道是 B"。
func (r *Router) resolveTarget(explicitLane, requestedModel string) (laneID, upstream string, strict bool) {
	requestedModel = strings.TrimSpace(requestedModel)
	prefix, rest := ParseModelID(requestedModel)
	upstream = strings.TrimSpace(rest)

	// ① 前缀命中**已知车道** → 硬锁定。
	if prefix != "" {
		if canon, ok := r.canonicalLane(prefix); ok {
			return canon, upstream, true
		}
		// ⚠️ 前缀不对应任何已知车道（`foo/bar` 是上游自己的命名空间）→
		//   整串当成裸模型，交给目录匹配，**不能**把它当车道。
		upstream = requestedModel
	}

	// ② 客户端显式指定的车道。
	if explicit := strings.TrimSpace(explicitLane); explicit != "" {
		if canon, ok := r.canonicalLane(explicit); ok {
			return canon, upstream, true
		}
	}

	// ③ 目录匹配（顺序稳定）。
	if r.catalog != nil {
		if id := ResolveLaneForModel(r.catalog.Models(), upstream); id != "" {
			return id, upstream, false
		}
	}

	// ④ ⚠️ **不再兜底 Zen** —— 解析不出来就明确失败，
	//   否则任何拼错/无前缀的模型都会被静默送到 Zen。
	return "", upstream, false
}

// canonicalLane 把用户写的车道前缀归一化成真实的车道 id。
//
// ⚠️ 比较去首尾空白 + 忽略大小写；车道 id 本身是小写常量。
func (r *Router) canonicalLane(raw string) (string, bool) {
	p := strings.ToLower(strings.TrimSpace(raw))
	if p == "" {
		return "", false
	}
	if r.lanes != nil {
		if _, ok := r.lanes.Get(p); ok {
			return p, true
		}
		return "", false
	}
	switch p {
	case ZenLaneID, KiloLaneID, OpenRouterLaneID, CustomLaneID:
		return p, true
	}
	return "", false
}

// decodePath 把解析路径写成日志可读的一行。
//
// ⚠️ 用户排障时要能直接看到"这条请求到底是怎么被解码的" ——
//
//	前缀锁定 / 目录匹配 / 完全没命中，三种路径必须区分。
func decodePath(strict bool, laneID, upstream string) string {
	switch {
	case strict && laneID != "":
		return "前缀锁定 → " + laneID + "/" + upstream
	case laneID != "":
		return "目录匹配 → " + laneID + "/" + upstream
	default:
		return "无车道前缀，目录中也未命中"
	}
}

// EffortLevels 是统一的档位阶梯。
//
// ⚠️ 免密车道**接受但忽略** `reasoning_effort` ——
// 唯一真正生效的是 `max_tokens`。所以那里的档位是**输出预算**。
//
// 而 OpenRouter 用的是**真档位**（`reasoning.effort`）。
// 两套语义在 lane 层各自处理，网关层只给出一个统一的档位名。
var EffortLevels = []string{"light", "balanced", "deep"}

// DefaultEffort 是默认档位。
const DefaultEffort = "balanced"

// EffortOff 是"关掉思考"这个意图的统一表示。
//
// ⚠️ **它必须是一个独立档位**，不能被归并进 `light`。
// 之前 `NormalizeEffort` 把 `none`/`off`/`disabled` 全映射成 `light`，
// 理由是"免密车道做不到真的关掉思考" —— 但那把**意图**也一起丢了。
// OpenRouter 车道是**能**关掉的（除非模型 mandatory），而它拿到的永远是
// `light`，于是"关掉思考"这个请求**静默失效**。
//
// 正确做法：**保留意图**，让每条车道自己决定能不能做到：
//
//	免密车道  → 忽略（做不到），或降到最低预算
//	OpenRouter → 真的发 `reasoning.enabled: false`
//	             （mandatory 的模型降级成最低档位）
const EffortOff = "off"

// NormalizeEffort 把客户端的各种写法收敛到统一档位。
//
// ⚠️ "关掉思考"的写法（none/off/disabled）收敛到 **EffortOff**，
// 而不是 `light` —— 意图必须保留到 lane 层。
// 见 EffortOff 的注释。
func NormalizeEffort(level string) string {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "none", "off", "disabled", "disable", "false":
		return EffortOff
	case "minimal", "low", "light":
		return "light"
	case "medium", "balanced", "default":
		return "balanced"
	case "high", "xhigh", "max", "deep":
		return "deep"
	default:
		return ""
	}
}

// EffortBudget 是档位对应的输出预算。
type EffortBudget struct {
	Light int
	// Balanced 是均衡档的预算。
	Balanced int
	// Deep 是深思档的预算（0 = 用模型的最大输出）。
	Deep int
	// AlwaysThinkingFactor 是"思考关不掉"时的倍数。
	//
	// ⚠️ 思考 token 与可见答案**共享**同一个上限。
	// 实测某个模型 82% 的输出 token 是推理 ——
	// 8192 的上限只剩 ~1500 给答案，长回合约每三次就 `length` 截断。
	AlwaysThinkingFactor int
}

// DefaultEffortBudget 是默认的档位预算。
var DefaultEffortBudget = EffortBudget{
	Light:                2048,
	Balanced:             8192,
	Deep:                 0,
	AlwaysThinkingFactor: 2,
}

// BudgetFor 算一个模型在某个档位下的输出预算。
//
// 优先级（来自实测）：
//
//  1. 档位的天花板 —— 永远生效
//  2. 模型容量（×1.5 headroom）—— 作为 deep 档的天花板
//  3. 客户端显式的 max_tokens —— 压过上面两者
//  4. 配置的兜底值 —— 只约束"未知能力"的模型
//
// ⚠️ 第 4 条是个陷阱：曾经兜底值会从上方压住**所有**模型，
// 于是那个实测 256K 的模型只答了 32K。
func BudgetFor(level string, info ModelInfo, requested int, budget EffortBudget) int {
	ceiling := 0
	normalized := NormalizeEffort(level)
	switch normalized {
	case "light":
		ceiling = budget.Light
	case "balanced":
		ceiling = budget.Balanced
	case "deep":
		ceiling = budget.Deep
	case EffortOff:
		// ⚠️ **"关掉思考"** ——
		// 免密车道做不到真的关掉（它接受但忽略 reasoning_effort），
		// 所以这里给**最紧的预算**：思考占掉的那部分越少，留给答案的就越多。
		//
		// ⚠️ 但**不承诺**真的关掉了 ——
		//   UI 上要如实说"这条车道关不掉思考"。
		ceiling = budget.Light
	}
	// deep 档没有固定天花板 → 用模型容量。
	if ceiling <= 0 {
		ceiling = info.MaxOutput
	}
	// 模型容量也压着 —— 档位不能超过模型的上限。
	if info.MaxOutput > 0 && ceiling > info.MaxOutput {
		ceiling = info.MaxOutput
	}
	// 思考关不掉的模型 → 整档加倍。
	//
	// ⚠️ 但"关掉思考"时**不加倍** ——
	//   用户明确要求少思考，加倍正好相反。
	if !info.CanDisableThinking && info.Reasoning &&
		budget.AlwaysThinkingFactor > 1 && normalized != EffortOff {
		ceiling *= budget.AlwaysThinkingFactor
	}
	// 客户端显式给的 max_tokens 压过一切。
	if requested > 0 {
		ceiling = requested
	}
	return ceiling
}
