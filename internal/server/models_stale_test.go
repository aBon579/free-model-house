package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aBon579/free-model-house/internal/lane"
	"github.com/aBon579/free-model-house/internal/store"
)

// TestHandleModelsShowsStaleAsUnknown —— 端到端：
//
//	handleModels 走 catalog.Models()（带过期判定）。
//	复刻用户场景：手动探过的 OR 模型（间隔 15 分钟），
//	间隔内显示"可用"，过期后必须显示"未探测"。
//
// 用户实测：02:14 手探一次 OR 模型，之后状态一直挂着"可用"。
// 若本测试 PASS，问题就在"NAS 跑的还是旧二进制"；
// 若 FAIL，后端真有 bug。
func TestHandleModelsShowsStaleAsUnknown(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer db.Close()

	catalog := lane.NewCatalog()
	catalog.Replace([]lane.ModelInfo{{ID: "openrouter/m1", Lane: lane.OpenRouterLaneID, Free: true}})
	catalog.SetProbeStaleness(15 * time.Minute)

	admin := NewAdmin(NewKeys(db), lane.NewRegistry(), catalog, db, nil)

	// ① 14 分钟前的手探结论 —— 间隔内，显示"可用"。
	catalog.ApplyProbe(lane.ProbeResult{
		Model: "openrouter/m1",
		State: lane.StateAvailable,
		At:    time.Now().Add(-14 * time.Minute).UnixMilli(),
	})
	if st := modelsStateOf(t, admin); st != string(lane.StateAvailable) {
		t.Fatalf("间隔内该显示可用，实际 %q", st)
	}

	// ② 16 分钟前的结论 —— 已过期，必须显示"未探测"。
	catalog.ApplyProbe(lane.ProbeResult{
		Model: "openrouter/m1",
		State: lane.StateAvailable,
		At:    time.Now().Add(-16 * time.Minute).UnixMilli(),
	})
	if st := modelsStateOf(t, admin); st != string(lane.StateUnknown) {
		t.Fatalf("过期后该显示未探测，实际 %q —— 后端真有 bug", st)
	}

	// ③ 侧栏诊断字段：过期后 ProbedAt 保留（区分"过期"与"没探过"）。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	admin.handleModels(rec, req)
	var out struct {
		Models []struct {
			ID       string `json:"id"`
			ProbedAt int64  `json:"probedAt"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	for _, m := range out.Models {
		if m.ID == "openrouter/m1" && m.ProbedAt == 0 {
			t.Fatal("过期后 ProbedAt 该保留（区分『过期』与『从没探过』）")
		}
	}
}

// modelsStateOf 调 handleModels 抽指定模型的状态。
func modelsStateOf(t *testing.T, a *Admin) string {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/models", nil)
	a.handleModels(rec, req)
	var out struct {
		Models []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	for _, m := range out.Models {
		if m.ID == "openrouter/m1" {
			return m.State
		}
	}
	t.Fatal("响应里没有这个模型")
	return ""
}
