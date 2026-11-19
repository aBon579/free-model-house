package creditlane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/aBon579/free-model-house/internal/credit"
	"github.com/aBon579/free-model-house/internal/lane"
)

// **两家上游的响应帧形翻译**。
//
// 请求侧已经统一了（都吃 OpenAI chat 形状），响应侧没有：
//
//	Qoder    每帧多一层信封，内层 `body` 才是 OpenAI chunk（**字符串**装的 JSON）
//	TRAE     具名事件（`output`/`token_usage`/`done`/`error`），字段各家一套
//
// 翻译的产物一律是**规范 OpenAI chunk**，再交给内置的 chat 解码器。
// 不是为了省事：块序号、工具调用槽位、思考 token 的四种拼法、
// usage 合并这些规则已经在内置车道上调过（带实测注释），
// 在这里重写一遍等于开第二套会写错的实现。

// stream 发一次上游请求并把响应流读成统一事件。
func stream(ctx context.Context, p credit.Prepared, client *http.Client, emit func(lane.Chunk)) (*lane.Usage, lane.FinishReason, *lane.Error) {
	body, uerr := lane.OpenStream(ctx, client, http.MethodPost, p.URL, p.Headers, p.Body)
	if uerr != nil {
		return nil, lane.FinishError, envelopeFailure(uerr)
	}
	defer body.Close()

	tr, err := translator(p.Wire)
	if err != nil {
		return nil, lane.FinishError, err
	}
	return translateStream(body, emit, tr)
}

// translator 按帧形挑一个翻译器。
//
// ⚠️ **认不出来就报错，不要退回 chat**。
//
//	把未知帧形当标准帧读，最可能的结果是"内容全空但流正常结束"——
//	客户端收到一个没有字的回答，日志里什么都没有。
func translator(wire string) (frameTranslator, *lane.Error) {
	switch wire {
	case "qoder":
		return qoderFrames{}, nil
	case "trae":
		return traeFrames{}, nil
	default:
		return nil, &lane.Error{Code: lane.CodeServer, Message: "credit 上游返回了不认识的响应帧形：" + wire}
	}
}

// frameTranslator 把一帧非标准内容翻译成规范 OpenAI chunk。
type frameTranslator interface {
	// frames 吃一帧，产出 0 个或多个 chunk；返回非 nil 错误则中止整个流。
	frames(event string, data []byte) ([][]byte, *lane.Error)
}

// translateStream 逐帧翻译并喂给内置 chat 解码器。
func translateStream(body io.Reader, emit func(lane.Chunk), tr frameTranslator) (*lane.Usage, lane.FinishReason, *lane.Error) {
	dec, err := lane.NewStreamDecoder("chat", emit)
	if err != nil {
		return nil, lane.FinishError, &lane.Error{Code: lane.CodeServer, Message: err.Error()}
	}
	scanErr := lane.ScanSSE(body, func(event string, data []byte) *lane.Error {
		frames, terr := tr.frames(event, data)
		if terr != nil {
			return terr
		}
		for _, f := range frames {
			if e := dec.Feed(f); e != nil {
				return e
			}
		}
		return nil
	})
	usage, finish, uerr := dec.Result()
	if scanErr != nil {
		return usage, lane.FinishError, scanErr
	}
	if uerr != nil {
		return usage, lane.FinishError, uerr
	}
	return usage, finish, nil
}

// ── Qoder ──────────────────────────────────────────────────────────

// qoderFrames 剥 Qoder 加密端点的响应信封。
//
// 上游每一帧长这样（**内层 body 并没有加密**，要加密的只有请求体）：
//
//	data:{"headers":{…},"body":"{\"choices\":[{\"delta\":{\"content\":\"Q\"}}]}","statusCodeValue":200,"statusCode":"OK"}
type qoderFrames struct{}

func (qoderFrames) frames(_ string, data []byte) ([][]byte, *lane.Error) {
	inner := envelopeBody(data)
	switch classifyInner(inner) {
	case innerChunk:
		return [][]byte{inner}, nil
	case innerHeartbeat:
		// ⚠️ 心跳必须**整帧丢掉**。它的形状是 `body: null`，
		//   用"不含 choices 就算错误"那种字符串嗅探会把它打成业务错误 ——
		//   模型明明正常答完了，客户端却收到一个可重试的 5xx。
		return nil, nil
	default:
		return nil, streamFailure(inner)
	}
}

