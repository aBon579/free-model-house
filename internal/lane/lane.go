package lane

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// **Lane 抽象** —— 上游源的统一接口。
// 关键认识：**一个源 = 一条 lane，不是一个模型。**
// 模型是清单里的一行，带 lane 标签。
// 这样做的好处：故障转移、限流账本、能力过滤、看板
// **全部与源无关** —— 加一个新源不需要改任何一处核心逻辑。
// 免密车道（zen）与 OpenRouter 的性质完全不同：
//	zen         没有凭据、共享配额、可能被 IP 风控、需要客户端指纹
//	openrouter  要用户自己的 Key、按 Key 限速、额度是"每天 N 次"
// 但它们对上层是**一样的**：给一个 Turn，产出一串统一事件。
// 详见 docs/design/00-overview.md §5.2。

// Lane 是一个上游源。
type Lane interface {
	// ID 是源的标识（"zen" / "openrouter"）。
	ID() string

	// ListModels 拉取该源的模型清单。
	ListModels(ctx context.Context) ([]ModelInfo, *Error)

	// Do 发起一次推理，产出统一事件。
	Do(ctx context.Context, turn *Turn, emit func(Chunk)) (*Usage, FinishReason, *Error)

	// Probe 探一个模型的可用性与首字延迟。
	Probe(ctx context.Context, model string) ProbeResult

	// Ready 报告这条 lane 是否可用（比如没配 Key）。
	Ready() bool

	// Status 报告这条 lane 的状态（给看板用）。
	Status() LaneStatus
}

// AutoProbeable 是"这条车道参与自动探测吗"的可选接口。
//
// ⚠️ **为什么要区分** ——
//
// 自动探测每 15 分钟跑一轮，**每个模型一次真实推理**。
// 对**匿名共享池**（免密车道、Kilo）来说这不花任何人的钱。
// 但对**用用户自己 Key 的车道**（OpenRouter）来说 ——
//
//	实测（2026-10-20）：一轮 18 个模型就把当天的
//	`free-models-per-day` 配额打满了。
//
// 那还是**用户自己的额度**。拿它做周期���全量探测不合适。
//
// 所以实现了这个接口的车道**不参与自动探测** ——
// 它的可用性由**真实流量**验证（面板上的"实际服务"列）。
//
// ⚠️ **不实现这个接口的车道默认参与探测**。
type AutoProbeable interface {
	// AutoProbeable 报告这条车道是否参与自动探测。
	AutoProbeable() bool
}

// laneAutoProbeable 报告一条车道是否参与自动探测。
//
// ⚠️ 不实现 `AutoProbeable` 的车道**默认参与**。
func laneAutoProbeable(l Lane) bool {
	if ap, ok := l.(AutoProbeable); ok {
		return ap.AutoProbeable()
	}
	return true
}

// Credentialed 是"这条车道要不要花**用户自己的**额度"的可选接口。
//
// ⚠️ **为什么需要它** ——
//
// 「自动跨模型调用」的许可**不能一刀切**：
//
//	① 免密车道之间（zen ↔ Kilo）切换 —— **不花任何人的钱**。
//	② 切到**用用户自己 Key 的车道**（OpenRouter）——
//	   花**用户自己的额度**，所以**必须**用户显式开。
//
// ⚠️ 之前两种情况被当成一回事（都挂在同一个开关下），
// 结果是：zen 被封了，用户得手动开开关才能用上 Kilo ——
// 而 Kilo 明明是免费的。
type Credentialed interface {
	// NeedsCredential 报告这条车道是否需要用户自己的凭据。
	NeedsCredential() bool
}

// laneNeedsCredential 报告一条车道是否需要用户自己的凭据。
//
// ⚠️ **不实现 `Credentialed` 的车道默认**不需要** ——
//
//	它多半是免密的共享池。
//
// ⚠️ **未就绪的车道本来就会被跳过** ——
//
//	没配 Key 的 OpenRouter 车道 `Ready()` 返回 false，
//	所以"没声明但其实要 Key"这种情况不会漏出去。
func laneNeedsCredential(l Lane) bool {
	if c, ok := l.(Credentialed); ok {
		return c.NeedsCredential()
	}
	return false
}

