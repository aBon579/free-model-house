package credit

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	zcodeOrigin     = "https://zcode.z.ai"
	zcodeBizOrigin  = "https://api.z.ai"
	zcodeVersion    = "3.14.3" // Wire compatibility version, not an assertion of official client identity.
	zcodeConfigPath = "/api/v1/client/configs?app_version=" + zcodeVersion + "&platform=unknown"
	zcodeSDK        = "https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js"
)

type zcodeAdapter struct {
	client   *http.Client
	mu       sync.Mutex
	activity map[[32]byte]string
}

var _ Adapter = (*zcodeAdapter)(nil)

// NewZCode uses only the supplied transport; it never reads installed-client credentials.
// Redirects are disabled even on injected clients so credentials cannot leave fixed origins.
func NewZCode(client *http.Client) Adapter {
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	if copyClient.Timeout == 0 {
		copyClient.Timeout = 30 * time.Second
	}
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &zcodeAdapter{client: &copyClient, activity: make(map[[32]byte]string)}
}

func zcodeError(status int, code, message string) *APIError {
	return &APIError{Status: status, Code: code, Message: message}
}
func zcodeInvalid() error {
	return zcodeError(502, "invalid_response", "ZCode 返回了无法识别的数据")
}
func zcodeLoginRequired() error {
	return zcodeError(401, "login_required", "ZCode 登录已失效，请重新登录；此服务没有可用的刷新协议")
}
func zcodeCaptchaRequired() error {
	return zcodeError(409, "captcha_required", "请在 Credit 页面完成人机验证")
}

func zcodeBusinessError(code int) error {
	switch code {
	case 401:
		return zcodeLoginRequired()
	case 1001:
		return zcodeError(404, "plan_not_found", "活动不存在")
	case 1002:
		return zcodeError(409, "plan_expired", "活动已结束")
	case 1003:
		return zcodeError(409, "already_claimed", "此活动已经领取")
	case 1004:
		return zcodeError(403, "not_eligible", "当前账号不符合活动条件")
	case 1005, 1113:
		return zcodeError(429, "quota_exhausted", "额度或活动名额不足")
	case 3001:
		return zcodeError(400, "invalid_parameter", "ZCode 拒绝了请求参数")
	case 3007:
		return zcodeCaptchaRequired()
	case 3009:
		return zcodeError(429, "concurrency_limit", "ZCode 并发请求过多，请稍后重试")
	case 3012:
		return zcodeError(403, "unusual_activity", "ZCode 安全策略拒绝了请求，请在官方页面检查账号状态")
	default:
		return zcodeError(502, "upstream_error", "ZCode 未能完成请求")
	}
}

// Never expose response bodies, upstream messages, URLs with secrets, or transport errors.
// Unlike JSONRequest this also classifies business codes inside non-2xx JSON responses.
func (z *zcodeAdapter) request(ctx context.Context, method, endpoint string, headers map[string]string, body any) (any, error) {
	u, err := url.Parse(endpoint)
	if err != nil || !zcodeHTTPS(u) || (u.Hostname() != "zcode.z.ai" && u.Hostname() != "api.z.ai") {
		return nil, zcodeInvalid()
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, zcodeError(400, "invalid_request", "请求无法编码为 JSON")
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, zcodeInvalid()
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		if !zcodeHeader(v) {
			return nil, zcodeError(400, "invalid_credential", "凭据或验证参数格式无效")
		}
		req.Header.Set(k, v)
	}
	resp, err := z.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, zcodeError(502, "upstream_unavailable", "无法连接 ZCode，请稍后重试")
	}
	defer resp.Body.Close()
	b, readErr := io.ReadAll(io.LimitReader(resp.Body, (8<<20)+1))
	var value any
	decodeErr := json.Unmarshal(b, &value)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, zcodeLoginRequired()
	}
	if readErr == nil && decodeErr == nil {
		m := Object(value)
		if raw, exists := m["code"]; exists {
			code, ok := zcodeCode(raw)
			if !ok {
				return nil, zcodeInvalid()
			}
			if code != 0 && !(u.Hostname() == "api.z.ai" && code == 200) {
				return nil, zcodeBusinessError(code)
			}
		}
		if m["success"] == false {
			return nil, zcodeError(502, "upstream_error", "ZCode 未能完成请求")
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, zcodeError(resp.StatusCode, "upstream_http_error", fmt.Sprintf("ZCode 返回 HTTP %d", resp.StatusCode))
	}
	if resp.StatusCode == http.StatusNoContent && len(b) == 0 {
		return map[string]any{}, nil
	}
	if readErr != nil || len(b) > 8<<20 || decodeErr != nil {
		return nil, zcodeInvalid()
	}
	if u.Hostname() == "zcode.z.ai" {
		if _, ok := Object(value)["code"]; !ok {
			return nil, zcodeInvalid()
		}
	}
	return value, nil
}

