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

// **Kilo 免 key 车道**。
//
// ⚠️ **这是第三条车道，也是最省事的一条** ——
//
// 免密车道（zen）要客户端指纹、会话铸造、工具闸门；
// OpenRouter 要用户自己的 Key（而且免费额度**按天算**）。
//
// Kilo 两条都不要：它是一个**标准 OpenAI 兼容**的网关，
// 它的免费切片**完全不需要凭据** ——
//
//	不需要 Key、不需要登录、没有指纹闸门。
//
// 所以它是**最简单**的一个源：固定 base URL + 每次一个普通 POST。
//
// ⚠️ **代价要说清楚**（上游自己的模型卡上写着）：
//
//	Kilo 免费池的 prompt **可能被上游记录并用于改进服务**。
//	所以这个池子适合**随手干活的活**，**不要往里塞机密**。
//
// ⚠️ **它和 OpenRouter 的模型 id 重叠** ——
//   同样的 `nvidia/nemotron-3.5-super-vl:free`，
//   走 Kilo 不花你的 OR 额度，走 OR 花。
//   所以**默认让 Kilo 优先**。

// KiloLaneID 是这条车道的标识。
const KiloLaneID = "kilo"

// KiloBase 是网关根地址。
const KiloBase = "https://api.kilo.ai/api/gateway"

// KiloUserAgent 是这条车道用的 UA。
//
// ⚠️ Kilo **不检查** UA（实测免 key 直接通），
//
//	但还是发一个 —— 万一以后上游加了检查，我们不至于当场断掉。
const KiloUserAgent = "free-model-house/0.1"

// KiloLane 是 Kilo 免 key 车道。
type KiloLane struct {
	client *http.Client
	// baseURL 是 API 根（测试时可覆盖）。
	baseURL string

	mu sync.Mutex
	// modelCount 是上次清单里的免费模型数。
	modelCount int
	// lastRefresh 是上次刷新清单的时间。
	lastRefresh int64
	// lastError 是上次刷新的错误。
	lastError string
	// efforts 是每个模型声明的思考档位。
	efforts map[string][]string
	// mandatory 记录哪些模型**强制**思考。
	//
	// ⚠️ 这类模型发 `reasoning.enabled: false` 会得到 400。
	mandatory map[string]bool
}

func NewKiloLane() *KiloLane {
	return &KiloLane{
		client:    Transport(),
		baseURL:   KiloBase,
		efforts:   map[string][]string{},
		mandatory: map[string]bool{},
	}
}

func (k *KiloLane) ID() string { return KiloLaneID }

// ⚠️ Kilo **永远 ready** —— 它不需要凭据。
//
//	唯一的失败模式是网络不通，而那会在请求时报错。
func (k *KiloLane) Ready() bool { return true }

// NeedsCredential 报告这条车道**不需要**用户自己的凭据。
//
// ⚠️ 显式声明 `false`（而不是"不实现接口"）——
//
//	意图清楚，也防止以后有人误加。
func (k *KiloLane) NeedsCredential() bool { return false }

// Status 返回这条车道的状态。
func (k *KiloLane) Status() LaneStatus {
	k.mu.Lock()
	defer k.mu.Unlock()
	return LaneStatus{
		ID:          KiloLaneID,
		Name:        "Kilo 免 key 池",
		Ready:       true,
		ModelCount:  k.modelCount,
		LastRefresh: k.lastRefresh,
		LastError:   k.lastError,
		// ⚠️ **参与自动探测** —— 它是**匿名共享池**，
		//   探它不花任何人的钱。
		AutoProbe: true,
	}
}

// kiloRow 是清单里的一行。
type kiloRow struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Description 是**上游自己写的**模型简介。
	//
	// ⚠️ 清单里带着它，是为了将来能**直接展示上游原话** ——
	//	包括 "prompts may be used to train" 这类条款。
	//	自己写总结只会说不到点上，还会漏掉免责。
	Description string `json:"description"`
	IsFree      bool   `json:"isFree"`
	Context     int    `json:"contextLength"`
	Created     int64  `json:"created"`
	// MayTrainOnYourPrompts 标记上游会不会拿你的 prompt 去训练。
	//
	// ⚠️ **这是上游自己声明的** —— 实测每个免费模型都是 true。
	//   界面上要**如实告诉用户**，不能藏着。
	MayTrainOnYourPrompts bool          `json:"mayTrainOnYourPrompts"`
	Reasoning             kiloReasoning `json:"reasoning"`
}

// kiloReasoning 是思考能力的声明。
type kiloReasoning struct {
	Supported bool     `json:"supported"`
	Efforts   []string `json:"efforts"`
	// Mandatory 表示"思考关不掉"。
	Mandatory bool `json:"mandatory"`
}

