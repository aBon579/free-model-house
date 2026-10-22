package lane

import (
	"context"
	"testing"
)

// 「自动跨模型调用」的语义
//
//	关闭 —— 只重试**当前模型**。重试仍失败就**直接报错**，
//	        一个模型都不换。
//	开启 —— 允许换模型。切换顺序：
//	        ① Kilo ↔ Zen（都是免密的，不花任何人的钱）
//	        ② 两条免密车道都没有可用模型了 → OpenRouter
//
// ⚠️ 跟旧版的关键区别：
// 旧版"跨车道回落"是**默认允许免密车道之间互切**的
// （只有用 OpenRouter 才要许可）—— 于是**关掉开关也拦不住换模型**。
// 新版是**真开关**：关了就一个模型都不换。

// newPoolRouter 搭一个三车道路由器（zen / Kilo / OpenRouter）。
func newPoolRouter(t *testing.T, autoSwitch bool) (*Router, *fakeLane, *fakeLane, *fakeLane) {
	t.Helper()
	zen := &fakeLane{id: ZenLaneID, ready: true, text: "ok", emitText: true, behavior: map[string]*Error{}}
	kilo := &fakeLane{id: KiloLaneID, ready: true, text: "ok", emitText: true, behavior: map[string]*Error{}}
	or := &fakeLane{
		id: OpenRouterLaneID, ready: true, text: "ok", emitText: true,
		needsCredential: true, behavior: map[string]*Error{},
	}

	router, catalog := newTestRouter(zen, kilo, or)
	catalog.Replace([]ModelInfo{
		{ID: "zen/model-a", Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: "kilo/model-k", Lane: KiloLaneID, Enabled: true, Free: true},
		{ID: "openrouter/model-c", Lane: OpenRouterLaneID, Enabled: true, Free: true},
	})
	router.SetAutoModelSwitch(autoSwitch)
	return router, zen, kilo, or
}

// poolTurn 请求 zen/model-a。
func poolTurn() *Turn {
	return &Turn{
		Model:    "zen/model-a",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}},
	}
}

// TestAutoSwitchOffDoesNotSwitchModel 验证**关闭时一个模型都不换**。
//
// ⚠️ 这是本次语义变更的**核心断言** ——
//
//	zen/model-a 挂了 → **不**换到 kilo/model-k
//	→ **不**换到 openrouter/model-c
//	→ 直接报错
func TestAutoSwitchOffDoesNotSwitchModel(t *testing.T) {
	router, zen, kilo, or := newPoolRouter(t, false)

	zen.behavior["zen/model-a"] = &Error{Code: CodeAbuseBlocked, Message: "出口被风控"}

	_, uerr := router.Complete(context.Background(), poolTurn(), func(Chunk) {}, func() bool { return false })
	if uerr == nil {
		t.Fatal("该失败 —— 关闭时只重试当前模型，不换")
	}
	if len(kilo.called()) != 0 {
		t.Fatalf("关闭时不该换到 Kilo，实际调了 %v", kilo.called())
	}
	if len(or.called()) != 0 {
		t.Fatalf("关闭时不该碰 OpenRouter，实际调了 %v", or.called())
	}
}

// TestAutoSwitchOffRetriesSameModel 验证**关闭时会重试当前模型**。
//
// ⚠️ 用户的原话："遇到当前调用的模型限流或不可用，
// **重试后还是不行**就调用失败不进行模型切换。"
//
// 所以"重试"必须真的发生 —— 不是一撞就报错。
func TestAutoSwitchOffRetriesSameModel(t *testing.T) {
	router, zen, _, _ := newPoolRouter(t, false)

	// 抖动类失败 —— 重试有意义。
	calls := 0
	zen.onCall = func(model string) *Error {
		calls++
		if calls == 1 {
			return &Error{Code: CodeServer, Message: "上游 502"}
		}
		return nil
	}

	res, uerr := router.Complete(context.Background(), poolTurn(), func(Chunk) {}, func() bool { return false })
	if uerr != nil {
		t.Fatalf("重试一次就该成功: %v", uerr)
	}
	if calls < 2 {
		t.Fatalf("该重试同一个模型，实际只调了 %d 次", calls)
	}
	if res.Model != "zen/model-a" {
		t.Fatalf("该还是 zen/model-a，实际 %q", res.Model)
	}
}

