package lane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// **OpenCode Zen 免密车道**。
// 这是整个项目最关键的一环。免密车道的上游会逐项校验
// 端点路径、User-Agent、会话头、会话 ID 编码、工具定义与 tool_choice，
// 任何一条不满足都返回 403 FreeTierError。
// 闸门相关的字面量全部在 zen_fingerprint.go 里；本文件只负责**编排**。
// 详见 docs/design/01-zen-fingerprint.md。

// ZenLane 是免密车道。
type ZenLane struct {
	client *http.Client
	// sessions 是 seed → 上游 session id 的稳定映射。
	// ⚠️ 配额按 session 记账。每请求换一个 session 会立刻耗尽配额。
	// 而且它还决定 prompt cache 能不能命中。
	sessions *SessionMap
	// base 是上游根地址（测试时可覆盖）。
	base string

	// mu 保护下面的状态字段。
	mu sync.Mutex
	// modelCount 是上次清单里的模型数。
	modelCount int
	// lastRefresh 是上次刷新清单的时间。
	lastRefresh int64
	// lastError 是上次刷新的错误。
	lastError string
}

// ZenLaneID 是这条车道的标识。
const ZenLaneID = "zen"

func NewZenLane(sessions *SessionMap) *ZenLane {
	return &ZenLane{
		client:   Transport(),
		sessions: sessions,
		base:     ZenBaseURL,
	}
}

// ID 实现 Lane。
func (z *ZenLane) ID() string { return ZenLaneID }

// SetBaseURL 覆盖上游根地址。
// ⚠️ **只给测试用** ——
// 断流续写需要一个"会截断的上游"来验证，
// 而真实的免密上游不会稳定地截断。
func (z *ZenLane) SetBaseURL(base string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.base = base
}

// Ready 实现 Lane。
// ⚠️ 免密车道**永远 ready** —— 它不需要任何凭据。
func (z *ZenLane) Ready() bool { return true }

// NeedsCredential 报告这条车道**不需要**用户自己的凭据。
//
// ⚠️ 显式声明 `false` —— zen 是免密共享池。
func (z *ZenLane) NeedsCredential() bool { return false }

// Status 实现 Lane。
func (z *ZenLane) Status() LaneStatus {
	z.mu.Lock()
	defer z.mu.Unlock()
	return LaneStatus{
		ID:              ZenLaneID,
		Name:            "OpenCode Zen 免密车道",
		Ready:           true,
		ModelCount:      z.modelCount,
		LastRefresh:     z.lastRefresh,
		LastError:       z.lastError,
		NeedsCredential: false,
		// ⚠️ **参与自动探测** —— 它是**匿名共享池**，
		//   探它不花任何人的钱。
		AutoProbe: true,
	}
}

// Probe 实现 Lane。
// ⚠️ 判定依据是**上游的反应**，不是名字 —— 见 freeverify.go。
func (z *ZenLane) Probe(ctx context.Context, model string) ProbeResult {
	result := ProbeResult{Model: model, State: StateUnknown, At: time.Now().UnixMilli()}
	start := time.Now()

	// ⚠️ 探测**消耗上游额度**，所以发一个最小的请求。
	turn := lane_probe_turn(model)

	// 探针要尽快拿到结论，所以给一个短超时。
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var firstDelta time.Time
	_, _, uerr := z.Do(ctx, turn, func(c Chunk) {
		if firstDelta.IsZero() && (c.Kind == ChunkTextDelta || c.Kind == ChunkReasonDelta) {
			firstDelta = time.Now()
		}
	})
	result.LatencyMs = time.Since(start).Milliseconds()
	if !firstDelta.IsZero() {
		result.TTFTMs = firstDelta.Sub(start).Milliseconds()
	} else {
		result.TTFTMs = result.LatencyMs
	}

	if uerr == nil {
		result.State = StateAvailable
		return result
	}
	result.State, result.Detail = probeStateFromError(uerr)
	return result
}

