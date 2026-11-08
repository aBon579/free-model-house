package lane

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// probeLane 是一个可控的假车道，用来测试探测调度。
type probeLane struct {
	id string
	// fail 报告要失败的模型。
	fail map[string]bool
	// state 报告每个模型的状态。
	state map[string]ProbeState

	mu sync.Mutex
	// probed 记录被探过的模型。
	probed []string
	// concurrent 记录最大并发数。
	concurrent int
	// current 记录当前并发数。
	current int
}

func (p *probeLane) ID() string  { return p.id }
func (p *probeLane) Ready() bool { return true }
func (p *probeLane) ListModels(ctx context.Context) ([]ModelInfo, *Error) {
	return nil, nil
}
func (p *probeLane) Status() LaneStatus { return LaneStatus{ID: p.id, Ready: true} }

// Do 满足 Lane 接口（探测测试用不到它）。
func (p *probeLane) Do(ctx context.Context, turn *Turn, emit func(Chunk)) (*Usage, FinishReason, *Error) {
	return nil, FinishError, &Error{Code: CodeServer, Message: "not implemented"}
}

func (p *probeLane) Probe(ctx context.Context, model string) ProbeResult {
	p.mu.Lock()
	p.probed = append(p.probed, model)
	p.current++
	if p.current > p.concurrent {
		p.concurrent = p.current
	}
	p.mu.Unlock()

	// 模拟一点延迟，好观察并发。
	time.Sleep(5 * time.Millisecond)

	p.mu.Lock()
	p.current--
	p.mu.Unlock()

	if p.fail[model] {
		return ProbeResult{Model: model, State: StateThrottled, Detail: "rate limited", At: time.Now().UnixMilli()}
	}
	st := p.state[model]
	if st == "" {
		st = StateAvailable
	}
	return ProbeResult{Model: model, State: st, At: time.Now().UnixMilli()}
}

func (p *probeLane) probedModels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, len(p.probed))
	copy(out, p.probed)
	return out
}

func (p *probeLane) maxConcurrent() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.concurrent
}

// newProbeSetup 组一个调度器。
func newProbeSetup(t *testing.T, batch int, models ...string) (*ProbeScheduler, *probeLane, *Catalog) {
	t.Helper()
	l := &probeLane{id: "zen", fail: map[string]bool{}, state: map[string]ProbeState{}}
	registry := NewRegistry()
	registry.Register(l)
	catalog := NewCatalog()
	infos := make([]ModelInfo, 0, len(models))
	for _, m := range models {
		infos = append(infos, ModelInfo{ID: m, Lane: "zen", Enabled: true, Free: true})
	}
	catalog.Replace(infos)
	s := NewProbeScheduler(catalog, registry, ProbeOptions{
		BatchSize: batch, Concurrency: 2, Interval: time.Minute,
	})
	return s, l, catalog
}

// TestProbeSchedulerOnlyProbesFreeModels 验证**只探免费的模型**。
//
// ⚠️ 探测付费模型**可能**真扣钱 ——
//
//	虽然实测它在凭据检查阶段就 401 了，
//	但那只是运气好，不能依赖。
func TestProbeSchedulerOnlyProbesFreeModels(t *testing.T) {
	l := &probeLane{id: "zen", fail: map[string]bool{}, state: map[string]ProbeState{}}
	registry := NewRegistry()
	registry.Register(l)
	catalog := NewCatalog()
	catalog.Replace([]ModelInfo{
		{ID: "free-model", Lane: "zen", Enabled: true, Free: true},
		{ID: "paid-model", Lane: "zen", Enabled: true, Free: false},
	})
	s := NewProbeScheduler(catalog, registry, ProbeOptions{BatchSize: 10, Concurrency: 1})

	targets := s.PickTargets()
	if len(targets) != 1 {
		t.Fatalf("该只探 1 个（免费的那个），实际 %d: %+v", len(targets), targets)
	}
	if targets[0].Model != "free-model" {
		t.Fatalf("该探免费模型，实际 %q", targets[0].Model)
	}
}

