// Package creditlane 把 Credit 页登录的账号模型接进网关模型池。
//
// **为什么需要这一层** ——
//
// internal/credit 管的是**账号**：登录、额度、验证码、轮换。
// internal/lane  管的是**模型**：清单、探测、故障转移、用量账本。
// 两边互不知情，结果就是 Credit 页加进来的模型**只在界面上看得见** ——
// `/v1/models` 里没有它，局域网那把 `fmh-` key 也调不动它。
//
// 这个包是它们之间唯一的接缝，而且**只做翻译**：
// 凭据、轮换、冷却、封禁的判断仍然全部留在 credit.Manager 里。
// 车道这边一旦自己记住"哪个账号能用"，就会出现两份事实 ——
// 而两份事实的分歧只在半夜的上游抖动里暴露。
package creditlane

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/aBon579/free-model-house/internal/credit"
	"github.com/aBon579/free-model-house/internal/lane"
)

// LaneID 是这条车道的标识。
//
// ⚠️ 真身是 `lane.CreditLaneID` —— 路由器要按它裁剪回落链，
//
//	所以两处必须同一个常量，不能各写一遍 "credit"。
const LaneID = lane.CreditLaneID

// Backend 是这条车道真正用到的那部分 credit.Manager。
//
// ⚠️ **为什么是个接口而不是直接收 *credit.Manager** ——
//
//	推理链路要能测。真 Manager 要 vault、要 sqlite、要真登录，
//	而帧形翻译与账号轮换这两件事**跟凭据无关**。
//	把依赖收到六个方法上，测试就能拿假实现把轮换跑完。
type Backend interface {
	Snapshot() credit.Snapshot
	Candidates(provider, model, accountID string) []credit.Account
	Prepare(ctx context.Context, id string, in credit.Inference) (credit.Prepared, error)
	RecordSuccess(id string)
	RecordFailure(id string, err error, delay time.Duration)
	HTTPClient() *http.Client
}

// Lane 是 credit 账号池对外呈现的那条车道。
type Lane struct {
	backend Backend
}

// New 建一条 credit 车道。
func New(backend Backend) *Lane {
	return &Lane{backend: backend}
}

// ID 是车道标识。
func (l *Lane) ID() string { return LaneID }

// NeedsCredential 报告这些模型花的是**用户自己的**额度。
//
// ⚠️ 它决定了两件事：
//
//	① 「自动跨模型调用」默认不许切进来 —— 得用户显式开；
//	② 界面上"未探测"不代表坏了，只代表没花用户的钱去探。
func (l *Lane) NeedsCredential() bool { return true }

// AutoProbeable 报告这条车道**不参与**自动批量探测。
//
// ⚠️ 一轮探测 = 每个模型一次真实推理，而 credit 池里躺着几十上百个模型 ——
//
//	拿它们做周期探测等于**每 10 分钟烧一次用户的额度**。
//	可用性由真实流量验证，界面上只允许手动探测。
func (l *Lane) AutoProbeable() bool { return false }

// Ready 报告有没有**至少一个**启用的账号带着可用模型。
//
// ⚠️ 不缓存清单 —— credit 的账号状态是**用户随时能改**的（停用、换模型、
//
//	登录过期），缓存一份"就绪"标记就会在删账号之后还往外路由请求。
//	Snapshot 是纯内存拷贝，代价抵不上这个风险。
func (l *Lane) Ready() bool {
	for _, a := range l.backend.Snapshot().Accounts {
		if len(visibleModels(a)) > 0 {
			return true
		}
	}
	return false
}

// Status 是给看板看的车道状态。
func (l *Lane) Status() lane.LaneStatus {
	snap := l.backend.Snapshot()
	accounts, usable := 0, 0
	for _, a := range snap.Accounts {
		if !a.Enabled {
			continue
		}
		accounts++
		if len(visibleModels(a)) > 0 {
			usable++
		}
	}
	reason := ""
	switch {
	case accounts == 0:
		reason = "还没有添加 Credit 账号"
	case usable == 0:
		reason = "账号里的模型都被关掉了"
	}
	return lane.LaneStatus{
		ID:              LaneID,
		Name:            "Credit 账号池",
		Ready:           accounts > 0 && usable > 0,
		Reason:          reason,
		ModelCount:      len(l.ListModels0()),
		NeedsCredential: true,
		AutoProbe:       false,
		AutoProbeReason: "Credit 账号用的是你自己的额度，不参与自动批量探测；可在模型清单里手动探测",
	}
}