// lane_probe_turn 构造探测用的请求。
//
// ⚠️ **`max_tokens` 不能太小** ——
//
// 这是实测踩过的坑：原来用 16，某个模型把 16 个 token
// **全烧在 reasoning 上**，一个可见 token 都没吐，
// 于是撞上 `max_tokens` 截断（finish = max-tokens）。
//
// 后果很严重：
//   - 探测**测不到"能不能出答案"** —— 它只测到"能不能开始思考"；
//   - `finish=max-tokens` 与"上游拒绝"在结果上混在一起；
//   - 首字延迟被算成了整个请求的耗时（14~40 秒），
//     把这个模型在排序里压到最后。
//
// 256 是实测下来的甜点：
//   - 足够让模型先思考再吐出答案（实测 reasoning 11~17 + 输出 14~26）；
//   - 仍然很小，不会像真实请求那样烧配额。
const probeMaxTokens = 256

// lane_probe_turn 构造探测请求。
func lane_probe_turn(model string) *Turn {
	return &Turn{
		Model: model,
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{TextPart("ping")}},
		},
		MaxTokens: probeMaxTokens,
		Seed:      "probe:" + model,
	}
}

// probeStateFromError 把一个分类过的错误映射成探测结论。
// ⚠️ **关键的不对称**：5xx / 传输故障**不**算在模型头上。
// `Service Unavailable` 是**反向代理的理由短语** ——
// 按它匹配会把一个**正在工作**的模型从选择器里摘掉。
// 配额与传输故障同理：那不是"这个模型的判决"。
// 一次抖动不该让模型消失一整轮。
func probeStateFromError(uerr *Error) (ProbeState, string) {
	if uerr == nil {
		return StateAvailable, ""
	}
	switch uerr.Code {
	case CodeQuota:
		return StateThrottled, uerr.Message
	case CodeRegion:
		return StateRegionBlock, uerr.Message
	case CodeAbuseBlocked:
		// ⚠️ 出口被风控 —— 这不是模型的判决，是**车道级**的。
		// 但从单个模型的角度看，它此刻确实不可用。
		return StateAbuseBlocked, uerr.Message
	case CodeFreeTier:
		return StateUnavailable, "上游闸门拒绝：" + uerr.Message
	case CodeCredential:
		// ⚠️ "Missing API key" 是决定性的 —— 这个模型**不是**免费的。
		return StateUnavailable, "免密凭据不适用于这个模型"
	case CodeClient:
		// 路由拒绝（模型不存在/上下文超限）—— 指名了这个模型。
		return StateUnavailable, uerr.Message
	case CodeUnsupportedInput:
		// ⚠️ **模型不支持纯文本**（vision-only 那种）——
		//   它不是"不可用"，只是纯文本用不了它。
		//   标成"未探测"而不是"不可用" ——
		//   这样它还留在清单里，用户看到图片请求仍然能用它。
		return StateUnknown, "这个模型不支持纯文本输入（可能只支持图片）"
	case CodeServer, CodeTransport, CodeReset, CodeTimeout, CodeEmpty, CodeUnknown:
		// ⚠️ 不算在模型头上 —— 保持 unknown。
		return StateUnknown, uerr.Message
	default:
		return StateUnknown, uerr.Message
	}
}

// zenListing 是上游的模型清单。
// ⚠️ **上游不声明任何容量**。实测（2026-10-19）：85 行的字段只有
// `id` / `object` / `created` / `owned_by` —— **一个容量字段都没有**。
// 所以容量必须来自**本地规则表**（见 catalog.go 的 capabilityRows）
// 与**公开目录**（见 openref.go）。清单只负责给出**权威的 id 集合**。
// ⚠️ 解析用 listing.go 的 ParseListing（兼容多种信封），
// 这个结构仅为说明字段含义。
type zenListing struct {
	Data []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
}

// FallbackCatalogIDs 覆盖"冷启动时还没有网络"的情况。
// NAS 开机时网络可能还没通 —— 如果清单拉不到就什么都用不了。
// 所以这里存一份**实测过的**免费 id 快照，冷启动时先用它兜底，
// 联网后立刻被真实清单替换。
// ⚠️ 快照会过期（上游会下线模型）。但"过期的模型 id"只会在
// 路由时拿到一个"模型不存在"的错误 —— 而限流账本与探测会把它摘掉。
// **有总比没有好**：模型清单空着 = 整个网关不可用。
// 快照时间：2026-10-19 实测。
var FallbackCatalogIDs = []string{
	"minimax-m3.1-flash-free",
	"deepseek-v4.1-flash-free",
	"glm-5.3-flash-free",
	"mimo-v2.6-flash-free",
	"longcat-2.5-preview-free",
	"nemotron-3.5-super-vl-free",
	"nemotron-3.5-lightning-free",
	"beam-free",
	"jev-1.13-free",
	"big-pickle",
}

