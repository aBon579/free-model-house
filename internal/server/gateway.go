package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aBon579/free-model-house/internal/lane"
)

// 本文件是 **推理面** —— 对外暴露 OpenAI / Anthropic 兼容 API。
// 三条线：/v1/chat/completions（OpenAI Chat）、/v1/responses（OpenAI Responses）、
// /v1/messages（Anthropic Messages）。
// ⚠️ 核心机制是**延迟提交**：在第一个"答案承载"的 delta 之前不写响应头。
// 这样上游的 429 + Retry-After 还能作为**真正的 HTTP 429** 返回 ——
// 而 agent 的退避逻辑正是靠它。
// 一旦发出第一个字节，200 就已提交，失败只能以流内错误事件传递。

// Gateway 是推理面。
type Gateway struct {
	router  *lane.Router
	catalog *lane.Catalog
	lanes   *lane.Registry
	keys    *Keys
	// onUsage 收到一次用量记录。
	onUsage func(lane.UsageRecord)
	// requestTimeout 是单次请求的上限。
	requestTimeout time.Duration
	// mu 保护可变开关。
	mu sync.Mutex
}

// NewGateway 建一个推理面。
func NewGateway(router *lane.Router, catalog *lane.Catalog, lanes *lane.Registry, keys *Keys) *Gateway {
	return &Gateway{
		router:         router,
		catalog:        catalog,
		lanes:          lanes,
		keys:           keys,
		requestTimeout: 300 * time.Second,
	}
}

// SetUsageHook 设置用量回调。
func (g *Gateway) SetUsageHook(fn func(lane.UsageRecord)) { g.onUsage = fn }

// Routes 返回推理面的路由处理函数。
func (g *Gateway) Routes() http.Handler {
	mux := http.NewServeMux()

	// ⚠️ CORS 预检**不鉴权** —— 浏览器无法在预检里带 key。
	// 而且预检不消耗任何额度。
	mux.HandleFunc("/v1/chat/completions", g.handle(g.serveChat))
	mux.HandleFunc("/chat/completions", g.handle(g.serveChat))
	mux.HandleFunc("/v1/responses", g.handle(g.serveResponses))
	mux.HandleFunc("/responses", g.handle(g.serveResponses))
	mux.HandleFunc("/v1/messages", g.handle(g.serveMessages))
	mux.HandleFunc("/messages", g.handle(g.serveMessages))
	mux.HandleFunc("/v1/messages/count_tokens", g.handle(g.serveCountTokens))
	mux.HandleFunc("/messages/count_tokens", g.handle(g.serveCountTokens))
	mux.HandleFunc("/v1/models", g.handle(g.serveModels))
	mux.HandleFunc("/models", g.handle(g.serveModels))

	return mux
}

// handle 包一层：预检 + 鉴权 + 错误处理。
func (g *Gateway) handle(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			writeCORS(w, r)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// 鉴权 —— **推理 Key**。
		// ⚠️ 单端口下，推理面必须认局域网 Key —— 因为 LAN 里的 agent
		//   拿的就是它，而它们访问的正是同一个 :8678 端口。
		//
		// ⚠️ **管理面不走这里** —— 管理面用**密码**。
		//   那个区分保证了：推理 Key 泄露 = 白嫖额度，但进不了管理面。
		presented := BearerOf(r)
		if !g.keys.MatchAny(presented) {
			writeOpenAIError(w, http.StatusUnauthorized, "invalid_request_error", "missing or invalid API key")
			return
		}
		// ⚠️ **两层限流** —— Keys.Allow 里裁决：
		//
		//	网关级 != off → 所有 Key 共用网关的桶（强制跟随）
		//	网关级 == off → 这把 Key 自己的桶（deep = 不限）
		//
		// ⚠️ 拿不到名额就 429 —— 客户端会退避重试，
		//   比 403 好（403 会让它直接放弃）。
		if !g.keys.Allow(presented) {
			w.Header().Set("Retry-After", "10")
			writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error",
				"已达限流上限 —— 稍后重试")
			return
		}
		defer g.keys.Release(presented)
		// 请求 id —— 客户端可以自带，这样两边日志能对上。
		reqID := requestIDOf(r)
		w.Header().Set("X-FMH-Request-Id", reqID)

		defer func() {
			if rec := recover(); rec != nil {
				writeOpenAIError(w, http.StatusInternalServerError, "server_error", "internal error")
			}
		}()
		next(w, r)
	}
}