// TestAutoSwitchOffGivesUpAfterRetries 验证**重试用尽后报错**。
func TestAutoSwitchOffGivesUpAfterRetries(t *testing.T) {
	router, zen, _, _ := newPoolRouter(t, false)

	zen.behavior["zen/model-a"] = &Error{Code: CodeServer, Message: "上游 502"}

	_, uerr := router.Complete(context.Background(), poolTurn(), func(Chunk) {}, func() bool { return false })
	if uerr == nil {
		t.Fatal("该失败 —— 重试用尽就该报错")
	}
	// 初始 1 次 + 重试 2 次 = 3 次。
	if n := len(zen.called()); n > 4 {
		t.Fatalf("重试不该无限，实际调了 %d 次", n)
	}
}

// TestAutoSwitchOffDoesNotRetryQuota 验证**429 不重试**。
//
// ⚠️ 配额按 session 记账 —— 重试 = 再烧一次。
func TestAutoSwitchOffDoesNotRetryQuota(t *testing.T) {
	router, zen, _, _ := newPoolRouter(t, false)

	zen.behavior["zen/model-a"] = &Error{Code: CodeQuota, Message: "限流", RetryAfterSec: 5}

	_, uerr := router.Complete(context.Background(), poolTurn(), func(Chunk) {}, func() bool { return false })
	if uerr == nil {
		t.Fatal("该失败")
	}
	if n := len(zen.called()); n != 1 {
		t.Fatalf("429 不该重试，实际调了 %d 次", n)
	}
}

// TestAutoSwitchOnSwitchesToKeylessLane 验证**开启时换到免密车道**。
func TestAutoSwitchOnSwitchesToKeylessLane(t *testing.T) {
	router, zen, kilo, or := newPoolRouter(t, true)

	zen.behavior["zen/model-a"] = &Error{Code: CodeAbuseBlocked, Message: "出口被风控"}

	res, uerr := router.Complete(context.Background(), poolTurn(), func(Chunk) {}, func() bool { return false })
	if uerr != nil {
		t.Fatalf("该换到 Kilo: %v", uerr)
	}
	if len(kilo.called()) == 0 {
		t.Fatal("该换到 Kilo")
	}
	if res.Lane != KiloLaneID {
		t.Fatalf("该报 kilo 车道，实际 %q", res.Lane)
	}
	// ⚠️ 免密车道之间不花用户额度 —— **不该**碰 OpenRouter。
	if len(or.called()) != 0 {
		t.Fatalf("不该碰 OpenRouter，实际调了 %v", or.called())
	}
}

// TestAutoSwitchOnPrefersKeylessOverOpenRouter 验证**免密车道优先于 OpenRouter**。
//
// ⚠️ 用户的原话："切换优先级为 kilo、zen 之间切换，
// 这两个车道都没有可用的模型再进行 OR 车道的探测。"
func TestAutoSwitchOnPrefersKeylessOverOpenRouter(t *testing.T) {
	router, zen, kilo, or := newPoolRouter(t, true)

	zen.behavior["zen/model-a"] = &Error{Code: CodeAbuseBlocked, Message: "zen 被风控"}

	res, uerr := router.Complete(context.Background(), poolTurn(), func(Chunk) {}, func() bool { return false })
	if uerr != nil {
		t.Fatalf("该成功: %v", uerr)
	}
	// ⚠️ **必须真的用上了 Kilo** —— 免密车道优先。
	if len(kilo.called()) == 0 {
		t.Fatalf("该优先用免密车道 Kilo，实际调用 zen=%v kilo=%v", zen.called(), kilo.called())
	}
	if res.Lane == OpenRouterLaneID {
		t.Fatal("该优先用免密车道，不该直接落到 OpenRouter")
	}
	if len(or.called()) != 0 {
		t.Fatalf("Kilo 可用时不该碰 OpenRouter，实际调了 %v", or.called())
	}
}

