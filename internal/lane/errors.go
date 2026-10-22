// Package lane 与上游免费源对话。
//
// 本文件定义统一错误码 —— 它决定"能不能换模型"、"该冷却谁"、"客户端该做什么"。
//
// 每个错误码有两个正交属性：Kind（出了什么事）与 Scope（该罚谁）。
// 分离之后，"重试吗"、"冷却谁"、"返回什么 HTTP 码" 就是三个独立决定。
//
// 详见 docs/design/00-overview.md §5.4 与 §5.9。
package lane

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Code 是统一错误码。
type Code string

const (
	// CodeFreeTier 上游的免费车道闸门拒绝了整个车道。
	//
	// ⚠️ 必须独立成码：上游的闸门在读 body 之前就决定了，
	// 对所有模型、所有出口一视同仁。归错码会让客户端以为"你的 key 错了"
	// 从而停掉故障转移，而重试只是白白烧预算。
	CodeFreeTier Code = "FREE_TIER_BLOCKED"

	// CodeAbuseBlocked 上游因出口 IP 的信誉拒绝了整个车道。
	//
	// 与 CodeFreeTier 是不同的层：CodeFreeTier 是身份不对（改指纹有用），
	// CodeAbuseBlocked 是出口被封（改指纹没用，换出口有用）。
	// 混淆两者会导致"改了半天的指纹其实是对的"这种误判。
	//
	// ⚠️ 它让整条车道都不可用 —— 单模型切换救不了它。
	CodeAbuseBlocked Code = "EGRESS_BLOCKED"

	// CodeQuota 配额/限流。配额按 session 记账，换模型是唯一有效的杠杆。
	CodeQuota Code = "RATE_LIMIT"

	// CodeRegion 地域封锁。换出口可能有用。
	CodeRegion Code = "REGION_BLOCKED"

	// CodeCredential 上游凭据真的错了。
	CodeCredential Code = "INVALID_CREDENTIAL"

	// CodeAuthRequired 上游要求鉴权但没给。
	CodeAuthRequired Code = "AUTHORIZATION_REQUIRED"

	// CodePermission 上游拒绝了权限。
	CodePermission Code = "PERMISSION_DENIED"

	// CodeClient 请求自身的问题（4xx，除了 408/425）。
	// 重放同样的 body 会重现同样的拒绝 → 不在故障转移集里。
	CodeClient Code = "CLIENT_ERROR"

	// CodeServer 上游 5xx。
	CodeServer Code = "SERVER"

	// CodeTransport 传输层故障（连接被重置、DNS 失败……）。
	// 可切换 —— 传输故障是"出口形状"的，不是"这个模型的判决"。
	CodeTransport Code = "TRANSPORT"

	// CodeReset 连接被重置。
	CodeReset Code = "TRANSPORT_RESET"

	// CodeTimeout 超时。
	CodeTimeout Code = "TIMEOUT"

	// CodeEmpty 上游返回了空。
	CodeEmpty Code = "EMPTY_RESPONSE"

	// CodeAborted 客户端取消了。
	CodeAborted Code = "ABORTED"

	// CodeUnsupportedInput 模型不支持这种输入（比如只吃图片的模型收到纯文本）。
	//
	// ⚠️ **它不是"不可用"** —— 模型本身是好的，只是这次请求的类型它接不了。
	//   所以它既不该让模型从清单里消失，也不该进"不可用"状态。
	//   它是**候选过滤**的事：纯文本请求不该选它。
	//
	//   上游原话（实测 2026-10-20）：
	//   "Model does not support this protocol."
	CodeUnsupportedInput Code = "UNSUPPORTED_INPUT"

	// CodeUnknown 没归上类。
	CodeUnknown Code = "UNKNOWN"
)

// Scope 是"该罚谁"。
type Scope string

const (
	// ScopeRequest 请求自己的错。重放会重现 → 不冷却、不切换。
	ScopeRequest Scope = "request"
	// ScopeAccount 这个凭据的错。冷却这个凭据。
	ScopeAccount Scope = "account"
	// ScopeProvider 这个端点/模型的错。冷却这个模型。
	ScopeProvider Scope = "provider"
	// ScopeTransient 短时抖动。短暂冷却。
	ScopeTransient Scope = "transient"
)