// fallbackCatalog 由快照构造一份清单。
//
// ⚠️ **必须同时设置 modelCount / lastRefresh** ——
//
//	「上游源」页面的"模型数"取自 `Status().ModelCount`，
//	而那个字段**只在成功拉取时**被设置。
//	兜底路径原来漏了设置 → 上游一挂，页面显示"模型数 0"，
//	但清单里明明有 10 个模型 —— 自相矛盾。
func fallbackCatalog() []ModelInfo {
	out := make([]ModelInfo, 0, len(FallbackCatalogIDs))
	for _, id := range FallbackCatalogIDs {
		info := ModelInfo{
			// ⚠️ **ID 带车道前缀** —— 不同车道可以有同名模型
			// （Kilo 和 OpenRouter 的免费 id 完全重叠）。
			ID:         FormatModelID(ZenLaneID, id),
			Lane:       ZenLaneID,
			Wire:       ZenWireFor(id),
			Free:       true,
			Enabled:    true,
			Capability: CapabilitySourceHeuristic,
			State:      "unknown",
		}
		applyHeuristicCapabilities(&info, id)
		out = append(out, info)
	}
	return out
}

// ProbeFree 探一个模型"是不是真的免费"。
// ⚠️ 这个探测**消耗上游额度**，而且只发一个**最小**请求。
// 判定依据是**上游的反应**，不是名字：
//
//	401 "Missing API key"  → 确定**不是**免费车道
//	200 / 429 / 403 闸门   → 在免费车道上
//
// 探"不是免费的"其实很便宜 —— 付费模型在凭据检查阶段就被拒了，
// 根本不会真的推理。真正消耗额度的是探"确实免费"的那些。
func (z *ZenLane) ProbeFree(ctx context.Context, model string) *Error {
	// ⚠️ 用同一个探测请求形状 —— 免得两处形状不一致，
	//   得出互相矛盾的结论。
	turn := lane_probe_turn(model)
	// 探针要尽快拿到结论，所以给一个短超时。
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	_, _, uerr := z.Do(ctx, turn, func(Chunk) {})
	return uerr
}

// ListModels 拉取免密车道的模型清单。
// ⚠️ 清单地址：`/inference/v1/models`。
// 旧的 `/zen/v1/models` 也仍然可用（实测两者都返回同样的 85 个模型），但统一用新的那个。
// ⚠️ 清单**混合了免费与付费**。只有 free 模式的 id 能在没有用户凭据
// 的情况下应答 —— 所以必须用 IsFreeLane 过滤。
// 实测 2026-10-19：85 个里有 10 个在免费车道上。
// ⚠️ **上游不声明任何容量**（实测：85 行的字段只有
// id/object/created/owned_by）。所以容量来自：
//
//	① 本地规则表（地板）
//	② 公开目录（models.dev / OpenRouter，无需 key）—— 见 openref.go
//
// ⚠️ 拉取失败时**回落到快照**（见 FallbackCatalogIDs）。
// 一次失败的拉取**绝不能让清单变小** —— 清单空着 = 整个网关不可用。
func (z *ZenLane) ListModels(ctx context.Context) ([]ModelInfo, *Error) {
	models, uerr := z.listModelsLive(ctx)
	if uerr != nil {
		// 拉不到就用快照兜底 —— 冷启动没有网络时仍要有清单可用。
		//
		// ⚠️ **必须设 modelCount / lastRefresh / lastError** ——
		//   「上游源」页面的"模型数"取自 `Status().ModelCount`，
		//   只在成功拉取时被设置。兜底路径漏了设置的话，
		//   页面会显示"模型数 0"而清单里有 10 个 —— 自相矛盾。
		fallback := fallbackCatalog()
		z.mu.Lock()
		z.modelCount = len(fallback)
		z.lastRefresh = time.Now().UnixMilli()
		z.lastError = uerr.Message
		z.mu.Unlock()
		return fallback, uerr
	}
	// ⚠️ 一次成功的拉取如果返回空，**也不能**让清单变空 ——
	// 那说明上游出问题了，不说明"没有免费模型"。
	if len(models) == 0 {
		fallback := fallbackCatalog()
		z.mu.Lock()
		z.modelCount = len(fallback)
		z.lastRefresh = time.Now().UnixMilli()
		z.mu.Unlock()
		return fallback, nil
	}
	return models, nil
}

