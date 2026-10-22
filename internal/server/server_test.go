package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aBon579/free-model-house/internal/lane"
	"github.com/aBon579/free-model-house/internal/store"
)

// timeInPast 返回一个过去的时间（测试用）。
func timeInPast() time.Time { return time.Now().Add(-time.Hour) }

// 钥匙

func newTestKeys(t *testing.T) (*Keys, *store.DB) {
	t.Helper()
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewKeys(db), db
}

// TestKeysHaveFixedPrefix 验证推理 Key 用**固定前缀** `fmh-`。
//
// 用户的要求："项目自己生成一个固定前缀的 API key"。
func TestKeysHaveFixedPrefix(t *testing.T) {
	keys, _ := newTestKeys(t)
	key := keys.LanKey()
	if !strings.HasPrefix(key, "fmh-") {
		t.Errorf("局域网 Key 的前缀不对: %q", key)
	}
	// 主体 40 位 hex。
	body := strings.TrimPrefix(key, "fmh-")
	if len(body) != 40 {
		t.Errorf("主体长度不对: %q", key)
	}
}

// TestKeysPersistAcrossRestart 验证 Key **重启后不变**。
func TestKeysPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	db1, err := store.Open(dir)
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	k1 := NewKeys(db1)
	lan := k1.LanKey()
	db1.Close()

	db2, err := store.Open(dir)
	if err != nil {
		t.Fatalf("重开库失败: %v", err)
	}
	defer db2.Close()
	k2 := NewKeys(db2)
	if k2.LanKey() != lan {
		t.Errorf("局域网 Key 变了: %q → %q", lan, k2.LanKey())
	}
}

// TestRotateLanKeyInvalidatesOld 验证**轮换后旧 Key 立即失效**。
func TestRotateLanKeyInvalidatesOld(t *testing.T) {
	keys, _ := newTestKeys(t)
	lanBefore := keys.LanKey()

	newLan := keys.RotateLanKey()
	if newLan == lanBefore {
		t.Fatal("局域网 Key 该变")
	}
	if !keys.MatchLan(newLan) {
		t.Fatal("新 Key 该能通过校验")
	}
	if keys.MatchLan(lanBefore) {
		t.Fatal("旧 Key 该立即失效")
	}
}

// TestBootstrapTokenIsSingleUse 验证一次性令牌**只能用一次**。
func TestBootstrapTokenIsSingleUse(t *testing.T) {
	keys, _ := newTestKeys(t)
	token := keys.NewBootstrapToken()
	session, ok := keys.RedeemBootstrap(token)
	if !ok {
		t.Fatal("第一次兑换该成功")
	}
	if !keys.ValidSession(session) {
		t.Fatal("会话该有效")
	}
	// ⚠️ 第二次兑换必须失败。
	if _, ok := keys.RedeemBootstrap(token); ok {
		t.Fatal("令牌不该能兑换第二次")
	}
}

// TestBootstrapRejectsWrongToken 验证错的令牌被拒。
func TestBootstrapRejectsWrongToken(t *testing.T) {
	keys, _ := newTestKeys(t)
	keys.NewBootstrapToken()
	if _, ok := keys.RedeemBootstrap("fmh-bogus-token"); ok {
		t.Fatal("错的令牌不该被接受")
	}
}

// TestSessionExpiry 验证会话过期后失效。
func TestSessionExpiry(t *testing.T) {
	keys, _ := newTestKeys(t)
	session := keys.NewSession("test")
	if !keys.ValidSession(session) {
		t.Fatal("新会话该有效")
	}
	// 把它过期。
	keys.mu.Lock()
	keys.sessions[session] = sessionEntry{expires: timeInPast()}
	keys.mu.Unlock()
	if keys.ValidSession(session) {
		t.Fatal("过期的会话该失效")
	}
}

// 推理面

// stubLane 是一个总是成功的假车道。
type stubLane struct {
	id    string
	ready bool
	text  string
}

func (s *stubLane) ID() string  { return s.id }
func (s *stubLane) Ready() bool { return s.ready }
func (s *stubLane) ListModels(ctx context.Context) ([]lane.ModelInfo, *lane.Error) {
	return []lane.ModelInfo{{ID: lane.FormatModelID(s.id, "m1"), Lane: s.id, Enabled: true, Free: true}}, nil
}
func (s *stubLane) Status() lane.LaneStatus {
	return lane.LaneStatus{ID: s.id, Ready: s.ready, ModelCount: 1}
}
func (s *stubLane) Probe(ctx context.Context, model string) lane.ProbeResult {
	return lane.ProbeResult{Model: model, State: lane.StateAvailable}
}
func (s *stubLane) Do(ctx context.Context, turn *lane.Turn, emit func(lane.Chunk)) (*lane.Usage, lane.FinishReason, *lane.Error) {
	emit(lane.Chunk{Kind: lane.ChunkTextDelta, Index: 0, Delta: s.text})
	return &lane.Usage{Input: 10, Output: 5, Total: 15}, lane.FinishStop, nil
}

