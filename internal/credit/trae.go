package credit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	traeAgent        = "https://trae-api-cn.mchost.guru"
	traeOAuth        = "https://api.trae.com.cn"
	traeUG           = "https://api.trae.cn"
	traeCallback     = "http://127.0.0.1:18080/authorize"
	traeVersion      = "0.1.52"
	traeVersionCode  = "20260811"
	traeOAuthClient  = "en1oxy7wnw8j9n"
	traeExchangePath = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	traeUserPath     = "/cloudide/api/v3/trae/GetUserInfo"
	traeModelsPath   = "/api/ide/v1/batch_get_detail_param"
	traeChatPath     = "/api/agent/v3/llm_utils_chat"
	traeUsagePath    = "/trae/api/v2/pay/ide_user_ent_usage"
	traeStatusPath   = "/trae/api/v2/ug/checkin_credits/status"
	traeClaimPath    = "/trae/api/v2/ug/checkin_credits/claim"
	traeCheckinID    = "daily-checkin"
	traeExpiryLead   = 60 * time.Second
)

// These are protocol function names, not a static model availability list.
var traeFunctions = []string{
	"ui_builder_v2", "solo_coder", "chat_v3", "solo_builder", "builder_v3", "builder", "chat", "inline_chat", "git_ai",
	"custom_agent_generation", "utils", "code_reviewer", "code_review_summary", "solo_agent", "solo_agent_remote",
	"solo_work_remote", "solo_agent_lite", "solo_work_lite", "solo_design_lite", "solo_design_remote", "multimodal", "system_diagnosis",
}
var traeChatFunctions = []string{
	"solo_agent", "solo_work_lite", "solo_agent_remote", "solo_work_remote", "solo_agent_lite", "chat_v3", "builder_v3",
	"solo_coder", "solo_design_lite", "solo_design_remote", "git_ai", "code_reviewer", "code_review_summary", "multimodal", "system_diagnosis",
}

type traeModelLimits struct{ context, output int }
type traeAdapter struct {
	client *http.Client
	mu     sync.RWMutex
	// Max limits are deliberately separate from Model's normal-mode limits.
	maxLimits map[string]map[string]traeModelLimits
}

// NewTrae uses only official endpoints. The caller owns persistence of rotated
// credentials; read-only operations never silently rotate a refresh token.
func NewTrae(client *http.Client) Adapter {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	// Do not forward credentials to a redirect destination (including another host).
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &traeAdapter{client: &copyClient, maxLimits: make(map[string]map[string]traeModelLimits)}
}

func traeRandom(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("TRAE 无法生成安全随机标识")
	}
	return hex.EncodeToString(b), nil
}

// traeTraceID 派生一次登录的关联 id：拼接串的后 16 字符，不足 16 时左补零。
func traeTraceID(machineID, deviceID string) string {
	joined := machineID + deviceID
	if len(joined) < 16 {
		return strings.Repeat("0", 16-len(joined)) + joined
	}
	return joined[len(joined)-16:]
}

func (a *traeAdapter) StartLogin(ctx context.Context, method string) (*Login, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if method != "" && method != "default" && method != "browser" {
		return nil, errors.New("TRAE 仅支持浏览器登录并手动粘贴回调 URL")
	}
	data := Credential{}
	for _, key := range []string{"machineId", "deviceId"} {
		v, err := traeRandom(16)
		if err != nil {
			return nil, err
		}
		data[key] = v
	}
	// ⚠️ client_id 是**产品**标识，不是每次登录现编的随机值：授权页第一屏就校验它，
	//	发随机串过去只会得到「登录失败」，用户连授权的机会都没有。
	data["clientId"] = traeOAuthClient
	// trace 由上面两个设备号派生 —— 它是回调反查本次会话的唯一线索
	//（TRAE 不保证把 machine_id/device_id 原样回传，但会回传 login_trace_id）。
	data["login_trace_id"] = traeTraceID(data.Text("machineId"), data.Text("deviceId"))
	data["callbackURL"] = traeCallback
	data["deviceBrand"] = "PC"
	data["deviceType"] = "PC"
	data["osVersion"] = "1.0"
	data["appVersion"] = traeVersion
	data["appType"] = "stable"
	data["pluginVersion"] = "2.3.62834"
	q := url.Values{
		"login_version": {"1"}, "auth_from": {"solo"}, "login_channel": {"native_ide"}, "plugin_version": {"2.3.62834"},
		"auth_type": {"local"}, "redirect": {"0"}, "client_id": {data.Text("clientId")}, "login_trace_id": {data.Text("login_trace_id")},
		"auth_callback_url": {traeCallback}, "machine_id": {data.Text("machineId")}, "device_id": {data.Text("deviceId")},
		"x_machine_id": {data.Text("machineId")}, "x_device_id": {data.Text("deviceId")}, "x_device_brand": {"PC"},
		"x_device_type": {"PC"}, "x_os_version": {"1.0"}, "x_app_version": {traeVersion}, "x_app_type": {"stable"},
	}
	return &Login{ID: data.Text("login_trace_id"), Provider: "trae", URL: "https://www.trae.cn/authorization?" + q.Encode(),
		State: "pending", ExpiresAt: time.Now().Add(10 * time.Minute).UnixMilli(), Interval: 2, Data: data,
		Message: "在浏览器授权后，将地址栏中的 http://127.0.0.1:18080/authorize 回调完整粘贴回来；无需本地监听，页面无法访问不影响登录。"}, nil
}

