package credit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

type traeTestTransport func(*http.Request) (*http.Response, error)

func (f traeTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func traeTestAdapter(t *testing.T, handler func(*http.Request, map[string]any) (int, string)) Adapter {
	t.Helper()
	return NewTrae(&http.Client{Transport: traeTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Scheme != "https" {
			t.Fatal("unexpected request method or scheme")
		}
		switch r.URL.Host {
		case "api.trae.cn", "api.trae.com.cn", "trae-api-cn.mchost.guru":
		default:
			t.Fatal("request escaped official endpoints")
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatal("missing JSON content type")
		}
		if _, ok := r.Context().Deadline(); !ok {
			t.Fatal("missing request timeout")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		status, text := handler(r, body)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(text)), Header: make(http.Header), Request: r}, nil
	})})
}
func traeTestCredential() Credential {
	return Credential{"accessToken": "mock-access", "refreshToken": "mock-refresh", "uid": "test-user-123", "machineId": "mock-machine", "deviceId": "mock-device", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}
}
func traeTestJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func traeTestJWT(exp int64) string {
	body, _ := json.Marshal(map[string]any{"exp": exp})
	return "e30." + base64.RawURLEncoding.EncodeToString(body) + ".test"
}
func traeTestBody(t *testing.T, p Prepared) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(p.Body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func traeTestNoNetwork(t *testing.T) Adapter {
	return traeTestAdapter(t, func(*http.Request, map[string]any) (int, string) {
		t.Fatal("unexpected upstream request")
		return 500, "{}"
	})
}

func TestTraeLoginMetadataAndTokenRotation(t *testing.T) {
	calls := []string{}
	var login *Login
	a := traeTestAdapter(t, func(r *http.Request, body map[string]any) (int, string) {
		calls = append(calls, r.URL.Path)
		if r.Header.Get("User-Agent") != "Trae/0.1.52" {
			t.Fatal("wrong OAuth user agent")
		}
		switch r.URL.Path {
		case traeExchangePath:
			if body["ClientID"] != login.Data.Text("clientId") || body["RefreshToken"] != "callback-refresh" || body["ClientSecret"] != "-" || body["UserID"] != "" {
				t.Fatal("incorrect exchange body")
			}
			if r.Header.Get("Authorization") != "" {
				t.Fatal("exchange should not send access authorization")
			}
			return 200, `{"Result":{"Token":"new-access","RefreshToken":"rotated-refresh","TokenExpireDuration":3600}}`
		case traeUserPath:
			if r.Header.Get("X-Cloudide-Token") != "new-access" || body["ReqSource"] != "IDE" || body["IDEVersion"] != traeVersion {
				t.Fatal("incorrect user info request")
			}
			return 200, `{"Result":{"UserID":9007199254740993,"ScreenName":"示例账号","EnterpriseID":"tenant","NonPlainTextMobile":"130****1234"}}`
		default:
			t.Fatal("unexpected login endpoint")
			return 500, "{}"
		}
	})
	var err error
	login, err = a.StartLogin(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatal("StartLogin performed a network operation")
	}
	if login.ExpiresAt < time.Now().Add(9*time.Minute).UnixMilli() || login.ExpiresAt > time.Now().Add(11*time.Minute).UnixMilli() || login.Interval != 2 {
		t.Fatal("incorrect login clock units")
	}
	u, err := url.Parse(login.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Host != "www.trae.cn" || u.Path != "/authorization" || q.Get("auth_callback_url") != traeCallback {
		t.Fatal("wrong browser authorization endpoint")
	}
	for _, key := range []string{"login_version", "auth_from", "login_channel", "plugin_version", "auth_type", "client_id", "redirect", "login_trace_id", "machine_id", "device_id", "x_device_id", "x_machine_id", "x_device_brand", "x_device_type", "x_os_version", "x_app_version", "x_app_type"} {
		if q.Get(key) == "" {
			t.Fatalf("missing login parameter %s", key)
		}
	}
	if q.Get("client_id") != login.Data.Text("clientId") || q.Get("login_trace_id") != login.Data.Text("login_trace_id") || login.Data.Text("appVersion") != traeVersion {
		t.Fatal("login metadata not retained")
	}
	second, err := a.StartLogin(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"clientId", "login_trace_id", "machineId", "deviceId"} {
		if second.Data.Text(key) == login.Data.Text(key) {
			t.Fatalf("nonrandom %s", key)
		}
	}
	if credential, done, err := a.PollLogin(context.Background(), login, ""); err != nil || done || credential != nil {
		t.Fatal("empty polling should remain pending")
	}
	callback := url.Values{"refreshToken": {"callback-refresh"}, "login_trace_id": {login.Data.Text("login_trace_id")},
		"userInfo": {url.PathEscape(`{"UserID":9007199254740993,"ScreenName":"测试","TenantID":"callback-tenant"}`)}, "userJwt": {`{"Token":"old-access","RefreshToken":"ignored-refresh"}`}}
	credential, done, err := a.PollLogin(context.Background(), login, traeCallback+"?"+callback.Encode())
	if err != nil || !done {
		t.Fatalf("login failed: %v", err)
	}
	if credential.Text("accessToken") != "new-access" || credential.Text("refreshToken") != "rotated-refresh" || credential.Text("uid") != "9007199254740993" {
		t.Fatal("token rotation or exact uid lost")
	}
	if credential.Text("machineId") != login.Data.Text("machineId") || credential.Text("deviceId") != login.Data.Text("deviceId") || credential.Text("phone") != "130****1234" {
		t.Fatal("identity metadata lost")
	}
	if traeTime(credential["expiresAt"]) < time.Now().Add(59*time.Minute).UnixMilli() {
		t.Fatal("duration not converted to milliseconds")
	}
	if !reflect.DeepEqual(calls, []string{traeExchangePath, traeUserPath}) {
		t.Fatal("wrong login call sequence")
	}
}

func TestTraeMalformedCallbacksNeverReachNetwork(t *testing.T) {
	a := traeTestNoNetwork(t)
	login, err := a.StartLogin(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	cases := []string{
		"not a URL", "https://example.invalid/authorize?refreshToken=secret", "http://127.0.0.1:18081/authorize?refreshToken=secret",
		traeCallback + "?refreshToken=%zz", traeCallback + "?refreshToken=a&refreshToken=b", traeCallback + "?userInfo=%7Bbad&refreshToken=secret",
		traeCallback + "?userJwt=[]", traeCallback + "?code=pkce", traeCallback + "?authCodeInfo=pkce", traeCallback + "?refreshToken=secret&login_trace_id=wrong",
		traeCallback + "?refreshToken=secret&client_id=wrong", traeCallback + "?refreshToken=secret#fragment", traeCallback + "?userInfo=%7B%7D",
	}
	for _, callback := range cases {
		_, done, err := a.PollLogin(context.Background(), login, callback)
		if err == nil || done {
			t.Fatal("malformed callback was accepted")
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "pkce") {
			t.Fatal("callback contents leaked")
		}
	}
	login.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	if _, _, err := a.PollLogin(context.Background(), login, traeCallback+"?refreshToken=secret"); err == nil {
		t.Fatal("expired login accepted")
	}
	if _, err := a.StartLogin(context.Background(), "https://evil.invalid"); err == nil {
		t.Fatal("method was treated as a URL")
	}
}

func TestTraeLoginTokenOnlyAndUserInfoFailure(t *testing.T) {
	for _, withRefresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "access-only", true: "jwt-refresh-fallback"}[withRefresh], func(t *testing.T) {
			calls := 0
			a := traeTestAdapter(t, func(r *http.Request, body map[string]any) (int, string) {
				calls++
				if r.URL.Path == traeExchangePath && withRefresh {
					if body["RefreshToken"] != "jwt-refresh" {
						t.Fatal("userJwt refresh fallback lost")
					}
					return 200, `{"result":{"token":"exchanged","refreshToken":"rotated","tokenExpireDuration":3600}}`
				}
				if r.URL.Path != traeUserPath {
					t.Fatal("unexpected exchange for access-only login")
				}
				return 503, `{"message":"private-token"}`
			})
			login, _ := a.StartLogin(context.Background(), "default")
			jwt := map[string]any{"Token": traeTestJWT(time.Now().Add(time.Hour).Unix())}
			if withRefresh {
				jwt["RefreshToken"] = "jwt-refresh"
			}
			q := url.Values{"userInfo": {`{"UserID":"uid","TenantID":"tenant"}`}, "userJwt": {traeTestJSON(t, jwt)}}
			c, done, err := a.PollLogin(context.Background(), login, traeCallback+"?"+q.Encode())
			if err != nil || !done || c.Text("uid") != "uid" || c.Text("enterpriseId") != "tenant" || c.Text("warning") == "" {
				t.Fatal("valid callback fallback failed")
			}
			if withRefresh && calls != 2 || !withRefresh && calls != 1 {
				t.Fatal("wrong fallback request count")
			}
		})
	}
}