// Error 是统一的上游错误。
type Error struct {
	Code Code
	// Scope 决定冷却谁；空表示由 Code 的默认值推导。
	Scope Scope
	// Message 给人看的说明。**不得**包含任何凭据。
	Message string
	// Status 是上游的 HTTP 状态码（0 表示流内失败）。
	Status int
	// RetryAfterSec 是上游给的退避秒数（0 表示没给）。
	RetryAfterSec int
	// Unavailable 表示"这个组合彻底不行了"（不是"等一等"）。
	Unavailable bool
	// Exhausted 表示"候选链走完了"。
	Exhausted bool
	// MissingAPIKey 表示上游说"缺 API key" ——
	// 也就是 `Bearer public` 不适用于这个模型。
	//
	// ⚠️ 这是"这个模型是不是免费的"的**权威信号**（见 freeverify.go）。
	MissingAPIKey bool
	// Body 是上游的原始错误体（已脱敏），仅用于诊断。
	Body string
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return string(e.Code) + ": " + e.Message
}

// scopeOf 返回错误的 scope；未显式指定时由 Code 推导。
func (e *Error) scopeOf() Scope {
	if e == nil {
		return ScopeTransient
	}
	if e.Scope != "" {
		return e.Scope
	}
	// ⚠️ **"这个模型不免费"是模型级的事** ——
	//   不是凭据级。
	//
	//   免密车道上，`401 Missing API key` 说的是
	//   "`Bearer public` 不适用于这个模型"，
	//   也就是**换模型有用**（别的模型可能还是免费的）。
	//
	//   所以它的 scope 是 Provider —— 冷却这个模型，
	//   而不是冷却整个凭据。
	if e.MissingAPIKey {
		return ScopeProvider
	}
	switch e.Code {
	case CodeClient:
		return ScopeRequest
	case CodeCredential, CodeAuthRequired, CodePermission:
		return ScopeAccount
	case CodeQuota, CodeRegion, CodeFreeTier, CodeAbuseBlocked, CodeServer, CodeEmpty:
		return ScopeProvider
	case CodeTransport, CodeReset, CodeTimeout:
		return ScopeTransient
	default:
		return ScopeTransient
	}
}

// ScopeOf 暴露错误的 scope。
func (e *Error) ScopeOf() Scope { return e.scopeOf() }

// switchableCodes 是**可以换一个候选试试**的错误码集合。
//
// ⚠️ 这是全项目唯一判断"能不能换人"的地方。不要在别处重复判断。
var switchableCodes = map[Code]bool{
	CodeQuota:        true,
	CodeRegion:       true,
	CodeFreeTier:     true,
	CodeAbuseBlocked: true,
	CodeServer:       true,
	CodeTimeout:      true,
	CodeEmpty:        true,
	CodeTransport:    true,
	CodeReset:        true,
	CodeUnknown:      true,
	// ⚠️ "这个模型不吃这种输入" → **换模型有用**（换个纯文本模型就行）。
	CodeUnsupportedInput: true,
}

// Switchable 报告这个错误是否值得换一个候选再试。
//
// 不在集合里的：
//
//	CodeClient        —— 重放同样的 body 会重现同样的拒绝
//	CodeCredential    —— 凭据错了，换模型没用
//	CodeAborted       —— 客户端自己取消的
//
// ⚠️ **一个例外**：`Missing API key`。
//
//	凭据错误通常说明"你配的 key 不对" —— 换模型确实没用。
//	但 `401 Missing API key` 在免密车道上有**另一个含义**：
//	上游在查凭据时发现 `Bearer public` **不适用于这个模型**，
//	也就是**这个模型不是免费的**。
//
//	那换模型**是有用的** —— 车道里别的模型可能还是免费的。
//
//	所以 `MissingAPIKey` 的 scope 是 **Provider**（模型级），
//	不是 Account（凭据级）。这个区分靠 `Error.MissingAPIKey` 标志做。
func (e *Error) Switchable() bool {
	if e == nil {
		return false
	}
	// ⚠️ "这个模型不免费" → 换模型**有用**。
	if e.MissingAPIKey {
		return true
	}
	if e.scopeOf() == ScopeRequest {
		return false
	}
	return switchableCodes[e.Code]
}

