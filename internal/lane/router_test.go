package lane

import (
	"context"
	"sync"
	"testing"
	"time"
)

// fakeLane 是一个可控的假车道，用来验证故障转移。
type fakeLane struct {
	id string
	// ready 报告是否就绪。
	ready bool
	// behavior 是每个模型的行为。
	behavior map[string]*Error
	text     string
	// calls 记录被调用过的模型。
	calls []string
	// emitText 成功时是否发出文本。
	emitText bool
	// emitThenFail 是"先吐文本再失败"的模型。
	//
	// ⚠️ 这是"吐过内容后不能换模型"的真实场景 ——
	// 上游在流到一半时拒绝（配额在流中途耗尽）。
	emitThenFail map[string]*Error
	// needsCredential 标记这条假车道**要花用户自己的额度**。
	//
	// ⚠️ **必须跟真实车道一致** ——
	//   真实的 OpenRouter 车道声明 `NeedsCredential() == true`，
	//   zen / Kilo 声明 `false`。
	//   测试假车道不声明的话，路由器会以为它**免费**，
	//   于是回落到它 —— 测试就测不出"该不该用用户额度"了。
	needsCredential bool
	// onCall 是"每次调用前"的钩子 —— 返回非 nil 就当这次失败。
	//
	// ⚠️ 用来测"**前 N 次失败、第 N+1 次成功**"这种时序行为
	//   （比如"重试同一个模型"）。
	onCall func(model string) *Error
	// mu 保护 calls（并发测试会同时调用）。
	mu sync.Mutex
}

func (f *fakeLane) ID() string  { return f.id }
func (f *fakeLane) Ready() bool { return f.ready }

// NeedsCredential 声明这条假车道要不要花用户自己的额度。
func (f *fakeLane) NeedsCredential() bool { return f.needsCredential }
func (f *fakeLane) ListModels(ctx context.Context) ([]ModelInfo, *Error) {
	return nil, nil
}
func (f *fakeLane) Status() LaneStatus { return LaneStatus{ID: f.id, Ready: f.ready} }
func (f *fakeLane) Probe(ctx context.Context, model string) ProbeResult {
	return ProbeResult{Model: model, State: StateUnknown}
}

func (f *fakeLane) Do(ctx context.Context, turn *Turn, emit func(Chunk)) (*Usage, FinishReason, *Error) {
	// ⚠️ **并发安全的记录** ——
	//   这个假车道会被多个 goroutine 同时调用
	//   （见 concurrency_test.go）。
	//
	//   不加锁的话 `go test -race` 会报数据竞争 ——
	//   而那**不是**生产代码的锅，是测试辅助函数的锅。
	f.mu.Lock()
	f.calls = append(f.calls, turn.Model)
	hook := f.onCall
	f.mu.Unlock()
	// ⚠️ **钩子优先** —— 它能模拟"前几次失败、之后成功"。
	if hook != nil {
		if uerr := hook(turn.Model); uerr != nil {
			return nil, FinishError, uerr
		}
	}
	// ⚠️ **先吐内容再失败** —— 模拟上游在流到一半时拒绝。
	if uerr, ok := f.lookup(f.emitThenFail, turn.Model); ok {
		if f.emitText {
			emit(Chunk{Kind: ChunkTextDelta, Index: 0, Delta: f.text})
		}
		return nil, FinishError, uerr
	}
	if uerr, ok := f.lookup(f.behavior, turn.Model); ok {
		return nil, FinishError, uerr
	}
	if f.emitText {
		emit(Chunk{Kind: ChunkTextDelta, Index: 0, Delta: f.text})
	}
	return &Usage{Input: 10, Output: 5, Total: 15}, FinishStop, nil
}

// lookup 查一个模型的行为。
//
// ⚠️ **两种键都认** ——
//
//	`turn.Model` 是**裸名**（上游只认这个），
//	但测试习惯用**全名**（`zen/model-a`）来注册。
//	这里两种都试一遍，免得每次改测试都要改键。
func (f *fakeLane) lookup(m map[string]*Error, model string) (*Error, bool) {
	if uerr, ok := m[model]; ok {
		return uerr, true
	}
	for k, v := range m {
		if _, bare := ParseModelID(k); bare == model {
			return v, true
		}
	}
	return nil, false
}

