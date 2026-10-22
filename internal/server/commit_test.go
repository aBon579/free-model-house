package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aBon579/free-model-house/internal/lane"
)

// thinkThenFailLane 是一个**先思考、然后失败**的车道。
// ⚠️ 这模拟真实场景：思考模型先输出一大段 reasoning，
// 然后上游撞上配额墙。
type thinkThenFailLane struct {
	id string
}

func (t *thinkThenFailLane) ID() string  { return t.id }
func (t *thinkThenFailLane) Ready() bool { return true }
func (t *thinkThenFailLane) ListModels(ctx context.Context) ([]lane.ModelInfo, *lane.Error) {
	return nil, nil
}
func (t *thinkThenFailLane) Status() lane.LaneStatus {
	return lane.LaneStatus{ID: t.id, Ready: true}
}
func (t *thinkThenFailLane) Probe(ctx context.Context, model string) lane.ProbeResult {
	return lane.ProbeResult{Model: model, State: lane.StateAvailable}
}

func (t *thinkThenFailLane) Do(ctx context.Context, turn *lane.Turn, emit func(lane.Chunk)) (*lane.Usage, lane.FinishReason, *lane.Error) {
	// 先输出一大段思考。
	emit(lane.Chunk{Kind: lane.ChunkReasonDelta, Index: 0, Delta: "让我想想这个问题…"})
	emit(lane.Chunk{Kind: lane.ChunkReasonDelta, Index: 0, Delta: "首先我需要考虑…"})
	// 然后失败。
	return nil, lane.FinishError, &lane.Error{
		Code: lane.CodeQuota, Message: "rate limited", RetryAfterSec: 9,
	}
}

// TestLanLightTierLimitsLanKey 验证**轻量档**会限流 LAN Key，
// 但**本机 Key 照常**。
//
// ⚠️ 这就是"局域网推理"档位的准确语义：
// 限流**只对 LAN Key 生效** —— 本机 Key（管理台自己）不受限，
// 否则管理台自己的轮询会被自己的限流打掉。
func TestLanLightTierLimitsLanKey(t *testing.T) {
	gw, keys := newTestGateway(t)
	// 轻量档：每分钟 1 次、并发 1。
	gw.SetLanTier(LanTierLight, 1, 1)

	body := `{"model":"zen/m1","messages":[{"role":"user","content":"hi"}]}`

	// 第一个 LAN 请求 → 200。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+keys.LanKey())
	rec := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("轻量档第一个请求该 200，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 第二个 → 429（超了每分钟 1 次的配额）。
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+keys.LanKey())
	rec2 := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("超限该 429，实际 %d: %s", rec2.Code, rec2.Body.String())
	}

	// ⚠️ **第三个请求也是 429** ——
	//
	//	本机 Key 已砍掉（管理台改用密码登录），
	//	所以现在**所有**推理请求都走同一套限额。
	req3 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req3.Header.Set("Authorization", "Bearer "+keys.LanKey())
	rec3 := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusTooManyRequests {
		t.Fatalf("限额对所有 Key 一致，该 429，实际 %d: %s", rec3.Code, rec3.Body.String())
	}
}

// TestLanDeepTierNoLimit 验证**深度档不限流**。
func TestLanDeepTierNoLimit(t *testing.T) {
	gw, keys := newTestGateway(t)
	gw.SetLanTier(LanTierDeep, 0, 0)

	body := `{"model":"zen/m1","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+keys.LanKey())
		rec := httptest.NewRecorder()
		gw.Routes().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("深度档第 %d 个请求该 200，实际 %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}
}

// TestReasoningDoesNotCommitResponse 验证**思考不会提交响应**。
// ⚠️ 这是延迟提交机制的核心承诺：
//
//	"想了一分钟然后撞上配额墙"时，
//	客户端必须能拿到**真正的 HTTP 429 + Retry-After**。
//
// 如果思考内容触发了 200 的提交，客户端就只看到流内错误，
// 它的退避逻辑（靠 Retry-After）就废了。
func TestReasoningDoesNotCommitResponse(t *testing.T) {
	keys, db := newTestKeys(t)
	_ = db

	registry := lane.NewRegistry()
	registry.Register(&thinkThenFailLane{id: "zen"})
	catalog := lane.NewCatalog()
	catalog.Replace([]lane.ModelInfo{{ID: "m1", Lane: "zen", Enabled: true}})
	router := lane.NewRouter(catalog, registry, lane.NewBook())
	gw := NewGateway(router, catalog, registry, keys)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zen/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+keys.LanKey())
	rec := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec, req)

	// ⚠️ 期望 429（真状态码）而不是 200。
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("思考不应提交响应；期望 429，实际 %d。响应体：%s",
			rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "9" {
		t.Fatalf("Retry-After 应为 9，实际 %q", got)
	}
	// ⚠️ 而且不该有 SSE 帧。
	if strings.Contains(rec.Body.String(), "data: ") {
		t.Fatalf("不该是 SSE 流：%s", rec.Body.String())
	}
}