// RetrySameModel 报告**重试同一个模型**是否值得。
//
// ⚠️ 这与 Switchable 是**两个不同的问题**。
//
// Switchable = "换一个候选试试"。
// RetrySameModel = "同一个模型再试一次"。
//
// 429 的答案是**否**：配额按 session 记账，重试 = 再烧一次。
// 唯一有效的杠杆是**换模型**。
//
// 但 FREE_TIER 的答案在不同场景下不同：
// 同一个车道内重试毫无意义（闸门在每个出口都一样），
// 但**换模型**是有意义的（免密车道被封 → 换另一条免密车道）。
// 所以它 Switchable 为真，而 RetrySameModel 为假 ——
// 路由器据此只换候选，不在同一模型上原地重试。
func (e *Error) RetrySameModel() bool {
	if e == nil {
		return false
	}
	switch e.Code {
	case CodeQuota:
		// 配额按 session 记账 —— 重试是第二次烧钱。
		return false
	case CodeFreeTier:
		// 闸门对所有出口一视同仁 —— 原地重试只是白烧预算。
		return false
	case CodeAbuseBlocked:
		// 出口 IP 被风控 —— 原地重试更是白烧预算。
		return false
	case CodeRegion:
		// 地域封锁在同一出口上无解。
		return false
	case CodeUnsupportedInput:
		// ⚠️ 重放**同样的** body 还是会撞同一个拒绝 ——
		//   这个模型就是不吃纯文本。
		//   该做的是**换模型**（见 Switchable）。
		return false
	case CodeServer, CodeTimeout, CodeEmpty, CodeTransport, CodeReset, CodeUnknown:
		// 这些是"抖动"形状的 —— 再试一次常常就好了。
		return true
	default:
		return false
	}
}

// 分类

var (
	// freeTierRe 匹配上游闸门的拒绝文档。
	freeTierRe = regexp.MustCompile(`(?i)free.?tier|can only be used from within|free tier can only`)
	// identityRe 匹配"客户端身份不对"的拒绝。
	//
	// ⚠️ 与 abusiveRe **必须分开**。它们指向完全不同的层：
	// identityRe = 指纹不对（改指纹有用）
	// abusiveRe  = 出口 IP 被风控（改指纹没用，换出口有用）
	identityRe = regexp.MustCompile(`(?i)can only be used from within|only be used from within opencode|download it at`)
	// abusiveRe 匹配"出口 IP 被判定为滥用流量"。
	//
	// 上游的原话：
	//   "OpenCode's free models aren't available from your current network
	//    because we've detected abusive traffic from this IP address.
	//    This isn't related to your account. If you're on a VPN or proxy,
	//    turn it off and try again"
	//
	// ⚠️ 这一条极其重要：它是**车道级**的不可用 ——
	// 换模型没用，换出口（或换车道）才有用。
	abusiveRe = regexp.MustCompile(`(?i)detected abusive traffic|aren't available from your current network|abusive traffic from this ip|turn it off and try again`)
	// regionRe 匹配地域封锁。
	regionRe = regexp.MustCompile(`(?i)region|not available in your country|geo.?restrict|country code`)
	// quotaRe 匹配配额/限流。
	quotaRe = regexp.MustCompile(`(?i)rate.?limit|quota|too many requests|usage limit|free usage limit|overloaded|capacity`)
	// credentialRe 匹配凭据问题。
	credentialRe = regexp.MustCompile(`(?i)unauthorized|invalid.?api.?key|incorrect api key|authentication|no credentials|missing credentials|api key not valid`)
	// modelErrRe 匹配"模型不存在/不可用"这类**指名了模型**的拒绝。
	modelErrRe = regexp.MustCompile(`(?i)model .*(not found|not available|not supported|does not exist|unavailable|invalid)|no such model|unknown model`)
	// unsupportedInputRe 匹配"模型不支持这种输入"。
	//
	// ⚠️ 上游原话（实测 2026-10-20）：
	//   "Model does not support this protocol."
	//
	// ⚠️ **它要单独成一类** —— 不能并进"不可用"：
	//   那个模型**本身是好的**（给它一张图就能用），
	//   只是这次请求的类型它接不了。
	//   标成"不可用"会让它从清单里消失，用户再也看不到它。
	unsupportedInputRe = regexp.MustCompile(`(?i)does not support this (protocol|input|modality)|not support this (protocol|input|modality)|unsupported (input|modality|content type)`)
	// contextRe 匹配**上下文窗口超限**。
	//
	// ⚠️ 它是**请求自己的错** —— 换个模型也一样超（除非那个模型窗口更大，
	// 但那属于"候选过滤"的职责，不是"重试"的职责）。
	// 所以它必须是 request scope，不可切换。
	contextRe = regexp.MustCompile(`(?i)context (window|length)|too many tokens|maximum context|prompt is too long|exceeds (the )?context|input length and .max_tokens`)
	// htmlPageRe 匹配前置代理/WAF 的 HTML 页面。
	htmlPageRe = regexp.MustCompile(`(?is)^\s*(<!doctype html|<html|<head|<body)`)
)