// listModelsLive 拉一次真实清单（失败时返回错误）。
func (z *ZenLane) listModelsLive(ctx context.Context) ([]ModelInfo, *Error) {
	url := z.base + "/inference/v1/models"
	// ⚠️ 清单请求也带**完整的会话头** —— 与推理请求同一套。
	// 实测两个端点（`/zen/v1/models` 与 `/inference/v1/models`）
	// 在只带 UA+Bearer public 时也返回 200，所以清单本身**不**校验指纹。
	// 但带上全套头更接近真实客户端，也避免上游未来收紧清单端点时被打脸。
	session := z.sessions.For("catalog:zen")
	headers := ZenHeaders(session, false)
	headers["accept"] = "application/json"

	res, uerr := doUpstream(ctx, z.client, http.MethodGet, url, headers, nil)
	if uerr != nil {
		return nil, uerr
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, &Error{Code: CodeTransport, Message: "reading listing failed: " + err.Error()}
	}
	rows := ParseListing(raw)

	out := make([]ModelInfo, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		if row.ID == "" {
			continue
		}
		// 只保留免费车道的 id。
		if !IsFreeLane(row.ID) {
			continue
		}
		base := ZenBaseModelID(row.ID)
		if seen[base] {
			continue
		}
		seen[base] = true

		info := ModelInfo{
			// ⚠️ **ID 带车道前缀** —— 不同车道可以有同名模型
			// （Kilo 和 OpenRouter 的免费 id 完全重叠）。
			ID:         FormatModelID(ZenLaneID, base),
			Lane:       ZenLaneID,
			Wire:       ZenWireFor(base),
			Free:       true,
			Enabled:    true,
			Created:    row.Created,
			Capability: CapabilitySourceHeuristic,
		}
		// 能力表是地板：它给出基线，实测可以覆盖它。
		applyHeuristicCapabilities(&info, base)
		// 上游若真的声明了容量/模态（当前不声明，但将来可能），
		// 那比本地规则表更可信。
		if row.ContextWindow > 0 || row.MaxOutput > 0 {
			ApplyCapacity(&info, CapacityMeta{
				ContextWindow: row.ContextWindow,
				MaxOutput:     row.MaxOutput,
				Source:        CapabilitySourceListing,
			})
		}
		out = append(out, info)
	}

	// 记下状态（给看板用）。
	z.mu.Lock()
	z.modelCount = len(out)
	z.lastRefresh = time.Now().UnixMilli()
	z.lastError = ""
	z.mu.Unlock()

	return out, nil
}

// Do 发起一次推理。
// 它产出的统一事件通过 emit 回调流出。
func (z *ZenLane) Do(ctx context.Context, turn *Turn, emit func(Chunk)) (*Usage, FinishReason, *Error) {
	return z.DoWithWire(ctx, turn, "", emit)
}

// DoWithWire 发起一次推理，**可以指定用哪条线**。
//
// ⚠️ `wire` 为空时按模型名自动选（生产路径）。
//
//	指定它是给**诊断**用的 —— 用来找出某个模型到底该走哪条线。
func (z *ZenLane) DoWithWire(
	ctx context.Context,
	turn *Turn,
	wire string,
	emit func(Chunk),
) (*Usage, FinishReason, *Error) {
	// 1. 决定上游 wire 与端点。
	if wire == "" {
		wire = ZenWireFor(turn.Model)
	}
	endpoint := zenEndpointForWire(wire)
	url := z.base + endpoint

	// 2. 取一个稳定的上游 session。
	session := z.sessions.For(turn.Seed)

	// 3. 构造请求体。
	body, renameMap, uerr := z.buildBody(turn, wire)
	if uerr != nil {
		return nil, FinishError, uerr
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, FinishError, &Error{Code: CodeClient, Message: err.Error()}
	}

	// 4. 头发。
	headers := ZenHeaders(session, turn.Stream)
	headers["accept"] = "text/event-stream"

	res, uerr := doUpstream(ctx, z.client, http.MethodPost, url, headers, payload)
	if uerr != nil {
		return nil, FinishError, uerr
	}
	defer res.Body.Close()

	// 6. 解流。
	dec := newDecoder(wireKindOf(wire), renameMap, emit)
	usage, uerr := dec.decodeSSE(newIdleReader(res.Body, StreamIdleTimeout))
	if uerr != nil {
		return usage, FinishError, uerr
	}

	// 7. 收尾。
	finish := dec.finish
	if !dec.sawFinish {
		// 流在没有结束标记的情况下断了。
		// ⚠️ 这可能是一次可续写的切断 —— 交给上层判断，
		// 这里只如实报告"没有结束"。
		finish = FinishError
	}
	if usage == nil {
		usage = &Usage{}
	}
	return usage, finish, nil
}

