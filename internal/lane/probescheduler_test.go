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

// TestProbeSchedulerSkipsFreshProbes 验证**新鲜的结论不重复探**。
//
// ⚠️ 重复探 = 重复花钱。
//
// ⚠️ 但注意：现在一轮是**全量**的，所以"新鲜"不再导致跳过 ——
//
//	15 分钟的间隔 > 10 分钟的 TTL，每轮开始时结论都已经过期了。
//	这条规则只在**手动连点两次**时生效。
func TestProbeSchedulerSkipsFreshProbes(t *testing.T) {
	s, _, catalog := newProbeSetup(t, 10, "a", "b")
	catalog.SetProbe(ProbeResult{
		Model: "a", State: StateAvailable, At: time.Now().UnixMilli(),
	})
	targets := s.PickTargets()
	for _, tgt := range targets {
		if tgt.Model == "a" {
			t.Fatal("新鲜的结论不该被重探")
		}
	}
}

// TestProbeSchedulerRotates 验证**轮转** —— 不会一直探同一批。
//
// ⚠️ 不轮转的话，排序靠前的模型永远被探，
//
//	后面的模型永远探不到 —— 它们的可用性就永远是"未知"。
//
// ⚠️ 现在一轮是**全量**的，轮转只在**限流收缩**时才有意义。
func TestProbeSchedulerRotates(t *testing.T) {
	s, _, _ := newProbeSetup(t, 2, "a", "b", "c", "d", "e")

	seen := map[string]bool{}
	for round := 0; round < 5; round++ {
		targets := s.PickTargets()
		if len(targets) == 0 {
			t.Fatalf("第 %d 轮该有目标", round)
		}
		for _, tgt := range targets {
			seen[tgt.Model] = true
		}
	}
	if len(seen) < 5 {
		t.Fatalf("轮转没覆盖全部模型，只覆盖了 %d 个: %v", len(seen), seen)
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

// TestProbeSchedulerBacksOffOnFailure 验证**连续失败的模型退避**。
//
// ⚠️ 一个一直报错的模型，每轮都探它就是纯浪费。
//
// ⚠️ 退避是**线性**的（不是指数）——
//
//	连续失败 N 次就要等 N 轮再探。
//	所以 "bad" 的探测轮次是 1、2、4、7…（越来越稀），
//	而 "good" 每轮都探。
//
// ⚠️ 限流的模型**每轮会被探两次**（一次 + 一次复核），
//
//	所以这里按"轮次"统计，不按"次数"统计。
func TestProbeSchedulerBacksOffOnFailure(t *testing.T) {
	s, l, catalog := newProbeSetup(t, 10, "good", "bad")
	l.fail["bad"] = true
	// ⚠️ **关掉自适应批量** —— 否则"限流时探得少"会跟退避混在一起，
	//   两个变量互相干扰，测不出谁在起作用。
	s.SetAdaptiveBatch(false)

	badRounds := 0
	goodRounds := 0
	rounds := 8
	for i := 0; i < rounds; i++ {
		// ⚠️ **把上一轮的结论"老化"** ——
		//
		//	生产环境里两轮之间隔 15 分钟 > TTL(10 分钟)，
		//	所以每轮开始时结论都已过期，会被重新探。
		//	测试里连着跑，结论还是"新鲜"的，会被跳过 ——
		//	所以这里手动把时间戳往前推，模拟"过了一轮"。
		old := time.Now().Add(-2 * ProbeTTL).UnixMilli()
		for _, m := range catalog.Models() {
			catalog.SetProbe(ProbeResult{
				Model: m.ID, State: StateAvailable, At: old,
			})
		}

		before := len(l.probedModels())
		s.RunOnce(context.Background())
		seen := map[string]bool{}
		for _, m := range l.probedModels()[before:] {
			seen[m] = true
		}
		if seen["bad"] {
			badRounds++
		}
		if seen["good"] {
			goodRounds++
		}
	}
	if goodRounds != rounds {
		t.Fatalf("good 每轮都该被探，实际 %d/%d", goodRounds, rounds)
	}
	if badRounds >= goodRounds {
		t.Fatalf("失败模型该探得更稀：bad=%d good=%d", badRounds, goodRounds)
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
	// ⚠️ 基数是**候选池的大小**（20 个模型），不是固定的 batchSize。
	batch := s.currentBatchLocked(20)
	s.mu.Unlock()
	if batch != 5 {
		t.Fatalf("批量该降到 20/4=5，实际 %d", batch)
	}

	// ⚠️ **下一轮该只探 5 个**（不是 20 个）。
	//
	// ⚠️ 注意：失败退避也在起作用 —— 上一轮全失败，
	//   所以每个模型的 failures=1，要等一轮才再探。
	//   这正是"连续失败的少探"。
	before := len(l.probedModels())
	s.RunOnce(context.Background())
	secondRound := len(l.probedModels()) - before
	if secondRound > 5 {
		t.Fatalf("限流时该只探 5 个，实际 %d", secondRound)
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
	// 全部正常。
	s.RunOnce(context.Background())

	s.mu.Lock()
	ratio := s.lastThrottleRatio
	batch := s.currentBatchLocked(20)
	s.mu.Unlock()
	if ratio != 0 {
		t.Fatalf("限流比例该是 0，实际 %v", ratio)
	}
	if batch != 20 {
		t.Fatalf("批量该是满的 20，实际 %d", batch)
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