// TestFullProbeIgnoresFreshProbes 验证**关闭自适应时新鲜的结论也照样探**。
//
// ⚠️ 这是"一会多一会少"的根因之一：
//
//	探测间隔（10 分钟）≈ ProbeTTL（10 分钟）。
//	上一轮里**探得晚**的模型，到下一轮开始时还不到 10 分钟 ——
//	被"结论新鲜"这条规则跳过；探得早的又被重探。
//	于是每轮探多少个，取决于上一轮跑多久、哪些探得早 ——
//	用户看到的就是"数量不可预期"。
//
//	关闭自适应 = 全量快照，**不许**再按 TTL 跳过。
func TestFullProbeIgnoresFreshProbes(t *testing.T) {
	s, _, catalog := newProbeSetup(t, 10, "a", "b")
	catalog.SetProbe(ProbeResult{
		Model: "a", State: StateAvailable, At: time.Now().UnixMilli(),
	})
	if targets := s.PickTargets(); len(targets) != 2 {
		t.Fatalf("关闭自适应时该全量探 2 个，实际 %d: %+v", len(targets), targets)
	}
}

// TestProbeSchedulerRotates 验证**分批轮转是确定、可收敛的**。
//
// ⚠️ 只有在**开启自适应**时才分批；规则：
//
//	上次被选中最久远的优先，同代次按模型 id；
//	每轮取前 N 个并记账 → 相邻轮次不重复，覆盖一圈后回到开头。
//
// ⚠️ 覆盖全部 5 个模型后**必须重新从 a、b 开始** ——
//
//	这就是"可收敛"：不会出现某轮多某轮少。
func TestProbeSchedulerRotates(t *testing.T) {
	s, _, _ := newProbeSetup(t, 2, "a", "b", "c", "d", "e")
	s.SetAdaptiveBatch(true)

	want := [][]string{
		{"a", "b"},
		{"c", "d"},
		{"e", "a"}, // 一圈探完 → 回到最久没选的 a
		{"b", "c"},
	}
	for i, w := range want {
		got := modelSet(s.PickTargets())
		if len(got) != len(w) {
			t.Fatalf("第 %d 轮该探 %d 个，实际 %d: %v", i+1, len(w), len(got), got)
		}
		for _, id := range w {
			if !got[id] {
				t.Fatalf("第 %d 轮该包含 %q，实际 %v", i+1, id, got)
			}
		}
	}
}

// TestAdaptiveBatchKeepsSizeStable 验证**开启自适应时每轮数量恒定**。
func TestAdaptiveBatchKeepsSizeStable(t *testing.T) {
	s, _, _ := newProbeSetup(t, 3, "a", "b", "c", "d", "e", "f", "g", "h")
	s.SetAdaptiveBatch(true)
	for i := 0; i < 6; i++ {
		if n := len(s.PickTargets()); n != 3 {
			t.Fatalf("第 %d 轮该固定 3 个，实际 %d", i+1, n)
		}
	}
}

// TestProbeSchedulerCoversAllModels 验证**一轮探完全部模型**。
//
// ⚠️ 用户要的：每 15 分钟**全量**探测一次。
func TestProbeSchedulerCoversAllModels(t *testing.T) {
	s, _, _ := newProbeSetup(t, 3, "a", "b", "c", "d", "e", "f", "g")
	targets := s.PickTargets()
	if len(targets) != 7 {
		t.Fatalf("该一轮探完 7 个，实际 %d", len(targets))
	}
}

// TestFullProbeIgnoresFailureBackoff 验证**关闭自适应时失败模型也每轮都探**。
//
// ⚠️ 之前"连续失败就少探/不探"会让每轮数量随失败情况漂移 ——
//
//	用户看到的是"一会多一会少"。关闭自适应 = 全量快照，
//	失败的模型**恰恰**是最需要下一轮确认有没有恢复的。
func TestFullProbeIgnoresFailureBackoff(t *testing.T) {
	s, l, _ := newProbeSetup(t, 10, "good", "bad")
	// ⚠️ 用"不可用"而不是"限流" —— 避免触发限流复核的 3 秒等待。
	l.state["bad"] = StateUnavailable

	s.SetAdaptiveBatch(false)
	rounds := 4
	for i := 0; i < rounds; i++ {
		before := len(l.probedModels())
		s.RunOnce(context.Background())
		seen := nameSet(l.probedModels()[before:])
		if len(seen) != 2 {
			t.Fatalf("第 %d 轮该全量探 2 个，实际 %d: %v", i+1, len(seen), seen)
		}
		if !seen["good"] || !seen["bad"] {
			t.Fatalf("第 %d 轮该同时探 good 和 bad，实际 %v", i+1, seen)
		}
	}
}

// TestProbeSchedulerLimitsConcurrency 验证**并发上限**。
func TestProbeSchedulerLimitsConcurrency(t *testing.T) {
	s, l, _ := newProbeSetup(t, 8, "a", "b", "c", "d", "e", "f", "g", "h")
	s.concurrency = 2
	s.RunOnce(context.Background())
	if max := l.maxConcurrent(); max > 2 {
		t.Fatalf("并发不该超过 2，实际 %d", max)
	}
}