// newTestGateway 组一个网关。
func newTestGateway(t *testing.T) (*Gateway, *Keys) {
	t.Helper()
	keys, db := newTestKeys(t)
	_ = db
	registry := lane.NewRegistry()
	registry.Register(&stubLane{id: "zen", ready: true, text: "hello"})
	catalog := lane.NewCatalog()
	catalog.Replace([]lane.ModelInfo{
		{ID: lane.FormatModelID("zen", "m1"), Lane: "zen", Enabled: true, Free: true},
	})
	router := lane.NewRouter(catalog, registry, lane.NewBook())
	return NewGateway(router, catalog, registry, keys), keys
}

// TestGatewayRequiresKey 验证**没 Key 一律拒绝**。
func TestGatewayRequiresKey(t *testing.T) {
	gw, keys := newTestGateway(t)
	body := strings.NewReader(`{"model":"zen/m1","messages":[{"role":"user","content":"hi"}]}`)

	// 没带 Key。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	rec := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("没 Key 该 401，实际 %d", rec.Code)
	}

	// 错的 Key。
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zen/m1","messages":[{"role":"user","content":"hi"}]}`))
	req2.Header.Set("Authorization", "Bearer fmh-wrong")
	rec2 := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("错的 Key 该 401，实际 %d", rec2.Code)
	}

	// 正确的 Key。
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zen/m1","messages":[{"role":"user","content":"hi"}]}`))
	req3.Header.Set("Authorization", "Bearer "+keys.LanKey())
	rec3 := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("对的 Key 该 200，实际 %d: %s", rec3.Code, rec3.Body.String())
	}
}

// TestGatewayNonStreamReturnsContent 验证**非流式返回真实内容**。
//
// ⚠️ 这是一个被端到端测试抓出来的 bug：
// 非流式路径曾经从 `turn.Messages`（**请求**）里读答案，
// 结果永远返回空字符串。
// 单元测试测不出来 —— 因为它读的是"请求"，而请求里当然没有答案。
func TestGatewayNonStreamReturnsContent(t *testing.T) {
	gw, keys := newTestGateway(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zen/m1","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+keys.LanKey())
	rec := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("该 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(doc.Choices) == 0 {
		t.Fatal("该有 choices")
	}
	if doc.Choices[0].Message.Content != "hello" {
		t.Fatalf("内容不对: %q", doc.Choices[0].Message.Content)
	}
	if doc.Choices[0].FinishReason != "stop" {
		t.Fatalf("结束原因不对: %q", doc.Choices[0].FinishReason)
	}
}

