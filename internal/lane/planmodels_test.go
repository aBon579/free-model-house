package lane

import (
	"fmt"
	"testing"
	"time"
)

func planTestRouter() (*Router, *Catalog) {
	zen := &fakeLane{id: ZenLaneID, ready: true}
	router, catalog := newTestRouter(zen)
	catalog.Replace([]ModelInfo{
		{ID: "zen/a", Lane: ZenLaneID, Enabled: true},
		{ID: "zen/b", Lane: ZenLaneID, Enabled: true},
		{ID: "zen/c", Lane: ZenLaneID, Enabled: true},
		{ID: "zen/d", Lane: ZenLaneID, Enabled: true},
	})
	return router, catalog
}

func modelIDs(cands []candidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Model
	}
	return out
}

func TestPlanModelsOrder(t *testing.T) {
	router, _ := planTestRouter()
	router.SetPlanModelsEnabled(true)
	router.SetPlanModels([]string{"zen/c", "zen/a", "zen/b"})
	cands := router.BuildCandidates("", "zen/d", Needs{})
	got := modelIDs(cands)
	want := []string{"d", "c", "a", "b"}
	if len(got) != 4 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("计划顺序不对：got %v want(prefix) %v", got, want)
	}
}

func TestPlanModelsSkipsUnusable(t *testing.T) {
	router, catalog := planTestRouter()
	router.SetPlanModelsEnabled(true)
	router.SetPlanModels([]string{"zen/c", "zen/b"})
	catalog.SetProbe(ProbeResult{Model: "zen/c", State: StateUnavailable, At: time.Now().UnixMilli()})
	cands := router.BuildCandidates("", "zen/d", Needs{})
	got := modelIDs(cands)
	if got[1] != "b" {
		t.Fatalf("c 不可用时应跳到 b，实际 %v", got)
	}
	for i, m := range got {
		if m == "c" && i < 3 {
			t.Fatalf("c 不应被提前：%v", got)
		}
	}
}

func TestPlanModelsFallsBackWhenAllUnusable(t *testing.T) {
	router, catalog := planTestRouter()
	base := modelIDs(router.BuildCandidates("", "zen/d", Needs{}))
	router.SetPlanModelsEnabled(true)
	router.SetPlanModels([]string{"zen/a", "zen/b", "zen/c"})
	for _, m := range []string{"zen/a", "zen/b", "zen/c"} {
		catalog.SetProbe(ProbeResult{Model: m, State: StateUnavailable, At: time.Now().UnixMilli()})
	}
	got := modelIDs(router.BuildCandidates("", "zen/d", Needs{}))
	if len(got) != len(base) {
		t.Fatalf("长度不同：got %v base %v", got, base)
	}
	for i := range base {
		if got[i] != base[i] {
			t.Fatalf("三槽都不可用时应回落原顺序：got %v base %v", got, base)
		}
	}
}

// ⚠️ **计划列表自己的开关** —— 只填槽位不算数。
//
// 用户要的是"默认按车道优先级切，显式打开才按我点的顺序切"。
// 开关漏掉的话，之前填过的槽位会在打开「自动跨模型调用」时静默生效。
func TestPlanModelsInertUntilEnabled(t *testing.T) {
	router, _ := planTestRouter()
	router.SetPlanModels([]string{"zen/c", "zen/a", "zen/b"})
	off := modelIDs(router.BuildCandidates("", "zen/d", Needs{}))
	wantOff := []string{"d", "a", "b", "c"}
	if fmt.Sprint(off) != fmt.Sprint(wantOff) {
		t.Fatalf("开关关闭时应按池子顺序：got %v want %v", off, wantOff)
	}
	router.SetPlanModelsEnabled(true)
	on := modelIDs(router.BuildCandidates("", "zen/d", Needs{}))
	wantOn := []string{"d", "c", "a", "b"}
	if fmt.Sprint(on) != fmt.Sprint(wantOn) {
		t.Fatalf("开关开启时应按槽位顺序：got %v want %v", on, wantOn)
	}
}

// ⚠️ **「自动跨模型调用」关闭时候选链只有一个模型** ——
// 计划列表开着也一样：回落根本没开，槽位无从谈起。
func TestPlanModelsInertWhenAutoSwitchOff(t *testing.T) {
	router, _ := planTestRouter()
	router.SetAutoModelSwitch(false)
	router.SetPlanModelsEnabled(true)
	router.SetPlanModels([]string{"zen/c", "zen/a", "zen/b"})
	got := modelIDs(router.BuildCandidates("", "zen/d", Needs{}))
	if len(got) != 1 || got[0] != "d" {
		t.Fatalf("只该有请求的模型本身：got %v", got)
	}
}