// buildBody 构造上游请求体，并应用指纹。
// 返回：请求体、工具名重命名表（响应侧要还原）、错误。
func (z *ZenLane) buildBody(turn *Turn, wire string) (map[string]any, map[string]string, *Error) {
	style := ZenStyleChat
	switch wire {
	case "responses":
		style = ZenStyleFlat
	case "messages":
		style = ZenStyleClaude
	}

	body := map[string]any{
		"model":  turn.Model,
		"stream": true,
	}
	switch style {
	case ZenStyleFlat:
		body["input"] = toResponsesInput(turn.Messages)
		if sys := systemText(turn.Messages); sys != "" {
			body["instructions"] = sys
		}
		// Responses 线用 max_output_tokens，不是 max_tokens。
		body["max_output_tokens"] = maxTokensOf(turn)
		// ⚠️ store:false —— 免密车道不持久化对话。
		// 不显式关掉的话，上游可能尝试存储，这既不必要也可能被拒。
		body["store"] = false
	case ZenStyleClaude:
		msgs, sys := toClaudeMessages(turn.Messages)
		body["messages"] = msgs
		if sys != "" {
			body["system"] = sys
		}
		body["max_tokens"] = maxTokensOf(turn)
		body["anthropic_version"] = "2023-06-01"
	default:
		body["messages"] = toChatMessages(turn.Messages)
		body["max_tokens"] = maxTokensOf(turn)
		body["stream_options"] = map[string]any{"include_usage": true}
	}

	// ⚠️ 续写时**不声明调用方的工具** —— 我们只想让模型把话说完。
	tools := turn.Tools
	if turn.SuppressTools {
		tools = nil
	}
	if defs := toolDefsFor(style, tools); len(defs) > 0 {
		body["tools"] = defs
	}

	// 指纹。
	res := ZenApplyFingerprint(body, style)

	// 工具名还原表。
	rename := make(map[string]string, len(res.RenameMap))
	for alias, orig := range res.RenameMap {
		rename[alias] = orig
	}
	return body, rename, nil
}

// maxTokensOf 决定这一轮发给上游的输出预算。
func maxTokensOf(turn *Turn) int {
	if turn.MaxTokens > 0 {
		return turn.MaxTokens
	}
	return 8192
}

// wireKindOf 把 wire 名映射成解码器的 wire。
func wireKindOf(wire string) wireKind {
	switch wire {
	case "responses":
		return wireResponses
	case "messages":
		return wireMessages
	default:
		return wireChat
	}
}

// SessionMap 维护 seed → 上游 session id 的稳定映射。
// 它是**持久化**的：重启后复用同一个 session，prompt cache 才不会每次冷启动。
type SessionMap struct {
	mu       sync.Mutex
	sessions map[string]sessionEntry
	// mint 铸一个新 session id（可注入以便测试）。
	mint func() string
	// persist 持久化（可注入以便测试）。
	persist func(seed, id string)
	now     func() time.Time
}

// sessionEntry 是一个 seed 的映射记录。
type sessionEntry struct {
	id string
	// at 是最后一次确认使用的时间。
	at time.Time
}

// NewSessionMap 建一个会话映射。
func NewSessionMap() *SessionMap {
	return &SessionMap{
		sessions: map[string]sessionEntry{},
		mint:     func() string { return ZenMintSessionID(0) },
		now:      time.Now,
	}
}