// TestProbeSchedulerSingleRunAtATime 验证**同一时刻只跑一轮**。
//
// ⚠️ 否则慢的一轮没跑完，下一轮又开始了 ——
//
//	配额会被探测吃光。
func TestProbeSchedulerSingleRunAtATime(t *testing.T) {
	s, l, _ := newProbeSetup(t, 4, "a", "b", "c", "d")

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s.RunOnce(context.Background())
		}()
	}
	close(start)
	wg.Wait()

	// ⚠️ 5 个并发调用，但只有**一轮**真正跑了。
	// 每一轮探 4 个模型 → 最多 4 次。
	if n := len(l.probedModels()); n > 4 {
		t.Fatalf("只该跑一轮，最多 4 次探测，实际 %d 次: %v", n, l.probedModels())
	}
}

// TestProbeOneUpdatesCatalog 验证**手动探测会更新清单**。
func TestProbeOneUpdatesCatalog(t *testing.T) {
	s, _, catalog := newProbeSetup(t, 1, "m1")
	var got ProbeResult
	s.SetResultHook(func(r ProbeResult) { got = r })

	result := s.ProbeOne(context.Background(), "m1")
	if result.Model != "m1" {
		t.Fatalf("模型不对: %q", result.Model)
	}
	if result.State != StateAvailable {
		t.Fatalf("状态不对: %v", result.State)
	}
	if got.Model != "m1" {
		t.Fatalf("回调没触发: %+v", got)
	}
	// ⚠️ 模型行也应该更新。
	for _, m := range catalog.Models() {
		if m.ID == "m1" {
			if m.State != string(StateAvailable) {
				t.Fatalf("模型行状态没更新: %q", m.State)
			}
			if m.TTFTMs != result.TTFTMs {
				t.Fatalf("延迟没更新: %d", m.TTFTMs)
			}
		}
	}
}

// TestProbeOneUnknownModel 验证**探不存在的模型**不会崩。
func TestProbeOneUnknownModel(t *testing.T) {
	s, _, _ := newProbeSetup(t, 1, "m1")
	result := s.ProbeOne(context.Background(), "does-not-exist")
	if result.State != StateUnknown {
		t.Fatalf("该是 unknown，实际 %v", result.State)
	}
	if result.Detail == "" {
		t.Fatal("该说明原因")
	}
}

// TestProbeSummary 验证探测统计。
func TestProbeSummary(t *testing.T) {
	_, _, catalog := newProbeSetup(t, 1, "a", "b", "c")
	catalog.SetProbe(ProbeResult{Model: "a", State: StateAvailable, At: time.Now().UnixMilli()})
	catalog.SetProbe(ProbeResult{Model: "b", State: StateThrottled, At: time.Now().UnixMilli()})
	catalog.SetProbe(ProbeResult{Model: "c", State: StateUnknown, At: time.Now().UnixMilli()})

	s := catalog.SummarizeProbes()
	if s.Probed != 3 {
		t.Fatalf("该是 3，实际 %d", s.Probed)
	}
	if s.Available != 1 {
		t.Fatalf("可用该是 1，实际 %d", s.Available)
	}
	if s.Failed != 1 {
		t.Fatalf("失败该是 1，实际 %d", s.Failed)
	}
	if s.Unknown != 1 {
		t.Fatalf("未知该是 1，实际 %d", s.Unknown)
	}
}

// TestDisabledSchedulerSkipsAutoRuns 验证**关掉后自动探测不跑**。
func TestDisabledSchedulerSkipsAutoRuns(t *testing.T) {
	s, l, _ := newProbeSetup(t, 4, "a", "b", "c", "d")
	s.SetEnabled(false)

	s.RunOnce(context.Background())
	if n := len(l.probedModels()); n != 0 {
		t.Fatalf("关掉后不该探，实际 %d 次", n)
	}
}

// TestManualRunIgnoresEnabledFlag 验证**手动探测不受开关限制**。
//
// ⚠️ 用户点按钮是明确意图 —— 哪怕自动探测关着也该跑。
func TestManualRunIgnoresEnabledFlag(t *testing.T) {
	s, l, _ := newProbeSetup(t, 4, "a", "b", "c", "d")
	s.SetEnabled(false)

	s.RunOnceManual(context.Background())
	if n := len(l.probedModels()); n == 0 {
		t.Fatal("手动探测不该被开关拦住")
	}
}

