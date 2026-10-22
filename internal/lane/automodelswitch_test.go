package lane

import (
	"context"
	"testing"
)

// 「自动跨模型调用」
//
// ⚠️ 这是**整个设计里最关键、也最容易写错**的一环。
//
// 核心事实：
//
//	免密车道的失败分两类：
//	  ① **模型级**（限流、区域限制、模型不存在、凭据不对）
//	     → 换模型有用
//	  ② **车道级**（上游闸门拒绝、出口 IP 风控）
//	     → 换模型**完全没用**，必须换车道
//
// 换到 OpenRouter 意味着用**用户自己的额度**，
// 所以整个功能**默认关闭**，要用户显式开。
//
// ⚠️ **关闭时**（`autoModelSwitch == false`）：
// 只重试**当前模型**，重试仍失败就报错 —— **一个模型都不换**。

// newAutoSwitchRouter 搭一个两条车道的路由器。
func newAutoSwitchRouter(t *testing.T, zenErr, orErr *Error, autoSwitch bool) (*Router, *fakeLane, *fakeLane) {
	t.Helper()
	zen := &fakeLane{id: ZenLaneID, ready: true, text: "ok", emitText: true, behavior: map[string]*Error{}}
	or := &fakeLane{id: OpenRouterLaneID, ready: true, text: "ok", emitText: true, needsCredential: true, behavior: map[string]*Error{}}
	if zenErr != nil {
		zen.behavior["zen/model-a"] = zenErr
		zen.behavior["zen/model-b"] = zenErr
	}
	if orErr != nil {
		or.behavior["openrouter/model-c"] = orErr
	}

	router, catalog := newTestRouter(zen, or)
	catalog.Replace([]ModelInfo{
		{ID: "zen/model-a", Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: "zen/model-b", Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: "openrouter/model-c", Lane: OpenRouterLaneID, Enabled: true, Free: true},
	})
	router.SetAutoModelSwitch(autoSwitch)
	return router, zen, or
}

// crossTurn 建一个最简单的请求。
func crossTurn() *Turn {
	return &Turn{
		Model:    "zen/model-a",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}},
	}
}

// TestAutoSwitchFallsBackWhenZenBlocked 验证**免密被封时回落到 OpenRouter**。
//
// ⚠️ 这是「自动跨模型调用」**存在的唯一理由** ——
//
//	车道级封禁时，换模型完全没用。
func TestAutoSwitchFallsBackWhenZenBlocked(t *testing.T) {
	router, zen, or := newAutoSwitchRouter(t,
		&Error{Code: CodeAbuseBlocked, Message: "出口被风控"},
		nil, true)

	res, uerr := router.Complete(context.Background(), crossTurn(), func(Chunk) {}, func() bool { return false })

	if uerr != nil {
		t.Fatalf("不该失败: %v", uerr)
	}
	if res == nil || res.Usage == nil {
		t.Fatal("该有结果")
	}
	// ⚠️ **同车道的模型不该一个个试** ——
	//   车道级封禁对所有模型一视同仁。
	if len(zen.called()) > 1 {
		t.Fatalf("车道级失败不该逐个试模型，实际试了 %v", zen.called())
	}
	// 关键：真的回落到 OpenRouter 了。
	if len(or.called()) == 0 {
		t.Fatal("该回落到 OpenRouter")
	}
	// ⚠️ **车道收到的是裸名** —— 上游只认 `model-c`，
	//   车道前缀（`openrouter/`）是我们自己加的。
	if or.called()[0] != "model-c" {
		t.Fatalf("该用 model-c，实际 %q", or.called()[0])
	}
	// ⚠️ **但对外报告的是全名** —— 用户看到的是"这个模型"。
	if res.Model != "openrouter/model-c" {
		t.Fatalf("该报 openrouter/model-c，实际 %q", res.Model)
	}
}

// TestAutoSwitchDisabledDoesNotFallBack 验证**默认关闭时不回落**。
//
// ⚠️ 这是**默认行为** ——
//
//	换模型会消耗**用户自己的** OpenRouter 额度，
//	不能偷偷替用户决定。
func TestAutoSwitchDisabledDoesNotFallBack(t *testing.T) {
	router, _, or := newAutoSwitchRouter(t,
		&Error{Code: CodeAbuseBlocked, Message: "出口被风控"},
		nil, false)

	_, uerr := router.Complete(context.Background(), crossTurn(), func(Chunk) {}, func() bool { return false })

	if uerr == nil {
		t.Fatal("该失败")
	}
	// ⚠️ **绝不能**碰 OpenRouter。
	if len(or.called()) != 0 {
		t.Fatalf("没开自动跨模型调用就不该碰 OpenRouter，实际调了 %v", or.called())
	}
	// 错误要如实反映"被风控"。
	if uerr.Code != CodeAbuseBlocked {
		t.Fatalf("错误码该是 abuse-blocked，实际 %q", uerr.Code)
	}
}