// envelopeBody 取出信封里的内层帧；不像信封就原样交回（容错）。
func envelopeBody(data []byte) []byte {
	var env struct {
		Body json.RawMessage `json:"body"`
	}
	if json.Unmarshal(data, &env) != nil || len(env.Body) == 0 {
		return data
	}
	var asText string
	if json.Unmarshal(env.Body, &asText) == nil {
		return []byte(asText)
	}
	return env.Body
}

type innerKind int

const (
	innerHeartbeat innerKind = iota
	innerChunk
	innerError
)

// classifyInner 按 **JSON 结构**给内层帧分类。
//
// ⚠️ 三个反直觉的点，每一个都是实测过的坑：
//   - `choices: []` **是**正常帧（`stream_options.include_usage` 的末帧就是这个形状），
//     当错误丢掉就把答案截断了；
//   - `null` 与 `{}`、裸标量**是心跳**，不是错误（`typeof null === "object"` 那个语言坑）；
//   - 解析不了的原文按业务错误处理 —— 上游偶尔回纯文本错误体。
func classifyInner(raw []byte) innerKind {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return innerHeartbeat
	}
	if bytes.Contains(trimmed, []byte("[DONE]")) {
		return innerChunk
	}
	var parsed any
	if json.Unmarshal(trimmed, &parsed) != nil {
		return innerError
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		// null、标量、裸数组 —— 没有任何可消费的内容。
		return innerHeartbeat
	}
	if _, ok := obj["choices"].([]any); ok {
		return innerChunk
	}
	if _, ok := obj["usage"]; ok {
		return innerChunk
	}
	for _, key := range []string{"code", "message", "error", "statusCodeValue", "type"} {
		if _, ok := obj[key]; ok {
			return innerError
		}
	}
	return innerHeartbeat
}

// ── TRAE ───────────────────────────────────────────────────────────

// traeFrames 翻译 TRAE 的具名事件流。
//
// ⚠️ 这一家**没有 `[DONE]`**，收尾靠 `event: done` 里的 finish_reason。
// 事件名丢了就没法翻译（同一个 data 形状会出现在不同事件名下），
// 所以 codec.ScanSSE 特意把 `event:` 一起交了出来。
type traeFrames struct{}

func (traeFrames) frames(event string, data []byte) ([][]byte, *lane.Error) {
	switch strings.TrimSpace(event) {
	case "output":
		var raw struct {
			Response  string            `json:"response"`
			Reasoning string            `json:"reasoning_content"`
			ToolCalls []json.RawMessage `json:"tool_calls"`
		}
		if json.Unmarshal(data, &raw) != nil {
			return nil, nil
		}
		delta := map[string]any{}
		if raw.Response != "" {
			delta["content"] = raw.Response
		}
		if raw.Reasoning != "" {
			delta["reasoning_content"] = raw.Reasoning
		}
		if calls := traeToolCalls(raw.ToolCalls); len(calls) > 0 {
			delta["tool_calls"] = calls
		}
		if len(delta) == 0 {
			return nil, nil
		}
		return marshalFrames(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}})
	case "token_usage":
		// 上游这里给的是**扁平** usage，字段名不统一（三种缓存写法都见过），
		// 归一到内置解码器认得的那几个键上。
		var raw map[string]any
		if json.Unmarshal(data, &raw) != nil {
			return nil, nil
		}
		usage := map[string]any{}
		copyNumber(usage, "prompt_tokens", raw)
		copyNumber(usage, "completion_tokens", raw)
		copyNumber(usage, "total_tokens", raw)
		copyNumber(usage, "reasoning_tokens", raw)
		if cached := cacheReadTokens(raw); cached > 0 {
			usage["cache_read_input_tokens"] = cached
		}
		if len(usage) == 0 {
			return nil, nil
		}
		return marshalFrames(map[string]any{"usage": usage})
	case "done":
		var raw struct {
			FinishReason string `json:"finish_reason"`
		}
		if json.Unmarshal(data, &raw) != nil || raw.FinishReason == "" {
			// 没有 finish_reason 的 done 什么都不带 —— 但流确实结束了，
			// 补一个 stop 收尾，否则解码器会报"上游流未正常结束"。
			return marshalFrames(map[string]any{
				"choices": []any{map[string]any{"index": 0, "finish_reason": "stop"}},
			})
		}
		return marshalFrames(map[string]any{
			"choices": []any{map[string]any{"index": 0, "finish_reason": raw.FinishReason}},
		})
	case "error":
		return nil, streamFailure(data)
	default:
		// metadata / timing_cost / extra_info：与内容无关的旁路信息。
		return nil, nil
	}
}