func TestTraeValidateRefreshAndStableIdentity(t *testing.T) {
	original := traeTestCredential()
	original["expiresAt"] = time.Now().Add(20 * time.Second).UnixMilli()
	original["nickname"] = "saved-name"
	original["other"] = map[string]any{"retained": true}
	original["baseURL"] = "https://evil.invalid"
	before := traeTestJSON(t, original)
	calls := 0
	a := traeTestAdapter(t, func(r *http.Request, body map[string]any) (int, string) {
		calls++
		if r.URL.Path != traeExchangePath || body["ClientID"] != traeOAuthClient {
			t.Fatal("wrong refresh protocol")
		}
		return 200, traeTestJSON(t, map[string]any{"Result": map[string]any{"Token": "refreshed", "RefreshToken": "rotated", "TokenExpireAt": time.Now().Add(time.Hour).Unix(), "RefreshExpireAt": time.Now().Add(24 * time.Hour).Unix()}})
	})
	if a.Validate(original) == nil {
		t.Fatal("expiry lead time ignored")
	}
	updated, err := a.Refresh(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || a.Validate(updated) != nil || updated.Text("refreshToken") != "rotated" {
		t.Fatal("refresh failed")
	}
	for _, k := range []string{"uid", "machineId", "deviceId", "nickname", "baseURL"} {
		if updated[k] != original[k] {
			t.Fatalf("refresh changed %s", k)
		}
	}
	if traeTestJSON(t, original) != before {
		t.Fatal("refresh mutated original map")
	}
	if traeTime(updated["refreshExpiresAt"]) < time.Now().Add(23*time.Hour).UnixMilli() {
		t.Fatal("refresh expiry not retained")
	}
	Object(updated["other"])["retained"] = false
	if Object(original["other"])["retained"] != true {
		t.Fatal("refresh shares nested credential data")
	}
	// A JWT may expire before a stale stored expiry; take the earlier bound.
	original["accessToken"] = traeTestJWT(time.Now().Add(-time.Second).Unix())
	original["expiresAt"] = time.Now().Add(time.Hour).UnixMilli()
	if a.Validate(original) == nil {
		t.Fatal("JWT early expiry ignored")
	}
	delete(original, "refreshToken")
	if _, err := a.Refresh(context.Background(), original); err == nil {
		t.Fatal("expired unrefreshable token accepted")
	}
	if calls != 1 {
		t.Fatal("unrefreshable token caused exchange")
	}
	valid := traeTestCredential()
	delete(valid, "refreshToken")
	if _, err := a.Refresh(context.Background(), valid); err != nil {
		t.Fatal("valid nonrefreshable token rejected")
	}
}

func TestTraeRefreshMissingRotationAndSanitizedBusinessErrors(t *testing.T) {
	tests := []struct {
		name, response string
		status         int
		fail           bool
	}{
		{"missing-refresh", `{"Result":{"Token":"new","TokenExpireDuration":3600}}`, 200, false},
		{"business", `{"code":"9074","message":"private-secret"}`, 200, true},
		{"oauth-error", `{"ResponseMetadata":{"Error":{"Code":"InvalidToken","Message":"private-secret"}}}`, 200, true},
		{"missing-token", `{"Result":{}}`, 200, true},
		{"http", `private-secret`, 401, true},
		{"malformed", `private-secret`, 200, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := traeTestAdapter(t, func(*http.Request, map[string]any) (int, string) { return tt.status, tt.response })
			old := traeTestCredential()
			next, err := a.Refresh(context.Background(), old)
			if tt.fail {
				if err == nil {
					t.Fatal("failure accepted")
				}
				if strings.Contains(err.Error(), "private-secret") {
					t.Fatal("secret leaked")
				}
			} else if err != nil || next.Text("refreshToken") != old.Text("refreshToken") {
				t.Fatal("omitted refresh token erased credential")
			}
		})
	}
}