func traeCallbackObject(raw string) (map[string]any, error) {
	if raw == "" {
		return map[string]any{}, nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		var m map[string]any
		d := json.NewDecoder(strings.NewReader(raw))
		d.UseNumber()
		if json.Valid([]byte(raw)) && d.Decode(&m) == nil && m != nil {
			return m, nil
		}
		decoded, err := url.PathUnescape(raw)
		if err != nil || decoded == raw {
			break
		}
		raw = decoded
	}
	return nil, errors.New("TRAE 回调 JSON 参数格式无效")
}

func (a *traeAdapter) PollLogin(ctx context.Context, login *Login, callback string) (Credential, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if login == nil || login.Provider != "trae" || login.Data.Text("machineId") == "" || login.Data.Text("deviceId") == "" {
		return nil, false, errors.New("TRAE 登录会话无效，请重新开始")
	}
	if login.ExpiresAt <= time.Now().UnixMilli() {
		return nil, false, errors.New("TRAE 登录会话已过期")
	}
	if strings.TrimSpace(callback) == "" {
		return nil, false, nil
	}
	if len(callback) > 128*1024 {
		return nil, false, errors.New("TRAE 回调 URL 过长")
	}
	u, err := url.Parse(strings.TrimSpace(callback))
	if err != nil || u.Scheme != "http" || (u.Host != "127.0.0.1:18080" && u.Host != "localhost:18080") || u.Path != "/authorize" || u.User != nil || u.Fragment != "" {
		return nil, false, errors.New("TRAE 回调地址必须是本地 18080 端口的 /authorize URL")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, false, errors.New("TRAE 回调 URL 参数编码无效")
	}
	for _, key := range []string{"login_trace_id", "client_id", "refreshToken", "userInfo", "userJwt"} {
		if len(q[key]) > 1 {
			return nil, false, errors.New("TRAE 回调包含重复参数")
		}
	}
	if trace := q.Get("login_trace_id"); trace != "" && trace != login.Data.Text("login_trace_id") {
		return nil, false, errors.New("TRAE 回调不属于本次登录")
	}
	if client := q.Get("client_id"); client != "" && client != login.Data.Text("clientId") {
		return nil, false, errors.New("TRAE 回调客户端与本次登录不匹配")
	}
	info, err := traeCallbackObject(q.Get("userInfo"))
	if err != nil {
		return nil, false, err
	}
	jwt, err := traeCallbackObject(q.Get("userJwt"))
	if err != nil {
		return nil, false, err
	}
	c := login.Data.Clone()
	c["uid"] = traeString(info, "UserID", "userId", "uid")
	c["nickname"] = traeString(info, "ScreenName", "screenName")
	c["enterpriseId"] = traeString(info, "TenantID", "EnterpriseID")
	c["refreshToken"] = strings.TrimSpace(q.Get("refreshToken"))
	if c.Text("refreshToken") == "" {
		c["refreshToken"] = traeString(jwt, "RefreshToken", "refreshToken")
	}
	c["accessToken"] = traeString(jwt, "Token", "token", "accessToken")
	if c.Text("refreshToken", "accessToken") == "" {
		if q.Get("code") != "" || q.Get("authCode") != "" || q.Get("authCodeInfo") != "" {
			return nil, false, errors.New("TRAE 返回 PKCE 授权码；当前只支持 local token 回调，请重新开始浏览器登录")
		}
		return nil, false, errors.New("TRAE 回调缺少 refreshToken 或 userJwt.Token")
	}
	if c.Text("refreshToken") != "" {
		c, err = a.exchange(ctx, c)
		if err != nil {
			return nil, false, err
		}
	} else {
		c["expiresAt"] = traeTokenExpiry(jwt, c.Text("accessToken"))
	}
	remote, fetchErr := a.post(ctx, traeOAuth+traeUserPath, traeOAuthHeaders(c.Text("accessToken")), map[string]any{"ReqSource": "IDE", "IDEVersion": traeVersion})
	if fetchErr == nil {
		r := traePayload(remote)
		uid := traeString(r, "UserID", "userId", "uid")
		if uid != "" && c.Text("uid") != "" && uid != c.Text("uid") {
			return nil, false, errors.New("TRAE 用户身份与回调不一致")
		}
		for dest, keys := range map[string][]string{"uid": {"UserID", "userId", "uid"}, "nickname": {"ScreenName", "screenName"},
			"enterpriseId": {"EnterpriseID", "enterpriseId"}, "phone": {"NonPlainTextMobile", "nonPlainTextMobile"}, "email": {"NonPlainTextEmail", "nonPlainTextEmail"}} {
			if value := traeString(r, keys...); value != "" {
				c[dest] = value
			}
		}
	}
	if c.Text("uid") == "" {
		return nil, false, errors.New("TRAE 无法从回调或 GetUserInfo 确定用户身份")
	}
	if err := a.Validate(c); err != nil {
		return c, false, err
	}
	if fetchErr != nil {
		c["warning"] = "GetUserInfo 查询失败，已保留回调用户信息"
	}
	return c, true, nil
}