// For 取一个 seed 对应的上游 session id。
// 规则：已经铸造过且未过期 → 复用；过期（超过 zenSessionRemintAfter）→ 重铸；全新 → 铸造。
// ⚠️ 下游自带的 id 只有在**已经是上游接受的编码**时才原样转发。
// 否则上游会 403（它会解码那 12 位 hex 验证时间戳）。
func (m *SessionMap) For(seed string) string {
	if seed == "" {
		seed = "global"
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	if entry, ok := m.sessions[seed]; ok {
		if m.now().Sub(entry.at) < zenSessionRemintAfter {
			entry.at = m.now()
			m.sessions[seed] = entry
			return entry.id
		}
	}
	id := m.mint()
	m.sessions[seed] = sessionEntry{id: id, at: m.now()}
	if m.persist != nil {
		m.persist(seed, id)
	}
	return id
}

// Set 预置一个 seed 的映射（从持久化恢复时用）。
func (m *SessionMap) Set(seed, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[seed] = sessionEntry{id: id, at: m.now()}
}

// SetPersist 设置持久化回调。
func (m *SessionMap) SetPersist(fn func(seed, id string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.persist = fn
}

// Seed 用一批映射初始化（从数据库恢复时用）。
func (m *SessionMap) Seed(mapping map[string]string) {
	if len(mapping) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for seed, id := range mapping {
		if seed == "" || id == "" {
			continue
		}
		// ⚠️ 恢复时**不覆盖**已有的（内存里的更新）。
		if _, exists := m.sessions[seed]; !exists {
			m.sessions[seed] = sessionEntry{id: id, at: m.now()}
		}
	}
}

// Snapshot 导出全部映射。
func (m *SessionMap) Snapshot() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]string, len(m.sessions))
	for k, v := range m.sessions {
		out[k] = v.id
	}
	return out
}

// Len 返回映射条目数。
func (m *SessionMap) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// systemText 抽出系统提示。
func systemText(msgs []Message) string {
	var parts []string
	for _, m := range msgs {
		if m.Role != RoleSystem {
			continue
		}
		if t := m.TextOf(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// toChatMessages 把统一消息投影成 Chat 线。
func toChatMessages(msgs []Message) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			out = append(out, map[string]any{"role": "system", "content": m.TextOf()})
		case RoleTool:
			for _, p := range m.Parts {
				if p.Type != PartToolResult {
					continue
				}
				out = append(out, map[string]any{
					"role":         "tool",
					"tool_call_id": p.ToolCallID,
					"content":      toolResultText(p),
				})
			}
		case RoleAssistant:
			msg := map[string]any{"role": "assistant"}
			var text strings.Builder
			var calls []any
			for _, p := range m.Parts {
				switch p.Type {
				case PartText:
					text.WriteString(p.Text)
				case PartToolCall:
					calls = append(calls, map[string]any{
						"id":   p.ToolCallID,
						"type": "function",
						"function": map[string]any{
							"name":      p.ToolName,
							"arguments": p.Arguments,
						},
					})
				}
			}
			if text.Len() > 0 {
				msg["content"] = text.String()
			} else if len(calls) > 0 {
				msg["content"] = nil
			}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
			}
			out = append(out, msg)
		default:
			// 用户消息：文本 + 多模态。
			content := any(m.TextOf())
			var blocks []any
			for _, p := range m.Parts {
				switch p.Type {
				case PartImage:
					blocks = append(blocks, map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": p.DataURL},
					})
				case PartFile:
					blocks = append(blocks, map[string]any{
						"type":     "file",
						"filename": p.FileName,
					})
				}
			}
			if len(blocks) > 0 {
				content = append(blocks, map[string]any{"type": "text", "text": m.TextOf()})
			}
			out = append(out, map[string]any{"role": "user", "content": content})
		}
	}
	return out
}