// TestAutoSwitchOnFallsToOpenRouterWhenBothKeylessFail 验证**两条免密都挂了才用 OR**。
func TestAutoSwitchOnFallsToOpenRouterWhenBothKeylessFail(t *testing.T) {
	router, zen, kilo, or := newPoolRouter(t, true)

	zen.behavior["zen/model-a"] = &Error{Code: CodeAbuseBlocked, Message: "zen 被风控"}
	kilo.behavior["kilo/model-k"] = &Error{Code: CodeAbuseBlocked, Message: "Kilo 也被风控"}

	res, uerr := router.Complete(context.Background(), poolTurn(), func(Chunk) {}, func() bool { return false })
	if uerr != nil {
		t.Fatalf("该回落到 OpenRouter: %v", uerr)
	}
	if len(or.called()) == 0 {
		t.Fatal("该回落到 OpenRouter")
	}
	if res.Lane != OpenRouterLaneID {
		t.Fatalf("该报 openrouter 车道，实际 %q", res.Lane)
	}
}

// TestAutoSwitchOnCanSwitchWithinSameLane 验证**同一车道内也能换模型**。
//
// ⚠️ 语义已经是**模型级别**的 —— 不只是换车道。
func TestAutoSwitchOnCanSwitchWithinSameLane(t *testing.T) {
	zen := &fakeLane{id: ZenLaneID, ready: true, text: "ok", emitText: true, behavior: map[string]*Error{}}
	kilo := &fakeLane{id: KiloLaneID, ready: true, text: "ok", emitText: true, behavior: map[string]*Error{}}
	or := &fakeLane{
		id: OpenRouterLaneID, ready: true, text: "ok", emitText: true,
		needsCredential: true, behavior: map[string]*Error{},
	}
	router, catalog := newTestRouter(zen, kilo, or)
	catalog.Replace([]ModelInfo{
		{ID: "zen/model-a", Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: "zen/model-b", Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: "kilo/model-k", Lane: KiloLaneID, Enabled: true, Free: true},
		{ID: "openrouter/model-c", Lane: OpenRouterLaneID, Enabled: true, Free: true},
	})
	router.SetAutoModelSwitch(true)

	zen.behavior["zen/model-a"] = &Error{Code: CodeQuota, Message: "限流", RetryAfterSec: 5}
	zen.behavior["zen/model-b"] = &Error{Code: CodeQuota, Message: "限流", RetryAfterSec: 5}

	res, uerr := router.Complete(context.Background(), poolTurn(), func(Chunk) {}, func() bool { return false })
	if uerr != nil {
		t.Fatalf("该成功: %v", uerr)
	}
	if res.Model == "zen/model-a" {
		t.Fatal("该换一个模型")
	}
}

// TestAutoSwitchStopsAtMaxFailover 验证**换模型不会无限试**。
//
// ⚠️ 这是**安全阀** —— 池子里有几十个模型，
// 逐个试 = 一轮请求打满所有上游的额度。
func TestAutoSwitchStopsAtMaxFailover(t *testing.T) {
	zen := &fakeLane{id: ZenLaneID, ready: true, text: "ok", emitText: true, behavior: map[string]*Error{}}
	kilo := &fakeLane{id: KiloLaneID, ready: true, text: "ok", emitText: true, behavior: map[string]*Error{}}
	or := &fakeLane{
		id: OpenRouterLaneID, ready: true, text: "ok", emitText: true,
		needsCredential: true, behavior: map[string]*Error{},
	}

	router, catalog := newTestRouter(zen, kilo, or)
	// ⚠️ **20 个模型，全部失败**。
	//
	// ⚠️ **裸名必须每条车道都不重样** ——
	//   `fakeLane.lookup` 会按裸名兜底匹配，
	//   所以 "zen/m0" 和 "kilo/m0" 会互相命中。
	models := make([]ModelInfo, 0, 20)
	for i := 0; i < 20; i++ {
		laneID := ZenLaneID
		prefix := "z"
		if i%2 == 1 {
			laneID = KiloLaneID
			prefix = "k"
		}
		id := laneID + "/m" + prefix + string(rune('a'+i/2))
		models = append(models, ModelInfo{ID: id, Lane: laneID, Enabled: true, Free: true})
		zen.behavior[id] = &Error{Code: CodeQuota, Message: "限流", RetryAfterSec: 1}
		kilo.behavior[id] = &Error{Code: CodeQuota, Message: "限流", RetryAfterSec: 1}
	}
	catalog.Replace(models)
	router.SetAutoModelSwitch(true)

	_, uerr := router.Complete(context.Background(), poolTurn(), func(Chunk) {}, func() bool { return false })
	if uerr == nil {
		t.Fatal("该失败")
	}
	// ⚠️ **不该**试完 20 个 —— 上限是 6。
	total := len(zen.called()) + len(kilo.called())
	if total > 6 {
		t.Fatalf("试了 %d 个候选，超出上限 6: zen=%v kilo=%v", total, zen.called(), kilo.called())
	}
	if total < 2 {
		t.Fatalf("该试了不止一个，实际 %d", total)
	}
}

