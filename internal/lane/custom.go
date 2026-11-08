package lane

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"
)

// **自定义 Key 车道**。
//
// 用户在 Key Hub 里填入自己的 OpenAI 兼容端点（Base URL + Key + 模型），
// 这些模型会进入模型池，但**不参与自动批量探测** ——
// 探测会消耗用户自己的额度，和 OpenRouter 车道一样，
// 只允许在模型清单里手动探测。
//
// 对外模型 id 的格式是 `custom/<provider-id>/<upstream-model>`，
// 第一个斜杠之后的部分再按 `/` 拆出 provider id 与上游模型名。

// CustomLaneID 是自定义 Key 车道的标识。
const CustomLaneID = "custom"

// CustomProvider 是一条自定义 OpenAI 兼容端点。
type CustomProvider struct {
	ID      string
	Name    string
	BaseURL string
	APIKey  string
	Models  []string
	// ContextLength 是用户给这个端点填的上下文长度（0 = 没填）。
	//
	// ⚠️ 它会覆盖启发式推断值 —— 用户明确填了就以用户为准，
	//   这样 /v1/models 与设置页展示的是**同一个数**。
	ContextLength int
}

// CustomLane 聚合用户添加的所有自定义 OpenAI 兼容端点。
type CustomLane struct {
	client *http.Client

	mu          sync.RWMutex
	providers   []CustomProvider
	modelCount  int
	lastRefresh int64
	lastError   string
}

func NewCustomLane() *CustomLane {
	return &CustomLane{client: Transport()}
}

func (c *CustomLane) ID() string { return CustomLaneID }

func (c *CustomLane) Ready() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.providers) > 0
}

// NeedsCredential 报告这条车道要用用户自己的凭据。
func (c *CustomLane) NeedsCredential() bool { return true }

// AutoProbeable 报告这条车道**不参与自动批量探测**。
func (c *CustomLane) AutoProbeable() bool { return false }

func (c *CustomLane) Status() LaneStatus {
	c.mu.RLock()
	defer c.mu.RUnlock()
	reason := ""
	if len(c.providers) == 0 {
		reason = "还没有添加自定义 API Key"
	}
	return LaneStatus{
		ID:          CustomLaneID,
		Name:        "自定义 Key 池",
		Ready:       len(c.providers) > 0,
		Reason:      reason,
		ModelCount:  c.modelCount,
		LastRefresh: c.lastRefresh,
		LastError:   c.lastError,
		// ⚠️ 和 OpenRouter 一样：这些模型花的是用户自己的额度，
		// 所以只支持手动探测，不做周期性全量探测。
		AutoProbe:       false,
		AutoProbeReason: "自定义 Key 使用的是你自己的额度，不参与自动批量探测；可在模型清单里手动探测",
		NeedsCredential: true,
	}
}

// SetProviders 替换全部自定义端点（会刷新模型计数）。
func (c *CustomLane) SetProviders(providers []CustomProvider) {
	c.mu.Lock()
	c.providers = append([]CustomProvider(nil), providers...)
	c.modelCount = 0
	for _, p := range providers {
		c.modelCount += len(p.Models)
	}
	c.lastRefresh = time.Now().UnixMilli()
	c.lastError = ""
	c.mu.Unlock()
}

// Providers 返回当前端点副本（不含对调用方可变引用的模型切片）。
func (c *CustomLane) Providers() []CustomProvider {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]CustomProvider, 0, len(c.providers))
	for _, p := range c.providers {
		cp := p
		cp.Models = append([]string(nil), p.Models...)
		out = append(out, cp)
	}
	return out
}

