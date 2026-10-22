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

// **OpenRouter 车道**。
// 与免密车道相比，它**简单得多** —— 没有客户端指纹、没有会话铸造、
// 没有工具闸门。它是**标准 OpenAI 兼容**的。
// 但它有三个自己的特点：
//  1. **要用户自己的 Key**。所以它有"配没配"的状态。
//  2. **免费模型的判定有三条判据**（见 IsOpenRouterFree）。
//  3. **它有真档位**的思考控制 —— `reasoning: {effort: "..."}`，
//     这与免密车道"接受但忽略"完全不同。
// 详见 docs/design/00-overview.md §5.2.2。

// OpenRouterLaneID 是这条车道的标识。
const OpenRouterLaneID = "openrouter"

const OpenRouterBase = "https://openrouter.ai/api/v1"

// OpenRouterLane 是 OpenRouter 车道。
type OpenRouterLane struct {
	client *http.Client
	// apiKey 是用户的 OpenRouter Key。
	// ⚠️ 它**只在本机解密使用**，永远不下发给 LAN 客户端，
	// 管理台也只回显指纹。
	apiKey string
	// baseURL 是 API 根（测试时可覆盖）。
	baseURL string

	mu sync.Mutex
	// modelCount 是上次清单里的免费模型数。
	modelCount int
	// lastRefresh 是上次刷新清单的时间。
	lastRefresh int64
	// lastError 是上次刷新的错误。
	lastError string
	// quota 是上次查到的额度信息。
	quota *QuotaInfo
	// efforts 是每个模型声明的思考档位（清单刷新时填）。
	efforts map[string][]string
	// mandatory 记录哪些模型**强制**思考（思考关不掉）。
	// ⚠️ 对这类模型发 `reasoning.enabled: false` 会得到 **400**。
	mandatory map[string]bool
}

func NewOpenRouterLane(apiKey string) *OpenRouterLane {
	return &OpenRouterLane{
		client:  Transport(),
		apiKey:  apiKey,
		baseURL: OpenRouterBase,
	}
}

func (o *OpenRouterLane) ID() string { return OpenRouterLaneID }

// ⚠️ 没配 Key 就**不 ready** —— 它会从候选链里被排除，
// 而不是每次请求都失败一次。
func (o *OpenRouterLane) Ready() bool { return o.apiKey != "" }

// SetAPIKey 设置/更换 Key。
func (o *OpenRouterLane) SetAPIKey(key string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.apiKey = key
}

// APIKey 返回当前 Key（**只在本进程内用**，不外传）。
func (o *OpenRouterLane) APIKey() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.apiKey
}

// Status 实现 Lane。
func (o *OpenRouterLane) Status() LaneStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	status := LaneStatus{
		ID:              OpenRouterLaneID,
		Name:            "OpenRouter 免费模型",
		Ready:           o.apiKey != "",
		ModelCount:      o.modelCount,
		LastRefresh:     o.lastRefresh,
		LastError:       o.lastError,
		NeedsCredential: true,
		// ⚠️ **不参与自动探测** ——
		//   它的免费额度按天算而且很少，
		//   自动探测等于花**用户自己的**额度。
		AutoProbe: false,
		AutoProbeReason: "OpenRouter 的免费额度按天算且很少（实测一轮 18 个模型就打满" +
			" free-models-per-day），自动探测会花你自己的额度。它的可用性由真实流量验证。",
	}
	if o.apiKey == "" {
		status.Reason = "尚未配置 OpenRouter API Key"
	} else {
		status.CredentialHint = credentialHint(o.apiKey)
	}
	return status
}

// credentialHint 生成凭据指纹（永不回显全文）。
// 格式：`sk-or-v1-…abcd` —— 让人能确认"配对了"，
// 但不足以被用来调用。
func credentialHint(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "…"
	}
	return key[:8] + "…" + key[len(key)-4:]
}

// openRouterCatalogRow 是 OpenRouter 目录里的一行。
type openRouterCatalogRow struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextLength int    `json:"context_length"`
	Created       int64  `json:"created"`
	TopProvider   struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Architecture struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	SupportedParameters []string `json:"supported_parameters"`
	Pricing             struct {
		Prompt     string `json:"prompt"`
		Completion string `json:"completion"`
	} `json:"pricing"`
	Reasoning json.RawMessage `json:"reasoning"`
}