// TestAutoSwitchModelLevelFailureStillTriesSameLane 验证**模型级失败仍会试同车道**。
//
// ⚠️ 限流是**模型级**的 ——
//
//	换同车道的另一个模型**有用**，不能跳过。
func TestAutoSwitchModelLevelFailureStillTriesSameLane(t *testing.T) {
	router, zen, _ := newAutoSwitchRouter(t,
		&Error{Code: CodeQuota, Message: "限流", RetryAfterSec: 10},
		nil, true)

	router.Complete(context.Background(), crossTurn(), func(Chunk) {}, func() bool { return false })

	// ⚠️ **同车道的两个模型都要试** —— 限流是模型级的。
	if len(zen.called()) < 2 {
		t.Fatalf("限流是模型级的，该试完同车道，实际 %v", zen.called())
	}
}

// TestAutoSwitchErrorPreservesRetryAfter 验证**最终错误保留 Retry-After**。
//
// ⚠️ 客户端的退避逻辑靠它 ——
//
//	丢了它，客户端只能瞎猜（一秒后重试 → 再被拒）。
func TestAutoSwitchErrorPreservesRetryAfter(t *testing.T) {
	router, _, _ := newAutoSwitchRouter(t,
		&Error{Code: CodeQuota, Message: "限流", RetryAfterSec: 42},
		nil, false)

	_, uerr := router.Complete(context.Background(), crossTurn(), func(Chunk) {}, func() bool { return false })

	if uerr == nil {
		t.Fatal("该失败")
	}
	if uerr.RetryAfterSec != 42 {
		t.Fatalf("Retry-After 该是 42，实际 %d", uerr.RetryAfterSec)
	}
}

// TestAutoSwitchSkipsLaneOnGateRejection 验证**闸门拒绝时跳过同车道**。
//
// ⚠️ `CodeFreeTier` 是**闸门拒绝** ——
//
//	指纹不对，整个车道都进不去。
func TestAutoSwitchSkipsLaneOnGateRejection(t *testing.T) {
	router, zen, or := newAutoSwitchRouter(t,
		&Error{Code: CodeFreeTier, Message: "FreeTierError: 闸门拒绝"},
		nil, true)

	_, uerr := router.Complete(context.Background(), crossTurn(), func(Chunk) {}, func() bool { return false })
	if uerr != nil {
		t.Fatalf("该回落到 OR 成功: %v", uerr)
	}
	// ⚠️ 闸门拒绝 → 同车道只试**一个**。
	if len(zen.called()) != 1 {
		t.Fatalf("闸门拒绝该只试一个模型，实际 %v", zen.called())
	}
	if len(or.called()) == 0 {
		t.Fatal("该回落到 OpenRouter")
	}
}

// TestAutoSwitchCredentialErrorIsModelLevel 验证**凭据错误是模型级的**。
//
// ⚠️ `401 Missing API key` 在免密车道上有**特殊含义**：
// 上游发现 `Bearer public` **不适用于这个模型** ——
// 也就是这个模型需要用户自己的 key。
//
// 那换模型**是有用的**（别的免密模型可能还是免费的），
// 所以它必须落在 `ScopeProvider`（模型级）而不是
// `ScopeAccount`（凭据级）。
func TestAutoSwitchCredentialErrorIsModelLevel(t *testing.T) {
	router, zen, _ := newAutoSwitchRouter(t,
		&Error{Code: CodeCredential, Message: "Missing API key", MissingAPIKey: true},
		nil, true)

	router.Complete(context.Background(), crossTurn(), func(Chunk) {}, func() bool { return false })

	// ⚠️ "这个模型不免费" → **不**跳过同车道。
	if len(zen.called()) < 2 {
		t.Fatalf("这个模型不免费该试完同车道，实际 %v", zen.called())
	}
}

// TestAutoSwitchAccountCredentialErrorDoesNotSwitch 验证**普通凭据错误不换候选**。
//
// ⚠️ 凭据配错了 —— 换模型也是错的。
//
//	一个个试下去只是白烧一整轮候选。
func TestAutoSwitchAccountCredentialErrorDoesNotSwitch(t *testing.T) {
	router, zen, _ := newAutoSwitchRouter(t,
		&Error{Code: CodeCredential, Message: "Invalid API key"},
		nil, true)

	_, uerr := router.Complete(context.Background(), crossTurn(), func(Chunk) {}, func() bool { return false })

	if uerr == nil {
		t.Fatal("该失败")
	}
	// ⚠️ 凭据级错误 → **立刻**返回，不试第二个模型。
	if len(zen.called()) != 1 {
		t.Fatalf("凭据级错误不该换候选，实际 %v", zen.called())
	}
}

// TestAutoSwitchBothLanesFail 验证**两条车道都挂**时如实报错。
func TestAutoSwitchBothLanesFail(t *testing.T) {
	router, _, or := newAutoSwitchRouter(t,
		&Error{Code: CodeAbuseBlocked, Message: "出口被风控"},
		&Error{Code: CodeQuota, Message: "OR 也限流", RetryAfterSec: 7},
		true)

	_, uerr := router.Complete(context.Background(), crossTurn(), func(Chunk) {}, func() bool { return false })

	if uerr == nil {
		t.Fatal("该失败")
	}
	if len(or.called()) == 0 {
		t.Fatal("该试过 OpenRouter")
	}
	if !uerr.Exhausted {
		t.Fatal("该标记为候选耗尽")
	}
}