// LaneStatus 是一条 lane 的状态。
type LaneStatus struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Ready 报告是否可用。
	Ready bool `json:"ready"`
	// Reason 是不 Ready 的原因（给用户看）。
	Reason string `json:"reason,omitempty"`
	// ModelCount 是模型数量。
	ModelCount int `json:"modelCount"`
	// LastRefresh 是上次刷新清单的时间。
	LastRefresh int64 `json:"lastRefresh,omitempty"`
	// LastError 是上次刷新的错误。
	LastError string `json:"lastError,omitempty"`
	// NeedsCredential 标记这条 lane 需要用户自己的凭据。
	NeedsCredential bool `json:"needsCredential,omitempty"`
	// CredentialHint 是凭据的指纹（绝不回显全文）。
	CredentialHint string `json:"credentialHint,omitempty"`
	// AutoProbe 标记这条车道**是否参与自动探测**。
	//
	// ⚠️ **必须是 false 时，界面上要说明为什么** ——
	//   否则用户看到满屏"未探测"会以为是 bug。
	//
	//   比如 OpenRouter：它的免费额度按天算且很少，
	//   自动探测等于**花用户自己的钱**。
	AutoProbe bool `json:"autoProbe"`
	// AutoProbeReason 是**不参与自动探测的原因**（给用户看的话）。
	AutoProbeReason string `json:"autoProbeReason,omitempty"`
}

// Registry 管理所有 lane。
type Registry struct {
	mu    sync.RWMutex
	lanes map[string]Lane
	// order 是 lane 的展示顺序。
	order []string
}

func NewRegistry() *Registry {
	return &Registry{lanes: map[string]Lane{}}
}

// Register 注册一条 lane。
func (r *Registry) Register(l Lane) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.lanes[l.ID()]; !exists {
		r.order = append(r.order, l.ID())
	}
	r.lanes[l.ID()] = l
}

// Get 取一条 lane。
func (r *Registry) Get(id string) (Lane, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	l, ok := r.lanes[id]
	return l, ok
}

// All 返回所有 lane（按注册顺序）。
func (r *Registry) All() []Lane {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Lane, 0, len(r.order))
	for _, id := range r.order {
		if l, ok := r.lanes[id]; ok {
			out = append(out, l)
		}
	}
	return out
}

// Status 返回所有 lane 的状态。
func (r *Registry) Status() []LaneStatus {
	lanes := r.All()
	out := make([]LaneStatus, 0, len(lanes))
	for _, l := range lanes {
		out = append(out, l.Status())
	}
	return out
}

// ParseModelID 解析一个对外的模型 id。
// 格式：`<lane>/<upstream-model>` 或 `<upstream-model>`（无前缀）。
// ⚠️ 上游模型 id 里**可能含斜杠**（如 `deepseek/deepseek-r1:free`），
// 所以按**第一个**斜杠分隔：`openrouter/deepseek/deepseek-r1:free`
// 无前缀时，交给调用方按清单里的归属解析。
func ParseModelID(id string) (lane string, model string) {
	id = strings.TrimSpace(id)
	i := strings.IndexByte(id, '/')
	if i <= 0 {
		return "", id
	}
	return id[:i], id[i+1:]
}

// FormatModelID 拼一个对外的模型 id。
func FormatModelID(lane, model string) string {
	if lane == "" {
		return model
	}
	return lane + "/" + model
}

// ResolveLaneForModel 猜一个无前缀模型 id 属于哪条 lane。
// 规则：先看清单里有没有这个 id；有就用它的 lane。
// 猜不到 → 交给调用方的默认车道。
func ResolveLaneForModel(catalog []ModelInfo, model string) string {
	base := baseModelID(model)
	for _, m := range catalog {
		if m.ID == model || baseModelID(m.ID) == base {
			return m.Lane
		}
	}
	// 退一步：按 id 后缀猜。
	for _, m := range catalog {
		if strings.Contains(m.ID, model) || strings.Contains(model, m.ID) {
			return m.Lane
		}
	}
	return ""
}

// ProbeState 是一个模型的探测状态。
type ProbeState string