// toClaudeMessages 把统一消息投影成 Messages 线。
// 返回：消息数组、系统提示（独立字段）。
func toClaudeMessages(msgs []Message) ([]any, string) {
	out := make([]any, 0, len(msgs))
	system := systemText(msgs)
	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			continue // 已经进 system 字段了
		case RoleTool:
			for _, p := range m.Parts {
				if p.Type != PartToolResult {
					continue
				}
				out = append(out, map[string]any{
					"role": "user",
					"content": []any{map[string]any{
						"type":        "tool_result",
						"tool_use_id": p.ToolCallID,
						"content":     toolResultText(p),
						"is_error":    p.IsError,
					}},
				})
			}
		case RoleAssistant:
			var blocks []any
			for _, p := range m.Parts {
				switch p.Type {
				case PartText:
					blocks = append(blocks, map[string]any{"type": "text", "text": p.Text})
				case PartToolCall:
					blocks = append(blocks, map[string]any{
						"type":  "tool_use",
						"id":    p.ToolCallID,
						"name":  p.ToolName,
						"input": json.RawMessage(orEmptyObject(p.Arguments)),
					})
				}
			}
			if len(blocks) > 0 {
				out = append(out, map[string]any{"role": "assistant", "content": blocks})
			}
		default:
			var blocks []any
			text := m.TextOf()
			if text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			for _, p := range m.Parts {
				switch p.Type {
				case PartImage:
					blocks = append(blocks, map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": imageMediaType(p.DataURL),
							"data":       dataURLPayload(p.DataURL),
						},
					})
				case PartFile:
					blocks = append(blocks, map[string]any{
						"type": "document",
						"source": map[string]any{
							"type":       "base64",
							"media_type": p.MediaType,
							"data":       p.FileBase64,
						},
					})
				}
			}
			if len(blocks) > 0 {
				out = append(out, map[string]any{"role": "user", "content": blocks})
			}
		}
	}
	return out, system
}

// toResponsesInput 把统一消息投影成 Responses 线的 input 数组。
func toResponsesInput(msgs []Message) []any {
	out := make([]any, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case RoleSystem:
			continue // 进 instructions 了
		case RoleTool:
			for _, p := range m.Parts {
				if p.Type != PartToolResult {
					continue
				}
				out = append(out, map[string]any{
					"type":    "function_call_output",
					"call_id": p.ToolCallID,
					"output":  toolResultText(p),
				})
			}
		case RoleAssistant:
			for _, p := range m.Parts {
				switch p.Type {
				case PartText:
					out = append(out, map[string]any{
						"type": "message",
						"role": "assistant",
						"content": []any{map[string]any{
							"type": "output_text",
							"text": p.Text,
						}},
					})
				case PartToolCall:
					out = append(out, map[string]any{
						"type":      "function_call",
						"call_id":   p.ToolCallID,
						"name":      p.ToolName,
						"arguments": p.Arguments,
					})
				}
			}
		default:
			var content []any
			text := m.TextOf()
			if text != "" {
				content = append(content, map[string]any{"type": "input_text", "text": text})
			}
			for _, p := range m.Parts {
				switch p.Type {
				case PartImage:
					content = append(content, map[string]any{
						"type":      "input_image",
						"image_url": p.DataURL,
					})
				case PartFile:
					content = append(content, map[string]any{
						"type":     "input_file",
						"filename": p.FileName,
					})
				}
			}
			if len(content) > 0 {
				out = append(out, map[string]any{
					"type":    "message",
					"role":    "user",
					"content": content,
				})
			}
		}
	}
	return out
}

// orEmptyObject 保证参数是合法的 JSON 对象。
func orEmptyObject(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "{}"
	}
	if isValidJSONObject(s) {
		return s
	}
	return "{}"
}

// imageMediaType 从 data URL 里取出 MIME 类型。
func imageMediaType(dataURL string) string {
	if i := strings.Index(dataURL, ";"); i > 5 && strings.HasPrefix(dataURL, "data:") {
		return dataURL[5:i]
	}
	return "image/png"
}

// dataURLPayload 从 data URL 里取出载荷部分。
func dataURLPayload(dataURL string) string {
	if i := strings.Index(dataURL, ","); i >= 0 && strings.HasPrefix(dataURL, "data:") {
		return dataURL[i+1:]
	}
	return dataURL
}

// toolDefsFor 按 wire 渲染工具定义。
func toolDefsFor(style ZenWireStyle, tools []ToolDef) []any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]any, 0, len(tools))
	for _, t := range tools {
		switch style {
		case ZenStyleClaude:
			out = append(out, map[string]any{
				"name":         t.Name,
				"description":  t.Description,
				"input_schema": t.Parameters,
			})
		case ZenStyleFlat:
			out = append(out, map[string]any{
				"type":        "function",
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
				"strict":      false,
			})
		default:
			out = append(out, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			})
		}
	}
	return out
}
