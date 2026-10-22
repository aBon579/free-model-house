package lane

import (
	"context"
	"net/http"
	"testing"
)

// newTestHTTPClient 造一个指向无效地址的客户端（必定连不上）。
func newTestHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{}}
}

// TestZenFallbackSetsModelCount 验证**兜底路径也设置 modelCount**。
//
// ⚠️ 「上游源」页面的"模型数"取自 `Status().ModelCount`，
// 而那个字段只在**成功拉取**时被设置。
// 兜底路径漏了设置 → 上游一挂，页面显示"模型数 0"
// 但清单里有 10 个模型 —— 自相矛盾。
func TestZenFallbackSetsModelCount(t *testing.T) {
	z := &ZenLane{
		base:     "http://127.0.0.1:1", // 必然连不上
		sessions: NewSessionMap(),
		client:   newTestHTTPClient(),
	}
	models, uerr := z.ListModels(context.Background())
	if uerr == nil {
		t.Fatal("该拉取失败")
	}
	if len(models) == 0 {
		t.Fatal("拉取失败时清单不能是空的")
	}
	if z.Status().ModelCount != len(models) {
		t.Fatalf("modelCount 该是 %d，实际 %d", len(models), z.Status().ModelCount)
	}
	if z.Status().LastError == "" {
		t.Fatal("该记下错误原因")
	}
}

// TestKiloFallbackSetsModelCount 验证 Kilo 的兜底路径同样设置 modelCount。
func TestKiloFallbackSetsModelCount(t *testing.T) {
	k := &KiloLane{
		baseURL: "http://127.0.0.1:1", // 必然连不上
		client:  newTestHTTPClient(),
	}
	models, uerr := k.ListModels(context.Background())
	if uerr == nil {
		t.Fatal("该拉取失败")
	}
	if len(models) == 0 {
		t.Fatal("拉取失败时清单不能是空的")
	}
	if k.Status().ModelCount != len(models) {
		t.Fatalf("modelCount 该是 %d，实际 %d", len(models), k.Status().ModelCount)
	}
}

// TestOpenRouterFallbackSetsModelCount 验证 OpenRouter 的兜底路径同样设置 modelCount。
func TestOpenRouterFallbackSetsModelCount(t *testing.T) {
	o := &OpenRouterLane{
		baseURL: "http://127.0.0.1:1", // 必然连不上
		client:  newTestHTTPClient(),
	}
	models, uerr := o.ListModels(context.Background())
	if uerr == nil {
		t.Fatal("该拉取失败")
	}
	if len(models) == 0 {
		t.Fatal("拉取失败时清单不能是空的")
	}
	if o.Status().ModelCount != len(models) {
		t.Fatalf("modelCount 该是 %d，实际 %d", len(models), o.Status().ModelCount)
	}
}

// TestFallbackCatalogsAreFreeAndPrefixed 验证**快照里的模型都合法**。
//
// ⚠️ 快照里混进**付费**模型比不显示更糟 ——
//
//	用户点下去得到 401，还以为网关坏了。
func TestFallbackCatalogsAreFreeAndPrefixed(t *testing.T) {
	zen := fallbackCatalog()
	if len(zen) == 0 {
		t.Fatal("zen 快照不能是空的")
	}
	for _, m := range zen {
		if !m.Free {
			t.Fatalf("zen 快照里有非免费模型: %s", m.ID)
		}
		if m.Lane != ZenLaneID {
			t.Fatalf("车道前缀不对: %s", m.ID)
		}
	}

	for _, m := range fallbackKiloCatalog() {
		if !m.Free {
			t.Fatalf("kilo 快照里有非免费模型: %s", m.ID)
		}
		if m.Lane != KiloLaneID {
			t.Fatalf("车道前缀不对: %s", m.ID)
		}
	}

	for _, m := range fallbackOpenRouterCatalog() {
		if !m.Free {
			t.Fatalf("openrouter 快照里有非免费模型: %s", m.ID)
		}
		if m.Lane != OpenRouterLaneID {
			t.Fatalf("车道前缀不对: %s", m.ID)
		}
	}
}