func traeCredentialShape(c Credential) error {
	if c.Text("accessToken", "refreshToken") == "" {
		return errors.New("TRAE 缺少访问或刷新凭据")
	}
	for _, key := range []string{"uid", "machineId", "deviceId"} {
		if c.Text(key) == "" {
			return errors.New("TRAE 凭据缺少 uid、machineId 或 deviceId，请重新登录")
		}
	}
	for _, key := range []string{"accessToken", "refreshToken", "uid", "machineId", "deviceId", "clientId"} {
		if s, ok := c[key].(string); ok && strings.ContainsAny(s, "\r\n\x00") {
			return errors.New("TRAE 凭据字段包含无效字符")
		}
	}
	return nil
}

func (a *traeAdapter) Validate(c Credential) error {
	if err := traeCredentialShape(c); err != nil {
		return err
	}
	if c.Text("accessToken") == "" {
		return &APIError{Status: 401, Code: "token_expired", Message: "TRAE 需要先刷新访问凭据"}
	}
	if expiry := traeExpiry(c); expiry > 0 && expiry <= time.Now().Add(traeExpiryLead).UnixMilli() {
		return &APIError{Status: 401, Code: "token_expired", Message: "TRAE 访问凭据已过期或即将过期，请先刷新"}
	}
	return nil
}

// Refresh is explicit: even a token with a future exp may have been revoked early.
// Never modify the caller's map or replace stable device/account identities.
func (a *traeAdapter) Refresh(ctx context.Context, c Credential) (Credential, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := traeCredentialShape(c); err != nil {
		return nil, err
	}
	if c.Text("refreshToken") == "" {
		if err := a.Validate(c); err != nil {
			return nil, err
		}
		return c.Clone(), nil
	}
	if end := traeTime(c["refreshExpiresAt"]); end > 0 && end <= time.Now().UnixMilli() {
		return nil, errors.New("TRAE 刷新凭据已过期，请重新登录")
	}
	return a.exchange(ctx, c)
}

func (a *traeAdapter) exchange(ctx context.Context, c Credential) (Credential, error) {
	clientID := c.Text("clientId")
	if clientID == "" {
		clientID = traeOAuthClient
	}
	raw, err := a.post(ctx, traeOAuth+traeExchangePath, traeOAuthHeaders(""), map[string]any{
		"ClientID": clientID, "RefreshToken": c.Text("refreshToken"), "ClientSecret": "-", "UserID": "",
	})
	if err != nil {
		return nil, err
	}
	r := traePayload(raw)
	token := traeString(r, "Token", "token", "accessToken")
	if token == "" {
		return nil, errors.New("TRAE ExchangeToken 未返回访问凭据，请重新登录")
	}
	next := c.Clone()
	next["accessToken"] = token
	// An omitted refresh field means no rotation; do not erase the usable token.
	if refresh := traeString(r, "RefreshToken", "refreshToken"); refresh != "" {
		next["refreshToken"] = refresh
	}
	next["expiresAt"] = traeTokenExpiry(r, token)
	if expiry := traeTime(traeValue(r, "RefreshExpireAt", "refreshExpireAt")); expiry > 0 {
		next["refreshExpiresAt"] = expiry
	}
	return next, nil
}

func traeTokenExpiry(r map[string]any, token string) int64 {
	expiry := traeTime(traeValue(r, "TokenExpireAt", "tokenExpireAt"))
	if expiry == 0 {
		if n := traeNumber(traeValue(r, "TokenExpireDuration", "tokenExpireDuration")); n != nil && *n > 0 && *n < 1e10 {
			expiry = time.Now().UnixMilli() + int64(*n*1000)
		}
	}
	return traeEarlier(expiry, traeJWTExpiry(token))
}
func traeExpiry(c Credential) int64 {
	return traeEarlier(traeTime(c["expiresAt"]), traeJWTExpiry(c.Text("accessToken")))
}
func traeEarlier(a, b int64) int64 {
	if a == 0 || (b > 0 && b < a) {
		return b
	}
	return a
}
func traeJWTExpiry(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) > 64*1024 {
		return 0
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return 0
	}
	var claims map[string]any
	if json.Unmarshal(b, &claims) != nil {
		return 0
	}
	return traeTime(claims["exp"])
}

func traeOAuthHeaders(token string) map[string]string {
	h := map[string]string{"Content-Type": "application/json", "Accept": "application/json", "User-Agent": "Trae/" + traeVersion}
	if token != "" {
		h["X-Cloudide-Token"] = token
	}
	return h
}
func traeAgentHeaders(c Credential, stream bool) map[string]string {
	h := traeOAuthHeaders(c.Text("accessToken"))
	for k, v := range map[string]string{
		"Authorization": "Cloud-IDE-JWT " + c.Text("accessToken"), "X-Ide-Token": c.Text("accessToken"), "X-Uid": c.Text("uid"),
		"X-App-Id": "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8", "X-App-Version": "default", "X-Ide-Version": traeVersion,
		"X-Ide-Version-Code": traeVersionCode, "X-App-Version-Code": traeVersionCode, "X-Ide-Version-Type": "stable",
		"X-Device-Type": "macos", "X-OS-Version": "macOS 15.7.4", "X-Device-Brand": "Apple", "Request-Traffic-Type": "prod",
		"X-Machine-Id": c.Text("machineId"), "X-Device-Id": c.Text("deviceId"),
	} {
		h[k] = v
	}
	if stream {
		h["Accept"] = "text/event-stream"
	}
	return h
}

