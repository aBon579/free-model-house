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

// TestGatewayLightTierLimitsAllKeys 验证**网关级轻量档**会限流**所有** Key。
//
// ⚠️ 网关级非 off 时所有 Key 强制跟随 ——
//
//	不管单 Key 自己是什么档，都按网关的限额走。
//
// light = 10 次/分钟 —— 连打 11 个，第 11 个必须 429。
func TestGatewayLightTierLimitsAllKeys(t *testing.T) {
	gw, keys := newTestGateway(t)
	keys.SetGatewayTier(LanTierLight)

	body := `{"model":"zen/m1","messages":[{"role":"user","content":"hi"}]}`
	doReq := func() int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+keys.LanKey())
		rec := httptest.NewRecorder()
		gw.Routes().ServeHTTP(rec, req)
		return rec.Code
	}

	// 前 10 个该过（桶容量 = 每分钟 10）。
	for i := 0; i < 10; i++ {
		if code := doReq(); code != http.StatusOK {
			t.Fatalf("第 %d 个请求该 200，实际 %d", i+1, code)
		}
	}
	// 第 11 个 → 429。
	if code := doReq(); code != http.StatusTooManyRequests {
		t.Fatalf("第 11 个该 429（超了每分钟 10 次），实际 %d", code)
	}
}

// TestGatewayOffFallsBackToKeyTier 验证**网关级 off** 时按单 Key 档位限流。
//
// ⚠️ 这是两层限流的另一半：
//
//	网关 off → 单 Key 的 tier 接管；
//	两把不同档位的 Key 互不影响。
func TestGatewayOffFallsBackToKeyTier(t *testing.T) {
	gw, keys := newTestGateway(t)
	// 网关级 off —— 单 Key 接管。
	keys.SetGatewayTier(LanTierOff)

	// 主 Key（load 时自动迁移的那把）设成 light：每分钟 10 次。
	lanID := ""
	for _, k := range keys.List() {
		if k.Key == keys.LanKey() {
			lanID = k.ID
		}
	}
	if lanID == "" {
		t.Fatal("找不到默认 Key")
	}
	if _, err := keys.Update(lanID, "", string(LanTierLight), nil); err != nil {
		t.Fatalf("设单 Key 档位失败: %v", err)
	}
	// 再建一把 deep 的 Key。
	other, err := keys.Create("第二把", string(LanTierDeep))
	if err != nil {
		t.Fatalf("建第二把 Key 失败: %v", err)
	}

	body := `{"model":"zen/m1","messages":[{"role":"user","content":"hi"}]}`
	doReq := func(key string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		rec := httptest.NewRecorder()
		gw.Routes().ServeHTTP(rec, req)
		return rec.Code
	}

	// light 那把：前 10 个 200，第 11 个 429。
	for i := 0; i < 10; i++ {
		if code := doReq(keys.LanKey()); code != http.StatusOK {
			t.Fatalf("单 Key light 第 %d 个该 200，实际 %d", i+1, code)
		}
	}
	if code := doReq(keys.LanKey()); code != http.StatusTooManyRequests {
		t.Fatalf("单 Key light 第 11 个该 429，实际 %d", code)
	}

	// deep 那把：light 打满了也不影响它 —— 连打 12 个全 200。
	for i := 0; i < 12; i++ {
		if code := doReq(other.Key); code != http.StatusOK {
			t.Fatalf("单 Key deep 第 %d 个该 200，实际 %d", i+1, code)
		}
	}
}

// TestTierRestoreAfterGatewayUnlock 验证**锁定期间改档位被拒、解锁后原档位恢复**。
func TestTierRestoreAfterGatewayUnlock(t *testing.T) {
	keys, db := newTestKeys(t)

	// 单 Key 设 light。
	lanID := ""
	for _, k := range keys.List() {
		if k.Key == keys.LanKey() {
			lanID = k.ID
		}
	}
	if _, err := keys.Update(lanID, "", string(LanTierLight), nil); err != nil {
		t.Fatalf("设单 Key 档位失败: %v", err)
	}

	// 锁定 —— 网关级切到 standard。
	keys.SetGatewayTier(LanTierStandard)
	// 锁定期间改档位 → 必须被拒。
	if _, err := keys.Update(lanID, "", string(LanTierDeep), nil); err == nil {
		t.Fatal("网关级锁定时改单 Key 档位该被拒")
	}
	// 但档位设置**保留** —— 还是 light。
	got, _, _ := db.GetApiKeyByID(lanID)
	if got.Tier != string(LanTierLight) {
		t.Fatalf("锁定期间单 Key 档位被动了：实际 %s，该还是 light", got.Tier)
	}

	// 解锁 —— 网关级回 off。
	keys.SetGatewayTier(LanTierOff)
	// 解锁后 light 的限额**自动恢复**生效（不需要重设）。
	k1, _, _ := db.GetApiKeyByID(lanID)
	if k1.Tier != string(LanTierLight) {
		t.Fatalf("解锁后该恢复成 light，实际 %s", k1.Tier)
	}
	// 恢复后能正常改。
	if _, err := keys.Update(lanID, "", string(LanTierDeep), nil); err != nil {
		t.Fatalf("解锁后改档位该成功: %v", err)
	}
}

// TestGatewayDeepTierNoLimit 验证**网关级 deep 不限流**。
func TestGatewayDeepTierNoLimit(t *testing.T) {
	gw, keys := newTestGateway(t)
	keys.SetGatewayTier(LanTierDeep)

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