// traeToolCalls 把 SOLO 形状的工具调用改成规范 chunk 的形状。
//
// ⚠️ 线上字段叫 `function_call`，**不是** `function` ——
//
//	请求侧同样有这条规矩（trae.go 里把 `function` 改名成 `function_call` 再发出去）。
//	`namespace` 与 `partial_arguments` 是 SOLO 自己的东西，内置解码器不认，
//	留着只会让参数拼接多一条分支。
func traeToolCalls(raw []json.RawMessage) []any {
	out := make([]any, 0, len(raw))
	for _, item := range raw {
		var call map[string]any
		if json.Unmarshal(item, &call) != nil || call == nil {
			continue
		}
		norm := map[string]any{}
		for _, key := range []string{"index", "id", "type"} {
			if v, ok := call[key]; ok {
				norm[key] = v
			}
		}
		fn := map[string]any{}
		if src, ok := call["function_call"].(map[string]any); ok {
			fn = src
		} else if src, ok := call["function"].(map[string]any); ok {
			fn = src
		}
		delete(fn, "namespace")
		delete(fn, "partial_arguments")
		if len(fn) > 0 {
			norm["function"] = fn
		}
		if len(norm) == 0 {
			continue
		}
		out = append(out, norm)
	}
	return out
}

// cacheReadTokens 从三种缓存命中写法里取第一个非零值。
//
// ⚠️ 上游三家各用一种：Anthropic 形 `cache_read_input_tokens`、
//
//	OpenAI 形 `prompt_tokens_details.cached_tokens`、TRAE 形 `prompt_cache_hit_tokens`。
//	只读一种的后果是"缓存命中率恒为 0"，账本把每一次都按未命中计价。
func cacheReadTokens(usage map[string]any) int {
	if n := number(usage["cache_read_input_tokens"]); n > 0 {
		return n
	}
	if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
		if n := number(details["cached_tokens"]); n > 0 {
			return n
		}
	}
	return number(usage["prompt_cache_hit_tokens"])
}

func copyNumber(dst map[string]any, key string, src map[string]any) {
	if n := number(src[key]); n > 0 {
		dst[key] = n
	}
}

func number(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return 0
		}
		return int(i)
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(n))
		if err != nil {
			return 0
		}
		return i
	default:
		return 0
	}
}

func marshalFrames(top map[string]any) ([][]byte, *lane.Error) {
	raw, err := json.Marshal(top)
	if err != nil {
		return nil, &lane.Error{Code: lane.CodeServer, Message: "翻译后的帧无法序列化：" + err.Error()}
	}
	return [][]byte{raw}, nil
}

// ── 流内错误 ───────────────────────────────────────────────────────

// 上游在 **HTTP 200 的流里**下判决时用的业务码。
//
// ⚠️ 这些码必须自己认：措辞是中文的，`ClassifyFailure` 那套按英文
//
//	正则匹配的模式认不出来；而且状态码是 200，走状态码分支也认不出来。
const (
	// codeQueued 是 Qoder 的"模型正在排队"。
	codeQueued = "10605"
	// codeBilling 是 Qoder 的"今日额度用尽"。
	codeBilling = "110"
	// codeAuthExpired 是 Qoder 的"登录态失效"。
	codeAuthExpired = "105"

	// traeQuota / traeSoftLimit / traePlan 是 TRAE 的额度、软限流与套餐不符。
	traeQuota     = "4008"
	traeSoftLimit = "4011"
	traePlan      = "1005"
	// traeDeadSession 是会话已失效（要重新登录），traeBadParam 是参数不合法
	// （换账号也没用）。
	traeDeadSession = "401"
	traeBadParam    = "4001"
)

// queueWaitSec 是排队时上游没给等待量时的兜底等待。
//
// ⚠️ 服务端要等 **30 秒**，
//
//	而客户端把它归成"可重试的 5xx"后按 500…8000ms 快退避重试 5 次
//	（总共约 15.5 秒）—— **永远等不到**，只是白烧了整轮预算。
//	所以排队必须带上一个真实的退避秒数。
const queueWaitSec = 30