func traeUUID(hexID string) string {
	return hexID[:8] + "-" + hexID[8:12] + "-" + hexID[12:16] + "-" + hexID[16:20] + "-" + hexID[20:32]
}
func traeUGHeaders(c Credential) (map[string]string, error) {
	request, err := traeRandom(16)
	if err != nil {
		return nil, err
	}
	trace, err := traeRandom(8)
	if err != nil {
		return nil, err
	}
	request = request[:12] + "4" + request[13:16] + "8" + request[17:]
	// Domain-separated hashes provide stable, account-specific client metadata.
	identity := c.Text("uid")
	device := sha256.Sum256([]byte("trae.checkin.device:" + identity))
	market := sha256.Sum256([]byte("trae.checkin.market:" + identity))
	session := sha256.Sum256([]byte("trae.checkin.session:" + identity))
	digits := make([]byte, 15)
	for i := range digits {
		digits[i] = '0' + device[i]%10
	}
	market[6] = market[6]&15 | 64
	market[8] = market[8]&63 | 128
	// Leave Accept-Encoding to net/http so gzip is transparently decompressed.
	return map[string]string{
		"Content-Type": "application/json", "Accept": "*/*", "Accept-Language": "zh-CN", "User-Agent": "VSCode 1.107.1 (TRAE SOLO CN)",
		"Authorization": "Cloud-IDE-JWT " + c.Text("accessToken"), "X-Market-Client-Id": "VSCode 1.107.1",
		"X-Market-User-Id": traeUUID(hex.EncodeToString(market[:16])), "X-User-Region": "CN", "X-Device-Id": string(digits),
		"X-Lgw-Req-Sdk-Type": "3", "Package-Type": "stable_cn", "X-Lscbd-Aid": "787976", "X-Lscbd-Platform": "windows",
		"App-Version": traeVersion, "X-Tt-Trace-Id": "00-" + trace + "-01", "Vscode-Sessionid": hex.EncodeToString(session[:]),
		"X-Request-Id": traeUUID(request), "Sec-Fetch-Dest": "empty", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Site": "none",
	}, nil
}

func (a *traeAdapter) post(ctx context.Context, endpoint string, headers map[string]string, body any) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var raw json.RawMessage
	if err := JSONRequest(ctx, a.client, http.MethodPost, endpoint, headers, body, &raw); err != nil {
		return nil, err
	}
	var result map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&result) != nil || result == nil {
		return nil, errors.New("TRAE 返回无效对象响应")
	}
	if err := traeBusinessError(result); err != nil {
		return nil, err
	}
	return result, nil
}

// Only known wrappers are inspected. Error bodies/messages may contain secrets;
// never surface them, even when a business failure arrived with HTTP 200.
func traeBusinessError(m map[string]any) error {
	for _, key := range []string{"code", "Code", "status_code", "StatusCode"} {
		if value, exists := m[key]; exists {
			n := traeNumber(value)
			if n == nil || *n != 0 {
				code := "upstream_error"
				message := "TRAE 上游业务请求失败"
				if n != nil && *n == math.Trunc(*n) && math.Abs(*n) < 1e9 {
					code = strconv.FormatInt(int64(*n), 10)
					message += "（code=" + code + "）"
				}
				return &APIError{Code: code, Message: message}
			}
		}
	}
	if success, ok := m["success"].(bool); ok && !success {
		return errors.New("TRAE 上游未确认请求成功")
	}
	for _, key := range []string{"ResponseMetadata", "Error", "error", "BaseResp", "base_resp", "Result", "result", "data"} {
		if child, ok := m[key].(map[string]any); ok && len(child) > 0 {
			if err := traeBusinessError(child); err != nil {
				return err
			}
			if key == "Error" || key == "error" {
				return errors.New("TRAE 上游返回业务错误")
			}
		}
	}
	return nil
}
func traePayload(m map[string]any) map[string]any {
	for i := 0; i < 4; i++ {
		var next map[string]any
		for _, key := range []string{"Result", "result", "data"} {
			if obj, ok := m[key].(map[string]any); ok {
				next = obj
				break
			}
		}
		if next == nil {
			return m
		}
		m = next
	}
	return m
}
func traeValue(m map[string]any, keys ...string) any {
	for _, key := range keys {
		if v, ok := m[key]; ok && v != nil {
			return v
		}
	}
	return nil
}
func traeString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		switch v := m[key].(type) {
		case string:
			if s := strings.TrimSpace(v); s != "" {
				return s
			}
		case json.Number:
			return string(v)
		case float64:
			if !math.IsInf(v, 0) && !math.IsNaN(v) {
				return strconv.FormatFloat(v, 'f', -1, 64)
			}
		}
	}
	return ""
}
func traeNumber(v any) *float64 {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case json.Number:
		var err error
		f, err = n.Float64()
		if err != nil {
			return nil
		}
	case string:
		var err error
		f, err = strconv.ParseFloat(strings.TrimSpace(n), 64)
		if err != nil {
			return nil
		}
	default:
		return nil
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}
func traeTime(v any) int64 {
	if n := traeNumber(v); n != nil && *n > 0 && *n < 1e16 {
		if *n < 1e11 {
			return int64(*n * 1000)
		}
		return int64(*n)
	}
	return Timestamp(v)
}
func traeInt(v any) int {
	if n := traeNumber(v); n != nil && *n > 0 && *n < 1e9 {
		return int(*n)
	}
	return 0
}
func traeBool(v any) (bool, bool) {
	if b, ok := v.(bool); ok {
		return b, true
	}
	if s, ok := v.(string); ok {
		if s == "true" || s == "1" {
			return true, true
		}
		if s == "false" || s == "0" {
			return false, true
		}
	}
	if n := traeNumber(v); n != nil {
		if *n == 0 {
			return false, true
		}
		if *n == 1 {
			return true, true
		}
	}
	return false, false
}
func traeIsTrue(v any) bool  { b, ok := traeBool(v); return ok && b }
func traeIsFalse(v any) bool { b, ok := traeBool(v); return ok && !b }
func traeRank(function string) int {
	for i, f := range traeChatFunctions {
		if f == function {
			return i
		}
	}
	return -1
}