// TestAdaptiveBatchShrinksWhenThrottled 验证**上游越拥挤，探得越少**。
//
// ⚠️ 免密车道的配额是**全局共享**的。
//
//	高峰期继续按正常节奏探测 = 拿额度去撞限流墙：
//	探到的全是 429，既费额度又拿不到有效信息。
func TestAdaptiveBatchShrinksWhenThrottled(t *testing.T) {
	l := &probeLane{id: "zen", fail: map[string]bool{}, state: map[string]ProbeState{}}
	registry := NewRegistry()
	registry.Register(l)
	catalog := NewCatalog()
	models := make([]ModelInfo, 0, 20)
	for i := 0; i < 20; i++ {
		models = append(models, ModelInfo{
			ID: fmt.Sprintf("m%d", i), Lane: "zen", Enabled: true, Free: true,
		})
	}
	catalog.Replace(models)

	s := NewProbeScheduler(catalog, registry, ProbeOptions{
		BatchSize: 8, Concurrency: 2, Interval: time.Minute,
	})
	s.SetAdaptiveBatch(true)

	// 全部限流。
	for i := 0; i < 20; i++ {
		l.fail[fmt.Sprintf("m%d", i)] = true
	}
	s.RunOnce(context.Background())

	// ⚠️ 限流比例 100% → 批量该降到 1/4。
	s.mu.Lock()
	ratio := s.lastThrottleRatio
	s.mu.Unlock()
	if ratio < 0.99 {
		t.Fatalf("限流比例该是 1.0，实际 %v", ratio)
	}

	s.mu.Lock()
	// ⚠️ 基数是**批量上限**（8 个），不是候选池的大小。
	batch := s.currentBatchLocked(20)
	s.mu.Unlock()
	if batch != 2 {
		t.Fatalf("批量该降到 8/4=2，实际 %d", batch)
	}

	// ⚠️ **下一轮该只探 2 个**（不是 20 个）。
	//
	// ⚠️ 用轮次小结里的 Probed 而不是 Probe 调用次数 ——
	//   限流结论会被**复核**，同一个模型会被再问一次，
	//   调用次数是 4，但"这一轮探了几个模型"仍然是 2。
	probed := 0
	s.SetRoundHook(func(sum ProbeRoundSummary) { probed = sum.Probed })
	s.RunOnce(context.Background())
	if probed != 2 {
		t.Fatalf("限流时该只探 2 个模型，实际 %d", probed)
	}
}

// TestAdaptiveBatchGrowsWhenHealthy 验证**上游健康时批量恢复**。
func TestAdaptiveBatchGrowsWhenHealthy(t *testing.T) {
	l := &probeLane{id: "zen", fail: map[string]bool{}, state: map[string]ProbeState{}}
	registry := NewRegistry()
	registry.Register(l)
	catalog := NewCatalog()
	models := make([]ModelInfo, 0, 20)
	for i := 0; i < 20; i++ {
		models = append(models, ModelInfo{
			ID: fmt.Sprintf("m%d", i), Lane: "zen", Enabled: true, Free: true,
		})
	}
	catalog.Replace(models)

	s := NewProbeScheduler(catalog, registry, ProbeOptions{
		BatchSize: 8, Concurrency: 2, Interval: time.Minute,
	})
	s.SetAdaptiveBatch(true)
	// 全部正常。
	s.RunOnce(context.Background())

	s.mu.Lock()
	ratio := s.lastThrottleRatio
	batch := s.currentBatchLocked(20)
	s.mu.Unlock()
	if ratio != 0 {
		t.Fatalf("限流比例该是 0，实际 %v", ratio)
	}
	if batch != 8 {
		t.Fatalf("批量该是上限 8，实际 %d", batch)
	}
}

// TestAdaptiveBatchCanBeDisabled 验证**自适应可以关掉**。
func TestAdaptiveBatchCanBeDisabled(t *testing.T) {
	s, _, _ := newProbeSetup(t, 8, "a", "b", "c", "d", "e", "f", "g", "h")
	s.SetAdaptiveBatch(false)
	s.mu.Lock()
	s.lastThrottleRatio = 1.0 // 假装全被限流
	batch := s.currentBatchLocked(8)
	s.mu.Unlock()
	if batch != 8 {
		t.Fatalf("关掉自适应后该是满的 8，实际 %d", batch)
	}
}

// TestAdaptiveBatchNeverZero 验证**批量不会降到 0**。
func TestAdaptiveBatchNeverZero(t *testing.T) {
	s, _, _ := newProbeSetup(t, 4, "a", "b", "c", "d")
	s.SetAdaptiveBatch(true)
	s.mu.Lock()
	s.lastThrottleRatio = 1.0
	batch := s.currentBatchLocked(4)
	s.mu.Unlock()
	if batch < 1 {
		t.Fatalf("批量不能是 0，实际 %d", batch)
	}
}