// NeedsCredential 报告这条车道**需要用户自己的 Key**。
//
// ⚠️ 切到这条车道 = 花**用户自己的额度**，
//
//	所以它必须显式声明，路由器才知道要用户许可。
func (o *OpenRouterLane) NeedsCredential() bool { return true }

// fallbackOpenRouterCatalogIDs 是 OpenRouter 免费模型的快照。
//
// ⚠️ **为什么要有快照** ——
//
//	上游挂了（网络不通 / 被风控 / CA 证书缺失）时，
//	清单**不能**是空的 —— 清单空着 = 整个网关不可用。
//	用户至少还能看到"有哪些模型"，只是暂时用不了。
//
//	只收**确定是免费**的 id —— 快照里混进付费模型，
//	用户点下去会得到 401，比不显示更糟。
var fallbackOpenRouterCatalogIDs = []string{
	"nvidia/nemotron-3.5-lightning:free",
	"nvidia/nemotron-3-nano-omni-30b-a3b-reasoning:free",
	"nvidia/nemotron-3-super-120b-a12b:free",
	"nvidia/nemotron-3-ultra-550b-a55b:free",
	"nvidia/nemotron-3.5-content-safety:free",
	"nvidia/nemotron-3.5-super-vl:free",
	"openrouter/free",
	"qwen/qwen3-coder:free",
	"google/gemma-4-26b-a4b-it:free",
	"google/gemma-4-31b-it:free",
	"liquid/lfm-2.5-2.6b:free",
	"cohere/north-mini-code:free",
	"poolside/laguna-s-2.1:free",
	"poolside/laguna-xs-2.1:free",
	"recursive/flint-v1.0",
	"dots-studio/dots-3-note-preview:free",
	"thinkingmachines/inkling:free",
	"thinkingmachines/inkling-small:free",
	"xingchen-agi/xing4.0-29b-a4b:free",
}