// TestGatewayNonStreamAnthropicHasContent 验证 Anthropic 线的非流式也有内容。
func TestGatewayNonStreamAnthropicHasContent(t *testing.T) {
	gw, keys := newTestGateway(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"zen/m1","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+keys.LanKey())
	rec := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("该 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var doc struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	found := false
	for _, c := range doc.Content {
		if c.Type == "text" && c.Text == "hello" {
			found = true
		}
	}
	if !found {
		t.Fatalf("该有文本内容，实际 %+v", doc.Content)
	}
}

// TestGatewayAcceptsInferenceKey 验证**推理面认局域网 Key**。
//
// ⚠️ 单端口下这是必须的 —— LAN 里的 agent 拿的就是 LAN Key，
// 而它们访问的同一个 :8678。
func TestGatewayAcceptsInferenceKey(t *testing.T) {
	gw, keys := newTestGateway(t)
	body := `{"model":"zen/m1","messages":[{"role":"user","content":"hi"}]}`

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+keys.LanKey())
	rec := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("局域网 Key 该能调用推理面，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestWrongPasswordCannotLogin 验证**密码不对就登不进去**。
func TestWrongPasswordCannotLogin(t *testing.T) {
	keys, db := newTestKeys(t)
	admin := NewAdmin(keys, lane.NewRegistry(), lane.NewCatalog(), db, nil)
	admin.SetPassword(NewPassword(db))

	body, _ := json.Marshal(map[string]string{"password": "不对的密码"})
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	admin.handleLogin(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("密码不对不该登进去，实际 %d", rec.Code)
	}
}

// TestDefaultPasswordLoginRequiresChange 验证**初始密码登录后必须改密码**。
//
// ⚠️ 用初始密码 `admin` 登录是**成功**的 ——
//
//	但会话被标记成"必须改密码"，
//	管理台会立刻弹「设置新密码」。
//
//	直接拒绝登录的话，用户会一头雾水。
func TestDefaultPasswordLoginRequiresChange(t *testing.T) {
	keys, db := newTestKeys(t)
	admin := NewAdmin(keys, lane.NewRegistry(), lane.NewCatalog(), db, nil)
	pw := NewPassword(db)
	admin.SetPassword(pw)

	if !pw.IsDefault() {
		t.Fatal("新库该处于初始密码状态")
	}
	body, _ := json.Marshal(map[string]string{"password": DefaultAdminPassword})
	req := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	admin.handleLogin(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("初始密码该能登录，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var doc map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &doc)
	if doc["mustChangePassword"] != true {
		t.Fatalf("该标记 mustChangePassword，实际 %v", doc["mustChangePassword"])
	}
	// 应该有会话 Cookie。
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("该设置会话 Cookie")
	}
	if cookies[0].Name != sessionCookieName {
		t.Fatalf("Cookie 名不对: %q", cookies[0].Name)
	}
	if !cookies[0].HttpOnly {
		t.Error("Cookie 必须是 HttpOnly")
	}
}

// TestGatewayAcceptsXApiKey 验证也接受 `x-api-key` 头。
//
// Anthropic SDK 用的是这个。
func TestGatewayAcceptsXApiKey(t *testing.T) {
	gw, keys := newTestGateway(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"zen/m1","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Api-Key", keys.LanKey())
	rec := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("该 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestGatewayModelsEndpoint 验证模型清单可用。
func TestGatewayModelsEndpoint(t *testing.T) {
	gw, keys := newTestGateway(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+keys.LanKey())
	rec := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("该 200，实际 %d", rec.Code)
	}
	var doc struct {
		Object string `json:"object"`
		Data   []struct {
			ID            string `json:"id"`
			Free          bool   `json:"free"`
			CapabilitySrc string `json:"capability_source"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if doc.Object != "list" {
		t.Fatalf("object 不对: %q", doc.Object)
	}
	if len(doc.Data) == 0 {
		t.Fatal("清单不该为空")
	}
	// ⚠️ 模型 id 应该是 `车道/模型` 的形式。
	if !strings.Contains(doc.Data[0].ID, "/") {
		t.Fatalf("模型 id 形状不对: %q", doc.Data[0].ID)
	}
}

// TestGatewayStreamingCommitsLate 验证**延迟提交**。
//
// ⚠️ 这是整个设计的核心机制：
// 上游的 429 + Retry-After 必须能作为**真正的 HTTP 429** 返回。
func TestGatewayStreamingCommitsLate(t *testing.T) {
	keys, db := newTestKeys(t)
	_ = db
	registry := lane.NewRegistry()
	// 一个总是 429 的车道。
	registry.Register(&failingLane{id: "zen", ready: true, err: &lane.Error{
		Code: lane.CodeQuota, Message: "rate limited", RetryAfterSec: 7,
	}})
	catalog := lane.NewCatalog()
	catalog.Replace([]lane.ModelInfo{{ID: lane.FormatModelID("zen", "m1"), Lane: "zen", Enabled: true}})
	router := lane.NewRouter(catalog, registry, lane.NewBook())
	gw := NewGateway(router, catalog, registry, keys)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zen/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+keys.LanKey())
	rec := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec, req)

	// ⚠️ 状态码必须是 429，**不是** 200。
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("该返回 429，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// ⚠️ Retry-After 必须在 —— 客户端的退避逻辑靠它。
	if rec.Header().Get("Retry-After") != "7" {
		t.Fatalf("Retry-After 不对: %q", rec.Header().Get("Retry-After"))
	}
	// ⚠️ 响应体要是错误 JSON，**不是** SSE。
	if strings.Contains(rec.Body.String(), "data: ") {
		t.Fatalf("不该是 SSE 流: %s", rec.Body.String())
	}
}

// failingLane 是一个总是失败的车道。
type failingLane struct {
	id    string
	ready bool
	err   *lane.Error
}

func (f *failingLane) ID() string  { return f.id }
func (f *failingLane) Ready() bool { return f.ready }
func (f *failingLane) ListModels(ctx context.Context) ([]lane.ModelInfo, *lane.Error) {
	return nil, nil
}
func (f *failingLane) Status() lane.LaneStatus {
	return lane.LaneStatus{ID: f.id, Ready: f.ready}
}
func (f *failingLane) Probe(ctx context.Context, model string) lane.ProbeResult {
	return lane.ProbeResult{Model: model, State: lane.StateUnknown}
}
func (f *failingLane) Do(ctx context.Context, turn *lane.Turn, emit func(lane.Chunk)) (*lane.Usage, lane.FinishReason, *lane.Error) {
	return nil, lane.FinishError, f.err
}

// TestCountTokensIsLocalAndHonest 验证 token 计数**不调上游**且**如实标注**。
func TestCountTokensIsLocalAndHonest(t *testing.T) {
	gw, keys := newTestGateway(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		strings.NewReader(`{"model":"zen/m1","messages":[{"role":"user","content":"hello world"}]}`))
	req.Header.Set("Authorization", "Bearer "+keys.LanKey())
	rec := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("该 200，实际 %d", rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if doc["fmh_estimated"] != true {
		t.Fatal("必须标注这是估算 —— 不能谎称精确")
	}
	if _, ok := doc["input_tokens"]; !ok {
		t.Fatal("该有 input_tokens")
	}
}