func traeTestCatalog() string {
	return `{"code":0,"data":{"function_configs":[
		{"function":"solo_work_lite","config_info_list":[{"config_name":"dynamic-new","usage":"chat_completion","config_switch":true,"display_config":{"display_name":"Less capable"},"reasoning_effort_config":{"support_thinking":false}}]},
		{"function":"solo_agent","config_info_list":[
			{"config_name":"dynamic-new","usage":"chat_completion","config_switch":true,"display_config":{"display_name":"Dynamic New","multimodal":true,"max_mode":true},"context_window_tokens":{"dev":200000,"max":1000000},"model_detail_list":[{"model_name":"dynamic-new__max","max_tokens":64000},{"model_name":"dynamic-new__dev","max_tokens":32000}],"reasoning_effort_config":{"support_thinking":true,"options":["light",{"level":"high"},{"openclawLevel":"extra_high"}]},"display_contact_config":"{\"consumption_rate\":{\"enable\":true,\"data\":{\"rate\":0}}}"},
			{"config_name":"hidden","is_invisible_to_user":"true"},
			{"config_name":"disabled","config_switch":0},
			{"config_name":"custom","display_config":{"is_custom_model":true}},
			{"config_name":"embedding","usage":"embedding"},
			{"config_name":"unknown-limits","display_config":{"multimodal":false},"reasoning_effort_config":{"support_thinking":false,"options":["high"]}}
		]},
		{"function":"chat","config_info_list":[{"config_name":"uncallable"}]},
		{"function":"solo_design_remote","config_info_list":[{"config_name":"dynamic-new","display_config":{"display_name":"No efforts"}}]}
	]}}`
}