// called 返回被调用过的模型（**并发安全**）。
func (f *fakeLane) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// newTestRouter 组一个路由器。
//
// ⚠️ **默认开启「自动跨模型调用」** ——
//
//	大多数老测试测的是"候选链/过滤/排序"这些**候选构造**的逻辑，
//	它们需要一个**装着全部模型**的候选链。
//
//	测新语义（关闭时不换模型）的测试**显式关掉**它。
func newTestRouter(lanes ...Lane) (*Router, *Catalog) {
	catalog := NewCatalog()
	registry := NewRegistry()
	for _, l := range lanes {
		registry.Register(l)
	}
	throttle := NewBook()
	router := NewRouter(catalog, registry, throttle)
	router.SetAutoModelSwitch(true)
	return router, catalog
}

// TestRouterPicksRequestedModelFirst 验证请求的模型**永远排第一**。
func TestRouterPicksRequestedModelFirst(t *testing.T) {
	zen := &fakeLane{id: "zen", ready: true, text: "ok", emitText: true}
	or := &fakeLane{id: "openrouter", ready: true, text: "ok", emitText: true, needsCredential: true}
	router, catalog := newTestRouter(zen, or)
	catalog.Replace([]ModelInfo{
		{ID: "a-free", Lane: "zen", Enabled: true},
		{ID: "b-free", Lane: "zen", Enabled: true},
		{ID: "c:free", Lane: "openrouter", Enabled: true},
	})

	cands := router.BuildCandidates("", "zen/b-free", Needs{})
	if len(cands) == 0 {
		t.Fatal("没有候选")
	}
	if cands[0].Model != "b-free" {
		t.Fatalf("请求的模型应排第一，实际 %q", cands[0].Model)
	}
}

// TestRouterSwitchesOnQuota 验证 429 会**换模型**。
//
// 这是最重要的一条：配额按 session 记账，换模型是唯一有效的杠杆。
func TestRouterSwitchesOnQuota(t *testing.T) {
	zen := &fakeLane{
		id:    "zen",
		ready: true,
		behavior: map[string]*Error{
			// 请求的模型被限流。
			"a-free": {Code: CodeQuota, Message: "rate limited", RetryAfterSec: 10},
		},
		text: "ok", emitText: true,
	}
	router, catalog := newTestRouter(zen)
	catalog.Replace([]ModelInfo{
		{ID: "a-free", Lane: "zen", Enabled: true},
		{ID: "b-free", Lane: "zen", Enabled: true},
	})

	committed := false
	result, uerr := router.Complete(context.Background(),
		&Turn{Model: "zen/a-free", Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}}},
		func(Chunk) { committed = true },
		func() bool { return committed })

	if uerr != nil {
		t.Fatalf("应该成功（换到了 b-free）: %v", uerr)
	}
	if result.Model != "b-free" {
		t.Fatalf("应换到 b-free，实际 %q", result.Model)
	}
	if result.Attempts != 2 {
		t.Fatalf("应尝试 2 次，实际 %d", result.Attempts)
	}
	// 限流的模型应被记账。
	if !router.throttle.Throttled("a-free") {
		t.Error("被限流的模型应进限流账本")
	}
}

// TestRouterDoesNotSwitchAfterContent 验证**吐过内容后不换模型**。
//
// 换了会内容重复/拼接错乱。
func TestRouterDoesNotSwitchAfterContent(t *testing.T) {
	zen := &fakeLane{
		id:    "zen",
		ready: true,
		// ⚠️ 这个模型**先吐文本，再失败** ——
		// 这才是"吐过内容后不能换"的真实场景。
		emitThenFail: map[string]*Error{
			"a-free": {Code: CodeQuota, Message: "rate limited"},
		},
		text: "partial", emitText: true,
	}
	router, catalog := newTestRouter(zen)
	catalog.Replace([]ModelInfo{
		{ID: "a-free", Lane: "zen", Enabled: true},
		{ID: "b-free", Lane: "zen", Enabled: true},
	})

	committed := false
	result, uerr := router.Complete(context.Background(),
		&Turn{Model: "zen/a-free", Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}}},
		func(c Chunk) {
			if c.Kind == ChunkTextDelta {
				committed = true
			}
		},
		func() bool { return committed })

	if uerr == nil {
		t.Fatal("应该报错（不能换模型）")
	}
	if result.Attempts != 1 {
		t.Fatalf("吐过内容后不该再尝试，实际尝试 %d 次", result.Attempts)
	}
	// ⚠️ 绝不能试第二个候选 ——
	// 客户端会收到两份内容（"partial" + "ok"）。
	if len(zen.called()) != 1 {
		t.Fatalf("只该调用一次，实际 %v", zen.called())
	}
}