// ListModels 返回所有自定义端点的模型清单。
func (c *CustomLane) ListModels(ctx context.Context) ([]ModelInfo, *Error) {
	c.mu.RLock()
	providers := append([]CustomProvider(nil), c.providers...)
	c.mu.RUnlock()

	out := make([]ModelInfo, 0, c.modelCount)
	for _, p := range providers {
		for _, model := range p.Models {
			upstream := strings.TrimSpace(model)
			if upstream == "" {
				continue
			}
			info := ModelInfo{
				ID:         FormatModelID(CustomLaneID, p.ID+"/"+upstream),
				Lane:       CustomLaneID,
				Wire:       "chat",
				Name:       p.Name + " · " + upstream,
				Free:       true,
				Enabled:    true,
				Capability: CapabilitySourceHeuristic,
				State:      "unknown",
			}
			applyHeuristicCapabilities(&info, upstream)
			// ⚠️ 用户填写的上下文长度**覆盖**启发式推断 ——
			//   agent 端（/v1/models 的 context_window）看到的就是这个值。
			if p.ContextLength > 0 {
				info.ContextWindow = p.ContextLength
				info.Capability = CapabilitySourceUser
			}
			out = append(out, info)
		}
	}
	return out, nil
}

// findProvider 按 id 找端点。
func (c *CustomLane) findProvider(id string) (CustomProvider, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, p := range c.providers {
		if p.ID == id {
			return p, true
		}
	}
	return CustomProvider{}, false
}

// splitCustomModel 把 `<provider>/<upstream>` 拆开。
func splitCustomModel(model string) (string, string, bool) {
	i := strings.IndexByte(model, '/')
	if i <= 0 || i == len(model)-1 {
		return "", "", false
	}
	return model[:i], model[i+1:], true
}

// customChatURL 把用户填写的 Base URL 规范成 chat/completions 地址。
func customChatURL(base string) string {
	base = strings.TrimSpace(base)
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/chat/completions") {
		return base
	}
	if strings.HasSuffix(base, "/v1") {
		return base + "/chat/completions"
	}
	return base + "/v1/chat/completions"
}

// Do 发一次推理。
func (c *CustomLane) Do(ctx context.Context, turn *Turn, emit func(Chunk)) (*Usage, FinishReason, *Error) {
	providerID, upstream, ok := splitCustomModel(turn.Model)
	if !ok {
		return nil, FinishError, &Error{Code: CodeClient, Message: "自定义模型 id 必须包含端点 id 与上游模型名"}
	}
	provider, ok := c.findProvider(providerID)
	if !ok {
		return nil, FinishError, &Error{Code: CodeClient, Message: "自定义端点不存在：已从 Key Hub 删除？"}
	}

	body := map[string]any{
		"model":      upstream,
		"stream":     true,
		"max_tokens": maxTokensOf(turn),
		"messages":   toChatMessages(turn.Messages),
	}
	if defs := toolDefsFor(ZenStyleChat, turn.Tools); len(defs) > 0 {
		body["tools"] = defs
	}
	body["stream_options"] = map[string]any{"include_usage": true}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, FinishError, &Error{Code: CodeClient, Message: err.Error()}
	}

	headers := map[string]string{
		"content-type":  "application/json",
		"accept":        "text/event-stream",
		"authorization": "Bearer " + provider.APIKey,
	}

	res, uerr := doUpstream(ctx, c.client, http.MethodPost,
		customChatURL(provider.BaseURL), headers, payload)
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
func (c *CustomLane) Probe(ctx context.Context, model string) ProbeResult {
	result := ProbeResult{Model: model, State: StateUnknown, At: time.Now().UnixMilli()}
	start := time.Now()

	turn := &Turn{
		Model:     model,
		Messages:  []Message{{Role: RoleUser, Parts: []Part{TextPart("ping")}}},
		MaxTokens: probeMaxTokens,
		Seed:      "probe:" + model,
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var firstDelta time.Time
	_, _, uerr := c.Do(ctx, turn, func(ch Chunk) {
		if firstDelta.IsZero() && (ch.Kind == ChunkTextDelta || ch.Kind == ChunkReasonDelta) {
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