// queueWaitCapSec 是排队冷却的上限。
//
// ⚠️ 上游偶尔给一个离谱的 retryAfterSeconds（分钟级）。按它把账号关起来，
//
//	用户看到的是"这个模型突然全池不可用"，而实际只要等几秒。
//	这里罚的是**这个账号**，不是模型 —— 上限只限制等待，不改变结论。
const queueWaitCapSec = 300

// streamFailure 把一帧流内错误折成统一错误。
//
// ⚠️ 三条硬规矩，每一条都对应一类真实失败：
//
//	① 排队识别**绝不能拿顶层 `code` 当门** ——
//	   实测帧的顶层 code 是 403，业务码 10605 藏在 `message` 里
//	   **又套了一层 JSON 字符串**。拿 403 比 10605 必然不命中。
//	② 额度用尽是**确定性**结论，不能归进可重试集合（同理会白重试 5 次）。
//	③ `message` 原样保留、**不拼后缀** ——
//	   拼上 "(10605)" 那种后缀会破坏 message 里那段内层 JSON，
//	   二次解析直接失败，退避时间就只能退回兜底值。
func streamFailure(raw []byte) *lane.Error {
	var obj map[string]any
	if json.Unmarshal(bytes.TrimSpace(raw), &obj) != nil || obj == nil {
		// 压根不是 JSON：原文就是上游给的一句话。
		text := strings.TrimSpace(string(raw))
		if text == "" {
			text = "上游在流里返回了错误"
		}
		return lane.ClassifyFailure(0, text, 0)
	}
	return failureFrom(obj, 0)
}

// envelopeFailure 处理上游在 **HTTP 层**（不是流内帧）回来的错误体。
//
// ⚠️ 统一层不认识这种体时，会把整段原文塞进 Message 就交出来 ——
//
//	客户端于是看到 `"message":"{\"code\":4008,\"msg\":\"…\"}"`：
//	一串转义 JSON 人类读不清，业务码也被埋掉了（而码表只认 JSON 字段）。
//	能按 JSON 解析就重走一遍码表，解析不动就原样退回。
//	真正的 HTTP 状态码只有发请求那边知道，所以要当兜底状态传进去。
func envelopeFailure(uerr *lane.Error) *lane.Error {
	if uerr == nil || !strings.HasPrefix(strings.TrimSpace(uerr.Message), "{") {
		return uerr
	}
	var obj map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(uerr.Message)), &obj) != nil || obj == nil {
		return uerr
	}
	return failureFrom(obj, uerr.Status)
}

// failureFrom 是流内帧与 HTTP 错误体共用的判决入口。
//
// fallbackStatus 是 JSON 里没带状态字段时**唯一**可信的状态来源
// （丢了它，一个 503 会被降级成"认不出来"的默认结论）。
func failureFrom(obj map[string]any, fallbackStatus int) *lane.Error {
	text := failureText(obj)
	code := businessCode(obj)
	status := httpStatusOf(obj)
	if status == 0 {
		status = fallbackStatus
	}

	switch {
	case code == codeQueued || queuedFlag(obj):
		return &lane.Error{
			Code: lane.CodeQuota, Status: status, RetryAfterSec: queueRetrySec(obj),
			Message: withCode("模型正在排队，稍后再试", code, obj),
		}
	case code == codeBilling || code == traeQuota || code == traeSoftLimit || looksLikeBilling(text):
		// 额度/计费类：换**账号**是唯一有效的杠杆，所以按限流处理（可切换、会冷却这个账号），
		// 而不是当成"这个模型坏了"。
		return &lane.Error{Code: lane.CodeQuota, Status: status, Message: withCode(text, code, obj)}
	case code == codeAuthExpired || code == traeDeadSession:
		return &lane.Error{Code: lane.CodeCredential, Status: status,
			Message: withCode("登录态已失效，请在 Credit 页重新登录", code, obj)}
	case code == traePlan:
		return &lane.Error{Code: lane.CodePermission, Status: status,
			Message: withCode("账号套餐不包含这个模型", code, obj)}
	case code == traeBadParam:
		return &lane.Error{Code: lane.CodeClient, Status: status, Message: withCode(text, code, obj)}
	}
	// 认不出来的：把 message 交给统一分类器，让它按措辞（上下文超限、
	// 地区受限……）判；判不出来就是上游的 5xx 形状。
	return lane.ClassifyFailure(status, text, 0)
}

