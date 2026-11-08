package lane

import (
	"context"
	"strings"
	"testing"
)

// 「同名模型跨车道」的路由语义
//
//	zen/shared 与 kilo/shared 的**裸名相同**。
//	调用方点名其中一个时，必须落到**它所属的车道**，
//	不能被"排序靠前的另一条车道"抢走。
//
// ⚠️ 这正是部署后遇到的 bug：
//	旧实现用裸名匹配（`Model == upstream`），
//	候选链里先出现的那个同名模型会被提到第一位，
//	于是请求被静默路由到别的车道；关掉自动跨模型后直接失败。

// newSameNameRouter 搭两条都有 `shared` 的车道。
func newSameNameRouter(t *testing.T, autoSwitch bool) (*Router, *fakeLane, *fakeLane) {
	t.Helper()
	zen := &fakeLane{id: ZenLaneID, ready: true, text: "ok", emitText: true, behavior: map[string]*Error{}}
	kilo := &fakeLane{id: KiloLaneID, ready: true, text: "ok", emitText: true, behavior: map[string]*Error{}}
	router, catalog := newTestRouter(zen, kilo)
	// ⚠️ 顺序很重要：zen 在前 —— 旧实现会优先抢走 zen 的同名模型。
	catalog.Replace([]ModelInfo{
		{ID: "zen/shared", Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: "kilo/shared", Lane: KiloLaneID, Enabled: true, Free: true},
	})
	router.SetAutoModelSwitch(autoSwitch)
	return router, zen, kilo
}

func sharedTurn(model string) *Turn {
	return &Turn{
		Model:    model,
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}},
	}
}

// TestRequestedModelKeepsExplicitLane 验证**带前缀**的请求落在原车道。
func TestRequestedModelKeepsExplicitLane(t *testing.T) {
	router, zen, kilo := newSameNameRouter(t, false)

	res, uerr := router.Complete(context.Background(), sharedTurn("kilo/shared"),
		func(Chunk) {}, func() bool { return false })
	if uerr != nil {
		t.Fatalf("不该失败：%v", uerr.Message)
	}
	if res.Lane != KiloLaneID {
		t.Fatalf("请求 kilo/shared 却被路由到 %s", res.Lane)
	}
	if got := zen.called(); len(got) != 0 {
		t.Fatalf("不该碰 Zen，实际调了 %v", got)
	}
	if got := kilo.called(); len(got) == 0 {
		t.Fatalf("应该由 Kilo 应答，实际调用记录为空")
	}
}

// TestRequestedModelBareNameUsesCatalogLane 验证**裸名**按目录顺序解析，
// 且只允许那一条车道（关闭自动跨模型时）。
func TestRequestedModelBareNameUsesCatalogLane(t *testing.T) {
	router, zen, kilo := newSameNameRouter(t, false)

	// 目录里 zen/shared 在前 → 原车道就是 Zen。
	res, uerr := router.Complete(context.Background(), sharedTurn("shared"),
		func(Chunk) {}, func() bool { return false })
	if uerr != nil {
		t.Fatalf("不该失败：%v", uerr.Message)
	}
	if res.Lane != ZenLaneID {
		t.Fatalf("裸名应解析到目录第一条（Zen），实际 %s", res.Lane)
	}
	if got := kilo.called(); len(got) != 0 {
		t.Fatalf("不该碰 Kilo，实际调了 %v", got)
	}
	_ = zen
}

// TestRequestedModelMissingFailsClearly 验证原车道不可用时给出**清晰的失败**，
// 而不是悄悄换一条车道。
func TestRequestedModelMissingFailsClearly(t *testing.T) {
	router, zen, kilo := newSameNameRouter(t, false)
	zen.behavior["zen/shared"] = &Error{Code: CodeAbuseBlocked, Message: "出口被风控"}

	_, uerr := router.Complete(context.Background(), sharedTurn("zen/shared"),
		func(Chunk) {}, func() bool { return false })
	if uerr == nil {
		t.Fatal("该失败 —— 关闭时只重试当前模型")
	}
	if !strings.Contains(uerr.Message, "自动跨模型") {
		t.Fatalf("错误信息应说明未开启自动跨模型，实际 %q", uerr.Message)
	}
	if got := kilo.called(); len(got) != 0 {
		t.Fatalf("关闭时不该换到 Kilo，实际调了 %v", got)
	}
}