// TestRouterDoesNotSwitchOnClientError 验证**请求自己的错不换模型**。
func TestRouterDoesNotSwitchOnClientError(t *testing.T) {
	zen := &fakeLane{
		id:    "zen",
		ready: true,
		behavior: map[string]*Error{
			"a-free": {Code: CodeClient, Message: "prompt too long"},
		},
		text: "ok", emitText: true,
	}
	router, catalog := newTestRouter(zen)
	catalog.Replace([]ModelInfo{
		{ID: "a-free", Lane: "zen", Enabled: true},
		{ID: "b-free", Lane: "zen", Enabled: true},
	})

	result, uerr := router.Complete(context.Background(),
		&Turn{Model: "zen/a-free", Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}}},
		func(Chunk) {}, func() bool { return false })

	if uerr == nil {
		t.Fatal("应该报错")
	}
	if uerr.Code != CodeClient {
		t.Fatalf("错误码不对: %v", uerr.Code)
	}
	if result.Attempts != 1 {
		t.Fatalf("请求自己的错不该换模型，实际尝试 %d 次", result.Attempts)
	}
}

// TestRouterSkipsSameLaneOnGateRejection 验证**闸门拒绝时跳过同车道**。
//
// 闸门对所有模型一视同仁 —— 同车道内换模型是白费功夫。
func TestRouterSkipsSameLaneOnGateRejection(t *testing.T) {
	zen := &fakeLane{
		id:    "zen",
		ready: true,
		behavior: map[string]*Error{
			"a-free": {Code: CodeFreeTier, Message: "can only be used from within OpenCode"},
		},
		text: "ok", emitText: true,
	}
	router, catalog := newTestRouter(zen)
	catalog.Replace([]ModelInfo{
		{ID: "a-free", Lane: "zen", Enabled: true},
		{ID: "b-free", Lane: "zen", Enabled: true},
	})

	result, uerr := router.Complete(context.Background(),
		&Turn{Model: "zen/a-free", Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}}},
		func(Chunk) {}, func() bool { return false })

	if uerr == nil {
		t.Fatal("应该报错")
	}
	// ⚠️ 同车道的 b-free 不该被试。
	if len(zen.called()) != 1 {
		t.Fatalf("同车道不该继续试，实际调用 %v", zen.called())
	}
	if result.Attempts != 1 {
		t.Fatalf("应只尝试 1 次，实际 %d", result.Attempts)
	}
}

// TestRouterSkipsUnreadyLane 验证**没就绪的车道被跳过**。
//
// ⚠️ 这里请求**指名了** `zen/a-free`，
//
//	而「自动跨模型调用」是**关闭**的 ——
//	所以**不该**偷偷用 OpenRouter 兜底（那要花用户自己的额度）。
//	该失败。
func TestRouterSkipsUnreadyLane(t *testing.T) {
	zen := &fakeLane{id: "zen", ready: false}
	or := &fakeLane{id: "openrouter", ready: true, text: "ok", emitText: true, needsCredential: true}
	router, catalog := newTestRouter(zen, or)
	// ⚠️ **关掉「自动跨模型调用」** —— 指名了模型就不该换。
	router.SetAutoModelSwitch(false)
	catalog.Replace([]ModelInfo{
		{ID: "a-free", Lane: "zen", Enabled: true},
		{ID: "c:free", Lane: "openrouter", Enabled: true},
	})

	result, uerr := router.Complete(context.Background(),
		&Turn{Model: "zen/a-free", Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}}},
		func(Chunk) {}, func() bool { return false })
	// ⚠️ 名指了免密车道 → 不许偷偷换车道。
	if uerr == nil {
		t.Fatalf("指名免密车道时不该回落到 OpenRouter，实际 lane=%q", result.Lane)
	}
	if len(zen.called()) != 0 {
		t.Fatalf("没就绪的车道不该被调用: %v", zen.called())
	}
	if len(or.called()) != 0 {
		t.Fatalf("指名免密车道时不该碰 OpenRouter: %v", or.called())
	}
}

// TestRouterUnreadyLaneFallsBackWhenAllowed 验证**开了自动跨模型调用**时
// 没就绪的车道可以兜底。
func TestRouterUnreadyLaneFallsBackWhenAllowed(t *testing.T) {
	zen := &fakeLane{id: "zen", ready: false}
	or := &fakeLane{id: "openrouter", ready: true, text: "ok", emitText: true, needsCredential: true}
	router, catalog := newTestRouter(zen, or)
	catalog.Replace([]ModelInfo{
		{ID: "a-free", Lane: "zen", Enabled: true},
		{ID: "c:free", Lane: "openrouter", Enabled: true},
	})
	router.SetAutoModelSwitch(true)

	result, uerr := router.Complete(context.Background(),
		&Turn{Model: "zen/a-free", Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}}},
		func(Chunk) {}, func() bool { return false })
	if uerr != nil {
		t.Fatalf("开了自动跨模型调用就该成功: %v", uerr)
	}
	if result.Lane != "openrouter" {
		t.Fatalf("应落到 openrouter，实际 %q", result.Lane)
	}
	if len(zen.called()) != 0 {
		t.Fatalf("没就绪的车道不该被调用: %v", zen.called())
	}
}