// htmlPageHint 是 HTML 错误页的说明。
//
// 措辞要点：点名是**哪一跳**在说话，以及**触发条件**是什么，
// 但**不引用页面内容**（页面可能含内部信息，且对用户没用）。
const htmlPageHint = "the gateway's front proxy answered with an HTML error page — " +
	"usually a WAF or a body-size limit in front of the gateway, not your credentials; " +
	"if this hit a long conversation, its request size is the likely trigger"

// ClassifyFailure 把上游的状态码与错误体映射到统一错误码。
//
// ⚠️ 顺序很重要：**载荷模式先于状态码**。
// 一个反向代理的通用 503 "Service Unavailable" 理由短语不能被读成
// "这个模型不可用"；而一个 HTML 页面是**前置代理在说话**，
// 从来不是凭据存储在说话。
func ClassifyFailure(status int, payload string, retryAfterSec int) *Error {
	payload = strings.TrimSpace(payload)
	e := &Error{
		Message:       truncate(payload, 300),
		Status:        status,
		RetryAfterSec: retryAfterSec,
	}

	switch {
	case abusiveRe.MatchString(payload):
		// ⚠️ 必须**先于** freeTierRe 判断 ——
		// 这条消息里也含 "free models" 字样。
		//
		// 它与身份闸门是**不同的层**：改指纹没用，换出口才有用。
		e.Code = CodeAbuseBlocked
		e.Unavailable = true

	case identityRe.MatchString(payload):
		// 身份闸门 —— 指纹不对。
		e.Code = CodeFreeTier
		e.Unavailable = true

	case freeTierRe.MatchString(payload):
		// 车道级闸门 —— 不是这个调用方的凭据问题。
		e.Code = CodeFreeTier
		e.Unavailable = true

	case regionRe.MatchString(payload):
		e.Code = CodeRegion

	case status == http.StatusTooManyRequests || quotaRe.MatchString(payload):
		e.Code = CodeQuota
		if e.RetryAfterSec == 0 {
			// 没有 Retry-After 是这条车道的常态，
			// 这个默认值就是 429 实际会把模型停多久（见 throttle 包）。
			e.RetryAfterSec = DefaultQuotaCooldownSec
		}

	case isHTMLPage(payload):
		// 前置代理在拒绝，根本没走到模型。
		// 重放同样的 body 只会拿到同样的页面 → 不在可切换集里。
		e.Code = CodeClient
		e.Message = htmlPageHint
		if status > 0 {
			e.Message += " (HTTP " + strconv.Itoa(status) + ")"
		}

	case contextRe.MatchString(payload):
		// ⚠️ 上下文超限是**请求自己的错**。
		// 换个模型也一样超 —— 除非那个模型窗口更大，
		// 但那是"候选过滤"的职责，不是"重试"的职责。
		e.Code = CodeClient

	case credentialRe.MatchString(payload) || status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Code = CodeCredential
		// ⚠️ "Missing API key" 是一个**决定性**的信号：
		// 上游在查凭据时发现 `Bearer public` **不适用于这个模型** ——
		// 也就是这个模型需要用户自己的 key。
		//
		// 这是"这个模型到底免费吗"的权威答案（见 freeverify.go）。
		if missingAPIKeyRe.MatchString(payload) {
			e.MissingAPIKey = true
		}

	case unsupportedInputRe.MatchString(payload):
		// ⚠️ **模型不支持这种输入** —— 不是模型坏了，是这次请求的类型不对。
		//
		// 典型：只吃图片的模型（vision-only）收到纯文本。
		//
		// 它是**候选过滤**的事 —— 纯文本请求不该选它。
		// 但它**不是**"不可用"：模型本身是好的。
		//
		// ⚠️ scope 是 **Provider** 不是 Request ——
		//   换模型**有用**（换个纯文本模型就能过），
		//   但不能重放**同样的** body（还是会撞同一个拒绝）。
		e.Code = CodeUnsupportedInput
		e.Scope = ScopeProvider

	case status >= 500:
		e.Code = CodeServer

	case status == http.StatusNotFound || status == http.StatusBadRequest || status == http.StatusUnprocessableEntity || modelErrRe.MatchString(payload):
		e.Code = CodeServer
		e.Unavailable = true

	case status >= 400 && status != http.StatusRequestTimeout && status != http.StatusTooEarly:
		// 4xx 是请求自己的错：重放同样的 body 会重现同样的拒绝。
		// 408 和 425 是例外 —— 它们说的是网关自己的时序问题，重发可能不同。
		e.Code = CodeClient

	default:
		e.Code = CodeServer
	}
	return e
}