// TestRequestedModelFallsBackWhenSwitchOn 验证**开启**时仍然会回落到其他车道 ——
// 新增的"原车道优先"不能把既有的回落能力弄丢。
func TestRequestedModelFallsBackWhenSwitchOn(t *testing.T) {
	router, zen, _ := newSameNameRouter(t, true)
	zen.behavior["zen/shared"] = &Error{Code: CodeAbuseBlocked, Message: "出口被风控"}

	res, uerr := router.Complete(context.Background(), sharedTurn("zen/shared"),
		func(Chunk) {}, func() bool { return false })
	if uerr != nil {
		t.Fatalf("开启时应回落到 Kilo，实际失败：%v", uerr.Message)
	}
	if res.Lane != KiloLaneID {
		t.Fatalf("应回落到 Kilo，实际 %s", res.Lane)
	}
}

// TestRestrictToRequestedPrefersExactLane 直接锁住裁剪逻辑的匹配顺序。
func TestRestrictToRequestedPrefersExactLane(t *testing.T) {
	cands := []candidate{
		{Lane: KiloLaneID, Model: "shared", Full: "kilo/shared"},
		{Lane: ZenLaneID, Model: "shared", Full: "zen/shared"},
	}
	got := restrictToRequested(cands, ZenLaneID, "shared")
	if len(got) != 1 || got[0].Lane != ZenLaneID {
		t.Fatalf("应只保留 Zen 的候选，实际 %+v", got)
	}
	got = restrictToRequested(cands, KiloLaneID, "shared")
	if len(got) != 1 || got[0].Lane != KiloLaneID {
		t.Fatalf("应只保留 Kilo 的候选，实际 %+v", got)
	}
}

// **用户真实报障的模型名** —— 不是 `shared` 这种最短例子，
// 而是带 vendor 段与 `:free` 后缀的多段 id。
const realModel = "nvidia/nemotron-3.5-content-safety:free"

// newRealNameRouter 搭 zen/kilo 都有 `realModel` 的场景（zen 排前面）。
func newRealNameRouter(t *testing.T, autoSwitch bool) (*Router, *fakeLane, *fakeLane) {
	t.Helper()
	zen := &fakeLane{id: ZenLaneID, ready: true, text: "ok", emitText: true, behavior: map[string]*Error{}}
	kilo := &fakeLane{id: KiloLaneID, ready: true, text: "ok", emitText: true, behavior: map[string]*Error{}}
	router, catalog := newTestRouter(zen, kilo)
	catalog.Replace([]ModelInfo{
		{ID: FormatModelID(ZenLaneID, realModel), Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: FormatModelID(KiloLaneID, realModel), Lane: KiloLaneID, Enabled: true, Free: true},
	})
	router.SetAutoModelSwitch(autoSwitch)
	return router, zen, kilo
}

// TestRealModelNamePrefixLocksKilo 验证 `kilo/nvidia/…:free` 只走 kilo。
func TestRealModelNamePrefixLocksKilo(t *testing.T) {
	router, zen, kilo := newRealNameRouter(t, false)

	res, uerr := router.Complete(context.Background(), sharedTurn(KiloLaneID+"/"+realModel),
		func(Chunk) {}, func() bool { return false })
	if uerr != nil {
		t.Fatalf("不该失败：%v", uerr.Message)
	}
	if res.Lane != KiloLaneID {
		t.Fatalf("请求 %s/%s 被路由到 %s", KiloLaneID, realModel, res.Lane)
	}
	if got := zen.called(); len(got) != 0 {
		t.Fatalf("zen 上游必须零调用，实际 %v", got)
	}
	if got := kilo.called(); len(got) == 0 || got[0] != realModel {
		t.Fatalf("应由 kilo 上游收到裸名 %s，实际 %v", realModel, got)
	}
	// 解码路径要能说明"前缀锁定"。
	if !strings.Contains(res.Resolution, "前缀锁定") {
		t.Fatalf("解码路径应标明前缀锁定，实际 %q", res.Resolution)
	}
}