const (
	// StateUnknown 还没探过，或结论过期。
	StateUnknown ProbeState = "unknown"
	// StateAvailable 可用。
	StateAvailable ProbeState = "available"
	// StateThrottled 被限流。
	StateThrottled ProbeState = "throttled"
	// StateRegionBlock 地域封锁。
	StateRegionBlock ProbeState = "region-blocked"
	// StateUnavailable 明确不可用（模型不存在、无权限）。
	StateUnavailable ProbeState = "unavailable"
	// StateAbuseBlocked 出口被风控（整条车道不可用）。
	StateAbuseBlocked ProbeState = "abuse-blocked"
)

// ProbeResult 是一次探测的结论。
type ProbeResult struct {
	// Model 是**带车道前缀**的模型 id（`zen/mimo-v2.6-flash-free`）。
	//
	// ⚠️ **必须带车道** —— 不同车道可以有**同名**的模型
	// （Kilo 和 OpenRouter 的免费 id 完全重叠）。
	// 不带前缀的话，两边的结论会互相覆盖 ——
	// 用户会看到"这个模型一会儿可用一会儿不可用"。
	//
	// ⚠️ 用**用户看得见的那个形式**当键，而不是在旁边另开一个 lane 字段 ——
	//   这样前端、数据库、日志、限流账本**全都用同一个键**，
	//   不用在每个地方各写一遍"剥前缀/加前缀"的转换。
	Model     string     `json:"model"`
	State     ProbeState `json:"state"`
	Detail    string     `json:"detail,omitempty"`
	LatencyMs int64      `json:"latencyMs,omitempty"`
	// TTFTMs 是首字延迟 —— 选路的关键信号。
	TTFTMs int64 `json:"ttftMs,omitempty"`
	At     int64 `json:"at"`
}

// Negative 报告这个状态是**负面**的。
//
// ⚠️ "负面" = 探测给出了"这个模型现在不行"的结论：
//
//	限流 / 地区受限 / 出口被风控 / 明确不可用
//
// 这类结论是**可以被真实流量推翻**的 ——
// 因为免密车道的配额按出口 IP 记账、以秒级波动，
// 一次拒绝可能只是"这个出口在这一刻"的快照。
//
// available / unknown 不是负面结论（没结论，或者结论是好的）。
func (s ProbeState) Negative() bool {
	switch s {
	case StateThrottled, StateRegionBlock, StateAbuseBlocked, StateUnavailable:
		return true
	default:
		return false
	}
}

// Fresh 报告一个探测结论是否还新鲜。
// ⚠️ 过期后读作 unknown —— 模型**仍然留在清单里、仍然可路由**，
// 直到一次新探测来续上这个断言。
// 一次坏样本不该让模型藏一整轮。
func (p ProbeResult) Fresh(ttl time.Duration) bool {
	if p.At == 0 {
		return false
	}
	return time.Since(time.UnixMilli(p.At)) < ttl
}

// ProbeTTL 是探测结论的有效期。
const ProbeTTL = 10 * time.Minute

// EvidenceTTL 是"真实流量成功"**推翻探测结论**的有效期。
//
// 问题：免密车道的配额**按出口 IP 记账**，以秒级波动。
// 上游的一次拒绝只是"这个出口在这一刻"的快照 ——
// **不是这个模型的属性**。
//
// 于是面板会跟事实脱节：
// ① 探测撞上配额墙 → 写"被风控"
// ② 用户手动调通了   → 事实是"能用"
// ③ 下一轮探测又撞墙 → 覆盖回"被风控"
// ④ 面板永远说不行，实际一直能用
//
// FMH 的解法：**把"真实流量成功"当成一份带时间戳的证据**，
// 存在限流账本里（`Book.LastOK`，**本来就持久化**），
// 探测写结论时先问一句"有没有更新的成功证据"：
//
//   - 有 → 结论**降级**成"未探测"（unknown），不是"可用"。
//     为什么不直接写"可用"？因为"可用"是探测的结论，
//     而证据只说"**刚才**是通的" —— 现在的状态仍然未知。
//     unknown 的语义正好是"没有可靠结论"，
//     而且它**不会**把模型从路由里摘掉。
//   - 没有 → 正常写结论。
//
// ⚠️ 为什么不用"直接写可用"：
//
//	那样会让面板显示"可用"，但路由其实**不知道**它现在能不能用 ——
//	证据是过去的，能力是当下的。写 unknown 更诚实，
//	而且 unknown 在排序里排在 available 之后、负面结论之前，
//	路由仍然会试它（这正是我们要的：证据说"刚才能通"，那就再试一次）。
//
// ⚠️ 为什么不用"直接覆盖成可用"：
//
//	那会让**过期的成功**变成永久的"可用" ——
//	模型真的坏掉了，面板还一直说可用。
//	所以证据也有 TTL，过期就失效。
const EvidenceTTL = 10 * time.Minute