// queueRetrySec 取上游给的排队等待量。
//
// ⚠️ 单位**必须分开处理** —— 上游三家（乃至同一家两种帧）各用一种口径：
//
//	`retry_after_ms`/`retryAfterMs` 是毫秒，`retryAfterSeconds` 是秒。
//	把 8000 毫秒当 8000 秒，账号就"在冷却"整整两个多小时 ——
//	而这种错只有在用户抱怨"模型全池不可用"时才会被发现。
//	一个都没有才退回 queueWaitSec（实测值）。
func queueRetrySec(obj map[string]any) int {
	if ms, ok := nestedNumber(obj, "retry_after_ms", "retryAfterMs"); ok && ms > 0 {
		return clampSec((ms + 999) / 1000)
	}
	if secs, ok := nestedNumber(obj, "retryAfterSeconds"); ok && secs > 0 {
		return clampSec(secs)
	}
	return queueWaitSec
}

// clampSec 把等待量收进可用区间。
//
// ⚠️ 上限的存在是因为上游偶尔给分钟级甚至更大的值：这里罚的是**这个账号**，
//
//	照单全收会让用户看到"这个模型突然全池不可用"，而实际只要等几秒。
func clampSec(sec int) int {
	switch {
	case sec < 1:
		return 1
	case sec > queueWaitCapSec:
		return queueWaitCapSec
	default:
		return sec
	}
}

// httpStatusOf 取**真正像 HTTP 状态**的那个数字。
//
// ⚠️ 业务码不能当 HTTP 状态用 —— 这是实测过的自我欺骗：
//
//	Qoder 的 110 是计费墙，被塞成 status=110 之后，
//	`Error.HTTPStatus` 把它写进响应行，浏览器收到一个非法状态码的连接，
//	客户端只会说"连不上"，而不是"额度用完了"。
func httpStatusOf(obj map[string]any) int {
	for _, key := range []string{"statusCodeValue", "status_code", "http_status", "status"} {
		if n, ok := nestedNumber(obj, key); ok && n >= 100 && n <= 599 {
			return n
		}
	}
	return 0
}

// businessCode 递归找业务码。
//
// ⚠️ 要穿过**字符串里再套 JSON** 这一层（Qoder 的错误就是这么发的），
//
//	也要穿过 `error`/`body`/`result` 这些包装字段。深度封一层上限：
//	上游给个自引用结构不该把 CPU 吃掉。
func businessCode(obj map[string]any) string {
	v, ok := nestedValue(obj, "code")
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t)
	case float64:
		return strconv.FormatInt(int64(t), 10)
	case json.Number:
		return t.String()
	}
	return ""
}

// nestedMaxDepth 是穿透包装层数的上限。
//
// ⚠️ 上游给一个自引用/超深的结构不该把 CPU 吃掉。
const nestedMaxDepth = 6

// nestedValue 找业务字段，优先钻进包装层、沿途把"字符串装的 JSON"解开。
//
// ⚠️ 业务码、排队参数**都藏在内层** —— Qoder 的帧是
//
//	{"code":403,"message":"{\"code\":\"10605\",\"message\":\"{\\\"isQueued\\\":…}\"}"}
//	顶层那个 403 只是 HTTP 形状的外壳，**不是判决**。
//	拿顶层 `code` 去比 `'10605'`（顶层其实是 403）必然不命中，于是判决
//	**静默失效**（等待次数统计为 0，错误照旧抛 SERVER）。
//
// ⚠️ 所以要**先钻内层**再认外层，这个顺序不能反：
//
//	外层码留着只作最后兜底（有些帧真的只有外层码）。
//	键的顺序也不能随便：`message`/`error`/`body` 才是装判决的那几个，
//	`choices`、`usage` 这类内容字段钻进去做无谓的解析。
func nestedValue(v any, key string) (any, bool) {
	return nestedValueDepth(v, key, 0)
}

func nestedValueDepth(v any, key string, depth int) (any, bool) {
	// ⚠️ 每一层嵌套都比上一层短（字符串里装 JSON 要转义），所以必然终止；
	//	深度上限只是防上游给一个病态的深结构。
	if depth > nestedMaxDepth {
		return nil, false
	}
	switch t := v.(type) {
	case map[string]any:
		for _, wrap := range nestedWrapKeys {
			if child, ok := t[wrap]; ok {
				if val, found := nestedValueDepth(child, key, depth+1); found {
					return val, true
				}
			}
		}
		if val, ok := t[key]; ok && val != nil {
			return val, true
		}
	case []any:
		for _, item := range t {
			if val, found := nestedValueDepth(item, key, depth+1); found {
				return val, true
			}
		}
	case string:
		if obj := nestedObject(t); obj != nil {
			return nestedValueDepth(obj, key, depth+1)
		}
	}
	return nil, false
}