func (a *traeAdapter) Models(ctx context.Context, c Credential) ([]Model, error) {
	if err := a.Validate(c); err != nil {
		return nil, err
	}
	raw, err := a.post(ctx, traeAgent+traeModelsPath, traeAgentHeaders(c, false), map[string]any{
		"functions": traeFunctions, "agent_type": "", "current_config_info": map[string]any{"config_name": "", "is_custom_model": false},
		"mode_type": 0, "access_type": 0, "ab_force_vids": "", "ab_autotest_advanced_mode": 0, "show_custom_model": true,
	})
	if err != nil {
		return nil, err
	}
	groups, ok := traeValue(traePayload(raw), "function_configs", "FunctionConfigs").([]any)
	if !ok {
		return nil, errors.New("TRAE 动态模型目录格式无效；未启用静态兜底模型")
	}
	selected := make(map[string]Model)
	limits := make(map[string]traeModelLimits)
	for _, item := range groups {
		group := Object(item)
		function := traeString(group, "function", "Function")
		if traeRank(function) < 0 {
			continue
		}
		for _, config := range Array(traeValue(group, "config_info_list", "ConfigInfoList")) {
			m, max, valid := traeModel(Object(config), function)
			if !valid {
				continue
			}
			old, exists := selected[m.ID]
			if exists && (len(old.Efforts) > 0 && len(m.Efforts) == 0 || (len(old.Efforts) > 0) == (len(m.Efforts) > 0) && traeRank(old.Function) <= traeRank(m.Function)) {
				continue
			}
			selected[m.ID] = m
			limits[m.ID+"\x00"+m.Function] = max
		}
	}
	out := make([]Model, 0, len(selected))
	for _, m := range selected {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	a.mu.Lock()
	a.maxLimits[c.Text("uid")] = limits
	a.mu.Unlock()
	return out, nil
}

func traeModel(e map[string]any, function string) (Model, traeModelLimits, bool) {
	m := Model{ID: traeString(e, "config_name", "ConfigName"), Function: function}
	display := Object(traeValue(e, "display_config", "DisplayConfig"))
	usage := traeString(e, "usage", "Usage")
	if m.ID == "" || usage != "" && usage != "chat_completion" || traeIsFalse(traeValue(e, "config_switch", "ConfigSwitch")) ||
		traeIsTrue(traeValue(e, "is_invisible_to_user", "IsInvisibleToUser")) || traeIsTrue(traeValue(display, "is_custom_model", "IsCustomModel")) || traeIsTrue(e["is_custom_model"]) {
		return m, traeModelLimits{}, false
	}
	m.Name = traeString(display, "display_name", "DisplayName")
	if m.Name == "" {
		m.Name = m.ID
	}
	m.Vision = traeIsTrue(traeValue(display, "multimodal", "Multimodal"))
	m.MaxMode = traeIsTrue(traeValue(display, "max_mode", "MaxMode"))
	windows := Object(traeValue(e, "context_window_tokens", "ContextWindowTokens"))
	m.ContextWindow = traeInt(traeValue(windows, "dev", "Dev"))
	max := traeModelLimits{context: traeInt(traeValue(windows, "max", "Max"))}
	for _, item := range Array(traeValue(e, "model_detail_list", "ModelDetailList")) {
		d := Object(item)
		name := traeString(d, "model_name", "ModelName")
		tokens := traeInt(traeValue(d, "max_tokens", "MaxTokens"))
		if strings.HasSuffix(name, "__max") {
			max.output = tokens
		} else if strings.HasSuffix(name, "__dev") || m.MaxOutput == 0 {
			m.MaxOutput = tokens
		}
	}
	r := Object(traeValue(e, "reasoning_effort_config", "ReasoningEffortConfig"))
	if !traeIsFalse(traeValue(r, "support_thinking", "SupportThinking")) {
		for _, v := range Array(traeValue(r, "options", "Options")) {
			s := strings.TrimSpace(Text(v))
			if s == "" {
				s = traeString(Object(v), "openclawLevel", "level", "Level")
			}
			if s != "" && !traeContains(m.Efforts, s) {
				m.Efforts = append(m.Efforts, s)
			}
		}
		m.Reasoning = traeIsTrue(traeValue(r, "support_thinking", "SupportThinking")) || len(m.Efforts) > 0
	}
	contact := traeValue(e, "display_contact_config", "DisplayContactConfig")
	if s, ok := contact.(string); ok {
		var object map[string]any
		if json.Unmarshal([]byte(s), &object) == nil {
			contact = object
		}
	}
	rate := Object(traeValue(Object(contact), "consumption_rate", "ConsumptionRate"))
	if !traeIsFalse(rate["enable"]) {
		if n := traeNumber(Object(traeValue(rate, "data", "Data"))["rate"]); n != nil && *n >= 0 {
			m.Multiplier = n
		}
	}
	return m, max, true
}
func traeContains(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}

func (a *traeAdapter) ug(ctx context.Context, c Credential, path string, body any) (map[string]any, error) {
	h, err := traeUGHeaders(c)
	if err != nil {
		return nil, err
	}
	return a.post(ctx, traeUG+path, h, body)
}
func (a *traeAdapter) checkin(ctx context.Context, c Credential) (Campaign, error) {
	campaign := Campaign{ID: traeCheckinID, Name: "每日签到"}
	r, err := a.ug(ctx, c, traeStatusPath, map[string]any{})
	if err != nil {
		return campaign, err
	}
	p := traePayload(r)
	enabled, hasEnabled := traeBool(p["enable"])
	checked, hasChecked := traeBool(p["checked_in"])
	if !hasEnabled || !hasChecked {
		return campaign, errors.New("TRAE 签到状态字段缺失")
	}
	campaign.Claimable = enabled && !checked
	campaign.Claimed = checked
	campaign.Amount = traeNumber(p["credits"])
	campaign.ExpiresAt = traeTime(p["end_time"])
	return campaign, nil
}

func (a *traeAdapter) Credits(ctx context.Context, c Credential) (Credits, error) {
	out := Credits{Balances: []Balance{}, Campaigns: []Campaign{}, UpdatedAt: time.Now().UnixMilli()}
	if err := a.Validate(c); err != nil {
		return out, err
	}
	warnings := []string{}
	r, balanceErr := a.ug(ctx, c, traeUsagePath, map[string]any{"require_usage": true, "req_source": 2})
	if balanceErr == nil {
		packs, ok := traePayload(r)["user_entitlement_pack_list"].([]any)
		if !ok {
			balanceErr = errors.New("TRAE 额度包字段缺失")
		} else {
			// 一句警告按包数计数，不逐包 append：十几个包会把这条 Notice 撑成一堵
			// 重复文字的墙，真正该看的"有几个包算不出来"反而被埋掉。
			unknownTotal := 0
			for _, item := range packs {
				e, ok := item.(map[string]any)
				if !ok {
					warnings = append(warnings, "部分额度包格式无效")
					continue
				}
				base := Object(e["entitlement_base_info"])
				// ⚠️ `usage` 缺席是"这个包还没被消耗"，不是"用量未知"。依据是官方余额
				//    读数能对上：账号显示 1,054 = 10 个无 usage 的签到包 ×100
				//    + 54.3092（唯一带 usage 的签到包 100−45.6908）+ 0 + 0
				//    （新用户福利、每月登录赠送已用满）。当未知处理只会显示 54.3092，
				//    比官方少整整 1000 —— 那是把已知事实报成了未知。
				used := 0.0
				if v := traeNumber(Object(e["usage"])["credits_amount"]); v != nil {
					used = *v
				}
				total := traeNumber(Object(base["quota"])["credits_limit"])
				b := Balance{Name: traeString(base, "display_desc"), Unit: "credits", Total: total, Used: &used, ExpiresAt: traeTime(e["expire_time"])}
				if b.Name == "" {
					b.Name = traeString(e, "display_desc")
					if b.Name == "" {
						b.Name = "资源包"
					}
				}
				if b.ExpiresAt == 0 {
					b.ExpiresAt = traeTime(Object(e["base_info"])["end_time"])
				}
				if total != nil {
					remain := *total - used
					b.Remaining = &remain
				} else {
					// 拿不到总量才是真未知：没有基数，减不出剩余，也不许编。
					unknownTotal++
				}
				out.Balances = append(out.Balances, b)
			}
			if unknownTotal > 0 {
				warnings = append(warnings, strconv.Itoa(unknownTotal)+" 个额度包总量未知，未计入剩余")
			}
		}
	}
	campaign, statusErr := a.checkin(ctx, c)
	if statusErr == nil {
		out.Campaigns = append(out.Campaigns, campaign)
	}
	if balanceErr != nil {
		warnings = append(warnings, "额度查询失败："+balanceErr.Error())
	}
	if statusErr != nil {
		warnings = append(warnings, "签到状态查询失败："+statusErr.Error())
	}
	out.Warning = strings.Join(warnings, "；")
	if err := ctx.Err(); err != nil {
		return out, err
	}
	if balanceErr != nil && statusErr != nil {
		return out, errors.New("TRAE 额度和签到状态均查询失败")
	}
	return out, nil
}

func (a *traeAdapter) Claim(ctx context.Context, c Credential, id string) (ClaimResult, error) {
	out := ClaimResult{ID: traeCheckinID, Name: "每日签到"}
	if id != "" && id != traeCheckinID {
		return out, errors.New("TRAE 不支持该领取项目")
	}
	if err := a.Validate(c); err != nil {
		return out, err
	}
	// A read before the single mutation distinguishes an already claimed day.
	before, beforeErr := a.checkin(ctx, c)
	if before.Claimed {
		out.AlreadyClaimed = true
		out.Amount = before.Amount
		out.Message = "今日已签到"
		return out, nil
	}
	if beforeErr == nil && !before.Claimable {
		out.Message = "TRAE 当前未开放签到领取"
		return out, errors.New(out.Message)
	}
	r, err := a.ug(ctx, c, traeClaimPath, map[string]any{})
	if err != nil {
		out.Message = "领取未确认成功，请先查询签到状态；未自动重试"
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Code == "9074" {
			out.Message = "签到人数过多（9074），未重试或轮换设备"
		}
		return out, err
	}
	p := traePayload(r)
	// Claim must explicitly acknowledge success; an empty HTTP 200 is not success.
	ack := false
	for _, obj := range []map[string]any{r, p} {
		if n := traeNumber(traeValue(obj, "code", "Code")); n != nil && *n == 0 {
			ack = true
		}
	}
	if traeIsTrue(p["already_claimed"]) {
		out.AlreadyClaimed = true
		out.Message = "今日已签到"
		return out, nil
	}
	if !ack {
		out.Message = "TRAE 未确认领取结果，请查询签到状态"
		return out, errors.New(out.Message)
	}
	out.Claimed = true
	out.Amount = traeNumber(p["credits"])
	out.Message = "签到领取成功"
	if out.Amount == nil {
		after, statusErr := a.checkin(ctx, c)
		if statusErr == nil && after.Claimed {
			out.Amount = after.Amount
		}
		if out.Amount == nil {
			out.Message = "签到领取成功，奖励金额暂不可确认"
		}
	}
	return out, nil
}

func (a *traeAdapter) Prepare(ctx context.Context, c Credential, in Inference) (Prepared, error) {
	if err := ctx.Err(); err != nil {
		return Prepared{}, err
	}
	if err := a.Validate(c); err != nil {
		return Prepared{}, err
	}
	if in.Model.ID == "" || traeRank(in.Model.Function) < 0 {
		return Prepared{}, errors.New("TRAE 模型或 function 未获动态目录确认")
	}
	if in.MaxMode && !in.Model.MaxMode {
		return Prepared{}, errors.New("TRAE 当前模型未声明支持 Max 模式")
	}
	if in.Effort != "" && !traeContains(in.Model.Efforts, in.Effort) {
		return Prepared{}, errors.New("TRAE 当前模型未声明该推理档位")
	}
	if in.MaxTokens < 0 {
		return Prepared{}, errors.New("TRAE 输出 token 上限无效")
	}
	// JSON round-trip makes nested history/tool maps private to this request.
	encoded, err := json.Marshal(map[string]any{"messages": in.Messages, "tools": in.Tools})
	if err != nil {
		return Prepared{}, errors.New("TRAE 消息或工具定义不可序列化")
	}
	var source map[string]any
	if json.Unmarshal(encoded, &source) != nil {
		return Prepared{}, errors.New("TRAE 输入格式无效")
	}
	messages, err := traeMessages(Array(source["messages"]), in.Model.Vision)
	if err != nil {
		return Prepared{}, err
	}
	body := map[string]any{"model": in.Model.ID, "config_name": in.Model.ID, "function": in.Model.Function, "stream": true, "messages": messages}
	tools := Array(source["tools"])
	for _, item := range tools {
		tool := Object(item)
		fn, ok := tool["function"].(map[string]any)
		if tool["type"] != "function" || !ok || traeString(fn, "name") == "" {
			return Prepared{}, errors.New("TRAE 仅支持有效的 OpenAI function 工具定义")
		}
		if params, exists := fn["parameters"]; exists {
			if _, ok := params.(map[string]any); !ok {
				return Prepared{}, errors.New("TRAE 工具 parameters 必须是 JSON 对象")
			}
			b, _ := json.Marshal(params)
			fn["parameters"] = string(b)
		} else {
			fn["parameters"] = "{}"
		}
	}
	if len(tools) > 0 {
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	if in.Effort != "" {
		body["reasoning_effort"] = in.Effort
	}
	if in.SessionID != "" {
		body["session_id"] = in.SessionID
	}
	output := in.MaxTokens
	if output > 64000 {
		output = 64000
	}
	if in.Model.MaxOutput > 0 && output > in.Model.MaxOutput {
		output = in.Model.MaxOutput
	}
	if output > 0 {
		body["max_tokens"] = output
	}
	if in.MaxMode {
		a.mu.RLock()
		limits, found := a.maxLimits[c.Text("uid")][in.Model.ID+"\x00"+in.Model.Function]
		a.mu.RUnlock()
		if !found || limits.context <= 0 {
			// Rehydrate metadata after process restart rather than inventing a 1M window.
			if _, err := a.Models(ctx, c); err != nil {
				return Prepared{}, err
			}
			a.mu.RLock()
			limits, found = a.maxLimits[c.Text("uid")][in.Model.ID+"\x00"+in.Model.Function]
			a.mu.RUnlock()
		}
		if !found || limits.context <= 0 {
			return Prepared{}, errors.New("TRAE 动态目录未提供此模型的 Max 上下文窗口")
		}
		output = in.MaxTokens
		if output <= 0 {
			output = limits.output
			if output <= 0 {
				output = 64000
			}
		}
		if limits.output > 0 && output > limits.output {
			output = limits.output
		}
		if output > 64000 {
			output = 64000
		}
		if output >= limits.context {
			return Prepared{}, errors.New("TRAE Max 输出预算必须小于上下文窗口")
		}
		body["model_auto_selection"] = map[string]any{"strategy": "max", "fallback_to_advance_model": nil, "entitlement_id": nil}
		body["model_selection_strategy"] = "max"
		body["mode_type"] = 1
		body["context_window_size"] = limits.context
		body["prompt_max_tokens"] = limits.context - output
		body["max_tokens"] = output
	}
	b, err := json.Marshal(body)
	if err != nil {
		return Prepared{}, errors.New("TRAE 请求不可序列化")
	}
	return Prepared{URL: traeAgent + traeChatPath, Headers: traeAgentHeaders(c, true), Body: b, Wire: "trae"}, nil
}

// Validate the tool-call state machine instead of silently dropping orphan calls
// or results. Parallel results stay contiguous; images in results are deferred
// until the entire tool-result group has finished.
func traeMessages(history []any, vision bool) ([]map[string]any, error) {
	if len(history) == 0 {
		return nil, errors.New("TRAE 消息列表不能为空")
	}
	out := make([]map[string]any, 0, len(history))
	pending := make(map[string]bool)
	seen := make(map[string]bool)
	deferredImages := []any{}
	for _, item := range history {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("TRAE 消息格式无效")
		}
		role := Text(m["role"])
		if role == "developer" {
			role = "system"
			m["role"] = role
		}
		if role != "system" && role != "user" && role != "assistant" && role != "tool" {
			return nil, errors.New("TRAE 不支持该消息角色")
		}
		if len(pending) > 0 && role != "tool" {
			return nil, errors.New("TRAE 工具调用后必须紧跟对应的工具结果")
		}
		parts, err := traeContent(m["content"], vision)
		if err != nil {
			return nil, err
		}
		if role == "tool" {
			id := Text(m["tool_call_id"])
			if !pending[id] {
				return nil, errors.New("TRAE 工具结果缺少对应的 tool_call_id")
			}
			delete(pending, id)
			textParts := []any{}
			for _, p := range parts {
				if Object(p)["type"] == "image_url" {
					deferredImages = append(deferredImages, p)
				} else {
					textParts = append(textParts, p)
				}
			}
			parts = textParts
		}
		if rawCalls, exists := m["tool_calls"]; exists && rawCalls != nil {
			calls, ok := rawCalls.([]any)
			if !ok || role != "assistant" {
				return nil, errors.New("TRAE tool_calls 必须属于 assistant 消息")
			}
			for _, item := range calls {
				call := Object(item)
				id := Text(call["id"])
				fn, ok := call["function"].(map[string]any)
				if !ok || id == "" || seen[id] || traeString(fn, "name") == "" || call["type"] != "function" {
					return nil, errors.New("TRAE 工具调用缺少唯一 ID 或函数名")
				}
				args, ok := fn["arguments"].(string)
				if !ok || !json.Valid([]byte(args)) {
					return nil, errors.New("TRAE 工具调用 arguments 必须是有效 JSON 字符串")
				}
				seen[id] = true
				pending[id] = true
				call["function_call"] = fn
				delete(call, "function")
			}
		}
		if m["content"] != nil || role != "assistant" {
			m["content"] = parts
		}
		out = append(out, m)
		if len(pending) == 0 && len(deferredImages) > 0 {
			out = append(out, map[string]any{"role": "user", "content": deferredImages})
			deferredImages = []any{}
		}
	}
	if len(pending) != 0 {
		return nil, errors.New("TRAE 历史中存在尚未提供结果的工具调用")
	}
	return out, nil
}

func traeContent(content any, vision bool) ([]any, error) {
	if content == nil {
		return []any{}, nil
	}
	if s, ok := content.(string); ok {
		return []any{map[string]any{"type": "text", "text": s}}, nil
	}
	parts, ok := content.([]any)
	if !ok {
		return nil, errors.New("TRAE content 必须为文本或 OpenAI 内容数组")
	}
	for _, p := range parts {
		part, ok := p.(map[string]any)
		if !ok {
			return nil, errors.New("TRAE 内容块格式无效")
		}
		switch part["type"] {
		case "text":
			if _, ok := part["text"].(string); !ok {
				return nil, errors.New("TRAE 文本块缺少 text")
			}
		case "image_url":
			if !vision {
				return nil, errors.New("TRAE 当前模型不支持图片")
			}
			image := traeString(Object(part["image_url"]), "url")
			if image == "" {
				return nil, errors.New("TRAE 图片块缺少 URL")
			}
			// Images are forwarded, never downloaded locally (no arbitrary URL fetch).
			if !strings.HasPrefix(image, "data:image/") && !strings.HasPrefix(image, "https://") && !strings.HasPrefix(image, "http://") {
				return nil, errors.New("TRAE 图片 URL 格式不支持")
			}
		default:
			return nil, errors.New("TRAE 仅支持文本与图片内容块")
		}
	}
	return parts, nil
}