// TestPrefixLaneMissingModelNeverFallsToZen 验证 kilo 清单里没有该模型时，
// **关闭自动跨模型**必须明确失败，且 zen 零调用。
func TestPrefixLaneMissingModelNeverFallsToZen(t *testing.T) {
	router, zen, kilo := newRealNameRouter(t, false)
	// Kilo 只剩另一个模型 —— 用户点名的那个在 zen 有、在 kilo 没有。
	router.catalog.Replace([]ModelInfo{
		{ID: FormatModelID(ZenLaneID, realModel), Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: FormatModelID(KiloLaneID, "some-other"), Lane: KiloLaneID, Enabled: true, Free: true},
	})

	_, uerr := router.Complete(context.Background(), sharedTurn(KiloLaneID+"/"+realModel),
		func(Chunk) {}, func() bool { return false })
	if uerr == nil {
		t.Fatal("kilo 没有这个模型时必须失败，不能静默去 zen")
	}
	if !strings.Contains(uerr.Message, KiloLaneID) {
		t.Fatalf("错误信息应点名车道 %s，实际 %q", KiloLaneID, uerr.Message)
	}
	if got := zen.called(); len(got) != 0 {
		t.Fatalf("zen 上游必须零调用，实际 %v", got)
	}
	if got := kilo.called(); len(got) != 0 {
		t.Fatalf("该模型不在 kilo，也不该调用 kilo，实际 %v", got)
	}
	// 候选链里也不允许出现 zen 的同名模型。
	cands := router.BuildCandidates("", KiloLaneID+"/"+realModel, Needs{})
	for _, c := range cands {
		if c.Lane != KiloLaneID {
			t.Fatalf("严格前缀下候选链只能有 kilo，实际出现 %s", c.Lane)
		}
	}
}

// TestPrefixCaseAndSpaceNormalized 验证大小写/多余空格的解析。
//
//	`KILO/...`  → 前缀归一成 kilo
//	`kilo /...` → 同上
//	`kilo/ ...` → 上游名去空白
//	`foo/...`   → 前缀不是已知车道，整串当裸模型（目录里没有 → 明确失败）
func TestPrefixCaseAndSpaceNormalized(t *testing.T) {
	router, _, _ := newRealNameRouter(t, false)

	for _, model := range []string{
		"KILO/" + realModel,
		"kilo /" + realModel,
		"kilo/ " + realModel,
	} {
		res, uerr := router.Complete(context.Background(), sharedTurn(model),
			func(Chunk) {}, func() bool { return false })
		if uerr != nil {
			t.Fatalf("%q 应解析到 kilo，实际失败：%v", model, uerr.Message)
		}
		if res.Lane != KiloLaneID {
			t.Fatalf("%q 应解析到 kilo，实际 %s", model, res.Lane)
		}
	}

	// 未知前缀 `foo/` 不是车道 → 整串当裸模型走目录匹配；
	// 目录里没有**这个整串**（zen 只有 nvidia/…），因此必须明确失败。
	_, uerr := router.Complete(context.Background(), sharedTurn("foo/"+realModel),
		func(Chunk) {}, func() bool { return false })
	if uerr == nil {
		t.Fatal("未知前缀且目录未命中时必须失败")
	}
	if !strings.Contains(uerr.Message, realModel) {
		t.Fatalf("错误信息应带上请求的模型名，实际 %q", uerr.Message)
	}
}

