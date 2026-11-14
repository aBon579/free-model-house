package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aBon579/free-model-house/internal/credit"
	"github.com/aBon579/free-model-house/internal/creditlane"
	"github.com/aBon579/free-model-house/internal/lane"
	"github.com/aBon579/free-model-house/internal/store"
)

// blockedTransport 记录有没有人试图连上游。
//
// ⚠️ Credit 页的路由测试**一律不许真连上游** ——
//
//	上游是用户真实的账号，一次领取/一次推理都会消耗他的额度。
type blockedTransport struct {
	calls atomic.Int64
}

func (b *blockedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	b.calls.Add(1)
	return nil, errors.New("测试里不允许连上游")
}

// newCreditAdmin 建一套带 Credit 管理器的管理面，返回它、总路由和有效会话。
func newCreditAdmin(t *testing.T, withCredit bool) (*Admin, *Server, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	vault, err := store.NewVault(dir)
	if err != nil {
		t.Fatalf("初始化凭据存储失败: %v", err)
	}
	transport := &blockedTransport{}
	keys := NewKeys(db)
	admin := NewAdmin(keys, lane.NewRegistry(), lane.NewCatalog(), db, vault)
	if withCredit {
		m, err := credit.NewManager(db, vault, &http.Client{Transport: transport})
		if err != nil {
			t.Fatalf("建 Credit 管理器失败: %v", err)
		}
		admin.SetCredit(m)
	}
	s := NewServer(Options{DataDir: dir}, NewGateway(nil, nil, nil, keys), keys)
	s.SetAdmin(admin)
	return admin, s, keys.NewSession("test")
}

// creditRequest 打一次总路由（照真实浏览器那样带会话 Cookie）。
func creditRequest(t *testing.T, s *Server, session, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, nil)
	} else {
		req = httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// decodeCredit 解出响应 JSON。
func decodeCredit(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("响应不是 JSON（%d）：%s", rec.Code, rec.Body.String())
	}
	return doc
}

// TestCreditRoutesRequireSession 验证 Credit 路由**没有匿名入口**。
//
// ⚠️ 它能读出上游账号的 uid、指纹和余额，所以必须先有管理会话。
func TestCreditRoutesRequireSession(t *testing.T) {
	_, s, _ := newCreditAdmin(t, true)
	for _, path := range []string{"/api/credit", "/api/credit/accounts", "/api/credit/login"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		s.handleAdmin(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s 没有会话也该拦掉，实际 %d：%s", path, rec.Code, rec.Body.String())
		}
	}
}