// isHTMLPage 判断一个响应体是不是前置代理的 HTML 页面。
func isHTMLPage(payload string) bool {
	return htmlPageRe.MatchString(payload)
}

// truncate 截断一段文本。
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// 上游错误体的解析

// upstreamErrorBody 是几种常见错误信封的并集。
type upstreamErrorBody struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
	Detail  string `json:"detail"`
}

// extractErrorPayload 从一个上游错误体里抽出最有信息量的那段文本。
//
// 上游的错误信封有好几种：OpenAI 的 `{"error":{"message":...}}`、
// Anthropic 的 `{"type":"error","error":{"message":...}}`、
// 还有直接把消息放在顶层的。分类要看**全部**这些位置 ——
// 只看一个会把 "rate limit" 读成别的。
func extractErrorPayload(body []byte) string {
	var doc upstreamErrorBody
	if err := json.Unmarshal(body, &doc); err != nil {
		return string(body)
	}
	candidates := []string{
		doc.Error.Message,
		doc.Message,
		doc.Detail,
		doc.Error.Type,
		doc.Error.Code,
		doc.Type,
	}
	for _, c := range candidates {
		if strings.TrimSpace(c) != "" {
			return c
		}
	}
	return string(body)
}

// 客户端可见的 HTTP 映射

// HTTPStatus 把统一错误码映射成对客户端的 HTTP 状态码。
//
// ⚠️ 上游凭据的问题**不**映射成 401 —— 那会让客户端以为"我的 key 错了"。
// 上游的 key 是 FMH 的事，客户端的 401 必须是"你的 FMH key 错了"。
func HTTPStatus(code Code) int {
	switch code {
	case CodeClient:
		return http.StatusBadRequest
	case CodeQuota:
		return http.StatusTooManyRequests
	case CodeTimeout:
		return http.StatusGatewayTimeout
	case CodeFreeTier, CodeRegion, CodeAbuseBlocked, CodeCredential, CodeAuthRequired, CodePermission,
		CodeServer, CodeTransport, CodeReset, CodeEmpty:
		return http.StatusBadGateway
	case CodeAborted:
		return 499 // nginx 的"客户端关闭连接"
	default:
		return http.StatusBadGateway
	}
}

// ClientErrorTypeOf 把统一错误码映射成 OpenAI/Anthropic 风格的 error type。
func ClientErrorTypeOf(code Code) string {
	switch code {
	case CodeQuota:
		return "rate_limit_error"
	case CodeClient:
		return "invalid_request_error"
	case CodeCredential, CodeAuthRequired, CodePermission:
		return "authentication_error"
	case CodeTimeout:
		return "timeout_error"
	default:
		return "api_error"
	}
}