// TestPrefixLaneMissingFallsBackWhenSwitchOn 验证**开启**自动跨模型时，
// 前缀车道缺模型才允许回落到 zen。
func TestPrefixLaneMissingFallsBackWhenSwitchOn(t *testing.T) {
	router, zen, _ := newRealNameRouter(t, true)
	router.catalog.Replace([]ModelInfo{
		{ID: FormatModelID(ZenLaneID, realModel), Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: FormatModelID(KiloLaneID, "some-other"), Lane: KiloLaneID, Enabled: true, Free: true},
	})

	res, uerr := router.Complete(context.Background(), sharedTurn(KiloLaneID+"/"+realModel),
		func(Chunk) {}, func() bool { return false })
	if uerr != nil {
		t.Fatalf("开启时应允许回落到 zen，实际失败：%v", uerr.Message)
	}
	if res.Lane != ZenLaneID {
		t.Fatalf("应回落到 zen，实际 %s", res.Lane)
	}
	if got := zen.called(); len(got) == 0 {
		t.Fatal("zen 应被调用")
	}
}

// TestZenPrefixOnlyTouchesZen 验证 `zen/同名模型` 也只走 zen（两种开关状态）。
func TestZenPrefixOnlyTouchesZen(t *testing.T) {
	for _, auto := range []bool{false, true} {
		router, zen, kilo := newRealNameRouter(t, auto)
		res, uerr := router.Complete(context.Background(), sharedTurn(ZenLaneID+"/"+realModel),
			func(Chunk) {}, func() bool { return false })
		if uerr != nil {
			t.Fatalf("auto=%v: 不该失败：%v", auto, uerr.Message)
		}
		if res.Lane != ZenLaneID {
			t.Fatalf("auto=%v: 请求 zen/%s 却被路由到 %s", auto, realModel, res.Lane)
		}
		if got := kilo.called(); len(got) != 0 {
			t.Fatalf("auto=%v: kilo 上游必须零调用，实际 %v", auto, got)
		}
		if got := zen.called(); len(got) == 0 || got[0] != realModel {
			t.Fatalf("auto=%v: zen 上游应收到裸名 %s，实际 %v", auto, realModel, got)
		}
	}
}

// TestKiloPrefixFirstAttemptWithSwitchOn 验证开启自动跨模型时，
// 第一次尝试仍然落在前缀车道（回落只发生在它失败之后）。
func TestKiloPrefixFirstAttemptWithSwitchOn(t *testing.T) {
	router, zen, kilo := newRealNameRouter(t, true)
	res, uerr := router.Complete(context.Background(), sharedTurn(KiloLaneID+"/"+realModel),
		func(Chunk) {}, func() bool { return false })
	if uerr != nil {
		t.Fatalf("不该失败：%v", uerr.Message)
	}
	if res.Lane != KiloLaneID {
		t.Fatalf("首次尝试应落在 kilo，实际 %s", res.Lane)
	}
	if len(res.Attempted) != 1 || res.Attempted[0].Lane != KiloLaneID {
		t.Fatalf("尝试记录应只有一次且落在 kilo，实际 %+v", res.Attempted)
	}
	if got := zen.called(); len(got) != 0 {
		t.Fatalf("zen 上游必须零调用，实际 %v", got)
	}
	if got := kilo.called(); len(got) == 0 || got[0] != realModel {
		t.Fatalf("kilo 上游应收到裸名 %s，实际 %v", realModel, got)
	}
}

// TestPrefixLaneMissingModelFailsWithReason 验证缺模型时的错误说明
// **车道 + 模型 + 原因**，且失败也记录了落点。
func TestPrefixLaneMissingModelFailsWithReason(t *testing.T) {
	router, zen, _ := newRealNameRouter(t, false)
	router.catalog.Replace([]ModelInfo{
		{ID: FormatModelID(ZenLaneID, realModel), Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: FormatModelID(KiloLaneID, "some-other"), Lane: KiloLaneID, Enabled: true, Free: true},
	})

	res, uerr := router.Complete(context.Background(), sharedTurn(KiloLaneID+"/"+realModel),
		func(Chunk) {}, func() bool { return false })
	if uerr == nil {
		t.Fatal("kilo 缺这个模型时必须失败")
	}
	for _, want := range []string{KiloLaneID, realModel, "不在该车道的可用模型池"} {
		if !strings.Contains(uerr.Message, want) {
			t.Fatalf("错误信息应包含 %q，实际 %q", want, uerr.Message)
		}
	}
	if got := zen.called(); len(got) != 0 {
		t.Fatalf("zen 上游必须零调用，实际 %v", got)
	}
	// ⚠️ 失败也要留下"最后落在哪"，日志才不是"车道=?"。
	if res.Lane != KiloLaneID || res.Model != KiloLaneID+"/"+realModel {
		t.Fatalf("失败落点应记为 kilo/%s，实际 lane=%q model=%q", realModel, res.Lane, res.Model)
	}
	if !strings.Contains(res.Resolution, "该车道没有可用候选") {
		t.Fatalf("解码路径应说明该车道没有候选，实际 %q", res.Resolution)
	}
}