// ListModels 把**所有启用账号**里的模型并成一份清单。
//
// ⚠️ **同 id 去重，不留第二份** ——
//
//	两台同提供方的账号往往带着同一批模型。清单里出现两行同名，
//	用户会以为是两个模型；探测结论也会互相覆盖。
//	账号级的轮换**不在这里做**，在 Do 里按 Candidates 排队。
func (l *Lane) ListModels(_ context.Context) ([]lane.ModelInfo, *lane.Error) {
	return l.ListModels0(), nil
}

// ListModels0 是不带错误返回的清单构造（车道接口要那个 *lane.Error，
// 而这份清单只来自本地状态，没有"拉取失败"这回事）。
func (l *Lane) ListModels0() []lane.ModelInfo {
	snap := l.backend.Snapshot()
	names := map[string]string{}
	for _, p := range snap.Providers {
		names[p.ID] = p.Name
	}
	out := []lane.ModelInfo{}
	seen := map[string]bool{}
	for _, a := range snap.Accounts {
		if !a.Enabled {
			continue
		}
		for _, m := range visibleModels(a) {
			id := FormatModelID(a.Provider, m.ID)
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, modelInfo(names[a.Provider], a.Provider, m))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// modelInfo 把 credit 的模型声明翻成清单里的一行。
func modelInfo(providerName, provider string, m credit.Model) lane.ModelInfo {
	name := strings.TrimSpace(m.Name)
	if name == "" {
		name = m.ID
	}
	if providerName == "" {
		providerName = provider
	}
	return lane.ModelInfo{
		ID:   FormatModelID(provider, m.ID),
		Lane: LaneID,
		Wire: "chat",
		Name: providerName + " · " + name,
		// ⚠️ `Free` 回到它的字面意思：**这一次推理不扣额度**。
		//   整池一律给 true 会让 0× 的 Qwen3.8-Flash 和 1.4× 的 Kimi-K3
		//   在模型池里长得一模一样 —— 而挑模型正是按这个挑的，
		//   标错等于让人把付费模型当免费的烧。
		//   上游没给倍率（nil）时**不猜**：宁可什么标记都不打。
		Free:       m.Multiplier != nil && *m.Multiplier == 0,
		Multiplier: m.Multiplier,
		Enabled:    true,
		Vision:     m.Vision,
		Reasoning:  m.Reasoning,
		// ⚠️ 有档位就说明思考关得掉；没有档位又标了 reasoning，
		//   那是**强制思考**的模型 —— 路由要按这个加倍输出预算。
		CanDisableThinking: !m.Reasoning || len(m.Efforts) > 0,
		ContextWindow:      m.ContextWindow,
		MaxOutput:          m.MaxOutput,
		Efforts:            append([]string(nil), m.Efforts...),
		// 能力结论来自上游自己的声明，比名称启发式硬。
		Capability: lane.CapabilitySourceListing,
		State:      string(lane.StateUnknown),
	}
}

// visibleModels 返回一个账号**实际可用**的那几个模型。
//
// ⚠️ `DisabledModels` 是用户手动关掉的那批 ——
//
//	它们必须从清单里消失，而不只是"路由时跳过"：
//	留在 /v1/models 里，客户端就还能点名为它的请求，然后收到一个看不懂的失败。
func visibleModels(a credit.Account) []credit.Model {
	if !a.Enabled {
		return nil
	}
	off := map[string]bool{}
	for _, id := range a.DisabledModels {
		off[id] = true
	}
	out := make([]credit.Model, 0, len(a.Models))
	for _, m := range a.Models {
		if strings.TrimSpace(m.ID) == "" || off[m.ID] {
			continue
		}
		out = append(out, m)
	}
	return out
}

// FormatModelID 拼对外的模型 id：`credit/<提供方>/<上游模型>`。
//
// ⚠️ 第一个斜杠是**车道**分隔（lane.ParseModelID 按它切），
//
//	所以车道内再套一层提供方前缀 —— 这是自定义 Key 车道立的规矩，
//	照它做，网关里所有"剥前缀/加前缀"的地方才不用改。
func FormatModelID(provider, model string) string {
	return lane.FormatModelID(LaneID, provider+"/"+model)
}

// splitModel 从**已剥掉车道前缀**的模型 id 里切出提供方与上游模型名。
func splitModel(id string) (string, string, bool) {
	i := strings.IndexByte(id, '/')
	if i <= 0 || i == len(id)-1 {
		return "", "", false
	}
	return id[:i], id[i+1:], true
}

// Do 发一次推理，必要时在**同一个提供方**的账号之间轮换。
//
// ⚠️ 轮换只许发生在**还没吐任何内容**之前 ——
//
//	换账号重放会从头生成一遍，客户端已经收到的那些字要变成两遍。
//	这与路由器"committed 之后不再换候选"是同一条规矩，只是粒度更细：
//	它在车道之间换，这里在**账号**之间换。
func (l *Lane) Do(ctx context.Context, turn *lane.Turn, emit func(lane.Chunk)) (*lane.Usage, lane.FinishReason, *lane.Error) {
	provider, model, ok := splitModel(turn.Model)
	if !ok {
		return nil, lane.FinishError, &lane.Error{Code: lane.CodeClient, Message: "credit 模型 id 要写成 credit/<提供方>/<上游模型>"}
	}

	cands := l.backend.Candidates(provider, model, "")
	if len(cands) == 0 {
		return nil, lane.FinishError, l.noCandidate(provider, model)
	}

	// ⚠️ 两家上游的请求体**都吃 OpenAI chat 形状**。所以这里统一用 ChatWireTurn，
	//   不能按 wire 名去拼两套消息 —— 那样只会多一套会写错的地方。
	messages, tools := lane.ChatWireTurn(turn)
	in := credit.Inference{
		Model:     credit.Model{ID: model},
		Messages:  messages,
		Tools:     tools,
		MaxTokens: maxTokensOf(turn),
		Effort:    turn.Effort,
		SessionID: sessionID(turn.Seed),
	}

	sink, emitted := guard(emit)
	var lastErr *lane.Error
	for _, a := range cands {
		if ctx.Err() != nil {
			return nil, lane.FinishError, &lane.Error{Code: lane.CodeAborted, Message: "cancelled"}
		}
		// Prepare 只做本地签名/加密（Qoder 那边要跑 wasm），不碰网络。
		prepared, err := l.backend.Prepare(ctx, a.ID, in)
		if err != nil {
			lastErr = localError(err)
			l.backend.RecordFailure(a.ID, err, coolLocal)
			continue
		}
		usage, finish, uerr := stream(ctx, prepared, l.backend.HTTPClient(), sink)
		if uerr == nil {
			l.backend.RecordSuccess(a.ID)
			return usage, finish, nil
		}
		lastErr = uerr
		if uerr.Code == lane.CodeAborted {
			return usage, lane.FinishError, uerr
		}
		if emitted() {
			// 已经发过内容 → 只能带着这个结论收尾，不许重放。
			return usage, lane.FinishError, uerr
		}
		l.backend.RecordFailure(a.ID, uerr, cooldownOf(uerr))
		if !rotatable(uerr) {
			// 换账号还会撞同一个拒绝 —— 别把整池账号挨个试一遍。
			break
		}
	}
	return nil, lane.FinishError, lastErr
}

// guard 把 emit 包一层，记录"是否已经吐过真正的内容"。
//
// ⚠️ 只数**内容类**事件：块起始与用量不算数 ——
//
//	它们在内容之前到，但对客户端而言还是"什么都没收到"。
func guard(emit func(lane.Chunk)) (func(lane.Chunk), func() bool) {
	sent := false
	wrapped := func(ch lane.Chunk) {
		switch ch.Kind {
		case lane.ChunkTextDelta, lane.ChunkReasonDelta, lane.ChunkToolCallDelta:
			sent = true
		}
		if emit != nil {
			emit(ch)
		}
	}
	return wrapped, func() bool { return sent }
}

// noCandidate 说清"这个模型现在为什么没有可用账号"。
//
// ⚠️ **三种原因的错码不能混**，因为错码决定路由器还试不试：
//
//	账号全在冷却 → CodeQuota（等一等有意义，也可以换别的车道）
//	模型被禁用   → CodeClient（重放还是一样，别再试）
//	没有账号带它 → CodeCredential（是这条路没了，不是模型坏了）
func (l *Lane) noCandidate(provider, model string) *lane.Error {
	var (
		known    bool
		cooling  bool
		disabled bool
		offModel bool
	)
	for _, a := range l.backend.Snapshot().Accounts {
		if a.Provider != provider {
			continue
		}
		has := false
		for _, m := range a.Models {
			if m.ID == model {
				has = true
			}
		}
		if !has {
			continue
		}
		known = true
		if !a.Enabled {
			disabled = true
			continue
		}
		for _, id := range a.DisabledModels {
			if id == model {
				offModel = true
			}
		}
		if a.CooldownUntil > time.Now().UnixMilli() {
			cooling = true
		}
	}
	switch {
	case !known:
		return &lane.Error{Code: lane.CodeCredential, Status: 0,
			Message: "没有 credit 账号带着这个模型：账号可能已删除，或模型清单还没刷新"}
	case offModel && !cooling && !disabled:
		return &lane.Error{Code: lane.CodeClient, Message: "这个模型在 credit 账号里被关掉了"}
	case cooling:
		return &lane.Error{Code: lane.CodeQuota, Message: "credit 账号正在冷却（上一个请求被拒），稍后再试"}
	default:
		return &lane.Error{Code: lane.CodeCredential, Message: "credit 账号已停用"}
	}
}

// Probe 手动探一个 credit 模型。
//
// ⚠️ 它会**真的烧一次用户的额度** —— 所以只有界面上那个手动按钮会走到这里，
//
//	周期探测通过 AutoProbeable()=false 被挡在外面。
func (l *Lane) Probe(ctx context.Context, model string) lane.ProbeResult {
	result := lane.ProbeResult{Model: model, State: lane.StateUnknown, At: time.Now().UnixMilli()}
	start := time.Now()
	provider, upstream, ok := splitModel(strings.TrimPrefix(strings.TrimSpace(model), LaneID+"/"))
	if !ok {
		result.State, result.Detail = lane.StateUnavailable, "credit 模型 id 要写成 credit/<提供方>/<上游模型>"
		return result
	}
	turn := &lane.Turn{
		Model:     provider + "/" + upstream,
		Messages:  []lane.Message{{Role: lane.RoleUser, Parts: []lane.Part{lane.TextPart("ping")}}},
		MaxTokens: probeMaxTokens,
		Seed:      "probe:" + model,
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var first time.Time
	_, _, uerr := l.Do(ctx, turn, func(ch lane.Chunk) {
		if first.IsZero() && (ch.Kind == lane.ChunkTextDelta || ch.Kind == lane.ChunkReasonDelta) {
			first = time.Now()
		}
	})
	result.LatencyMs = time.Since(start).Milliseconds()
	if !first.IsZero() {
		result.TTFTMs = first.Sub(start).Milliseconds()
	} else {
		result.TTFTMs = result.LatencyMs
	}
	if uerr == nil {
		result.State = lane.StateAvailable
		return result
	}
	// ⚠️ 状态用共享判断，**措辞留自己的** —— "换出口"那种话对 credit 没意义。
	result.State = lane.ProbeStateForError(uerr)
	result.Detail = uerr.Message
	return result
}

const probeMaxTokens = 256

// maxTokensOf 给一个输出预算兜底值。
//
// ⚠️ 不能留 0 —— credit 的三家都把 max_tokens 原样塞进请求体，
//
//	0 在 Qoder 那边是"一个 token 都不给"，会被当成非法请求。
func maxTokensOf(turn *lane.Turn) int {
	if turn.MaxTokens > 0 {
		return turn.MaxTokens
	}
	return 8192
}

// sessionID 把路由器的 seed 收成可上送的 id。
//
// ⚠️ 路由器会在 seed 后面拼 `\x00<车道>` —— 那是**本地**的区分符，
//
//	原样送进 JSON 会让上游收到带控制字符的会话 id。
func sessionID(seed string) string {
	if i := strings.IndexByte(seed, 0); i >= 0 {
		seed = seed[:i]
	}
	return strings.TrimSpace(seed)
}

// 冷却时长：被拒的账号要凉多久。
//
// ⚠️ 这些数字是**取舍**不是测量：太短会把同一个拒绝再撞一遍，
//
//	太长会让用户明明有第二个账号却等不到。
const (
	// coolQuota 是额度/限流类：这类冷却按小时记，几分钟太短。
	coolQuota = 10 * time.Minute
	// coolLocal 是 Prepare 阶段的本地失败（签名/清单/参数）：与账号状态无关，
	//   惩罚账号是错的，所以只给一个很短的抖动窗口。
	coolLocal = 15 * time.Second
	// coolAuth 是凭据失效：只有重新登录才能救，凉多久都不影响结果，
	//   给一个足够长但不至于永久的值，别在每次请求里重复审。
	coolAuth = 30 * time.Minute
	// coolTransient 是 5xx/超时/断流。
	coolTransient = 60 * time.Second
)

// cooldownOf 决定一个失败要把这个账号关多久。
//
// ⚠️ **只有账号级的问题才该罚账号** ——
//
//	请求本身不合法（CodeClient）换哪个账号都一样，罚它只会让整池空掉。
func cooldownOf(uerr *lane.Error) time.Duration {
	switch uerr.Code {
	case lane.CodeQuota:
		return coolQuota
	case lane.CodeCredential, lane.CodeAuthRequired, lane.CodePermission:
		return coolAuth
	case lane.CodeClient, lane.CodeUnsupportedInput:
		// 请求本身不合法 —— 换哪个账号都一样，罚账号只会让整池空掉。
		return 0
	case lane.CodeAborted:
		return 0
	default:
		return coolTransient
	}
}

// rotatable 报告**换一个账号**是否可能改变结果。
//
// ⚠️ 这与车道那边"要不要换模型"（`Error.Switchable`）是两个不同的问题：
//
//	请求本身不合法、或这个模型就是不吃这种输入 ——
//	同一份请求在**每个账号**上拿到的判决都一样。
//	这时候轮换只是把一次失败放大成"账号数 × 一次完整上游请求"，
//	既慢又白烧额度。
func rotatable(uerr *lane.Error) bool {
	switch uerr.Code {
	case lane.CodeClient, lane.CodeUnsupportedInput:
		return false
	default:
		return true
	}
}

// localError 把 Prepare 阶段的失败包成车道错误。
//
// ⚠️ Prepare 不碰网络，但它会**带着上游的判决**回来：
//
//	上游的判决本来就包在 `*credit.APIError{Status, Code, Message}` 里回来，
//	那是上游给过的分类信息，丢掉它、一律说成"请求有问题"，
//	路由器就会放弃本该换账号/换车道的故障转移。
//	所以能按状态码分类的**交给 ClassifyFailure**，判不出来的算**我们这侧**的故障。
func localError(err error) *lane.Error {
	if err == nil {
		return nil
	}
	var api *credit.APIError
	if errors.As(err, &api) && api.Status > 0 {
		return lane.ClassifyFailure(api.Status, api.Message, 0)
	}
	msg := strings.TrimSpace(err.Error())
	if msg == "" {
		msg = "credit 请求构建失败"
	}
	return &lane.Error{Code: lane.CodeServer, Message: msg}
}