// Catalog 是合并后的模型清单。
type Catalog struct {
	mu     sync.RWMutex
	models []ModelInfo
	// probes 是每个模型的探测结论。
	probes map[string]ProbeResult
	byLane map[string][]ModelInfo
	// evidence 是"真实流量成功"的时间戳（epoch ms）。
	//
	// ⚠️ 它跟 `Book.LastOK` 是**同一件事**的两个视角 ——
	//   这里是内存副本（探测写结论时要立刻查）；
	//   持久化那份在 `Book` 里（重启后能恢复）。
	evidence map[string]int64
}

func NewCatalog() *Catalog {
	return &Catalog{
		probes:   map[string]ProbeResult{},
		byLane:   map[string][]ModelInfo{},
		evidence: map[string]int64{},
	}
}

// Replace 用一份新清单替换整个内容。
// ⚠️ 探测结论**不清空** —— 一个新模型进来了不该让其他模型的
// 探测结论失效。清单刷新是 30 分钟一次，探测结论有 10 分钟 TTL，
// 清空会让路由在每次刷新后都退化成"没有可用性信息"。
// ⚠️ **但模型行的状态要重新套用** ——
// 新清单里的模型行是"干净"的（没有 state / 延迟）。
// 如果不把已知的探测结论套回去，清单一刷新，
// 所有模型的可用性就变成空的了
// （前端会显示"全部未知"，而结论其实就在 probes 里）。
func (c *Catalog) Replace(models []ModelInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.models = models
	c.byLane = map[string][]ModelInfo{}
	for _, m := range models {
		c.byLane[m.Lane] = append(c.byLane[m.Lane], m)
	}
	// ⚠️ 套用已有的探测结论。
	c.applyProbesLocked()
}

// applyProbesLocked 把探测结论套到模型行上。
// ⚠️ 调用方必须**持有锁**。
//
// ⚠️ **直接用模型 id 当键** —— 它已经带车道前缀了
// （`zen/mimo-v2.6-flash-free`），所以不同车道的同名模型天然不撞。
func (c *Catalog) applyProbesLocked() {
	for i := range c.models {
		p, ok := c.probes[c.models[i].ID]
		if !ok {
			continue
		}
		c.models[i].State = string(p.State)
		c.models[i].TTFTMs = p.TTFTMs
		// ⚠️ **记下探测时间** —— 手动和自动都记。
		c.models[i].ProbedAt = p.At
		if p.State == StateAvailable {
			c.models[i].Capability = CapabilitySourceProbe
		}
	}
}

// Merge 合并一批模型（只增不减）。
// ⚠️ **一次失败的拉取绝不能让清单变小** ——
// 清单空着 = 整个网关不可用。
//
// ⚠️ **去重靠模型 id 本身** ——
//
//	模型 id 已经带车道前缀了（`kilo/nvidia/x:free`），
//	所以不同车道的同名模型**天然不撞** ——
//	Kilo 和 OpenRouter 都提供 `nvidia/nemotron-3.5-super-vl:free`，
//	但它们是**两个不同的入口**（一个免 key，一个花用户额度）。
func (c *Catalog) Merge(models []ModelInfo) {
	c.mu.Lock()
	defer c.mu.Unlock()
	seen := map[string]bool{}
	for _, m := range c.models {
		seen[m.ID] = true
	}
	for _, m := range models {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		c.models = append(c.models, m)
		c.byLane[m.Lane] = append(c.byLane[m.Lane], m)
	}
	// ⚠️ 新进来的模型也要套用探测结论（可能之前探过）。
	c.applyProbesLocked()
}

// Models 返回全部模型。
func (c *Catalog) Models() []ModelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ModelInfo, len(c.models))
	copy(out, c.models)
	return out
}