// requestIDOf 取或铸一个请求 id。
// ⚠️ 接受客户端自带的 `X-Request-Id` / `X-Client-Request-Id` ——
// 这样客户端日志与网关日志能对上。
func requestIDOf(r *http.Request) string {
	for _, h := range []string{"X-Request-Id", "X-Client-Request-Id"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" && len(v) <= 128 {
			return v
		}
	}
	return store_NewRequestID()
}

// serveModels 返回模型清单。
func (g *Gateway) serveModels(w http.ResponseWriter, r *http.Request) {
	writeCORS(w, r)
	models := g.catalog.Models()
	// 按展示顺序排。
	lane.SortModels(models)

	data := make([]map[string]any, 0, len(models))
	for _, m := range models {
		if !m.Enabled {
			continue
		}
		// ⚠️ **关闭的车道不出现** —— agent 拿到的清单就是它能调用的集合；
		//   列一个"其实调不到"的模型只会让它白白重试。
		if !g.router.LaneEnabled(m.Lane) {
			continue
		}
		entry := map[string]any{
			// ⚠️ `m.ID` **已经带车道前缀**了 —— 别再拼一次。
			"id":       m.ID,
			"object":   "model",
			"created":  m.Created,
			"owned_by": "free-model-house",
			"lane":     m.Lane,
			"free":     m.Free,
		}
		if m.Name != "" {
			entry["name"] = m.Name
		}
		if m.ContextWindow > 0 {
			entry["context_window"] = m.ContextWindow
		}
		if m.MaxOutput > 0 {
			entry["max_output_tokens"] = m.MaxOutput
		}
		if m.Vision || m.AudioInput || m.FileInput {
			modalities := []string{"text"}
			if m.Vision {
				modalities = append(modalities, "image")
			}
			if m.AudioInput {
				modalities = append(modalities, "audio")
			}
			if m.FileInput {
				modalities = append(modalities, "file")
			}
			entry["input_modalities"] = modalities
		}
		if m.Reasoning {
			entry["reasoning"] = true
			entry["supported_reasoning_levels"] = m.Efforts
		}
		// ⚠️ capability_source 让调用方知道这个结论**有多可靠**。
		entry["capability_source"] = string(m.Capability)
		if m.State != "" {
			entry["state"] = m.State
		}
		if m.TTFTMs > 0 {
			entry["ttft_ms"] = m.TTFTMs
		}
		data = append(data, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

// serveChat 处理 OpenAI Chat。
func (g *Gateway) serveChat(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	turn, stream, err := g.turnFromChat(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	g.run(w, r, turn, stream, lane.StyleChat, body)
}

// serveResponses 处理 OpenAI Responses。
func (g *Gateway) serveResponses(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	turn, stream, err := g.turnFromResponses(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	g.run(w, r, turn, stream, lane.StyleResponses, body)
}

// serveMessages 处理 Anthropic Messages。
func (g *Gateway) serveMessages(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	turn, stream, err := g.turnFromMessages(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	g.run(w, r, turn, stream, lane.StyleMessages, body)
}

// run 跑一次推理并把结果写出去。
func (g *Gateway) run(w http.ResponseWriter, r *http.Request, turn *lane.Turn, stream bool, style lane.EncoderStyle, body map[string]any) {
	writeCORS(w, r)

	// 指纹工具名（要压制它们）。
	suppress := map[string]bool{"read": true, "shell": true}
	callerTools := map[string]bool{}
	for _, t := range turn.Tools {
		callerTools[t.Name] = true
	}
	rename := map[string]string{}

	reqID := requestIDOf(r)

	// 非流式：先把结果攒起来再一次性写出。
	// ⚠️ **必须从收上来的 chunk 重建答案**，不能从 turn.Messages 读 ——
	//   那是**请求**，里面没有模型的回答。
	if !stream {
		// 收集助手侧的内容。
		collected := collectAssistant(turn)
		result, uerr := g.complete(r.Context(), turn, func(c lane.Chunk) {
			collected.add(c)
		})
		if uerr != nil {
			g.writeError(w, style, uerr)
			g.recordUsage(turn, result, uerr)
			return
		}
		g.recordUsage(turn, result, uerr)
		g.writeNonStream(w, style, collected.rebuild(), result, reqID)
		return
	}

	// 流式。
	// ⚠️ **延迟提交**：先不写头，等第一个"答案承载"的 delta。
	// 这样上游的 429 + Retry-After 还能作为真正的 HTTP 429 返回。
	writer := newSSEWriter(w, r)
	committed := false

	encoder := g.newEncoder(style, writer, reqID, turn, suppress, callerTools, rename)

	ctx, cancel := context.WithTimeout(r.Context(), g.requestTimeout)
	defer cancel()

	// ⚠️ 续写状态 —— 跟踪已经输出的内容，
	// 以便在流被截断时把它喂回上游。
	cont := lane.NewContinuationState()

	// ⚠️ **思考缓冲**。
	// 思考内容在提交之前**不能写进流**：encoder.Write 会立刻写 SSE 帧，
	// 而写帧会触发 200 的提交。一旦提交了，上游后来的 429 就只能变成流内错误事件 ——
	// 客户端拿不到真正的 HTTP 429 与 `Retry-After`，它的退避逻辑就废了。
	// 而"想了一分钟然后撞上配额墙"恰恰是最常见的失败形态。
	// 所以：思考先攒着，等第一个正文/工具调用到达时，**先把攒的思考倒出来**，
	// 再写正文 —— 顺序不变，只是不提前提交。
	var pending []lane.Chunk

	result, uerr := g.complete(ctx, turn, func(c lane.Chunk) {
		cont.Observe(c)

		// 思考 → 缓冲，不提交。
		if !committed && c.Kind == lane.ChunkReasonDelta {
			pending = append(pending, c)
			return
		}

		if !committed {
			// ⚠️ 第一个"答案承载"的 delta 到了 —— 现在才提交。
			committed = true
			writer.commit()
			// ⚠️ **先把缓冲的思考倒出来** ——
			// 客户端看到的顺序仍然是"先思考、后正文"。
			for _, p := range pending {
				_ = encoder.Write(p)
			}
			pending = nil
		}
		if err := encoder.Write(c); err != nil {
			return
		}
	})

	// ⚠️ **断流续写**。
	// 只有在"已经提交"（已经吐过内容）时才需要 —— 没提交的话，故障转移已经处理过了。
	if uerr == nil && committed && cont.NeedsContinuation() {
		// ⚠️ **续写要发给实际服务的那个模型** —— 不是请求里写的那个。
		//   故障转移可能换过模型/车道：客户端要 `zen/model-a`，实际是
		//   `openrouter/model-b` 应答的，那续写就该发给 `openrouter/model-b`。
		//   用 `turn.Model` 会续写到**错误的模型**上 —— 续写的内容接不上，
		//   而且浪费一次上游调用。
		contTurn := turn
		if result != nil && result.Model != "" {
			cp := *turn
			cp.Lane = result.Lane
			cp.Model = result.Model
			contTurn = &cp
		}
		report := g.continueStream(ctx, contTurn, cont, writer, encoder)
		if report.Outcome == lane.ContinuationOK {
			// 续写成功了 —— 合并用量。
			if result != nil && result.Usage != nil {
				result.Usage.Output += report.RecoveredTokens
				result.Usage.Total += report.RecoveredTokens
			}
		}
	}

	if uerr != nil && !committed {
		// ⚠️ 还没提交 → 可以用真正的 HTTP 状态码。
		writer.abort()
		g.writeError(w, style, uerr)
		g.recordUsage(turn, result, uerr)
		return
	}
	g.recordUsage(turn, result, uerr)

	// 已提交 → 收尾。
	finish := lane.FinishStop
	if uerr != nil {
		finish = lane.FinishError
	}
	var usage *lane.Usage
	if result != nil {
		usage = result.Usage
	}
	_ = encoder.Finish(finish, usage)
	writer.finish()
}

// continueStream 尝试断流续写。
// ⚠️ 续写失败**不报错** —— 已经提交了，只能以流内错误事件告知客户端。
func (g *Gateway) continueStream(
	ctx context.Context,
	turn *lane.Turn,
	cont *lane.ContinuationState,
	writer *sseWriter,
	encoder lane.Encoder,
) lane.ContinuationReport {
	// 找到实际服务的车道 —— 续写要发给**同一个**模型。
	laneID := turn.Lane
	model := turn.Model
	l, ok := g.lanes.Get(laneID)
	if !ok {
		return lane.ContinuationReport{
			Outcome: lane.ContinuationFailed,
			Reason:  "找不到车道",
		}
	}

	req := lane.ContinuationRequest{
		Base:      turn.Messages,
		Tools:     turn.Tools,
		Model:     model,
		Lane:      laneID,
		Effort:    turn.Effort,
		MaxTokens: turn.MaxTokens,
		Seed:      turn.Seed,
	}

	report, err := lane.Continue(ctx, l, req, cont, func(c lane.Chunk) {
		// ⚠️ 续写产生的新内容**要发给客户端**。
		_ = encoder.Write(c)
	})
	if err != nil {
		report.Outcome = lane.ContinuationFailed
		report.Reason = err.Error()
	}
	_ = writer
	return report
}

// complete 跑一次推理（含故障转移）。
func (g *Gateway) complete(ctx context.Context, turn *lane.Turn, emit func(lane.Chunk)) (*lane.TurnResult, *lane.Error) {
	committed := false
	result, uerr := g.router.Complete(ctx, turn, func(c lane.Chunk) {
		if c.Kind == lane.ChunkTextDelta || c.Kind == lane.ChunkToolCallDelta {
			committed = true
		}
		emit(c)
	}, func() bool { return committed })
	return result, uerr
}

// newEncoder 建一个对应协议的 encoder。
func (g *Gateway) newEncoder(style lane.EncoderStyle, w lane.Writer, reqID string, turn *lane.Turn,
	suppress, callerTools map[string]bool, rename map[string]string) lane.Encoder {

	switch style {
	case lane.StyleResponses:
		return lane.NewResponsesEncoder(w, reqID, turn.Model, suppress, callerTools, rename)
	case lane.StyleMessages:
		msgID := "msg_" + reqID
		return lane.NewMessagesEncoder(w, reqID, turn.Model, msgID, suppress, callerTools, rename)
	default:
		return lane.NewChatEncoder(w, reqID, turn.Model, suppress, callerTools, rename)
	}
}

// writeNonStream 写非流式响应。
func (g *Gateway) writeNonStream(w http.ResponseWriter, style lane.EncoderStyle, turn *lane.Turn,
	result *lane.TurnResult, reqID string) {

	usage := &lane.Usage{}
	finish := lane.FinishStop
	// 标头：实际服务的模型。
	// ⚠️ result 可能是 nil（理论上不该，但别在这儿崩）。
	if result != nil {
		if result.Usage != nil {
			usage = result.Usage
		}
		finish = result.Finish
		w.Header().Set("X-FMH-Model", result.Model)
		w.Header().Set("X-FMH-Lane", result.Lane)
	}

	switch style {
	case lane.StyleMessages:
		writeJSON(w, http.StatusOK, BuildAnthropicMessage(turn, usage, finish, reqID))
	case lane.StyleResponses:
		writeJSON(w, http.StatusOK, BuildResponsesObject(turn, usage, finish, reqID))
	default:
		writeJSON(w, http.StatusOK, BuildChatCompletion(turn, usage, finish, reqID))
	}
}

// writeError 把一个错误写出去。
func (g *Gateway) writeError(w http.ResponseWriter, style lane.EncoderStyle, uerr *lane.Error) {
	status := lane.HTTPStatus(uerr.Code)
	if uerr.RetryAfterSec > 0 {
		w.Header().Set("Retry-After", itoa(uerr.RetryAfterSec))
	}
	if style == lane.StyleMessages {
		writeAnthropicError(w, status, lane.ClientErrorTypeOf(uerr.Code), uerr.Message)
		return
	}
	writeOpenAIError(w, status, lane.ClientErrorTypeOf(uerr.Code), uerr.Message)
}

// recordUsage 记一次用量。
func (g *Gateway) recordUsage(turn *lane.Turn, result *lane.TurnResult, uerr *lane.Error) {
	if g.onUsage == nil {
		return
	}
	rec := lane.UsageRecord{
		At:    time.Now().UnixMilli(),
		Day:   time.Now().Format("2006-01-02"),
		Model: turn.Model,
		// ⚠️ 请求的模型也用**全名**（`车道/模型`）——
		// 否则它跟 ActualModel 格式不同，
		// UI 会把"同一个模型"误判成"发生了切换"。
		Client:   turn.ClientID,
		Turns:    1,
		Requests: 1,
	}
	if result != nil {
		rec.Lane = result.Lane
		// ⚠️ `result.Model` **已经是全名**（带车道前缀）—— 别再拼。
		rec.ActualModel = result.Model
		rec.Attempts = result.Attempts
		if result.Usage != nil {
			rec.InputTokens = result.Usage.Input
			rec.OutputTokens = result.Usage.Output
			rec.ReasoningTokens = result.Usage.Reasoning
			// ⚠️ 费用只在 OpenRouter 报 —— 免密车道是 0。
			rec.CostUSD = result.Usage.CostUSD
		}
	}
	if uerr != nil {
		rec.Failed = 1
	}
	g.onUsage(rec)
}

// serveCountTokens 做本地 token 估算。
// ⚠️ **不调用上游** —— 不消耗额度。
// 而且**必须诚实**：标明这是估算。
func (g *Gateway) serveCountTokens(w http.ResponseWriter, r *http.Request) {
	writeCORS(w, r)
	body, err := readBody(r)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	tokens := lane.EstimateTokens(body)
	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens": tokens,
		// ⚠️ 明确标注这是估算 —— 绝不能谎称精确。
		"fmh_estimated": true,
		"fmh_method":    "heuristic-4chars",
	})
}

// readBody 读请求体（有限大小）。
func readBody(r *http.Request) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return map[string]any{}, nil
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, errJSON
	}
	return body, nil
}

// writeJSON 写一个 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// writeOpenAIError 写一个 OpenAI 形状的错误。
func writeOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    errType,
			"param":   nil,
			"code":    nil,
		},
	})
}

// writeAnthropicError 写一个 Anthropic 形状的错误。
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}

// writeCORS 写 CORS 头。
// ⚠️ 推理面允许跨域（浏览器里的 harness 能用）——
// 鉴权靠 Key，不靠来源。
func writeCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "*"
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Api-Key, X-Request-Id")
	w.Header().Set("Access-Control-Expose-Headers", "X-FMH-Model, X-FMH-Lane, X-FMH-Fallback, X-FMH-Request-Id")
	w.Header().Add("Vary", "Origin")
}

// itoa 一个极简的整数转字符串。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