// fallbackOpenRouterCatalog 由快照构造一份 OpenRouter 清单。
func fallbackOpenRouterCatalog() []ModelInfo {
	out := make([]ModelInfo, 0, len(fallbackOpenRouterCatalogIDs))
	for _, id := range fallbackOpenRouterCatalogIDs {
		info := ModelInfo{
			ID:         FormatModelID(OpenRouterLaneID, id),
			Lane:       OpenRouterLaneID,
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

// ListModels 拉取 OpenRouter 的免费模型清单。
// ⚠️ **`/api/v1/models` 不需要 Key**（实测：带假 key 也返回 200）。
//
//	所以没配 Key 也能列出免费模型 —— 只是推理不了。
//	这让 UI 能在用户还没配 Key 时就展示"有哪些模型可用"。
func (o *OpenRouterLane) ListModels(ctx context.Context) ([]ModelInfo, *Error) {
	models, uerr := o.listModelsLive(ctx)
	if uerr != nil {
		// 拉不到就用快照兜底 —— 冷启动没网络时仍要有清单可用。
		//
		// ⚠️ **必须设 modelCount** —— 「上游源」页面的"模型数"
		//   取自 `Status().ModelCount`，只在成功拉取时被设置。
		fallback := fallbackOpenRouterCatalog()
		o.mu.Lock()
		o.modelCount = len(fallback)
		o.lastRefresh = time.Now().UnixMilli()
		o.lastError = uerr.Message
		o.mu.Unlock()
		return fallback, uerr
	}
	// ⚠️ 一次成功的拉取如果返回空，**也不能**让清单变空 ——
	// 那说明上游出问题了，不说明"没有免费模型"。
	if len(models) == 0 {
		fallback := fallbackOpenRouterCatalog()
		o.mu.Lock()
		o.modelCount = len(fallback)
		o.lastRefresh = time.Now().UnixMilli()
		o.mu.Unlock()
		return fallback, nil
	}
	return models, nil
}

// listModelsLive 拉一次真实清单（失败时返回错误）。
func (o *OpenRouterLane) listModelsLive(ctx context.Context) ([]ModelInfo, *Error) {
	url := o.baseURL + "/models"
	headers := map[string]string{
		"accept":     "application/json",
		"user-agent": "free-model-house/0.1",
	}
	// 有 Key 就带上 —— 有些端点会按 Key 返回个性化清单。
	if key := o.APIKey(); key != "" {
		headers["authorization"] = "Bearer " + key
	}

	res, uerr := doUpstream(ctx, o.client, http.MethodGet, url, headers, nil)
	if uerr != nil {
		o.recordError(uerr.Message)
		return nil, uerr
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		uerr := &Error{Code: CodeTransport, Message: "reading catalog failed: " + err.Error()}
		o.recordError(uerr.Message)
		return nil, uerr
	}

	var doc struct {
		Data []openRouterCatalogRow `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		uerr := &Error{Code: CodeServer, Message: "openrouter catalog is not JSON"}
		o.recordError(uerr.Message)
		return nil, uerr
	}

	out := make([]ModelInfo, 0, len(doc.Data))
	for _, row := range doc.Data {
		if !IsOpenRouterFree(row.ID) {
			continue
		}
		supports, efforts, canDisable := parseOpenRouterReasoning(row.Reasoning)
		info := ModelInfo{
			// ⚠️ **ID 带车道前缀** —— 不同车道可以有同名模型
			// （Kilo 和 OpenRouter 的免费 id 完全重叠）。
			ID:   FormatModelID(OpenRouterLaneID, row.ID),
			Lane: OpenRouterLaneID,
			// ⚠️ OpenRouter 是标准 OpenAI 兼容 —— 走 chat 线。
			//   （它也支持 /responses，但那是另一条路，v1.0 不做。）
			Wire:               "chat",
			Name:               row.Name,
			Free:               true,
			Enabled:            true,
			Created:            row.Created,
			ContextWindow:      row.ContextLength,
			MaxOutput:          row.TopProvider.MaxCompletionTokens,
			Reasoning:          supports,
			CanDisableThinking: canDisable,
			Efforts:            efforts,
			Capability:         CapabilitySourceListing,
		}
		if info.EffortDefault == "" && len(efforts) > 0 {
			info.EffortDefault = efforts[0]
		}
		for _, in := range row.Architecture.InputModalities {
			switch in {
			case "image":
				info.Vision = true
			case "audio":
				info.AudioInput = true
			case "file", "pdf":
				info.FileInput = true
			}
		}
		out = append(out, info)
	}

	o.mu.Lock()
	o.modelCount = len(out)
	o.lastRefresh = time.Now().UnixMilli()
	o.lastError = ""
	o.mu.Unlock()

	// 把档位表记下来（推理时的"就近收敛"要用）。
	table := map[string][]string{}
	mandatory := map[string]bool{}
	for _, m := range out {
		if len(m.Efforts) > 0 {
			table[m.ID] = m.Efforts
		}
		// ⚠️ `CanDisableThinking == false` 且支持思考 → 强制思考。
		if m.Reasoning && !m.CanDisableThinking {
			mandatory[m.ID] = true
		}
	}
	o.setEfforts(table)
	o.setMandatory(mandatory)

	return out, nil
}

// setMandatory 设置"强制思考"的模型表。
func (o *OpenRouterLane) setMandatory(table map[string]bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.mandatory = table
}

func (o *OpenRouterLane) recordError(msg string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lastError = msg
}

// Do 发起一次推理。
// ⚠️ OpenRouter 是标准 OpenAI 兼容，所以请求体是普通的 chat 形状，
// **不需要**任何指纹。
// ⚠️ 唯一的特殊：**思考档位**。
//
//	OpenRouter 用统一的 `reasoning: {effort: "..."}` 对象，
//	而且 `reasoning.enabled: false` 可以关掉思考。
//	这与免密车道完全不同 —— 那边是"接受但忽略"，这边是**真档位**。
func (o *OpenRouterLane) Do(ctx context.Context, turn *Turn, emit func(Chunk)) (*Usage, FinishReason, *Error) {
	key := o.APIKey()
	if key == "" {
		return nil, FinishError, &Error{
			Code:    CodeCredential,
			Message: "OpenRouter 车道尚未配置 API Key",
		}
	}

	// 请求体。
	body := map[string]any{
		"model":    turn.Model,
		"stream":   true,
		"messages": toChatMessages(turn.Messages),
	}
	if mt := maxTokensOf(turn); mt > 0 {
		body["max_tokens"] = mt
	}
	if len(turn.Tools) > 0 && !turn.SuppressTools {
		if tools := toolDefsFor(ZenStyleChat, turn.Tools); tools != nil {
			body["tools"] = tools
		}
	}
	// 思考档位 —— OpenRouter 的真档位。
	if patch := o.effortPatch(turn); patch != nil {
		body["reasoning"] = patch
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, FinishError, &Error{Code: CodeClient, Message: err.Error()}
	}

	headers := map[string]string{
		"content-type":  "application/json",
		"accept":        "text/event-stream",
		"authorization": "Bearer " + key,
		"user-agent":    "free-model-house/0.1",
		// ⚠️ OpenRouter 建议带上这两个头做归因（可选，但更规范）。
		"http-referer": "https://github.com/aBon579/free-model-house",
		"x-title":      "Free Model House",
	}

	url := o.baseURL + "/chat/completions"
	res, uerr := doUpstream(ctx, o.client, http.MethodPost, url, headers, payload)
	if uerr != nil {
		return nil, FinishError, uerr
	}
	defer res.Body.Close()

	dec := newDecoder(wireChat, nil, emit)
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

// effortPatch 构造 OpenRouter 的思考参数。
// ⚠️ OpenRouter 的形状是 `reasoning: {effort: "..."}`，
// 而且 `reasoning.enabled: false` 可以**关掉**思考 ——
// 这与免密车道"接受但忽略"完全不同。
// ## ⚠️ 三条实测得来的硬规则
// ① **`mandatory: true` 的模型不能关思考** ——
// 发 `reasoning.enabled: false` 会得到 **400**：
// "Reasoning is mandatory for this endpoint and cannot be disabled."
// 所以对这类模型，"关掉思考"要**降级成最低档位**，而不是硬发 `enabled:false`。
// ② **档位要收敛到模型声明过的** ——
// OpenRouter 会给 `supported_efforts`，发一个它不支持的档位可能 400，
// 也可能被静默忽略。一个"点了没反应"的档位比没有档位更糟。
// ③ **没声明档位的模型不能随便发 effort** ——
// 实测 `mandatory:false` 但 `supported_efforts` 为空的模型，
// 发 `reasoning.effort` 是安全的（会被忽略），
// 但发 `enabled:false` 也安全（因为不 mandatory）。
// 实测数据（2026-10-19，18 个免费模型）：
//
//	mandatory=true  + 有档位  → minimax/minimax-m3.1-flash:free
//	                    （max,xhigh,high,medium,low）
//	mandatory=true  + 无档位  → liquid/lfm-2.5-2.6b:free
//	mandatory=false + 有档位  → nvidia/nemotron-3-super-120b-a12b:free
//	                    （medium,low,none —— **含 none**！）
//	mandatory=false + 无档位  → 大部分
func (o *OpenRouterLane) effortPatch(turn *Turn) map[string]any {
	if turn.Effort == "" {
		return nil
	}

	allowed, known := o.effortsOf(turn.Model)
	mandatory := o.mandatoryReasoning(turn.Model)

	// 免密车道用 light/balanced/deep；OpenRouter 用自己的档位名。
	level := turn.Effort
	switch level {
	case "light":
		level = "low"
	case "balanced":
		level = "medium"
	case "deep":
		level = "high"
	}

	// ⚠️ **"关掉思考"的语义**。
	// ⚠️⚠️ **`mandatory` 必须先判** ——
	//   实测：强制思考的模型上，`effort:none` 也返回 **400**
	//   （不只是 `enabled:false` 会 400）。
	//   所以哪怕模型声明了 `none` 档，
	//   只要它是 mandatory 的，就不能发任何"关掉"的语义。
	// 实测矩阵：
	//	mandatory=false  effort:none      → 200，reasoning_tokens=0 ✅
	//	mandatory=false  enabled:false    → 200，reasoning_tokens=0 ✅
	//	mandatory=false  effort:high      → 200（被忽略，无效果）
	//	mandatory=true   effort:none      → **400** ❌
	//	mandatory=true   enabled:false    → **400** ❌
	//	mandatory=true   effort:low       → 200 ✅
	if strings.EqualFold(turn.Effort, EffortOff) {
		// ① **强制思考** → 降级成最低档位。
		if mandatory {
			level = lowestEffort(allowed)
			if level == "" {
				// 连档位都没有 → 干脆不发 reasoning 参数。
				return nil
			}
			return map[string]any{"effort": level}
		}
		// ② 不强制思考 → 真的关掉。
		// 声明了 `none` 档就用档位（更"原生"），
		// 否则用 `enabled:false` —— 实测两者都有效。
		if containsString(allowed, "none") {
			return map[string]any{"effort": "none"}
		}
		return map[string]any{"enabled": false}
	}

	// 档位收敛。
	// ⚠️ **"没声明档位"有两种情况，要分开**：
	//	① 模型**在表里但档位为空** → 上游明确说了"我不支持档位"
	//	   → 不发 effort（发了也没用）
	//	② 模型**不在表里** → 我们还没探到它的档位
	//	   → 发统一档位（上游会忽略，无害）
	if known {
		if len(allowed) > 0 {
			if !containsString(allowed, level) {
				level = nearestEffort(allowed, level)
			}
			return map[string]any{"effort": level}
		}
		// ① 明确不支持档位 → 不发。
		return nil
	}

	// ② 未知 → 发统一档位。
	// 实测：无档位的模型接受 `effort`（200，只是被忽略）。所以发是安全的。
	return map[string]any{"effort": level}
}

// effortsOf 返回一个模型的档位，以及"我们是否知道它的档位"。
// ⚠️ `known=false` 表示**这个模型还没出现在档位表里**
// （清单还没拉，或者它是新模型）。
func (o *OpenRouterLane) effortsOf(model string) (levels []string, known bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.efforts == nil {
		return nil, false
	}
	levels, known = o.efforts[model]
	return levels, known
}

// mandatoryReasoning 报告一个模型是否**强制**思考。
func (o *OpenRouterLane) mandatoryReasoning(model string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.mandatory[model]
}

// lowestEffort 返回允许档位里最低的一个。
func lowestEffort(allowed []string) string {
	if len(allowed) == 0 {
		return ""
	}
	best := ""
	bestRank := 1 << 30
	for _, a := range allowed {
		if a == "none" {
			// "none" 不是"最低档"，它是"关掉"。
			// 降级用档位时不能用它。
			continue
		}
		r, ok := effortRank[a]
		if !ok {
			continue
		}
		if r < bestRank {
			bestRank = r
			best = a
		}
	}
	return best
}

// effortRank 给档位排名（越大越"用力"）。
var effortRank = map[string]int{
	"minimal": 0,
	"none":    1,
	"low":     2,
	"medium":  3,
	"high":    4,
	"xhigh":   5,
	"max":     6,
}

// allowedEfforts 返回一个模型声明的思考档位。
func (o *OpenRouterLane) allowedEfforts(model string) []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.efforts == nil {
		return nil
	}
	return o.efforts[model]
}

// setEfforts 设置模型的档位表（清单刷新时调用）。
func (o *OpenRouterLane) setEfforts(table map[string][]string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.efforts = table
}

// nearestEffort 在允许的档位里找一个最接近的。
func nearestEffort(allowed []string, want string) string {
	order := map[string]int{
		"minimal": 0, "low": 1, "medium": 2, "high": 3, "xhigh": 4, "max": 5,
	}
	wantRank, ok := order[want]
	if !ok {
		wantRank = 2
	}
	best := allowed[0]
	bestDist := 1 << 30
	for _, a := range allowed {
		r, ok := order[a]
		if !ok {
			continue
		}
		d := r - wantRank
		if d < 0 {
			d = -d
		}
		if d < bestDist {
			bestDist = d
			best = a
		}
	}
	return best
}

// containsString 判断一个字符串是否在列表里。
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// QuotaInfo 是 OpenRouter 的额度信息。
type QuotaInfo struct {
	// Limit 是总额度（美元）；0 = 无限。
	Limit float64 `json:"limit"`
	// Usage 是已用（美元）。
	Usage float64 `json:"usage"`
	// Remaining 是剩余（美元）。
	Remaining float64 `json:"remaining"`
	// FreeTierUsageToday 是今天在免费模型上用的额度。
	FreeTierUsageToday float64 `json:"freeTierUsageToday,omitempty"`
	Label              string  `json:"label,omitempty"`
}

// openRouterKeyResponse 是 `/api/v1/key` 的响应。
type openRouterKeyResponse struct {
	Data struct {
		Label          string  `json:"label"`
		Usage          float64 `json:"usage"`
		Limit          float64 `json:"limit"`
		LimitRemaining float64 `json:"limit_remaining"`
		FreeTierUsage  float64 `json:"free_tier_usage"`
		IsFreeTier     bool    `json:"is_free_tier"`
	} `json:"data"`
}

// FetchQuota 查 OpenRouter 的额度。
// ⚠️ 这个端点**要 Key**（实测：假 key → 401 "User not found."）。
// 它很有价值：用户能在看板上看到"我的免费额度还剩多少"，
// 而不用去 openrouter.ai 的网页上看。
func (o *OpenRouterLane) FetchQuota(ctx context.Context) (*QuotaInfo, *Error) {
	key := o.APIKey()
	if key == "" {
		return nil, &Error{Code: CodeCredential, Message: "尚未配置 OpenRouter API Key"}
	}
	headers := map[string]string{
		"accept":        "application/json",
		"authorization": "Bearer " + key,
		"user-agent":    "free-model-house/0.1",
	}
	res, uerr := doUpstream(ctx, o.client, http.MethodGet, o.baseURL+"/key", headers, nil)
	if uerr != nil {
		return nil, uerr
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, &Error{Code: CodeTransport, Message: err.Error()}
	}
	var doc openRouterKeyResponse
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, &Error{Code: CodeServer, Message: "openrouter quota is not JSON"}
	}
	info := &QuotaInfo{
		Limit:              doc.Data.Limit,
		Usage:              doc.Data.Usage,
		Remaining:          doc.Data.LimitRemaining,
		FreeTierUsageToday: doc.Data.FreeTierUsage,
		Label:              doc.Data.Label,
	}
	o.mu.Lock()
	o.quota = info
	o.mu.Unlock()
	return info, nil
}

// Quota 返回上次查到的额度。
func (o *OpenRouterLane) Quota() *QuotaInfo {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.quota
}

// Probe 实现 Lane。
// Probe 探一个模型。
//
// ⚠️ **OpenRouter 的免费额度是"按天"的，而且很少** ——
//
// 实测（2026-10-20）：一轮 18 个模型的探测就把当天的
// `free-models-per-day` 配额**打满了**，剩下的全是
// "Rate limit exceeded: free-models-per-day"。
//
// 而 OpenRouter 的额度是**用户自己的钱**（虽然是免费额度）。
// 拿它去做"每 15 分钟全量探测"是**不合适的**。
//
// 所以：**OR 车道不参与自动探测**。
// 它的可用性由**真实流量**来验证 ——
// 用户真的调了、通了，面板上的"实际服务"列就有记录。
//
// 想知道某个 OR 模型能不能用？**手动探这一个**（`POST /api/probes/run`）。
func (o *OpenRouterLane) Probe(ctx context.Context, model string) ProbeResult {
	result := ProbeResult{Model: model, State: StateUnknown, At: time.Now().UnixMilli()}
	if !o.Ready() {
		result.State = StateUnavailable
		result.Detail = "未配置 API Key"
		return result
	}
	start := time.Now()
	turn := &Turn{
		Model: model,
		Messages: []Message{
			{Role: RoleUser, Parts: []Part{TextPart("ping")}},
		},
		// ⚠️ 跟免密车道用**同一个**预算 —— 16 会让模型全烧在 reasoning 上。
		MaxTokens: probeMaxTokens,
		Seed:      "probe:" + model,
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var firstDelta time.Time
	_, _, uerr := o.Do(ctx, turn, func(c Chunk) {
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

// AutoProbeable 报告这条车道**是否参与自动探测**。
//
// ⚠️ **OpenRouter 返回 false** ——
//
//	它的免费额度按天算、而且很少（实测一轮 18 个就打满）。
//	拿用户的额度做每 15 分钟一轮的全量探测 = **直接花用户的钱**。
//
//	免密车道和 Kilo 都是**匿名共享池** —— 探它们不花任何人的钱。
func (o *OpenRouterLane) AutoProbeable() bool { return false }