// ModelsForLane 返回某条车道的模型。
func (c *Catalog) ModelsForLane(laneID string) []ModelInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]ModelInfo, len(c.byLane[laneID]))
	copy(out, c.byLane[laneID])
	return out
}

// Get 取一个模型。
func (c *Catalog) Get(id string) (ModelInfo, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, m := range c.models {
		if m.ID == id {
			return m, true
		}
	}
	return ModelInfo{}, false
}

// LaneLock 描述"整条车道被锁"的状态。
//
// ⚠️ **这是车道级的事实，不是模型级的判决。**
//
// 免密车道的可用性是**网络路径的属性**：
// 上游的原话是"detected abusive traffic from **this IP address**"，
// 而且地区封锁也是"not available in **your country**"。
//
// 所以当**所有**模型都被同一种理由拒绝时，
// 正确的结论是"**这个出口现在用不了这条车道**"，
// 而不是"这 10 个模型分别坏了"。
//
// ⚠️ 逐个模型标"出口被风控"是**误导** ——
//
//	用户会去逐个换模型，而真正要换的是出口。
type LaneLock struct {
	// Locked 报告整条车道是否被锁。
	Locked bool `json:"locked"`
	// Reason 是被锁的理由（"出口被风控" / "地区受限"）。
	Reason string `json:"reason,omitempty"`
	// State 是对应的状态（StateAbuseBlocked / StateRegionBlock）。
	State ProbeState `json:"state,omitempty"`
	// Count 是被同一理由拒绝的模型数。
	Count int `json:"count"`
	// Total 是这一轮探测的模型数。
	Total int `json:"total"`
}

// LaneLockOf 判断某条车道是不是**整条**被锁了。
//
// ⚠️ 判据（三个都要满足）：
//  1. 这一轮**探过足够多**的模型（至少 3 个）——
//     只探了 1 个就被拒，不能下"整条被锁"的结论。
//  2. **全部**被拒（没有一个是"可用"）。
//  3. 理由**一致** —— 全都是"出口被风控"，或者全都是"地区受限"。
//     理由不一致说明是各模型各自的问题，不是车道的问题。
//
// ⚠️ **限流不算车道被锁** —— 限流是**暂时**的，
//
//	而且是全局配额，不是"这个出口不行"。
func (c *Catalog) LaneLockOf(laneID string) LaneLock {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ids := c.byLane[laneID]
	total := 0
	abuse := 0
	region := 0
	available := 0
	for _, m := range ids {
		p, ok := c.probes[m.ID]
		if !ok || !p.Fresh(ProbeTTL) {
			continue
		}
		total++
		switch p.State {
		case StateAvailable:
			available++
		case StateAbuseBlocked:
			abuse++
		case StateRegionBlock:
			region++
		}
	}

	out := LaneLock{Total: total}
	if total < 3 || available > 0 {
		return out
	}
	// ⚠️ 理由一致才算车道被锁。
	switch {
	case abuse == total:
		out.Locked = true
		out.State = StateAbuseBlocked
		out.Reason = "出口被风控 —— 整条免密车道在这个出口上都不能用"
	case region == total:
		out.Locked = true
		out.State = StateRegionBlock
		out.Reason = "地区受限 —— 整条免密车道在这个出口所在的地区都不能用"
	}
	out.Count = abuse + region
	return out
}

// SetProbe 记录一个探测结论。
//
// ⚠️ **键就是模型 id 本身** —— 它已经带车道前缀了
// （`zen/mimo-v2.6-flash-free`），所以同名模型不会撞。
func (c *Catalog) SetProbe(result ProbeResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probes[result.Model] = result
}

// Probe 返回一个模型的探测结论。
//
// ⚠️ **键是带车道前缀的模型 id**（`zen/mimo-v2.6-flash-free`）。
func (c *Catalog) Probe(model string) ProbeResult {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.probes[model]
}

// ProbeIn 返回某条车道上某个模型的探测结论。
//
// ⚠️ **给"还没加前缀"的旧 id 用的** ——
//
//	它会拼上车道前缀再查。
//	新代码直接用 `Probe(FormatModelID(lane, id))`。
func (c *Catalog) ProbeIn(laneID, model string) ProbeResult {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if p, ok := c.probes[FormatModelID(laneID, model)]; ok {
		return p
	}
	return ProbeResult{}
}