// TestFailureRecordsEveryAttempt 验证失败时每次尝试都有车道/上游/结果。
func TestFailureRecordsEveryAttempt(t *testing.T) {
	router, zen, _ := newRealNameRouter(t, false)
	zen.behavior[ZenLaneID+"/"+realModel] = &Error{Code: CodeAbuseBlocked, Message: "上游风控"}

	res, uerr := router.Complete(context.Background(), sharedTurn(ZenLaneID+"/"+realModel),
		func(Chunk) {}, func() bool { return false })
	if uerr == nil {
		t.Fatal("上游报错时必须失败")
	}
	if len(res.Attempted) == 0 {
		t.Fatal("失败也要记录每一次尝试")
	}
	a := res.Attempted[0]
	if a.Lane != ZenLaneID || a.Model != realModel || a.Full != ZenLaneID+"/"+realModel {
		t.Fatalf("尝试记录不对：%+v", a)
	}
	if !strings.Contains(a.Result, "失败") || !strings.Contains(a.Result, "上游风控") {
		t.Fatalf("尝试结果应带错误摘要，实际 %q", a.Result)
	}
	if res.Lane != ZenLaneID || res.Model != ZenLaneID+"/"+realModel {
		t.Fatalf("失败落点应记为 zen/%s，实际 lane=%q model=%q", realModel, res.Lane, res.Model)
	}
}

// TestBareNamesStillCallable 验证裸名仍能按目录匹配调用（开关两种状态）。
func TestBareNamesStillCallable(t *testing.T) {
	for _, auto := range []bool{false, true} {
		zen := &fakeLane{id: ZenLaneID, ready: true, text: "ok", emitText: true, behavior: map[string]*Error{}}
		kilo := &fakeLane{id: KiloLaneID, ready: true, text: "ok", emitText: true, behavior: map[string]*Error{}}
		router, catalog := newTestRouter(zen, kilo)
		catalog.Replace([]ModelInfo{
			{ID: "nemotron-3.5-super-vl-free", Lane: ZenLaneID, Enabled: true, Free: true},
			{ID: "gpt-4o-mini", Lane: KiloLaneID, Enabled: true, Free: true},
		})
		router.SetAutoModelSwitch(auto)

		for _, tc := range []struct {
			model string
			lane  string
		}{
			{"nemotron-3.5-super-vl-free", ZenLaneID},
			{"gpt-4o-mini", KiloLaneID},
		} {
			res, uerr := router.Complete(context.Background(), sharedTurn(tc.model),
				func(Chunk) {}, func() bool { return false })
			if uerr != nil {
				t.Fatalf("auto=%v: 裸名 %q 应能调用，实际失败：%v", auto, tc.model, uerr.Message)
			}
			if res.Lane != tc.lane {
				t.Fatalf("auto=%v: 裸名 %q 应落到 %s，实际 %s", auto, tc.model, tc.lane, res.Lane)
			}
			got := zen.called()
			if tc.lane == KiloLaneID {
				got = kilo.called()
			}
			if len(got) == 0 || got[0] != tc.model {
				t.Fatalf("auto=%v: %s 上游应收到裸名 %q，实际 %v", auto, tc.lane, tc.model, got)
			}
		}
	}
}