func TestTraeDynamicModelsAndMaxMode(t *testing.T) {
	calls := 0
	a := traeTestAdapter(t, func(r *http.Request, b map[string]any) (int, string) {
		calls++
		if r.URL.Path != traeModelsPath || len(Array(b["functions"])) != 22 || b["agent_type"] != "" || b["access_type"] != float64(0) || b["show_custom_model"] != true {
			t.Fatal("incorrect batch request")
		}
		if Object(b["current_config_info"])["is_custom_model"] != false || r.Header.Get("X-Ide-Token") != "mock-access" || r.Header.Get("Accept") != "application/json" {
			t.Fatal("batch request metadata missing")
		}
		return 200, traeTestCatalog()
	})
	c := traeTestCredential()
	models, err := a.Models(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("unexpected visible model count %d", len(models))
	}
	m := models[0]
	if m.ID != "dynamic-new" || m.Name != "Dynamic New" || m.Function != "solo_agent" || m.ContextWindow != 200000 || m.MaxOutput != 32000 || !m.Vision || !m.Reasoning || !m.MaxMode || m.Multiplier == nil || *m.Multiplier != 0 {
		t.Fatal("dynamic metadata lost or mixed across channels")
	}
	if !reflect.DeepEqual(m.Efforts, []string{"light", "high", "extra_high"}) {
		t.Fatal("dynamic effort options lost")
	}
	unknown := models[1]
	if unknown.ContextWindow != 0 || unknown.MaxOutput != 0 || unknown.Multiplier != nil || unknown.Vision || unknown.Reasoning || len(unknown.Efforts) != 0 {
		t.Fatal("invented model capabilities")
	}
	p, err := a.Prepare(context.Background(), c, Inference{Model: m, Messages: []map[string]any{{"role": "user", "content": "hi"}}, MaxMode: true, MaxTokens: 1000, Effort: "extra_high"})
	if err != nil {
		t.Fatal(err)
	}
	body := traeTestBody(t, p)
	if Object(body["model_auto_selection"])["strategy"] != "max" || body["model_selection_strategy"] != "max" || body["mode_type"] != float64(1) || body["context_window_size"] != float64(1000000) || body["prompt_max_tokens"] != float64(999000) || body["max_tokens"] != float64(1000) {
		t.Fatal("incomplete Max mode parameters")
	}
	if calls != 1 {
		t.Fatal("cached max metadata was not reused")
	}
}

