package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aBon579/free-model-house/internal/lane"
)

// reasonThenTextLane 先输出一段思考，再输出正文。
type reasonThenTextLane struct {
	id string
}

func (t *reasonThenTextLane) ID() string  { return t.id }
func (t *reasonThenTextLane) Ready() bool { return true }
func (t *reasonThenTextLane) ListModels(ctx context.Context) ([]lane.ModelInfo, *lane.Error) {
	return nil, nil
}
func (t *reasonThenTextLane) Status() lane.LaneStatus {
	return lane.LaneStatus{ID: t.id, Ready: true}
}
func (t *reasonThenTextLane) Probe(ctx context.Context, model string) lane.ProbeResult {
	return lane.ProbeResult{Model: model, State: lane.StateAvailable}
}

func (t *reasonThenTextLane) Do(ctx context.Context, turn *lane.Turn, emit func(lane.Chunk)) (*lane.Usage, lane.FinishReason, *lane.Error) {
	emit(lane.Chunk{Kind: lane.ChunkReasonDelta, Index: 0, Delta: "THINKING-PART"})
	emit(lane.Chunk{Kind: lane.ChunkTextDelta, Index: 0, Delta: "ANSWER-PART"})
	emit(lane.Chunk{Kind: lane.ChunkFinish, Finish: lane.FinishStop})
	return &lane.Usage{Output: 5}, lane.FinishStop, nil
}

// TestReasoningIsDeliveredAfterBuffering 验证**缓冲的思考最终会送达**，
// 而且**顺序正确**（思考在前，正文在后）。
//
// ⚠️ 缓冲不能变成"丢掉思考" ——
//
//	客户端（尤其是 Claude Code）依赖思考内容。
func TestReasoningIsDeliveredAfterBuffering(t *testing.T) {
	keys, db := newTestKeys(t)
	_ = db

	registry := lane.NewRegistry()
	registry.Register(&reasonThenTextLane{id: "zen"})
	catalog := lane.NewCatalog()
	catalog.Replace([]lane.ModelInfo{{ID: "m1", Lane: "zen", Enabled: true}})
	router := lane.NewRouter(catalog, registry, lane.NewBook())
	gw := NewGateway(router, catalog, registry, keys)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"zen/m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+keys.LanKey())
	rec := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("该 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "THINKING-PART") {
		t.Fatalf("思考内容丢了：%s", body)
	}
	if !strings.Contains(body, "ANSWER-PART") {
		t.Fatalf("正文丢了：%s", body)
	}
	// ⚠️ 顺序：思考必须在正文**之前**。
	ti := strings.Index(body, "THINKING-PART")
	ai := strings.Index(body, "ANSWER-PART")
	if ti < 0 || ai < 0 || ti > ai {
		t.Fatalf("顺序不对：思考@%d 正文@%d", ti, ai)
	}
}