// TestSetIntervalAndBatchSize 验证**间隔与批量可动态调整**。
//
// ⚠️ 这是"设置改了要立即生效"的基础。
func TestSetIntervalAndBatchSize(t *testing.T) {
	s, _, _ := newProbeSetup(t, 2, "a", "b", "c", "d", "e")

	s.SetInterval(3 * time.Minute)
	if s.Interval() != 3*time.Minute {
		t.Fatalf("间隔没生效: %v", s.Interval())
	}
	// ⚠️ 批量只在**开启自适应**时生效。
	s.SetAdaptiveBatch(true)
	s.SetBatchSize(5)
	if len(s.PickTargets()) != 5 {
		t.Fatalf("批量没生效: %d", len(s.PickTargets()))
	}

	// ⚠️ 非法值不该破坏状态。
	s.SetInterval(0)
	s.SetBatchSize(-1)
	if s.Interval() != 3*time.Minute {
		t.Fatalf("非法间隔不该改状态: %v", s.Interval())
	}
	if len(s.PickTargets()) != 5 {
		t.Fatal("非法批量不该改状态")
	}
}

// TestFullProbeCoversAllEveryRound 验证**关闭自适应时每轮覆盖数恒定**。
//
// ⚠️ 这是用户反馈的直接回归测试：
//
//	"关闭自适应后，自动探测的模型数量一会多一会少"。
//	现在每轮的探测数必须**恒等于**参与自动探测的模型数（这里 9 个）。
func TestFullProbeCoversAllEveryRound(t *testing.T) {
	const models = 9
	ids := make([]string, 0, models)
	for i := 0; i < models; i++ {
		ids = append(ids, fmt.Sprintf("m%d", i))
	}
	s, l, _ := newProbeSetup(t, models, ids...)

	var mu sync.Mutex
	counts := make([]int, 0, 3)
	s.SetRoundHook(func(sum ProbeRoundSummary) {
		mu.Lock()
		counts = append(counts, sum.Probed)
		mu.Unlock()
	})

	for round := 1; round <= 3; round++ {
		s.RunOnce(context.Background())
		mu.Lock()
		got := counts[len(counts)-1]
		mu.Unlock()
		if got != models {
			t.Fatalf("第 %d 轮该探 %d 个，实际 %d", round, models, got)
		}
	}
	if n := len(l.probedModels()); n != models*3 {
		t.Fatalf("3 轮该探 %d 次，实际 %d", models*3, n)
	}
}

// probeOffLane 是一条**不参与自动探测**的假车道（模拟自定义 / OR）。
type probeOffLane struct{ probeLane }

func (p *probeOffLane) AutoProbeable() bool { return false }

// TestFullProbeExcludesNonAutoProbeable 验证**不参与自动探测的车道被排除**。
//
// ⚠️ 全量 ≠ 什么都探 —— 只覆盖"参与自动探测"的模型：
//
//	用用户自己 Key 的车道（OR / 自定义）照旧排除，否则会把用户额度打满。
func TestFullProbeExcludesNonAutoProbeable(t *testing.T) {
	on := &probeLane{id: "zen", fail: map[string]bool{}, state: map[string]ProbeState{}}
	off := &probeOffLane{probeLane{id: "or", fail: map[string]bool{}, state: map[string]ProbeState{}}}
	registry := NewRegistry()
	registry.Register(on)
	registry.Register(off)

	catalog := NewCatalog()
	catalog.Replace([]ModelInfo{
		{ID: "zen/a", Lane: "zen", Enabled: true, Free: true},
		{ID: "zen/b", Lane: "zen", Enabled: true, Free: true},
		{ID: "or/c", Lane: "or", Enabled: true, Free: true},
	})
	s := NewProbeScheduler(catalog, registry, ProbeOptions{
		BatchSize: 4, Concurrency: 2, Interval: time.Minute,
	})

	got := modelSet(s.PickTargets())
	if len(got) != 2 || !got["zen/a"] || !got["zen/b"] {
		t.Fatalf("该只探 2 个参与自动探测的模型，实际 %v", got)
	}
	if got["or/c"] {
		t.Fatal("不参与自动探测的车道不该被探")
	}
}

// modelSet 把探测目标转成模型名集合。
func modelSet(targets []probeTarget) map[string]bool {
	out := make(map[string]bool, len(targets))
	for _, tgt := range targets {
		out[tgt.Model] = true
	}
	return out
}

// nameSet 把模型名列表转成集合。
func nameSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}
