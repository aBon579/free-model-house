package lane

import (
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