// fallbackKiloCatalogIDs 是 Kilo 免费模型的快照。
//
// ⚠️ **为什么要有快照** ——
//
//	上游挂了（网络不通 / 被风控 / CA 证书缺失）时，
//	清单**不能**是空的 —— 清单空着 = 整个网关不可用。
//	用户至少还能看到"有哪些模型"，只是暂时用不了。
//
//	只收**确定是免费**的 id —— 快照里混进付费模型，
//	用户点下去会得到 401，比不显示更糟。
var fallbackKiloCatalogIDs = []string{
	"kilo-auto/free",
	"openrouter/free",
	"nvidia/nemotron-3.5-lightning:free",
	"nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free",
	"nvidia/nemotron-3-super-120b-a12b:free",
	"nvidia/nemotron-3-ultra-550b-a55b:free",
	"nvidia/nemotron-3.5-content-safety:free",
	"nvidia/nemotron-3.5-super-vl:free",
	"google/gemma-4-26b-a4b-it:free",
	"google/gemma-4-31b-it:free",
	"liquid/lfm-2.5-2.6b:free",
	"cohere/north-mini-code:free",
	"poolside/laguna-s-2.1:free",
	"poolside/laguna-xs-2.1:free",
	"recursive/flint-v1.0",
	"dots-studio/dots-3-note-preview:free",
	"stealth/glyph-cluster",
	"reflection/beam-free",
	"xingchen-agi/xing4.0-29b-a4b:free",
	"minimax/minimax-m3.1-flash:free",
}

