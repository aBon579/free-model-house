package lane

import (
	"fmt"
	"testing"
)

// creditFulls 把候选链映射成**带车道前缀**的 id，方便断言"落在哪条车道"。
func creditFulls(cands []candidate) []string {
	out := make([]string, len(cands))
	for i, c := range cands {
		out[i] = c.Full
	}
	return out
}

// creditTestRouter 组一个"免密车道 + credit 池（两个渠道）"的路由器。
//
// ⚠️ credit 模型 id 的渠道段（`qoder` / `trae`）就是回落分组的依据，
//
//	所以这里刻意让 `a` 在两个渠道都存在 —— 同名替补是首选路径。
func creditTestRouter() (*Router, *Catalog) {
	zen := &fakeLane{id: ZenLaneID, ready: true}
	cr := &fakeLane{id: CreditLaneID, ready: true}
	router, catalog := newTestRouter(zen, cr)
	catalog.Replace([]ModelInfo{
		{ID: "zen/z", Lane: ZenLaneID, Enabled: true},
		{ID: "credit/qoder/a", Lane: CreditLaneID, Enabled: true},
		{ID: "credit/qoder/b", Lane: CreditLaneID, Enabled: true},
		{ID: "credit/trae/a", Lane: CreditLaneID, Enabled: true},
		{ID: "credit/trae/c", Lane: CreditLaneID, Enabled: true},
	})
	return router, catalog
}

// ⚠️ **普通回落不参与 credit 车道** ——
// 用户开「自动跨模型调用」时读到的承诺是"Kilo ↔ Zen → OpenRouter"，
// 里面没有"悄悄打到用户自己账号的额度上"。
func TestNormalFallbackExcludesCredit(t *testing.T) {
	router, _ := creditTestRouter()
	got := creditFulls(router.BuildCandidates("", "zen/z", Needs{}))
	want := []string{"zen/z"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("免密车道的回落链里不该有 credit：got %v want %v", got, want)
	}
}

// ⚠️ **credit 请求只在 credit 池里找替补，且只找别的渠道** ——
//
//	额度与风控是渠道内账号级的：同渠道换模型大概率拿到同一个拒绝，
//	白烧一次上游请求（同渠道**换账号**是 `creditlane.Do` 那一层，不受影响）。
//	免密车道绝不借用 —— prompt 会被那些上游记录。
func TestCreditOriginFallsToOtherProvidersOnly(t *testing.T) {
	router, _ := creditTestRouter()
	got := creditFulls(router.BuildCandidates("", "credit/qoder/a", Needs{}))
	want := []string{"credit/qoder/a", "credit/trae/a", "credit/trae/c"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

// ⚠️ **别的渠道的同名模型排在其他模型之前** ——
// 换渠道跑同一个模型，客户端拿到的还是它点名的能力；
// 换成别的模型才是"悄悄改了答案的来源"。
func TestCreditPrefersSameNameAcrossProviders(t *testing.T) {
	router, _ := creditTestRouter()
	got := creditFulls(router.BuildCandidates("", "credit/trae/a", Needs{}))
	// 同渠道的 trae/c 不进链（账号级拒绝会原样再来一次）；
	// 别的渠道里同名替补（qoder/a）排在其他模型（qoder/b）之前。
	want := []string{"credit/trae/a", "credit/qoder/a", "credit/qoder/b"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("同名替补应排最前：got %v want %v", got, want)
	}
}

// ⚠️ **计划槽位是显式授权 —— 压过车道规则**，
// 但只在它**自己的开关**开着时成立。
func TestPlanSlotExemptsCreditFromLaneRules(t *testing.T) {
	router, _ := creditTestRouter()
	router.SetPlanModels([]string{"credit/trae/a"})

	off := creditFulls(router.BuildCandidates("", "zen/z", Needs{}))
	if fmt.Sprint(off) != fmt.Sprint([]string{"zen/z"}) {
		t.Fatalf("计划列表关着时不该有豁免：got %v", off)
	}

	router.SetPlanModelsEnabled(true)
	on := creditFulls(router.BuildCandidates("", "zen/z", Needs{}))
	want := []string{"zen/z", "credit/trae/a"}
	if fmt.Sprint(on) != fmt.Sprint(want) {
		t.Fatalf("计划槽位应被点到名的 credit 模型补上：got %v want %v", on, want)
	}
}

// ⚠️ **「自动跨模型调用」关闭时，credit 请求也只重试自己** ——
// 车道规则是回落期的规则，回落都没开，链里就只剩请求的那个模型。
func TestCreditSingleCandidateWhenSwitchOff(t *testing.T) {
	router, _ := creditTestRouter()
	router.SetAutoModelSwitch(false)
	got := creditFulls(router.BuildCandidates("", "credit/qoder/a", Needs{}))
	if fmt.Sprint(got) != fmt.Sprint([]string{"credit/qoder/a"}) {
		t.Fatalf("got %v", got)
	}
}

// ⚠️ **跳过必须按渠道，不能按车道** ——
// Credit 是一条车道装着三个渠道；Qoder 被拒完全不代表 TRAE 也被拒。
// 照抄整车道跳过会把本次唯一还有希望的替补一起跳过。
func TestSkipRestOfLaneIsPerProviderInCredit(t *testing.T) {
	cands := []candidate{
		{Lane: CreditLaneID, Model: "qoder/a", Full: "credit/qoder/a"},
		{Lane: CreditLaneID, Model: "qoder/b", Full: "credit/qoder/b"},
		{Lane: CreditLaneID, Model: "trae/a", Full: "credit/trae/a"},
		{Lane: ZenLaneID, Model: "z", Full: "zen/z"},
	}
	c := &candidate{Lane: CreditLaneID, Model: "qoder/a", Full: "credit/qoder/a"}
	skipRestOfLane(cands, 0, c)

	if !cands[1].tried {
		t.Fatalf("同渠道的 qoder/b 应被跳过")
	}
	if cands[2].tried {
		t.Fatalf("别的渠道 trae/a 不该被跳过")
	}
	if cands[3].tried {
		t.Fatalf("免密车道不该被 credit 的失败跳过")
	}
}