// nestedWrapKeys 是"判决可能藏在里面"的那几个包装字段。
var nestedWrapKeys = []string{"message", "error", "body", "result", "detail", "data", "response"}

// nestedObject 把一个"字符串装的 JSON 对象"解开；不是就返回 nil。
func nestedObject(s string) map[string]any {
	s = strings.TrimSpace(s)
	if len(s) == 0 || s[0] != '{' {
		return nil
	}
	var out map[string]any
	if json.Unmarshal([]byte(s), &out) != nil {
		return nil
	}
	return out
}

// nestedNumber 从若干候选键里取第一个能读成数字的值。
func nestedNumber(obj map[string]any, keys ...string) (int, bool) {
	for _, key := range keys {
		v, ok := nestedValue(obj, key)
		if !ok {
			continue
		}
		if f, isNum := numeric(v); isNum {
			return int(f), true
		}
	}
	return 0, false
}

// numeric 报告一个值是不是数字（含"数字字符串"）。
func numeric(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// queuedFlag 认排队标志本身，同样要穿透字符串里套的 JSON。
//
// ⚠️ 它是**没有业务码时**的唯一线索，实测有两种形状：
//
//	① `isQueued: true`（信封剥离后顶层可能压根没有 code）；
//	② `type: "model_queued"` —— 既没有 code 也没有 isQueued，只有这两个词。
//	漏判的代价不是"少等一会儿"，是排队被当成 5xx 白烧一整轮退避。
func queuedFlag(v any) bool {
	if on, ok := nestedValue(v, "isQueued"); ok {
		if b, yes := on.(bool); yes && b {
			return true
		}
	}
	for _, key := range []string{"type", "code"} {
		node, ok := nestedValue(v, key)
		if !ok {
			continue
		}
		if s, yes := node.(string); yes {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "queued", "model_queued":
				return true
			}
		}
	}
	return false
}

// failureText 取错误文案：**原样**，不拼任何后缀。
func failureText(obj map[string]any) string {
	for _, key := range []string{"message", "error_message", "detail", "msg"} {
		if s := strings.TrimSpace(textOf(obj[key])); s != "" {
			return s
		}
	}
	if errObj, ok := obj["error"].(map[string]any); ok {
		if s := strings.TrimSpace(textOf(errObj["message"])); s != "" {
			return s
		}
	}
	if s := strings.TrimSpace(textOf(obj["type"])); s != "" {
		return s
	}
	return "上游在流里返回了错误"
}

// textOf 把一个 JSON 值转成人话。
//
// ⚠️ `error` 字段既可能是字符串也可能是对象（OpenAI 与网关两种形状），
//
//	只按其中一种读会得到空串 —— 空文案的错误在界面上就是"不知道为什么失败"。
func textOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		return failureText(t)
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}

// withCode 只在**文案里没有码**时把码附上（给用户看的诊断信息，不参与再解析）。
func withCode(text string, code string, obj map[string]any) string {
	text = strings.TrimSpace(text)
	if text == "" {
		text = "上游在流里返回了错误"
	}
	if code == "" {
		return text
	}
	suffix := code
	if kind := strings.TrimSpace(textOf(obj["type"])); kind != "" {
		suffix += "/" + kind
	}
	if strings.Contains(text, suffix) {
		return text
	}
	return text + " (" + suffix + ")"
}

// looksLikeBilling 按措辞认额度类拒绝。
//
// ⚠️ 它是**兜底**，不是主判据：有些帧压根没有码，只有这句话。
//
//	认错的代价是不重试（保守），漏认的代价是白烧五轮重试。
//
// ⚠️ 关键词必须**窄**（参考实现特意警告过）：`quota`、`exceeded` 这种泛词
//
//	会误伤别的错误 —— 上下文超限的文案里就有 "exceeds"，而它该是 CodeClient
//	（请求自己的错，换账号换模型都没用），被认成额度就是把一次 400
//	变成"整池账号冷却 10 分钟"。
func looksLikeBilling(text string) bool {
	lower := strings.ToLower(text)
	for _, needle := range []string{
		"daily count exceeded", "billing_error", "billing daily",
		"额度不足", "额度用尽", "额度已用尽", "今日额度", "配额不足", "超出额度",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}