// TestRouterFiltersByModality 验证**图片请求不落到纯文本模型上**。
//
// ⚠️ 前提：那个"纯文本模型"的结论是**已知**的
// （实测或上游声明）—— 未知的一律放行（软过滤）。
func TestRouterFiltersByModality(t *testing.T) {
	zen := &fakeLane{id: "zen", ready: true, text: "ok", emitText: true}
	router, catalog := newTestRouter(zen)
	catalog.Replace([]ModelInfo{
		{ID: "text-only-free", Lane: "zen", Enabled: true, Vision: false,
			Capability: CapabilitySourceProbe},
		{ID: "vision-free", Lane: "zen", Enabled: true, Vision: true,
			Capability: CapabilitySourceProbe},
	})

	cands := router.BuildCandidates("", "zen/text-only-free", Needs{Image: true})
	for _, c := range cands {
		if c.Model == "text-only-free" {
			t.Fatal("已知是纯文本的模型不该出现在图片请求的候选里")
		}
	}
	if len(cands) == 0 {
		t.Fatal("应该有候选（vision-free）")
	}
}

// TestRouterUnknownCapabilityPasses 验证**未知能力通过但排后面**。
func TestRouterUnknownCapabilityPasses(t *testing.T) {
	zen := &fakeLane{id: "zen", ready: true, text: "ok", emitText: true}
	router, catalog := newTestRouter(zen)
	catalog.Replace([]ModelInfo{
		// ⚠️ 这个模型**没有**能力结论 —— 不是"测过不支持"，是"没测过"。
		{ID: "unknown-free", Lane: "zen", Enabled: true, Vision: false,
			Capability: CapabilitySourceUnknown},
		{ID: "vision-free", Lane: "zen", Enabled: true, Vision: true,
			Capability: CapabilitySourceProbe},
	})
	catalog.SetProbe(ProbeResult{Model: "vision-free", State: StateAvailable, At: nowMillis()})

	cands := router.BuildCandidates("", "zen/unknown-free", Needs{Image: true})
	if len(cands) < 2 {
		t.Fatalf("未知能力的模型应保留（软过滤），实际 %d 个候选", len(cands))
	}
	// ⚠️ 请求的模型永远排第一（这是设计）——
	// 所以这里要验证的是**其余候选**的相对顺序。
	// 已知能看图的应排在未知能力的前面。
	rest := cands[1:]
	if len(rest) == 0 {
		t.Fatal("应还有其他候选")
	}
	if rest[0].Model != "vision-free" {
		t.Fatalf("已知能力的应排在未知能力前面，实际 %q", rest[0].Model)
	}
}

// TestRouterDisabledModelsAreSkipped 验证被禁用的模型不参与路由。
func TestRouterDisabledModelsAreSkipped(t *testing.T) {
	zen := &fakeLane{id: "zen", ready: true, text: "ok", emitText: true}
	router, catalog := newTestRouter(zen)
	catalog.Replace([]ModelInfo{
		{ID: "on-free", Lane: "zen", Enabled: true},
		{ID: "off-free", Lane: "zen", Enabled: false},
	})
	cands := router.BuildCandidates("", "zen/on-free", Needs{})
	for _, c := range cands {
		if c.Model == "off-free" {
			t.Fatal("被禁用的模型不该在候选里")
		}
	}
}