// TestCreditStateAndAddAccount 验证快照与「添加账户」走得通，且一次上游都不碰。
func TestCreditStateAndAddAccount(t *testing.T) {
	admin, s, session := newCreditAdmin(t, true)

	rec := creditRequest(t, s, session, http.MethodGet, "/api/credit", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("读快照失败：%d %s", rec.Code, rec.Body.String())
	}
	doc := decodeCredit(t, rec)
	providers, _ := doc["providers"].([]any)
	if len(providers) != 3 {
		t.Fatalf("该有 3 个渠道，实际 %d：%s", len(providers), rec.Body.String())
	}
	if accounts, _ := doc["accounts"].([]any); len(accounts) != 0 {
		t.Fatalf("新库不该有账户：%v", accounts)
	}

	rec = creditRequest(t, s, session, http.MethodPost, "/api/credit/accounts",
		`{"provider":"qoder","name":"我的账号","credential":{"accessToken":"at-secret","uid":"uid-1","machineId":"mid-1"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("添加账户失败：%d %s", rec.Code, rec.Body.String())
	}
	account := decodeCredit(t, rec)["account"].(map[string]any)
	if account["id"] == "" || account["name"] != "我的账号" {
		t.Fatalf("返回的账户不对：%v", account)
	}
	// ⚠️ 只回指纹，绝不回显凭据明文。
	if _, ok := account["credential"]; ok {
		t.Error("响应里不该出现 credential 字段")
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("at-secret")) {
		t.Error("响应泄漏了凭据明文")
	}

	rec = creditRequest(t, s, session, http.MethodGet, "/api/credit", "")
	if accounts, _ := decodeCredit(t, rec)["accounts"].([]any); len(accounts) != 1 {
		t.Fatalf("添加后快照该有 1 个账户，实际 %v", accounts)
	}

	if got := admin.credit.Export(); len(got) != 1 {
		t.Fatalf("账户该已落进管理器，实际 %d 个", len(got))
	}
	if calls := s.admin.credit.HTTPClient().Transport.(*blockedTransport).calls.Load(); calls != 0 {
		t.Fatalf("添加账户不该碰上游，实际请求 %d 次", calls)
	}
}

// TestCreditExportNeedsConfirm 验证导出凭据必须显式确认，且确认后才给全量。
func TestCreditExportNeedsConfirm(t *testing.T) {
	_, s, session := newCreditAdmin(t, true)
	rec := creditRequest(t, s, session, http.MethodPost, "/api/credit/export", `{"confirm":false}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("没确认不该导出，实际 %d：%s", rec.Code, rec.Body.String())
	}

	creditRequest(t, s, session, http.MethodPost, "/api/credit/accounts",
		`{"provider":"qoder","name":"导出用","credential":{"accessToken":"at-secret","uid":"uid-1","machineId":"mid-1"}}`)
	rec = creditRequest(t, s, session, http.MethodPost, "/api/credit/export", `{"confirm":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("确认后该能导出：%d %s", rec.Code, rec.Body.String())
	}
	doc := decodeCredit(t, rec)
	if doc["format"] != "fmh-credit" || doc["version"] != float64(1) {
		t.Fatalf("备份格式不对：%v", doc)
	}
	if accounts, _ := doc["accounts"].([]any); len(accounts) != 1 {
		t.Fatalf("备份该含 1 个账户，实际 %v", accounts)
	}
}

// TestCreditTestActionCallsNoUpstream 验证「手动测试」在车道缺席时**不假装成功**。
func TestCreditTestActionCallsNoUpstream(t *testing.T) {
	_, s, session := newCreditAdmin(t, true)
	creditRequest(t, s, session, http.MethodPost, "/api/credit/accounts",
		`{"provider":"qoder","name":"测试用","credential":{"accessToken":"at-secret","uid":"uid-1","machineId":"mid-1"}}`)
	rec := creditRequest(t, s, session, http.MethodPost, "/api/credit/accounts/action",
		`{"id":"does-not-matter","action":"test","model":"qmodel"}`)
	doc := decodeCredit(t, rec)
	if doc["ok"] != false {
		t.Fatalf("车道没注册不该返回 ok=true：%v", doc)
	}
	if doc["message"] == "" || doc["message"] == nil {
		t.Error("要说清楚为什么没做")
	}
	if calls := s.admin.credit.HTTPClient().Transport.(*blockedTransport).calls.Load(); calls != 0 {
		t.Fatalf("手动测试在车道缺席时不该碰上游，实际请求 %d 次", calls)
	}
}

// TestCreditTestBadTargetHitsNoUpstream 验证点错了卡或模型时**一次上游都不发**。
//
// ⚠️ 这条路径是要紧的：手动测试**花的是用户自己的额度**。
//
//	如果"模型不属于这张卡"也要先打一发才知道，用户就为一次误点付了钱。
func TestCreditTestBadTargetHitsNoUpstream(t *testing.T) {
	admin, s, session, transport := newCreditLaneAdmin(t)
	rec := creditRequest(t, s, session, http.MethodPost, "/api/credit/accounts",
		`{"provider":"qoder","name":"测试用","credential":{"accessToken":"at-secret","uid":"uid-1","machineId":"mid-1"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("添加账户失败：%d %s", rec.Code, rec.Body.String())
	}
	account, _ := decodeCredit(t, rec)["account"].(map[string]any)
	id, _ := account["id"].(string)
	if id == "" {
		t.Fatalf("响应里没有账户 id：%v", account)
	}

	for _, body := range []string{
		`{"id":"查无此号","action":"test","model":"qmodel"}`,
		`{"id":"` + id + `","action":"test","model":"查无此模型"}`,
	} {
		rec = creditRequest(t, s, session, http.MethodPost, "/api/credit/accounts/action", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s → 该在打上游之前就说清选错了，实际 %d：%s", body, rec.Code, rec.Body.String())
		}
		if doc := decodeCredit(t, rec); doc["ok"] != false || doc["message"] == "" || doc["message"] == nil {
			t.Errorf("响应要给出能看懂的理由：%v", doc)
		}
		if calls := transport.calls.Load(); calls != 0 {
			t.Fatalf("选错目标不该消耗额度，实际请求上游 %d 次", calls)
		}
	}
	if admin.credit == nil {
		t.Fatal("管理器该已装上")
	}
}

// newCreditLaneAdmin 与 newCreditAdmin 同构，但把 credit 车道注册进路由表 ——
// 「手动测试」要有车道才走得到"发一次真实推理"那一步。
func newCreditLaneAdmin(t *testing.T) (*Admin, *Server, string, *blockedTransport) {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(dir)
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	vault, err := store.NewVault(dir)
	if err != nil {
		t.Fatalf("初始化凭据存储失败: %v", err)
	}
	transport := &blockedTransport{}
	keys := NewKeys(db)
	registry, catalog := lane.NewRegistry(), lane.NewCatalog()
	admin := NewAdmin(keys, registry, catalog, db, vault)
	m, err := credit.NewManager(db, vault, &http.Client{Transport: transport})
	if err != nil {
		t.Fatalf("建 Credit 管理器失败: %v", err)
	}
	admin.SetCredit(m)
	registry.Register(creditlane.New(m))
	s := NewServer(Options{DataDir: dir}, NewGateway(nil, nil, nil, keys), keys)
	s.SetAdmin(admin)
	return admin, s, keys.NewSession("test"), transport
}

// TestCreditUpdateAutoClaimAt 验证每账号的领取时刻**在路由这一层**的三种结论：
//
// ⚠️ 早于渠道刷新点的时刻必须是**看得懂的 400**，不能是 502/500 那种
//
//	"服务器出错了" —— 页面上的错误文案直接来自这里，用户要能明白是时间填早了。
func TestCreditUpdateAutoClaimAt(t *testing.T) {
	_, s, session := newCreditAdmin(t, true)
	rec := creditRequest(t, s, session, http.MethodPost, "/api/credit/accounts",
		`{"provider":"qoder","name":"时刻用","credential":{"accessToken":"at-secret","uid":"uid-1","machineId":"mid-1"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("添加账户失败：%d %s", rec.Code, rec.Body.String())
	}
	id := decodeCredit(t, rec)["account"].(map[string]any)["id"].(string)

	rec = creditRequest(t, s, session, http.MethodPost, "/api/credit/accounts/update",
		`{"id":"`+id+`","autoClaimAt":"09:00"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("Qoder 10:00 前领不到，该拒绝并说清理由，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if msg, _ := decodeCredit(t, rec)["error"].(string); msg == "" {
		t.Error("拒绝要有能显示给用户的原因")
	}

	for _, value := range []string{"10:30", ""} {
		rec = creditRequest(t, s, session, http.MethodPost, "/api/credit/accounts/update",
			`{"id":"`+id+`","autoClaimAt":"`+value+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("时刻 %q 该接受，实际 %d：%s", value, rec.Code, rec.Body.String())
		}
		rec = creditRequest(t, s, session, http.MethodGet, "/api/credit", "")
		accounts, _ := decodeCredit(t, rec)["accounts"].([]any)
		account, _ := accounts[0].(map[string]any)
		if got, _ := account["autoClaimAt"].(string); got != value {
			t.Fatalf("快照里的时刻 = %q，期望 %q", got, value)
		}
	}
	if calls := s.admin.credit.HTTPClient().Transport.(*blockedTransport).calls.Load(); calls != 0 {
		t.Fatalf("设置时刻不该碰上游，实际请求 %d 次", calls)
	}
}

// TestCreditUnknownSubroute 验证拼错的子路径是 404 而不是被当成快照。
func TestCreditUnknownSubroute(t *testing.T) {
	_, s, session := newCreditAdmin(t, true)
	rec := creditRequest(t, s, session, http.MethodGet, "/api/credit/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知子路径该 404，实际 %d：%s", rec.Code, rec.Body.String())
	}
}

// TestCreditDisabledSaysSo 验证 Credit 没启用时页面拿到的是明确原因。
func TestCreditDisabledSaysSo(t *testing.T) {
	_, s, session := newCreditAdmin(t, false)
	rec := creditRequest(t, s, session, http.MethodGet, "/api/credit", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("未启用该 503，实际 %d：%s", rec.Code, rec.Body.String())
	}
	if doc := decodeCredit(t, rec); doc["error"] == nil || doc["error"] == "" {
		t.Error("未启用也要说明原因")
	}
}