func TestTraeModelFailureHasNoInventedFallback(t *testing.T) {
	for _, response := range []string{`{"function_configs":[]}`, `{}`, `{"code":4001,"function_configs":[{"function":"solo_agent"}]}`} {
		a := traeTestAdapter(t, func(*http.Request, map[string]any) (int, string) { return 200, response })
		models, err := a.Models(context.Background(), traeTestCredential())
		if len(models) != 0 {
			t.Fatal("invented fallback model")
		}
		if response != `{"function_configs":[]}` && err == nil {
			t.Fatal("malformed catalog accepted")
		}
	}
}

func TestTraePrepareHeadersToolsImagesAndHistory(t *testing.T) {
	a := traeTestNoNetwork(t)
	c := traeTestCredential()
	c["baseURL"] = "https://evil.invalid"
	c["api_host"] = "https://evil.invalid"
	history := []map[string]any{
		{"role": "system", "content": "system"},
		{"role": "user", "content": []any{map[string]any{"type": "text", "text": "look"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,dGVzdA=="}}}},
		{"role": "assistant", "content": nil, "reasoning_content": "stored reasoning", "tool_calls": []any{
			map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{\"q\":1}"}},
			map[string]any{"id": "call_2", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}},
		}},
		{"role": "tool", "tool_call_id": "call_1", "content": []any{map[string]any{"type": "text", "text": "result one"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "https://example.invalid/image.png"}}}},
		{"role": "tool", "tool_call_id": "call_2", "content": "result two"},
		{"role": "user", "content": "continue"},
	}
	tools := []map[string]any{{"type": "function", "function": map[string]any{"name": "lookup", "description": "lookup a value", "parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "integer"}}}}}}
	beforeHistory := traeTestJSON(t, history)
	beforeTools := traeTestJSON(t, tools)
	in := Inference{Model: Model{ID: "model-dynamic", Function: "solo_agent", Vision: true, Reasoning: true, Efforts: []string{"high"}, MaxOutput: 32000}, Messages: history, Tools: tools, Effort: "high", MaxTokens: 200000, SessionID: "session"}
	p, err := a.Prepare(context.Background(), c, in)
	if err != nil {
		t.Fatal(err)
	}
	if p.URL != traeAgent+traeChatPath || p.Wire != "trae" {
		t.Fatal("wrong transport or endpoint")
	}
	for k, want := range map[string]string{
		"Accept": "text/event-stream", "Content-Type": "application/json", "Authorization": "Cloud-IDE-JWT mock-access", "X-Cloudide-Token": "mock-access", "X-Ide-Token": "mock-access", "X-Uid": "test-user-123",
		"X-App-Id": "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8", "X-App-Version": "default", "X-Ide-Version": "0.1.52", "X-Ide-Version-Code": "20260811", "X-App-Version-Code": "20260811",
		"X-Ide-Version-Type": "stable", "X-Device-Type": "macos", "X-OS-Version": "macOS 15.7.4", "X-Device-Brand": "Apple", "Request-Traffic-Type": "prod", "X-Machine-Id": "mock-machine", "X-Device-Id": "mock-device", "User-Agent": "Trae/0.1.52",
	} {
		if p.Headers[k] != want {
			t.Fatalf("wrong header %s", k)
		}
	}
	body := traeTestBody(t, p)
	if body["model"] != in.Model.ID || body["config_name"] != in.Model.ID || body["function"] != "solo_agent" || body["stream"] != true || body["reasoning_effort"] != "high" || body["max_tokens"] != float64(32000) || body["tool_choice"] != "auto" {
		t.Fatal("wrong TRAE body")
	}
	ms := Array(body["messages"])
	if len(ms) != 7 || Object(ms[3])["tool_call_id"] != "call_1" || Object(ms[4])["tool_call_id"] != "call_2" || Object(ms[5])["role"] != "user" {
		t.Fatal("tool result relationships/order lost")
	}
	assistant := Object(ms[2])
	calls := Array(assistant["tool_calls"])
	if assistant["content"] != nil || assistant["reasoning_content"] != "stored reasoning" || Object(calls[0])["function"] != nil || Object(Object(calls[0])["function_call"])["name"] != "lookup" {
		t.Fatal("wrong SOLO function_call transformation")
	}
	if Object(Array(Object(ms[0])["content"])[0])["text"] != "system" || Object(Array(Object(ms[5])["content"])[0])["type"] != "image_url" {
		t.Fatal("text/image conversion failed")
	}
	parameters := Object(Object(Array(body["tools"])[0])["function"])["parameters"]
	if s, ok := parameters.(string); !ok || !json.Valid([]byte(s)) {
		t.Fatal("tool parameters not JSON encoded string")
	}
	if traeTestJSON(t, history) != beforeHistory || traeTestJSON(t, tools) != beforeTools {
		t.Fatal("Prepare mutated caller history or tools")
	}
}

func TestTraePrepareRejectsBrokenToolRelationshipsAndCapabilities(t *testing.T) {
	a := traeTestNoNetwork(t)
	model := Model{ID: "m", Function: "solo_agent"}
	call := map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c", "type": "function", "function": map[string]any{"name": "f", "arguments": "{}"}}}}
	for _, messages := range [][]map[string]any{
		{{"role": "tool", "tool_call_id": "orphan", "content": "result"}},
		{call},
		{call, {"role": "user", "content": "interleaved"}, {"role": "tool", "tool_call_id": "c", "content": "result"}},
		{call, {"role": "tool", "tool_call_id": "wrong", "content": "result"}},
		{{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,a"}}}}},
	} {
		if _, err := a.Prepare(context.Background(), traeTestCredential(), Inference{Model: model, Messages: messages}); err == nil {
			t.Fatal("invalid history accepted")
		}
	}
	for _, in := range []Inference{
		{Model: model, MaxMode: true}, {Model: model, Effort: "high"}, {Model: Model{ID: "m", Function: "chat"}},
	} {
		if _, err := a.Prepare(context.Background(), traeTestCredential(), in); err == nil {
			t.Fatal("unsupported capability accepted")
		}
	}
}

func TestTraeCreditsPartialAndUnknownAmounts(t *testing.T) {
	for _, mode := range []string{"missing-usage", "balance-failure", "status-failure", "both-fail"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			var device, market, session, request string
			a := traeTestAdapter(t, func(r *http.Request, b map[string]any) (int, string) {
				calls++
				for _, key := range []string{"X-Market-Client-Id", "X-Market-User-Id", "X-User-Region", "X-Device-Id", "X-Lgw-Req-Sdk-Type", "Package-Type", "X-Lscbd-Aid", "X-Lscbd-Platform", "App-Version", "X-Tt-Trace-Id", "Vscode-Sessionid", "X-Request-Id", "Sec-Fetch-Dest", "Sec-Fetch-Mode", "Sec-Fetch-Site"} {
					if r.Header.Get(key) == "" {
						t.Fatalf("missing checkin header %s", key)
					}
				}
				if r.Header.Get("Authorization") != "Cloud-IDE-JWT mock-access" || r.Header.Get("App-Version") != traeVersion || len(r.Header.Get("X-Device-Id")) != 15 {
					t.Fatal("wrong checkin identity")
				}
				if calls == 1 {
					device = r.Header.Get("X-Device-Id")
					market = r.Header.Get("X-Market-User-Id")
					session = r.Header.Get("Vscode-Sessionid")
					request = r.Header.Get("X-Request-Id")
				} else if device != r.Header.Get("X-Device-Id") || market != r.Header.Get("X-Market-User-Id") || session != r.Header.Get("Vscode-Sessionid") || request == r.Header.Get("X-Request-Id") {
					t.Fatal("unstable device or reused request id")
				}
				if r.URL.Path == traeUsagePath {
					if b["require_usage"] != true || b["req_source"] != float64(2) {
						t.Fatal("wrong usage body")
					}
					if mode == "balance-failure" || mode == "both-fail" {
						return 200, `{"code":"1005","message":"private-secret"}`
					}
					return 200, `{"user_entitlement_pack_list":[{"entitlement_base_info":{"display_desc":"签到奖励","quota":{"credits_limit":150}},"usage":{"credits_amount":25},"expire_time":1900000000},{"entitlement_base_info":{"display_desc":"未知用量","quota":{"credits_limit":500}}}]}`
				}
				if r.URL.Path != traeStatusPath || len(b) != 0 {
					t.Fatal("wrong checkin status body")
				}
				if mode == "status-failure" || mode == "both-fail" {
					return 503, `private-secret`
				}
				return 200, `{"enable":true,"checked_in":false,"credits":150}`
			})
			result, err := a.Credits(context.Background(), traeTestCredential())
			if calls != 2 || result.Warning == "" || strings.Contains(result.Warning, "private-secret") {
				t.Fatal("partial status missing or unsafe")
			}
			if mode == "both-fail" {
				if err == nil {
					t.Fatal("total failure hidden")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "balance-failure" {
				if len(result.Balances) != 0 || len(result.Campaigns) != 1 {
					t.Fatal("failed balance fabricated as zero")
				}
				return
			}
			if len(result.Balances) != 2 || result.Balances[0].Remaining == nil || *result.Balances[0].Remaining != 125 || result.Balances[0].ExpiresAt != 1900000000000 {
				t.Fatal("wrong known balance")
			}
			if result.Balances[1].Total == nil || result.Balances[1].Used != nil || result.Balances[1].Remaining != nil {
				t.Fatal("unknown usage treated as zero")
			}
			if mode == "status-failure" && len(result.Campaigns) != 0 {
				t.Fatal("failed campaign status invented")
			}
		})
	}
}

func TestTraeClaimSingleAttemptAndHonestResult(t *testing.T) {
	for _, mode := range []string{"busy", "success", "unknown-amount", "already", "disabled", "empty-success"} {
		t.Run(mode, func(t *testing.T) {
			claims, statuses := 0, 0
			a := traeTestAdapter(t, func(r *http.Request, body map[string]any) (int, string) {
				if len(body) != 0 {
					t.Fatal("claim/status body must be empty object")
				}
				if r.URL.Path == traeStatusPath {
					statuses++
					if mode == "already" {
						return 200, `{"enable":true,"checked_in":true,"credits":150}`
					}
					if mode == "disabled" {
						return 200, `{"enable":false,"checked_in":false}`
					}
					if statuses > 1 {
						if mode == "unknown-amount" {
							return 503, `private-secret`
						}
						return 200, `{"enable":true,"checked_in":true,"credits":150}`
					}
					return 200, `{"enable":true,"checked_in":false,"credits":150}`
				}
				if r.URL.Path != traeClaimPath {
					t.Fatal("wrong mutation endpoint")
				}
				claims++
				if mode == "busy" {
					return 200, `{"code":"9074","message":"private-secret"}`
				}
				if mode == "empty-success" {
					return 200, `{}`
				}
				return 200, `{"code":0,"message":"success"}`
			})
			result, err := a.Claim(context.Background(), traeTestCredential(), traeCheckinID, "")
			if claims > 1 || strings.Contains(result.Message, "private-secret") {
				t.Fatal("retried claim or leaked secret")
			}
			switch mode {
			case "busy":
				var api *APIError
				if !errors.As(err, &api) || api.Code != "9074" || claims != 1 || result.Claimed || result.AlreadyClaimed || result.Amount != nil || statuses != 1 {
					t.Fatal("9074 not handled honestly")
				}
			case "already":
				if err != nil || claims != 0 || !result.AlreadyClaimed || result.Claimed {
					t.Fatal("already-claimed status wrong")
				}
			case "disabled":
				if err == nil || claims != 0 {
					t.Fatal("disabled claim attempted")
				}
			case "empty-success":
				if err == nil || result.Claimed {
					t.Fatal("empty 200 invented success")
				}
			case "unknown-amount":
				if err != nil || !result.Claimed || result.Amount != nil {
					t.Fatal("unknown reward invented as zero")
				}
			case "success":
				if err != nil || !result.Claimed || result.Amount == nil || *result.Amount != 150 {
					t.Fatal("successful amount not recovered")
				}
			}
		})
	}
}

func TestTraeNeverFollowsCredentialRedirects(t *testing.T) {
	calls := 0
	a := NewTrae(&http.Client{Transport: traeTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 307, Header: http.Header{"Location": []string{"https://example.invalid/steal"}}, Body: io.NopCloser(strings.NewReader("")), Request: r}, nil
	})})
	if _, err := a.Refresh(context.Background(), traeTestCredential()); err == nil || calls != 1 {
		t.Fatal("credential-bearing redirect followed")
	}
}