func zcodeCode(v any) (int, bool) {
	if s, ok := v.(string); ok {
		if len(s) == 0 || len(s) > 9 {
			return 0, false
		}
		for _, r := range s {
			if r < '0' || r > '9' {
				return 0, false
			}
		}
		n, err := strconv.Atoi(s)
		return n, err == nil
	}
	n := zcodeNumber(v)
	if n == nil || *n < 0 || *n > 999999999 || math.Trunc(*n) != *n {
		return 0, false
	}
	return int(*n), true
}
func zcodeNumber(v any) *float64 {
	var n float64
	switch v := v.(type) {
	case float64:
		n = v
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return nil
		}
		n = f
	case string:
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil
		}
		n = f
	default:
		return nil
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return nil
	}
	return &n
}
func zcodeTime(v any) int64 {
	if n := zcodeNumber(v); n != nil && *n > 0 && *n < 1e16 {
		x := int64(*n)
		if x < 100000000000 {
			x *= 1000
		}
		return x
	}
	return Timestamp(v)
}
func zcodeText(m map[string]any, keys ...string) string { return Credential(m).Text(keys...) }
func zcodeData(v any) any {
	if d, ok := Object(v)["data"]; ok {
		return d
	}
	return v
}
func zcodeHeader(s string) bool {
	if len(s) > 65536 {
		return false
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
func zcodeHTTPS(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && (u.Port() == "" || u.Port() == "443") && u.Fragment == ""
}
func zcodeOfficialURL(raw, provider string) bool {
	u, err := url.Parse(raw)
	if err != nil || !zcodeHTTPS(u) {
		return false
	}
	host := strings.ToLower(u.Hostname())
	domain := "z.ai"
	if provider == "bigmodel" {
		domain = "bigmodel.cn"
	}
	if host != domain && !strings.HasSuffix(host, "."+domain) {
		return false
	}
	// CLI callbacks must remain on the official HTTPS service, not an arbitrary redirect.
	for _, key := range []string{"redirect", "redirect_uri"} {
		for _, target := range u.Query()[key] {
			t, e := url.Parse(target)
			if e != nil || !zcodeHTTPS(t) || strings.ToLower(t.Hostname()) != "zcode.z.ai" {
				return false
			}
		}
	}
	return true
}
func zcodeSegment(s string) bool {
	return s != "" && len(s) <= 512 && s != "." && s != ".." && !strings.ContainsAny(s, "/\\") && zcodeHeader(s)
}
func zcodePlatform() string {
	if runtime.GOOS == "windows" {
		return "win32"
	}
	return runtime.GOOS
}
func zcodeHeaders(c Credential) map[string]string {
	h := map[string]string{"User-Agent": "FreeModelHouse/1.0", "HTTP-Referer": zcodeOrigin, "X-ZCode-App-Version": zcodeVersion, "X-Platform": zcodePlatform(), "anthropic-version": "2023-06-01", "Content-Type": "application/json"}
	if c.Text("machineId") != "" {
		h["X-Device-Mid"] = c.Text("machineId")
	}
	if c.Text("accessToken", "token") != "" {
		h["Authorization"] = "Bearer " + c.Text("accessToken", "token")
	}
	return h
}
func zcodeRandom(size int) (string, error) {
	b := make([]byte, size)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("无法生成安全的登录标识")
	}
	return hex.EncodeToString(b), nil
}
func zcodeMachine() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", errors.New("无法生成设备标识")
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

func (z *zcodeAdapter) StartLogin(ctx context.Context, provider string) (*Login, error) {
	if provider != "bigmodel" && provider != "zai" {
		return nil, zcodeError(400, "invalid_provider", "登录方式必须为 bigmodel 或 zai")
	}
	secret, err := zcodeRandom(32)
	if err != nil {
		return nil, err
	}
	machine, err := zcodeMachine()
	if err != nil {
		return nil, err
	}
	h := zcodeHeaders(nil)
	h["Authorization"] = "Bearer " + secret
	v, err := z.request(ctx, http.MethodPost, zcodeOrigin+"/api/v1/oauth/cli/init", h, map[string]any{"provider": provider})
	if err != nil {
		return nil, err
	}
	d := Object(zcodeData(v))
	id := zcodeText(d, "flow_id")
	target := zcodeText(d, "authorize_url")
	if !zcodeSegment(id) || !zcodeOfficialURL(target, provider) {
		return nil, zcodeError(502, "invalid_authorization_url", "ZCode 未返回有效的官方 HTTPS 授权地址或流程标识")
	}
	expires := zcodeTime(d["expires_at"])
	if expires == 0 {
		expires = time.Now().Add(5 * time.Minute).UnixMilli()
	}
	if expires <= time.Now().UnixMilli() {
		return nil, zcodeError(410, "login_expired", "授权流程已过期，请重新登录")
	}
	interval := 2
	if n := zcodeNumber(d["poll_interval_sec"]); n != nil && *n >= 1 && *n <= 300 {
		interval = int(*n)
	}
	return &Login{ID: id, Provider: "zcode", URL: target, State: "pending", ExpiresAt: expires, Interval: interval,
		Data: Credential{"flowID": id, "flowSecret": secret, "pollToken": d["poll_token"], "oauthProvider": provider, "machineId": machine}}, nil
}

func (z *zcodeAdapter) PollLogin(ctx context.Context, login *Login, _ string) (Credential, bool, error) {
	if login == nil || !zcodeSegment(login.Data.Text("flowID")) || login.Data.Text("flowSecret") == "" {
		return nil, false, zcodeError(400, "invalid_login", "登录流程无效，请重新发起")
	}
	if login.ExpiresAt > 0 && login.ExpiresAt <= time.Now().UnixMilli() {
		return nil, false, zcodeError(410, "login_expired", "授权流程已过期，请重新登录")
	}
	h := zcodeHeaders(nil)
	h["Authorization"] = "Bearer " + login.Data.Text("flowSecret")
	v, err := z.request(ctx, http.MethodGet, zcodeOrigin+"/api/v1/oauth/cli/poll/"+url.PathEscape(login.Data.Text("flowID")), h, nil)
	if err != nil {
		var api *APIError
		if errors.As(err, &api) && (api.Status == 408 || api.Status == 429 || api.Status >= 500) {
			return nil, false, nil
		}
		return nil, false, err
	}
	d := Object(zcodeData(v))
	switch zcodeText(d, "status") {
	case "pending":
		return nil, false, nil
	case "expired":
		return nil, false, zcodeError(410, "login_expired", "授权流程已过期，请重新登录")
	case "failed", "denied", "cancelled":
		return nil, false, zcodeError(403, "login_denied", "授权未完成或已被拒绝")
	case "ready":
	default:
		return nil, false, zcodeInvalid()
	}
	user := Object(d["user"])
	machine := zcodeText(d, "machineId", "device_mid")
	if machine == "" {
		machine = login.Data.Text("machineId")
	}
	c := Credential{"accessToken": zcodeText(d, "token", "accessToken"), "uid": zcodeText(user, "user_id", "userId", "uid", "id"), "machineId": machine, "codingPlanKey": "", "expiresAt": zcodeTime(d["expires_at"]), "oauthProvider": login.Data.Text("oauthProvider"), "name": zcodeText(user, "name", "email")}
	c, err = zcodeCredential(c)
	if err != nil {
		return nil, false, err
	}
	provider := login.Data.Text("oauthProvider")
	if provider != "bigmodel" && provider != "zai" {
		return nil, false, zcodeError(400, "invalid_login", "登录方式无效")
	}
	token := zcodeText(Object(d[provider]), "access_token", "accessToken")
	if token != "" {
		// An optional, read-only lookup. Never create a missing API key or fail free login.
		key, keyErr := z.existingCodingKey(ctx, c, token)
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if keyErr == nil {
			c["codingPlanKey"] = key
		} else {
			c["codingPlanWarning"] = "未能读取已有 Coding Plan 密钥，免费通道不受影响"
		}
	}
	return c, true, nil
}

// JWT decoding only extracts metadata; it does NOT verify the signature. Remote requests
// authenticate the token. Imported opaque tokens therefore require an explicit user ID.
func zcodeCredential(c Credential) (Credential, error) {
	token, uid, machine := c.Text("accessToken", "token"), c.Text("uid", "userId"), c.Text("machineId")
	if token == "" || machine == "" || !zcodeHeader(token) || !zcodeHeader(machine) || len(machine) > 512 {
		return nil, zcodeError(400, "invalid_credential", "需要有效的 accessToken/token 和 machineId")
	}
	expiry := zcodeTime(c["expiresAt"])
	if parts := strings.Split(token, "."); len(parts) == 3 {
		payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
		var claims map[string]any
		if err != nil || json.Unmarshal(payload, &claims) != nil {
			return nil, zcodeError(400, "invalid_credential", "JWT 格式无效")
		}
		jwtUID := zcodeText(claims, "userId", "uid", "user_id")
		if uid != "" && jwtUID != "" && uid != jwtUID {
			return nil, zcodeError(400, "invalid_credential", "JWT 与用户标识不一致")
		}
		if uid == "" {
			uid = jwtUID
		}
		if raw, exists := claims["exp"]; exists {
			t := zcodeTime(raw)
			if t == 0 || t <= time.Now().UnixMilli() {
				return nil, zcodeLoginRequired()
			}
			if expiry == 0 || t < expiry {
				expiry = t
			}
		}
	}
	if uid == "" || len(uid) > 512 || !zcodeHeader(uid) {
		return nil, zcodeError(400, "invalid_credential", "需要 uid/userId 或含用户标识的 JWT")
	}
	if expiry > 0 && expiry <= time.Now().UnixMilli() {
		return nil, zcodeLoginRequired()
	}
	if !zcodeHeader(c.Text("codingPlanKey")) {
		return nil, zcodeError(400, "invalid_credential", "Coding Plan 密钥格式无效")
	}
	out := make(Credential, len(c)+4)
	for k, v := range c {
		out[k] = v
	}
	out["accessToken"], out["uid"], out["machineId"], out["expiresAt"] = token, uid, machine, expiry
	return out, nil
}
func (z *zcodeAdapter) Validate(c Credential) error { _, err := zcodeCredential(c); return err }
func (z *zcodeAdapter) Refresh(ctx context.Context, c Credential) (Credential, error) {
	c, err := zcodeCredential(c)
	if err != nil {
		return nil, err
	}
	if _, err = z.configs(ctx, c); err != nil {
		return nil, err
	}
	// No refresh endpoint exists: retain the exact token and its original expiry.
	return c, nil
}

func (z *zcodeAdapter) existingCodingKey(ctx context.Context, c Credential, token string) (string, error) {
	h := zcodeHeaders(c)
	h["Authorization"] = "Bearer " + token
	delete(h, "HTTP-Referer")
	v, err := z.request(ctx, http.MethodGet, zcodeBizOrigin+"/api/biz/customer/getCustomerInfo", h, nil)
	if err != nil {
		return "", err
	}
	type candidate struct {
		org, project string
		score        int
	}
	var best candidate
	for _, rawOrg := range Array(Object(zcodeData(v))["organizations"]) {
		o := Object(rawOrg)
		org := zcodeText(o, "organizationId")
		if !zcodeSegment(org) {
			continue
		}
		for _, rawProject := range Array(o["projects"]) {
			p := Object(rawProject)
			project := zcodeText(p, "projectId")
			if n := zcodeNumber(p["projectType"]); n != nil && *n == 2 {
				continue
			}
			if !zcodeSegment(project) {
				continue
			}
			score := 1
			if strings.Contains(zcodeText(o, "organizationName"), "默认机构") {
				score += 4
			}
			if strings.Contains(zcodeText(p, "projectName"), "默认项目") {
				score += 2
			}
			if score > best.score {
				best = candidate{org, project, score}
			}
		}
	}
	if best.score == 0 {
		return "", nil
	}
	endpoint := zcodeBizOrigin + "/api/biz/v1/organization/" + url.PathEscape(best.org) + "/projects/" + url.PathEscape(best.project) + "/api_keys"
	v, err = z.request(ctx, http.MethodGet, endpoint, h, nil)
	if err != nil {
		return "", err
	}
	for _, raw := range Array(zcodeData(v)) {
		item := Object(raw)
		if zcodeText(item, "name") != "zcode-api-key" {
			continue
		}
		key := zcodeText(item, "apiKey")
		if !zcodeSegment(key) {
			continue
		}
		v, err = z.request(ctx, http.MethodGet, endpoint+"/copy/"+url.PathEscape(key), h, nil)
		if err != nil {
			return "", err
		}
		secret := zcodeText(Object(zcodeData(v)), "secretKey")
		if secret == "" {
			return "", nil
		}
		if !zcodeHeader(secret) {
			return "", zcodeInvalid()
		}
		return key + "." + secret, nil
	}
	return "", nil
}

func (z *zcodeAdapter) configs(ctx context.Context, c Credential) (map[string]any, error) {
	v, err := z.request(ctx, http.MethodGet, zcodeOrigin+zcodeConfigPath, zcodeHeaders(c), nil)
	if err != nil {
		return nil, err
	}
	return Object(zcodeData(v)), nil
}
func zcodeEntries(v any) []any {
	if a, ok := v.([]any); ok {
		return a
	}
	m := Object(v)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	a := make([]any, 0, len(keys))
	for _, k := range keys {
		a = append(a, m[k])
	}
	return a
}
func (z *zcodeAdapter) Models(ctx context.Context, c Credential) ([]Model, error) {
	c, err := zcodeCredential(c)
	if err != nil {
		return nil, err
	}
	d, err := z.configs(ctx, c)
	if err != nil {
		return nil, err
	}
	raw := d["builtinModels"]
	if raw == nil {
		raw = Object(d["configs"])["builtinModels"]
	}
	models := make([]Model, 0)
	seen := make(map[string]bool)
	for _, value := range zcodeEntries(raw) {
		m := Object(value)
		id := zcodeText(m, "modelId", "id")
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		name := zcodeText(m, "name")
		if name == "" {
			name = id
		}
		model := Model{ID: id, Name: name, Vision: Object(m["capabilities"])["vision"] == true, Function: zcodeText(m, "function")}
		if n := zcodeNumber(m["contextWindow"]); n != nil && *n > 0 && *n < 1<<31 {
			model.ContextWindow = int(*n)
		}
		for _, key := range []string{"maxCompletionTokens", "maxTokens"} {
			if n := zcodeNumber(m[key]); n != nil && *n > 0 && *n < 1<<31 {
				model.MaxOutput = int(*n)
				break
			}
		}
		for _, input := range Array(Object(m["modalities"])["input"]) {
			if input == "image" {
				model.Vision = true
			}
		}
		levels := Object(Object(m["reasoning"])["levels"])
		order := []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
		for _, effort := range order {
			if _, ok := levels[effort]; ok {
				model.Efforts = append(model.Efforts, effort)
			}
		}
		unknown := []string{}
		for effort := range levels {
			if !zcodeContains(order, effort) {
				unknown = append(unknown, effort)
			}
		}
		sort.Strings(unknown)
		model.Efforts = append(model.Efforts, unknown...)
		model.Reasoning = len(model.Efforts) > 0 || Object(m["capabilities"])["reasoning"] == true
		for _, value := range []any{m["multiplier"], m["creditMultiplier"], Object(m["billing"])["multiplier"], Object(m["pricing"])["multiplier"]} {
			if n := zcodeNumber(value); n != nil && *n >= 0 {
				model.Multiplier = n
				break
			}
		}
		models = append(models, model)
		if c.Text("codingPlanKey") != "" {
			coding := model
			coding.ID = "coding/" + id
			coding.Name = name + " (Coding Plan)"
			coding.Multiplier = nil // Free-credit billing is not a subscription price.
			models = append(models, coding)
		}
	}
	if len(models) == 0 {
		return nil, zcodeError(502, "models_unavailable", "官方配置未提供模型目录")
	}
	return models, nil
}
func zcodeContains(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}

// CaptchaConfig supplies configuration only. The caller must load the official SDK and
// obtain an actual user-completed, single-use verification parameter; no SDK runs here.
func (z *zcodeAdapter) CaptchaConfig(ctx context.Context, c Credential) (map[string]any, error) {
	c, err := zcodeCredential(c)
	if err != nil {
		return nil, err
	}
	d, err := z.configs(ctx, c)
	if err != nil {
		return nil, err
	}
	captcha := Object(Object(d["configs"])["captcha"])
	out := map[string]any{}
	for _, k := range []string{"sceneId", "prefix", "region"} {
		v := zcodeText(captcha, k)
		if v == "" || len(v) > 256 || !zcodeHeader(v) {
			return nil, zcodeError(502, "captcha_config_unavailable", "官方人机验证配置不可用")
		}
		out[k] = v
	}
	sdk := zcodeText(captcha, "sdkUrl", "scriptUrl")
	if sdk == "" {
		sdk = zcodeSDK
	}
	u, err := url.Parse(sdk)
	if err != nil || !zcodeHTTPS(u) || u.Hostname() != "o.alicdn.com" || u.Path != "/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js" {
		return nil, zcodeError(502, "unsafe_captcha_sdk", "官方配置中的人机验证 SDK 地址不被允许")
	}
	out["sdkUrl"] = sdk
	return out, nil
}

// Activity is emitted only on an explicit Credits call, never by polling, inference, or
// a background loop. Reserve before network I/O to deduplicate concurrent calls and even
// failures for this account/device/day. A bounded map prevents unbounded account cycling.
func (z *zcodeAdapter) reportActivity(ctx context.Context, c Credential) bool {
	day := time.Now().UTC().Format("2006-01-02")
	key := sha256.Sum256([]byte(c.Text("uid") + "\x00" + c.Text("machineId")))
	z.mu.Lock()
	for k, d := range z.activity {
		if d != day {
			delete(z.activity, k)
		}
	}
	if z.activity[key] == day {
		z.mu.Unlock()
		return true
	}
	if len(z.activity) >= 1024 {
		z.mu.Unlock()
		return false
	}
	z.activity[key] = day
	z.mu.Unlock()
	ok := true
	for _, event := range []string{"app_launch", "app_daily_active"} {
		_, err := z.request(ctx, http.MethodPost, zcodeOrigin+"/api/v1/event/report", zcodeHeaders(c), map[string]any{"event": event, "device_mid": c.Text("machineId"), "platform": zcodePlatform(), "app_version": zcodeVersion})
		if err != nil {
			ok = false
		}
		if ctx.Err() != nil {
			break
		}
	}
	return ok
}
func (z *zcodeAdapter) Credits(ctx context.Context, c Credential) (Credits, error) {
	out := Credits{Balances: []Balance{}, Campaigns: []Campaign{}, UpdatedAt: time.Now().UnixMilli()}
	c, err := zcodeCredential(c)
	if err != nil {
		return out, err
	}
	v, balanceErr := z.request(ctx, http.MethodGet, zcodeOrigin+"/api/v1/zcode-plan/billing/balance", zcodeHeaders(c), nil)
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if balanceErr != nil {
		var api *APIError
		if errors.As(balanceErr, &api) && (api.Status == 401 || api.Status == 403) {
			return out, balanceErr
		}
		out.Warning = "暂时无法读取余额；未知余额不代表零余额。"
	} else {
		d := Object(zcodeData(v))
		for _, raw := range Array(d["balances"]) {
			b := Object(raw)
			name := zcodeText(b, "show_name", "plan_id")
			if name == "" {
				name = "ZCode"
			}
			unit := zcodeText(b, "unit_type")
			if unit == "" {
				unit = "unknown"
			}
			remaining := zcodeNumber(b["available_units"])
			if remaining == nil {
				remaining = zcodeNumber(b["remaining_units"])
			}
			out.Balances = append(out.Balances, Balance{Name: name, Unit: unit, Total: zcodeNumber(b["total_units"]), Used: zcodeNumber(b["used_units"]), Remaining: remaining, ExpiresAt: zcodeTime(b["expires_at"])})
		}
		if d["displayMode"] == "enterprise" {
			out.Balances = []Balance{{Name: "ZCode Enterprise", Unit: "unknown"}}
			out.Warning = "企业账户未提供数值余额。"
			return out, nil
		}
		if !z.reportActivity(ctx, c) {
			out.Warning += "活跃事件未能上报，可领活动可能尚未更新。"
		}
	}
	if len(out.Balances) == 0 {
		out.Balances = append(out.Balances, Balance{Name: "ZCode", Unit: "unknown"})
		out.Warning += "上游未提供余额数值。"
	}
	endpoint := zcodeOrigin + "/api/v1/zcode-plan/billing/preview?app_version=" + zcodeVersion + "&platform=" + url.QueryEscape(zcodePlatform())
	v, err = z.request(ctx, http.MethodGet, endpoint, zcodeHeaders(c), nil)
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if err != nil {
		out.Warning += "可领活动暂时不可用。"
		return out, nil
	}
	for _, raw := range Array(Object(zcodeData(v))["plans"]) {
		p := Object(raw)
		id := zcodeText(p, "plan_id")
		if id == "" {
			continue
		}
		name := zcodeText(p, "name", "show_name")
		if name == "" {
			name = id
		}
		claimed := p["claimed"] == true || p["already_claimed"] == true
		claimable := !claimed
		if allowed, ok := p["claimable"].(bool); ok {
			claimable = allowed && !claimed
		}
		expires := zcodeTime(p["expires_at"])
		if expires > 0 && expires <= time.Now().UnixMilli() {
			claimable = false
		}
		out.Campaigns = append(out.Campaigns, Campaign{ID: id, Name: name, Amount: zcodeGrant(p), Claimable: claimable, Claimed: claimed, ExpiresAt: expires})
	}
	return out, nil
}
func zcodeGrant(p map[string]any) *float64 {
	if n := zcodeNumber(p["amount"]); n != nil {
		return n
	}
	ents := Array(p["entitlements"])
	if len(ents) == 0 {
		return nil
	}
	total := 0.0
	unit := zcodeText(Object(ents[0]), "unit_type")
	for _, raw := range ents {
		e := Object(raw)
		n := zcodeNumber(e["grant_units"])
		if n == nil || zcodeText(e, "unit_type") != unit {
			return nil
		}
		total += *n
	}
	if math.IsInf(total, 0) {
		return nil
	}
	return &total
}
func (z *zcodeAdapter) Claim(ctx context.Context, c Credential, planID, captchaVerifyParam string) (ClaimResult, error) {
	out := ClaimResult{ID: planID}
	if strings.TrimSpace(captchaVerifyParam) == "" {
		return out, zcodeCaptchaRequired()
	}
	if !zcodeHeader(captchaVerifyParam) {
		return out, zcodeError(400, "invalid_captcha", "人机验证参数格式无效，请重新验证")
	}
	if !zcodeSegment(planID) {
		return out, zcodeError(400, "invalid_plan", "请选择有效活动")
	}
	c, err := zcodeCredential(c)
	if err != nil {
		return out, err
	}
	config, err := z.CaptchaConfig(ctx, c)
	if err != nil {
		return out, err
	}
	h := zcodeHeaders(c)
	h["x-aliyun-captcha-verify-param"] = captchaVerifyParam
	h["x-aliyun-captcha-verify-region"] = Text(config["region"])
	// Exactly one claim attempt. Never synthesize, recycle, or retry a captcha parameter.
	v, err := z.request(ctx, http.MethodPost, zcodeOrigin+"/api/v1/zcode-plan/billing/claim", h, map[string]any{"plan_id": planID})
	var api *APIError
	if errors.As(err, &api) && api.Code == "already_claimed" {
		out.Claimed = true
		out.AlreadyClaimed = true
		out.Message = "此活动已经领取"
		return out, nil
	}
	if err != nil {
		return out, err
	}
	d := Object(zcodeData(v))
	out.Claimed = true
	out.Name = zcodeText(d, "name")
	out.Amount = zcodeGrant(d)
	out.Message = "领取成功"
	return out, nil
}

func zcodeInputError() error {
	return zcodeError(400, "invalid_messages", "消息、图片或工具参数格式不受支持")
}

func (z *zcodeAdapter) Prepare(ctx context.Context, c Credential, in Inference) (Prepared, error) {
	var out Prepared
	if err := ctx.Err(); err != nil {
		return out, err
	}
	c, err := zcodeCredential(c)
	if err != nil {
		return out, err
	}
	model := in.Model.ID
	coding := strings.HasPrefix(model, "coding/")
	if coding {
		model = strings.TrimPrefix(model, "coding/")
	}
	if strings.TrimSpace(model) == "" {
		return out, zcodeError(400, "invalid_model", "请选择模型")
	}
	if coding && c.Text("codingPlanKey") == "" {
		return out, zcodeError(409, "coding_plan_unavailable", "账号没有可用的已有 Coding Plan 密钥，请重新登录检查；不会自动创建密钥")
	}
	// Normalize and deep-copy nested JSON to support both []any and []map values, without
	// mutating caller messages, tool schemas, credentials, or one-time captcha state.
	b, err := json.Marshal(map[string]any{"messages": in.Messages, "tools": in.Tools})
	if err != nil {
		return out, zcodeInputError()
	}
	var normalized map[string]any
	if json.Unmarshal(b, &normalized) != nil {
		return out, zcodeInputError()
	}
	system, messages, err := zcodeMessages(Array(normalized["messages"]))
	if err != nil {
		return out, err
	}
	if in.OfficialIdentity {
		// 账号显式开了「官方客户端身份」：官方块排最前（上游看的是前缀结构），
		// FMH 的自述块与调用方自己的 system 跟在后面 —— 模拟上游不等于对用户隐瞒。
		system, err = zcodeOfficialSystem(system, model)
		if err != nil {
			return out, err
		}
		messages = zcodeWithContextPrefix(messages, time.Now())
	}
	tools, err := zcodeTools(Array(normalized["tools"]))
	if err != nil {
		return out, err
	}
	maxTokens := in.MaxTokens
	if maxTokens <= 0 {
		maxTokens = in.Model.MaxOutput
		if maxTokens <= 0 {
			maxTokens = 8192
		}
	}
	if in.Model.MaxOutput > 0 && maxTokens > in.Model.MaxOutput {
		maxTokens = in.Model.MaxOutput
	}
	body := map[string]any{"model": model, "stream": true, "max_tokens": maxTokens, "system": system, "messages": messages}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if in.Effort != "" {
		if len(in.Model.Efforts) > 0 && !zcodeContains(in.Model.Efforts, in.Effort) {
			return out, zcodeError(400, "invalid_effort", "模型不支持此思考档位")
		}
		body["output_config"] = map[string]any{"effort": in.Effort}
	}
	out.URL = zcodeOrigin + "/api/v1/zcode-plan/anthropic/v1/messages"
	out.Headers = zcodeHeaders(c)
	out.Wire = "messages"
	if coding {
		out.URL = zcodeBizOrigin + "/api/anthropic/v1/messages"
		out.Headers["Authorization"] = "Bearer " + c.Text("codingPlanKey")
		delete(out.Headers, "HTTP-Referer")
	} else if param := c.Text("captchaVerifyParam"); param != "" {
		if !zcodeHeader(param) {
			return Prepared{}, zcodeInputError()
		}
		config, err := z.CaptchaConfig(ctx, c)
		if err != nil {
			return Prepared{}, err
		}
		out.Headers["x-aliyun-captcha-verify-param"] = param
		out.Headers["x-aliyun-captcha-verify-region"] = Text(config["region"])
		// The stream owner must consume/remove this value once, before sending, and must
		// never replay this Prepared request. Prepare is deliberately side-effect-free.
	}
	out.Body, err = json.Marshal(body)
	if err != nil {
		return Prepared{}, zcodeInputError()
	}
	return out, nil
}

func zcodeMessages(input []any) ([]any, []map[string]any, error) {
	// Independently authored FMH identity, not the third-party identity template.
	// Mocks cannot establish whether the service requires an official-client identity.
	// A server rejection must be surfaced, never answered by impersonating that client.
	system := []any{map[string]any{"type": "text", "text": "This conversation is routed through Free Model House (FMH), an independent client. Be truthful about your identity and service provider; do not claim to be an official ZCode application."}}
	messages := []map[string]any{}
	for _, raw := range input {
		m := Object(raw)
		role := zcodeText(m, "role")
		blocks, err := zcodeContent(m["content"])
		if err != nil {
			return nil, nil, err
		}
		if role == "system" || role == "developer" {
			for _, block := range blocks {
				if Object(block)["type"] != "text" {
					return nil, nil, zcodeInputError()
				}
			}
			system = append(system, blocks...)
			continue
		}
		if role == "tool" {
			id := zcodeText(m, "tool_call_id")
			if id == "" {
				return nil, nil, zcodeInputError()
			}
			for _, b := range blocks {
				if t := Object(b)["type"]; t != "text" && t != "image" {
					return nil, nil, zcodeInputError()
				}
			}
			result := map[string]any{"type": "tool_result", "tool_use_id": id, "content": blocks}
			if flag, ok := m["is_error"].(bool); ok {
				result["is_error"] = flag
			}
			blocks = []any{result}
			role = "user"
		} else if role == "assistant" {
			if reasoning := zcodeText(m, "reasoning_content", "reasoning"); reasoning != "" {
				blocks = append([]any{map[string]any{"type": "thinking", "thinking": reasoning}}, blocks...)
			}
			for _, call := range Array(m["tool_calls"]) {
				tc := Object(call)
				f := Object(tc["function"])
				id, name := zcodeText(tc, "id"), zcodeText(f, "name")
				if id == "" || name == "" {
					return nil, nil, zcodeInputError()
				}
				args, err := zcodeArguments(f["arguments"])
				if err != nil {
					return nil, nil, err
				}
				blocks = append(blocks, map[string]any{"type": "tool_use", "id": id, "name": name, "input": args})
			}
		} else if role != "user" {
			return nil, nil, zcodeInputError()
		}
		for _, block := range blocks {
			t := zcodeText(Object(block), "type")
			if (role == "user" && (t == "thinking" || t == "redacted_thinking" || t == "tool_use")) || (role == "assistant" && t == "tool_result") {
				return nil, nil, zcodeInputError()
			}
		}
		if len(blocks) == 0 {
			continue
		}
		if len(messages) > 0 && messages[len(messages)-1]["role"] == role {
			last := messages[len(messages)-1]
			last["content"] = append(Array(last["content"]), blocks...)
		} else {
			messages = append(messages, map[string]any{"role": role, "content": blocks})
		}
	}
	if len(messages) == 0 {
		return nil, nil, zcodeInputError()
	}
	return system, messages, nil
}
func zcodeArguments(v any) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	if s, ok := v.(string); ok {
		if strings.TrimSpace(s) == "" {
			return map[string]any{}, nil
		}
		if json.Unmarshal([]byte(s), &v) != nil {
			return nil, zcodeInputError()
		}
	}
	m, ok := v.(map[string]any)
	if !ok || m == nil {
		return nil, zcodeInputError()
	}
	return m, nil
}
func zcodeContent(v any) ([]any, error) {
	if v == nil {
		return []any{}, nil
	}
	if s, ok := v.(string); ok {
		if s == "" {
			return []any{}, nil
		}
		return []any{map[string]any{"type": "text", "text": s}}, nil
	}
	input, ok := v.([]any)
	if !ok {
		return nil, zcodeInputError()
	}
	out := make([]any, 0, len(input))
	for _, raw := range input {
		b, ok := raw.(map[string]any)
		if !ok {
			return nil, zcodeInputError()
		}
		switch zcodeText(b, "type") {
		case "text":
			if _, ok := b["text"].(string); !ok {
				return nil, zcodeInputError()
			}
			out = append(out, b)
		case "thinking":
			if _, ok := b["thinking"].(string); !ok {
				return nil, zcodeInputError()
			}
			out = append(out, b)
		case "redacted_thinking":
			if zcodeText(b, "data") == "" {
				return nil, zcodeInputError()
			}
			out = append(out, b)
		case "reasoning":
			text := zcodeText(b, "text", "thinking")
			if text == "" {
				return nil, zcodeInputError()
			}
			b = map[string]any{"type": "thinking", "thinking": text}
			out = append(out, b)
		case "image_url":
			u := Text(b["image_url"])
			if u == "" {
				u = zcodeText(Object(b["image_url"]), "url")
			}
			source := map[string]any{"type": "url", "url": u}
			if strings.HasPrefix(u, "data:") {
				parts := strings.SplitN(strings.TrimPrefix(u, "data:"), ";base64,", 2)
				if len(parts) != 2 {
					return nil, zcodeInputError()
				}
				source = map[string]any{"type": "base64", "media_type": parts[0], "data": parts[1]}
			}
			if !zcodeImage(source) {
				return nil, zcodeInputError()
			}
			out = append(out, map[string]any{"type": "image", "source": source})
		case "image":
			if !zcodeImage(Object(b["source"])) {
				return nil, zcodeInputError()
			}
			out = append(out, b)
		case "tool_use":
			if zcodeText(b, "id") == "" || zcodeText(b, "name") == "" {
				return nil, zcodeInputError()
			}
			args, err := zcodeArguments(b["input"])
			if err != nil {
				return nil, err
			}
			b["input"] = args
			out = append(out, b)
		case "tool_result":
			if zcodeText(b, "tool_use_id") == "" {
				return nil, zcodeInputError()
			}
			content, err := zcodeContent(b["content"])
			if err != nil {
				return nil, err
			}
			for _, item := range content {
				if t := Object(item)["type"]; t != "text" && t != "image" {
					return nil, zcodeInputError()
				}
			}
			b["content"] = content
			out = append(out, b)
		default:
			return nil, zcodeInputError()
		}
	}
	return out, nil
}
func zcodeImage(source map[string]any) bool {
	switch source["type"] {
	case "url":
		u, err := url.Parse(zcodeText(source, "url"))
		return err == nil && zcodeHTTPS(u)
	case "base64":
		if !zcodeContains([]string{"image/png", "image/jpeg", "image/webp", "image/gif"}, zcodeText(source, "media_type")) {
			return false
		}
		data := zcodeText(source, "data")
		if data == "" {
			return false
		}
		_, err := base64.StdEncoding.DecodeString(data)
		return err == nil
	default:
		return false
	}
}
func zcodeTools(input []any) ([]any, error) {
	out := make([]any, 0, len(input))
	for _, raw := range input {
		t := Object(raw)
		if f, ok := t["function"].(map[string]any); ok {
			t = f
		}
		name := zcodeText(t, "name")
		if name == "" {
			return nil, zcodeInputError()
		}
		schema := t["input_schema"]
		if schema == nil {
			schema = t["parameters"]
		}
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if _, ok := schema.(map[string]any); !ok {
			return nil, zcodeInputError()
		}
		tool := map[string]any{"name": name, "input_schema": schema}
		if desc, ok := t["description"].(string); ok {
			tool["description"] = desc
		}
		if cache, ok := t["cache_control"]; ok {
			tool["cache_control"] = cache
		}
		out = append(out, tool)
	}
	return out, nil
}