// fallbackKiloCatalog 由快照构造一份 Kilo 清单。
func fallbackKiloCatalog() []ModelInfo {
	out := make([]ModelInfo, 0, len(fallbackKiloCatalogIDs))
	for _, id := range fallbackKiloCatalogIDs {
		info := ModelInfo{
			ID:         FormatModelID(KiloLaneID, id),
			Lane:       KiloLaneID,
			Wire:       "chat",
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

// ListModels 拉清单，只保留免费切片。
//
// ⚠️ **必须过滤 isFree** —— 这个网关上非免费 id 会返回 401，
//
//	而一个装满必然被拒的 id 的清单比没有清单更糟。
func (k *KiloLane) ListModels(ctx context.Context) ([]ModelInfo, *Error) {
	models, uerr := k.listModelsLive(ctx)
	if uerr != nil {
		// 拉不到就用快照兜底 —— 冷启动没网络时仍要有清单可用。
		//
		// ⚠️ **必须设 modelCount** —— 「上游源」页面的"模型数"
		//   取自 `Status().ModelCount`，只在成功拉取时被设置。
		fallback := fallbackKiloCatalog()
		k.mu.Lock()
		k.modelCount = len(fallback)
		k.lastRefresh = time.Now().UnixMilli()
		k.lastError = uerr.Message
		k.mu.Unlock()
		return fallback, uerr
	}
	// ⚠️ 一次成功的拉取如果返回空，**也不能**让清单变空 ——
	// 那说明上游出问题了，不说明"没有免费模型"。
	if len(models) == 0 {
		fallback := fallbackKiloCatalog()
		k.mu.Lock()
		k.modelCount = len(fallback)
		k.lastRefresh = time.Now().UnixMilli()
		k.mu.Unlock()
		return fallback, nil
	}
	return models, nil
}

// listModelsLive 拉一次真实清单（失败时返回错误）。
func (k *KiloLane) listModelsLive(ctx context.Context) ([]ModelInfo, *Error) {
	url := k.baseURL + "/models"
	headers := map[string]string{
		"accept":     "application/json",
		"user-agent": KiloUserAgent,
	}

	res, uerr := doUpstream(ctx, k.client, http.MethodGet, url, headers, nil)
	if uerr != nil {
		k.recordError(uerr.Message)
		return nil, uerr
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		uerr := &Error{Code: CodeTransport, Message: "reading catalog failed: " + err.Error()}
		k.recordError(uerr.Message)
		return nil, uerr
	}

	var doc struct {
		Data []kiloRow `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		uerr := &Error{Code: CodeServer, Message: "kilo catalog is not JSON"}
		k.recordError(uerr.Message)
		return nil, uerr
	}

	out := make([]ModelInfo, 0, len(doc.Data))
	efforts := map[string][]string{}
	mandatory := map[string]bool{}
	for _, row := range doc.Data {
		// ⚠️ **只保留免费的**。
		if !row.IsFree {
			continue
		}
		info := ModelInfo{
			// ⚠️ **ID 带车道前缀** —— 不同车道可以有同名模型
			// （Kilo 和 OpenRouter 的免费 id 完全重叠）。
			//   带上前缀，探测表 / 限流账本 / 清单**全都用同一个键**。
			ID:   FormatModelID(KiloLaneID, row.ID),
			Lane: KiloLaneID,
			// ⚠️ 标准 OpenAI 兼容 —— 走 chat 线。
			Wire:               "chat",
			Name:               kiloDisplayName(row),
			Free:               true,
			Enabled:            true,
			Created:            row.Created,
			ContextWindow:      row.Context,
			Capability:         CapabilitySourceHeuristic,
			State:              "unknown",
			Reasoning:          row.Reasoning.Supported,
			CanDisableThinking: !row.Reasoning.Mandatory && row.Reasoning.Supported,
		}
		if len(row.Reasoning.Efforts) > 0 {
			efforts[row.ID] = row.Reasoning.Efforts
			info.Efforts = row.Reasoning.Efforts
		}
		if row.Reasoning.Mandatory {
			mandatory[row.ID] = true
		}
		applyHeuristicCapabilities(&info, row.ID)
		out = append(out, info)
	}

	k.mu.Lock()
	k.modelCount = len(out)
	k.lastRefresh = time.Now().UnixMilli()
	k.lastError = ""
	k.efforts = efforts
	k.mandatory = mandatory
	k.mu.Unlock()

	return out, nil
}

// kiloDisplayName 给人看的名字。
//
// ⚠️ 去掉厂商前缀和 "(free)" 后缀 ——
//
//	清单里的名字带这些噪音，显示出来很乱。
func kiloDisplayName(row kiloRow) string {
	name := strings.TrimSpace(row.Name)
	if name == "" {
		name = row.ID
	}
	// 去掉 "vendor/" 前缀。
	if i := strings.Index(name, "/"); i >= 0 && i < 24 {
		// ⚠️ 只在斜杠前**像厂商名**时才剥（短、无空格）。
		head := name[:i]
		if head != "" && !strings.ContainsAny(head, " ") && len(head) <= 20 {
			name = name[i+1:]
		}
	}
	name = strings.TrimSpace(strings.TrimSuffix(name, "(free)"))
	if name == "" {
		name = row.ID
	}
	return name
}

// recordError 记一次错误。
func (k *KiloLane) recordError(msg string) {
	k.mu.Lock()
	k.lastError = msg
	k.mu.Unlock()
}

// Do 发一次推理。
func (k *KiloLane) Do(ctx context.Context, turn *Turn, emit func(Chunk)) (*Usage, FinishReason, *Error) {
	body := map[string]any{
		"model":      turn.Model,
		"stream":     true,
		"max_tokens": maxTokensOf(turn),
		"messages":   toChatMessages(turn.Messages),
	}
	// ⚠️ Kilo 用**标准** OpenAI 形状 —— 不需要指纹、不会话头。
	//   这就是它比免密车道简单的地方。
	if defs := toolDefsFor(ZenStyleChat, turn.Tools); len(defs) > 0 {
		body["tools"] = defs
	}
	body["stream_options"] = map[string]any{"include_usage": true}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, FinishError, &Error{Code: CodeClient, Message: err.Error()}
	}

	headers := map[string]string{
		"content-type": "application/json",
		"accept":       "text/event-stream",
		"user-agent":   KiloUserAgent,
	}

	res, uerr := doUpstream(ctx, k.client, http.MethodPost,
		k.baseURL+"/chat/completions", headers, payload)
	if uerr != nil {
		return nil, FinishError, uerr
	}
	defer res.Body.Close()

	dec := newDecoder(wireChat, map[string]string{}, emit)
	usage, uerr := dec.decodeSSE(newIdleReader(res.Body, StreamIdleTimeout))
	if uerr != nil {
		return usage, FinishError, uerr
	}
	finish := dec.finish
	if !dec.sawFinish {
		finish = FinishError
	}
	if usage == nil {
		usage = &Usage{}
	}
	return usage, finish, nil
}

// Probe 探一个模型。
func (k *KiloLane) Probe(ctx context.Context, model string) ProbeResult {
	result := ProbeResult{Model: model, State: StateUnknown, At: time.Now().UnixMilli()}
	start := time.Now()

	// ⚠️ 用**同一个**探测请求形状 —— 免得两处不一致。
	turn := &Turn{
		Model:     model,
		Messages:  []Message{{Role: RoleUser, Parts: []Part{TextPart("ping")}}},
		MaxTokens: probeMaxTokens,
		Seed:      "probe:" + model,
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var firstDelta time.Time
	_, _, uerr := k.Do(ctx, turn, func(c Chunk) {
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