// Probes 返回全部探测结论。
func (c *Catalog) Probes() map[string]ProbeResult {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]ProbeResult, len(c.probes))
	for k, v := range c.probes {
		out[k] = v
	}
	return out
}

// ApplyProbe 把探测结论写回模型行（能力 + 状态 + 延迟）。
//
// ⚠️ **负面结论会先跟"真实流量成功"对质** ——
//
//	如果有**更新的**成功证据，这个负面结论就不作数，
//	结论降级成"未探测"（unknown）。
//	见 EvidenceTTL 的说明。
func (c *Catalog) ApplyProbe(result ProbeResult) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// ⚠️ 负面结论 + 有更新的成功证据 → 降级成"未探测"。
	if result.State.Negative() && c.vouchedForLocked(result.Model, result.At) {
		result = ProbeResult{
			Model:  result.Model,
			State:  StateUnknown,
			Detail: "探测结论与真实流量不符（刚刚这个模型还成功过）—— 按未探测处理",
			At:     result.At,
		}
	}

	c.probes[result.Model] = result
	for i := range c.models {
		if c.models[i].ID != result.Model {
			continue
		}
		c.models[i].State = string(result.State)
		c.models[i].TTFTMs = result.TTFTMs
		// ⚠️ **记下探测时间** —— 手动和自动都记。
		c.models[i].ProbedAt = result.At
		// ⚠️ 探测是**最高信任**的来源 —— 它的模态结论可以写进模型。
		if result.State == StateAvailable {
			c.models[i].Capability = CapabilitySourceProbe
		}
		return
	}
}

// NoteEvidence 记一次**真实流量成功**（作为证据）。
//
// ⚠️ 它**不直接改状态** —— 状态是探测的结论，真实流量没资格写。
//
//	它只留下一份**带时间戳的证据**，让下一次探测写结论时对质。
//
//	这样分开的好处：
//	- 面板上的"可用"仍然意味着"探测验证过可用"，语义干净；
//	- 但一个**更新的成功**能让过期的负面结论失效；
//	- 证据会过期，所以模型真坏掉时面板最终还是会更新。
//
// ⚠️ 与 `Book.LastOK` 是同一件事的两个视角：这里是内存副本，
//
//	持久化那份在限流账本里。
func (c *Catalog) NoteEvidence(model string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evidence[model] = time.Now().UnixMilli()
}

// vouchedForLocked 报告"有没有**比结论更新**的成功证据"。
//
// ⚠️ 比结论**更新**才作数 —— 探测之后再成功的才算。
//
//	探测之前就有的成功，那是**过去**的事，
//	证明不了探测那一刻的情况。
//
// ⚠️ 调用方必须**持有锁**。
func (c *Catalog) vouchedForLocked(model string, verdictAt int64) bool {
	at, ok := c.evidence[model]
	if !ok {
		return false
	}
	// 证据比结论新 → 才作数。
	if at <= verdictAt {
		return false
	}
	// 证据过期了 → 不作数。
	return time.Since(time.UnixMilli(at)) < EvidenceTTL
}

// SortModels 按展示顺序排（车道 → 可用性 → 延迟 → 名字）。
func SortModels(models []ModelInfo) {
	sort.SliceStable(models, func(i, j int) bool {
		a, b := models[i], models[j]
		if a.Lane != b.Lane {
			return a.Lane < b.Lane
		}
		as, bs := availabilityRank(a.State), availabilityRank(b.State)
		if as != bs {
			return as < bs
		}
		if a.TTFTMs != b.TTFTMs {
			return a.TTFTMs < b.TTFTMs
		}
		return a.ID < b.ID
	})
}

// availabilityRank 把可用性状态排成序数。
// ⚠️ **可用性永远是最主要的项** —— 延迟只在同一可用性档位内排序。
func availabilityRank(state string) int {
	switch ProbeState(state) {
	case StateAvailable:
		return 0
	case StateUnknown:
		return 1
	case StateThrottled:
		return 2
	case StateRegionBlock:
		return 3
	case StateAbuseBlocked:
		return 4
	case StateUnavailable:
		return 5
	default:
		return 1
	}
}