// TestAutoSwitchOnAccountQuotaSkipsToOtherKeylessLane 验证**账号级配额**会直接跳到另一条免密车道。
//
// ⚠️ 这是实测踩到的坑：
//
//	zen 的 "Free usage limit reached" 是**整个账号**的配额，
//	同车道的其他模型**一样**会被拒。
//
//	不处理的话，候选链会把 6 次尝试（maxFailover）
//	**全部烧在 zen 上** —— 结果就是"开着自动跨模型调用，
//	却永远轮不到 Kilo"。
func TestAutoSwitchOnAccountQuotaSkipsToOtherKeylessLane(t *testing.T) {
	router, zen, kilo, _ := newPoolRouter(t, true)

	// ⚠️ **账号级**配额消息 —— 整个 zen 车道都不可用。
	zen.behavior["zen/model-a"] = &Error{
		Code:    CodeQuota,
		Message: "Free usage limit reached. Subscribe to OpenCode Go to keep going",
	}

	res, uerr := router.Complete(context.Background(), poolTurn(), func(Chunk) {}, func() bool { return false })
	if uerr != nil {
		t.Fatalf("该成功（换到 Kilo）: %v", uerr)
	}
	if res.Lane != KiloLaneID {
		t.Fatalf("该换到 Kilo，实际 %q", res.Lane)
	}
	// ⚠️ **不能**把 6 次尝试全烧在 zen 上。
	if len(zen.called()) != 1 {
		t.Fatalf("账号级配额该只试一个 zen 模型，实际试了 %v", zen.called())
	}
	if len(kilo.called()) == 0 {
		t.Fatal("该试过 Kilo")
	}
}

// TestAutoSwitchOnModelQuotaStaysInLane 验证**模型级配额**不跳车道。
//
// ⚠️ 只有**这一个模型**没额度 —— 同车道的其他模型还好着，
//
//	跳过它们就白白浪费了。
func TestAutoSwitchOnModelQuotaStaysInLane(t *testing.T) {
	router, zen, _, _ := newPoolRouter(t, true)

	// ⚠️ **再加一个 zen 模型** —— 同车道要有两个才能看出"跳没跳"。
	router.catalog.Replace([]ModelInfo{
		{ID: "zen/model-a", Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: "zen/model-b", Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: "kilo/model-k", Lane: KiloLaneID, Enabled: true, Free: true},
		{ID: "openrouter/model-c", Lane: OpenRouterLaneID, Enabled: true, Free: true},
	})

	// ⚠️ **模型级**配额 —— 消息里没有账号级的关键词。
	zen.behavior["zen/model-a"] = &Error{
		Code:    CodeQuota,
		Message: "This model has no quota remaining",
	}

	router.Complete(context.Background(), poolTurn(), func(Chunk) {}, func() bool { return false })

	// ⚠️ 同车道的其他模型**该继续试**。
	if len(zen.called()) < 2 {
		t.Fatalf("模型级配额该试完同车道，实际 %v", zen.called())
	}
}

// TestIsAccountQuota 验证**账号级配额**的判据。
func TestIsAccountQuota(t *testing.T) {
	account := []string{
		"Free usage limit reached. Subscribe to OpenCode Go to keep going",
		"Your usage limit reached for today",
		"Daily limit reached",
		"quota exhausted",
		"Too Many Requests",
	}
	for _, m := range account {
		if !isAccountQuota(m) {
			t.Fatalf("该认成账号级配额: %q", m)
		}
	}

	model := []string{
		"This model has no quota remaining",
		"model not available in your country",
		"",
	}
	for _, m := range model {
		if isAccountQuota(m) {
			t.Fatalf("不该认成账号级配额: %q", m)
		}
	}
}