// TestRouterContextFilterHasHeadroom 验证上下文过滤**有 1.5 倍余量**。
//
// 实测容量是下界 —— 候选被静默丢弃的代价是整个请求失败。
func TestRouterContextFilterHasHeadroom(t *testing.T) {
	zen := &fakeLane{id: "zen", ready: true, text: "ok", emitText: true}
	router, catalog := newTestRouter(zen)
	catalog.Replace([]ModelInfo{
		{ID: "small-free", Lane: "zen", Enabled: true, ContextWindow: 1000},
		{ID: "big-free", Lane: "zen", Enabled: true, ContextWindow: 100000},
	})

	// 1200 token：超过 small 的窗口(1000)，但小于 1000×1.5=1500。
	// ⇒ small **应该保留** —— 让上游当裁判，别自己先把它扔了。
	cands := router.BuildCandidates("", "zen/big-free", Needs{PromptTokens: 1200})
	found := false
	for _, c := range cands {
		if c.Model == "small-free" {
			found = true
		}
	}
	if !found {
		t.Fatal("略微超标的 prompt 不该把候选滤掉（上游才是裁判）")
	}

	// 2000 token：超过 1000×1.5=1500 ⇒ small 该被滤掉。
	cands2 := router.BuildCandidates("", "zen/big-free", Needs{PromptTokens: 2000})
	for _, c := range cands2 {
		if c.Model == "small-free" {
			t.Fatal("明显超标的候选该被滤掉")
		}
	}
}

// TestRouterContextFilterKeepsAllWhenEverythingTooSmall 验证
// **全被滤光时宁可超限也不失败**。
func TestRouterContextFilterKeepsAllWhenEverythingTooSmall(t *testing.T) {
	zen := &fakeLane{id: "zen", ready: true, text: "ok", emitText: true}
	router, catalog := newTestRouter(zen)
	catalog.Replace([]ModelInfo{
		{ID: "tiny-free", Lane: "zen", Enabled: true, ContextWindow: 100},
	})

	// 100000 token：远超 tiny 的 100×1.5。
	// ⇒ 但**不能**把候选清空 —— 否则整个请求直接失败。
	cands := router.BuildCandidates("", "zen/tiny-free", Needs{PromptTokens: 100000})
	if len(cands) == 0 {
		t.Fatal("全被滤光时不该清空候选（宁可超限也不失败）")
	}
}

// 档位预算

func TestNormalizeEffort(t *testing.T) {
	cases := map[string]string{
		"low": "light", "minimal": "light", "light": "light",
		// ⚠️ "关掉思考"**必须**保留成独立档位 ——
		// 归并进 light 会让意图在到达 lane 层之前就丢掉，
		// OpenRouter 车道就永远收不到"关掉"的请求。
		"none": EffortOff, "off": EffortOff, "disabled": EffortOff,
		"medium": "balanced", "balanced": "balanced", "default": "balanced",
		"high": "deep", "xhigh": "deep", "max": "deep", "deep": "deep",
		"":        "",
		"garbage": "",
	}
	for in, want := range cases {
		if got := NormalizeEffort(in); got != want {
			t.Errorf("%q → %q，期望 %q", in, got, want)
		}
	}
}

func TestBudgetForRespectsPriority(t *testing.T) {
	info := ModelInfo{MaxOutput: 32768, Reasoning: true, CanDisableThinking: true}

	// 档位天花板生效。
	if got := BudgetFor("light", info, 0, DefaultEffortBudget); got != 2048 {
		t.Errorf("light 档应为 2048，实际 %d", got)
	}
	// deep 档用模型容量。
	if got := BudgetFor("deep", info, 0, DefaultEffortBudget); got != 32768 {
		t.Errorf("deep 档应为模型容量 32768，实际 %d", got)
	}
	// 客户端显式给的压过一切。
	if got := BudgetFor("light", info, 999, DefaultEffortBudget); got != 999 {
		t.Errorf("客户端显式值应压过档位，实际 %d", got)
	}
}

func TestBudgetForDoublesForAlwaysThinking(t *testing.T) {
	// ⚠️ 思考关不掉的模型 → 整档加倍。
	info := ModelInfo{MaxOutput: 32768, Reasoning: true, CanDisableThinking: false}
	if got := BudgetFor("light", info, 0, DefaultEffortBudget); got != 2048*2 {
		t.Errorf("思考关不掉的模型应加倍，实际 %d", got)
	}
	// 能关掉的 → 不加倍。
	info2 := ModelInfo{MaxOutput: 32768, Reasoning: true, CanDisableThinking: true}
	if got := BudgetFor("light", info2, 0, DefaultEffortBudget); got != 2048 {
		t.Errorf("能关掉思考的不该加倍，实际 %d", got)
	}
}

func TestBudgetForNeverExceedsModelCapacity(t *testing.T) {
	// 档位天花板不能超过模型容量。
	info := ModelInfo{MaxOutput: 1000, Reasoning: true, CanDisableThinking: true}
	if got := BudgetFor("deep", info, 0, DefaultEffortBudget); got != 1000 {
		t.Errorf("不该超过模型容量，实际 %d", got)
	}
}

// nowMillis 返回当前毫秒。
func nowMillis() int64 {
	return time.Now().UnixMilli()
}
