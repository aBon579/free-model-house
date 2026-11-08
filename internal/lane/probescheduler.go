package lane

import (
	"context"
	"sort"
	"sync"
	"time"
)

// **自动探测调度器**。
// 免密车道的可用性是**波动的** —— 模型随时可能被限流、被风控、被下线。
// 只靠启动时拉一次的清单，用户看到的永远是"未知"。
// ⚠️ **但探测要花钱** —— 每次探测 = 一次真实推理。
//
// 两种模式（由「探测批量自适应」决定）：
//	① **关闭（默认）** —— 每一轮都**全量**探测：
//	   所有参与自动探测的模型（免费 + `AutoProbeable` 车道）都会被探。
//	   数量**恒等于池子大小**，不会"一会多一会少"。
//	② **开启** —— 才允许分批：按「上次探测最久远的优先」排好序，
//	   每轮取前 N 个（N = 批量上限），并立即记账 ——
//	   下一轮自然轮到没探过的那些，轮转不重复、可收敛。
//
// 两种模式共用的闸门：
//	③ 并发上限
//	④ 只探免费的 —— 万一上游改成"先扣费再校验"就会真扣钱
//	⑤ 不参与自动探测的车道（自定义 / OR）一律排除
// ⚠️ "免费判定"和"可用性探测"是两回事：
// 前者用 `ProbeFree` 的 `401 "Missing API key"`（0 次推理），
// 后者要真打一次推理（1 次）才能判"现在能不能用"。
// 详见 docs/design/00-overview.md §5.7。

// ProbeScheduler 是自动探测调度器。
type ProbeScheduler struct {
	mu      sync.Mutex
	catalog *Catalog
	lanes   *Registry
	// onResult 在每次探测后触发（用于持久化 + UI 刷新）。
	onResult func(ProbeResult)
	// onError 在调度器本身出错时触发。
	onError func(error)
	// onRound 在**每一轮结束时**触发（给日志用）。
	onRound func(ProbeRoundSummary)

	// batchSize 是每轮探测的模型数（**上限**）。
	// ⚠️ 这是**上限**，不是定值 ——
	//   实际每轮探几个由 `adaptiveBatch` 决定（见下）。
	batchSize int
	// concurrency 是同时进行的探测数。
	concurrency int
	// interval 是两轮之间的间隔。
	interval time.Duration

	// adaptiveBatch 是**自适应批量**的开关。
	//
	// ⚠️ **默认关闭** —— 关闭时每轮**全量**探测，
	//   面板上的结论才是一致的、数量才是可预期的。
	// 开启后才允许分批（见 pickTargets），且规则是确定的：
	//   上次探测最久远的优先 + 每轮最多 batchSize 个。
	adaptiveBatch bool
	// minBatch 是自适应批量的下限（不能探 0 个）。
	minBatch int
	// lastThrottleRatio 是**上一轮**的限流比例（0..1），用来算下一轮的批量。
	lastThrottleRatio float64
	// roundProbed 是本轮探了几个。
	roundProbed int
	// roundThrottled 是本轮有几个被限流。
	roundThrottled int
	// roundFailureReasons 统计本轮各失败原因的**次数**。
	//
	// ⚠️ **为什么需要它** ——
	//
	//	"探测完成，但模型状态全是「未探测」" 这句话**没有信息量**。
	//	用户不知道是网络断了、CA 证书缺了、还是上游挂了。
	//
	//	最常见的真相是：**所有模型都因为同一个原因失败**
	//	（TLS 握手失败 / 出口被封 / DNS 不通）。
	//	把共性原因提到轮次级别，用户一眼就能看出"整轮都失败"。
	roundFailureReasons map[string]int
	// lastRoundFailure 是**上轮**的共性失败原因（次数最多的那个）。
	lastRoundFailure string
	// lastFailureCount 是那个原因占了多少个。
	lastFailureCount int
	// roundProbedLast 是**本轮实际探了几个**（轮次结束时才准）。
	roundProbedLast int
	// roundThrottledLast 是**本轮有几个被限流**（轮次结束时才准）。
	roundThrottledLast int
	// roundAvailableLast 是**本轮有几个探通了**（轮次结束时才准）。
	//
	// ⚠️ **必须有它** ——
	//   否则小结只能说"8 个失败"，用户会误以为全挂了。
	//   实际上可能是"16 个成功、8 个失败"。
	roundAvailableLast int
	// roundAvailable 是本轮探通了几个。
	roundAvailable int

	// pickGen 是分批轮转的**代次** —— 每做一次分批选取 +1。
	pickGen int
	// lastPicked 记录每个模型上次被选中的代次（0 = 从没被选中过）。
	// ⚠️ 用独立代次而不是轮次号 —— 手动 `PickTargets` 不经过轮次自增，
	//   用轮次号会让"直接连选几次"永远选同一批。
	lastPicked map[string]int
	// round 是当前的轮次编号。
	round int
	// enabled 是自动探测的开关。
	enabled bool
	// lastRun 是上轮结束的时间。
	lastRun time.Time
	// running 标记是否正在跑一轮。
	running bool
	// roundTotal 是本轮要探的模型总数。
	roundTotal int
	// roundDone 是本轮已探完的数量。
	//
	// ⚠️ 这两个是给**前端**看的 ——
	//   整轮探测是异步的（可能几十秒），
	//   用户盯着一个转圈图标不知道要等多久，
	//   有了"12/27"就知道还剩多少。
	roundDone int
	// finalizing 标记"主体探完了，正在收尾（复核限流 / 落结论）"。
	//
	// ⚠️ **为什么要单独一个状态** ——
	//
	//	实测（2026-10-21）：12 个模型在 17.5 秒就探完了，
	//	但整轮 23.6 秒才结束 ——
	//	中间那 6 秒是在**等限流复核的冷却窗口**（3 秒）
	//	加上**写数据库**。
	//
	//	那 6 秒里 `done` 已经是 12/12 了，
	//	但 `running` 还是 true ——
	//	前端只能继续转圈，用户以为卡住了。
	//
	//	现在分开报：**主体探完** ≠ **整轮结束**。
	finalizing bool
	// completedRound 是**已跑完**的轮次编号。
	//
	// ⚠️ **不能用 `round`** —— 它在轮次**开始**时就 +1 了。
	//   前端要判断"这轮真的结束了"，得有一个**结束时**才变的值。
	completedRound int
}

// ProbeRoundSummary 是**一轮探测的小结**（给日志用）。
//
// ⚠️ **为什么需要它** ——
//
//	"探测完成" 这句话**没有任何信息量**。
//	用户看到的是"模型状态全是『未探测』"，却不知道为什么。
//
//	这几项加起来就能定位：
//
//	DurationMs 极短（比如 27 个模型总共 2 秒）
//	  → 请求**根本没发出去** —— 本地就失败了
//	    （证书读不到 / DNS 不通 / 代理配错）
//	DurationMs 正常 + CommonFailure 非空
//	  → 请求发出去了，上游统一拒绝 —— 看 CommonFailure
//	DurationMs 正常 + CommonFailure 为空
//	  → 大概率真的探到了，只是结论不是"可用"
type ProbeRoundSummary struct {
	// Round 是轮次编号。
	Round int
	// Probed 是这轮探了几个模型。
	Probed int
	// Available 是这轮探通了几个。
	//
	// ⚠️ **必须有它** —— 否则只能说"8 个失败"，
	//   用户会以为全挂了（实际可能是"16 个成功、8 个失败"）。
	Available int
	// DurationMs 是这轮总共花了多久（毫秒）。
	DurationMs int64
	// CommonFailure 是**出现次数最多**的失败原因。
	CommonFailure string
	// CommonCount 是那个原因占了多少个。
	CommonCount int
	// ThrottledCount 是这轮有几个被限流。
	ThrottledCount int
}

// SetRoundHook 设置轮次小结的回调（给日志用）。
func (s *ProbeScheduler) SetRoundHook(fn func(ProbeRoundSummary)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onRound = fn
}

// ProbeProgress 是探测进度（给前端看的）。
type ProbeProgress struct {
	// Running 标记是否正在跑一轮。
	Running bool `json:"running"`
	// Finalizing 标记"主体探完了，正在收尾"。
	//
	// ⚠️ 这段时间里 `Running` 仍是 true，
	//   但前端可以据此显示"正在复核"而不是继续转圈。
	Finalizing bool `json:"finalizing"`
	// Total 是本轮的模型总数。
	Total int `json:"total"`
	// Done 是已探完的数量。
	Done int `json:"done"`
	// Round 是**已跑完**的轮次编号 —— 每跑完一轮就 +1。
	//
	// ⚠️ 前端靠它判断"这一轮真的结束了" ——
	//   只看 `running` 从 true 变 false 是不够的：
	//   极快的轮次（0 个目标）可能**整个过程都观测不到**，
	//   按钮会一直卡在"正在探测"。
	//
	//   轮次号变了 = 确实跑完了一轮。
	Round int `json:"round"`
	// LastRunAt 是上轮结束的时间（epoch 毫秒）。
	LastRunAt int64 `json:"lastRunAt,omitempty"`
	// LastFailure 是**上轮最常见的失败原因**。
	//
	// ⚠️ 空字符串表示上轮没有失败。
	//
	//	它的用途：把"探测完成但全是未探测"翻译成
	//	"这一轮全部因为 X 失败" —— 前者让人困惑，后者让人能行动。
	LastFailure string `json:"lastFailure,omitempty"`
	// LastFailureCount 是这个原因占了多少个。
	LastFailureCount int `json:"lastFailureCount,omitempty"`
	// LastAvailable 是上轮探通了几个。
	//
	// ⚠️ **必须有它** —— 否则只能说"8 个失败"，
	// 用户会以为全挂了（实际可能是"16 个成功、8 个失败"）。
	LastAvailable int `json:"lastAvailable,omitempty"`
	// RoundCounts 是**上一轮批量探测**的统计。
	//
	// ⚠️ **这是"探测结论"卡片该显示的数字** ——
	//   "共探测 N 个"说的必须是**同一轮**里的事，
	//   不能把几天里手动探过的模型都算上。
	//
	// ⚠️ 间隔过了它就**不再代表当下**（readStale 判定见 handleProbes），
	//   由管理面决定显示 0 还是显示带"过期"标注的旧值。
	RoundProbed    int `json:"roundProbed,omitempty"`
	RoundAvailable int `json:"roundAvailable,omitempty"`
	RoundFailed    int `json:"roundFailed,omitempty"`
	RoundUnknown   int `json:"roundUnknown,omitempty"`
	// RoundAt 是上一轮**结束**的时间（epoch ms）。
	//
	// ⚠️ 管理面用它判断"这轮结论过了探测间隔没有" ——
	//   过了就该显示"未探测"而不是沿用旧数字。
	RoundAt int64 `json:"roundAt,omitempty"`
}

// Progress 返回当前探测进度。
func (s *ProbeScheduler) Progress() ProbeProgress {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := ProbeProgress{
		Running:    s.running,
		Finalizing: s.finalizing,
		Total:      s.roundTotal,
		Done:       s.roundDone,
		Round:      s.completedRound,
	}
	if !s.lastRun.IsZero() {
		p.LastRunAt = s.lastRun.UnixMilli()
	}
	p.LastFailure = s.lastRoundFailure
	p.LastFailureCount = s.lastFailureCount
	p.LastAvailable = s.roundAvailableLast

	// ⚠️ **上一轮的统计** —— 只含批量轮次，手动单探不进来。
	//   失败 = 探了但不是可用也不是未知；未知 = 结论不明（含瞬时故障复核后仍失败）。
	p.RoundProbed = s.roundProbedLast
	p.RoundAvailable = s.roundAvailableLast
	p.RoundFailed = s.roundProbedLast - s.roundAvailableLast
	if p.RoundFailed < 0 {
		p.RoundFailed = 0
	}
	p.RoundAt = s.lastRun.UnixMilli()
	return p
}

// ProbeOptions 是调度器的配置。
type ProbeOptions struct {
	// BatchSize 是每轮探测几个模型（默认 4）。
	BatchSize int
	// Concurrency 是并发探测数（默认 2）。
	Concurrency int
	// Interval 是轮询间隔（默认 10 分钟）。
	Interval time.Duration
}

func NewProbeScheduler(catalog *Catalog, lanes *Registry, opts ProbeOptions) *ProbeScheduler {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 4
	}
	if opts.Concurrency <= 0 {
		opts.Concurrency = 2
	}
	if opts.Interval <= 0 {
		opts.Interval = 10 * time.Minute
	}
	return &ProbeScheduler{
		catalog:     catalog,
		lanes:       lanes,
		batchSize:   opts.BatchSize,
		concurrency: opts.Concurrency,
		interval:    opts.Interval,
		lastPicked:  map[string]int{},
		enabled:     true,
		// ⚠️ **默认关闭** —— 关闭 = 每轮全量。
		adaptiveBatch: false,
		minBatch:      1,
	}
}

// SetResultHook 设置结果回调。
func (s *ProbeScheduler) SetResultHook(fn func(ProbeResult)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onResult = fn
}

// SetErrorHook 设置错误回调。
func (s *ProbeScheduler) SetErrorHook(fn func(error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onError = fn
}

// Start 启动调度循环。
// ⚠️ 它**不阻塞** —— 立刻返回，后台跑。
// ⚠️ 间隔是**可动态调整**的 ——
// 管理台改了"探测间隔"，要**立即生效**，不能等重启。
//
// ⚠️ **下一轮 = 本轮开始 + 间隔（墙钟对齐）** ——
//
//	之前是"跑完再等一个完整间隔"：一轮跑 80 秒，
//	实际周期就成了 80s + 15min = 16 分 20 秒，
//	用户设 15 分钟、体感永远超时 —— 就是这个累计漂移。
//	现在跑多久就从间隔里扣多少；间隔 < 一轮耗时时会**立刻接着跑下一轮**。
func (s *ProbeScheduler) Start(ctx context.Context) {
	go func() {
		// ⚠️ 启动后**先等一会儿**再探 ——
		// 刚启动时用户可能正要开始用，探测会跟真实请求抢配额。
		// ⚠️ 首轮的触发点也从**现在**算起 —— 30 秒启动延迟
		//   不该平移后面所有轮次（对齐墙钟后自然吸收）。
		roundStart := time.Now().Add(30 * time.Second)
		nextFire := roundStart.Add(s.Interval())

		timer := time.NewTimer(time.Until(nextFire))
		defer timer.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
				started := time.Now()
				s.RunOnce(ctx)
				// ⚠️ 下一轮 = **本轮开始** + 当前间隔 —— 跑的时长被扣掉。
				s.mu.Lock()
				interval := s.interval
				s.mu.Unlock()
				next := started.Add(interval)
				// 已经过点了（一轮比间隔还长）→ 立刻跑下一轮。
				if d := time.Until(next); d > 0 {
					timer.Reset(d)
				} else {
					timer.Reset(time.Second)
				}
			}
		}
	}()
}

// NextFireAt 返回"以 roundStart 为本轮开始"时下一轮的触发点。
// ⚠️ 语义：墙钟对齐 —— 间隔从**开始**算，不从"跑完"算。
func (s *ProbeScheduler) NextFireAt(roundStart time.Time) time.Time {
	return roundStart.Add(s.Interval())
}

// SetInterval 调整探测间隔（动态生效）。
func (s *ProbeScheduler) SetInterval(d time.Duration) {
	if d <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.interval = d
}

// Interval 返回当前探测间隔。
func (s *ProbeScheduler) Interval() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.interval
}

// SetBatchSize 调整每轮的探测数（动态生效）。
func (s *ProbeScheduler) SetBatchSize(n int) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.batchSize = n
}

// SetEnabled 开关自动探测。
// ⚠️ 关掉时，正在进行的一轮**不打断** ——
// 让它跑完比中途取消更干净（半截的探测结果会让人困惑）。
func (s *ProbeScheduler) SetEnabled(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.enabled = on
}

// Enabled 返回自动探测是否开启。
func (s *ProbeScheduler) Enabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enabled
}

// RunOnce 跑一轮探测。
// ⚠️ **同一时刻只允许一轮** ——
// 否则慢的一轮没跑完，下一轮又开始了，配额会被探测吃光。
func (s *ProbeScheduler) RunOnce(ctx context.Context) {
	s.run(ctx, false)
}

// RunOnceManual 跑一轮探测，**忽略自动探测的开关**。
// ⚠️ 用户在界面上点「探测一轮」是**明确意图** ——
// 哪怕自动探测关着，也该照跑。
func (s *ProbeScheduler) RunOnceManual(ctx context.Context) {
	s.run(ctx, true)
}

// run 是探测循环的内部实现。
func (s *ProbeScheduler) run(ctx context.Context, manual bool) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	// ⚠️ 关掉了就不跑 —— 但**手动触发**的探测不受这个限制。
	if !manual && !s.enabled {
		s.mu.Unlock()
		return
	}
	s.running = true
	// ⚠️ 轮次编号 —— 退避靠它计算"隔了几轮"。
	s.round++
	round := s.round
	// ⚠️ **重置本轮计数** —— 自适应批量要用。
	s.roundProbed = 0
	s.roundThrottled = 0
	s.roundFailureReasons = map[string]int{}
	s.roundProbedLast = 0
	s.roundThrottledLast = 0
	s.roundAvailableLast = 0
	s.roundAvailable = 0
	s.mu.Unlock()
	roundStart := time.Now()

	defer func() {
		s.mu.Lock()
		s.running = false
		s.finalizing = false
		s.lastRun = time.Now()
		// ⚠️ **轮次完成标记** —— 前端靠它知道"这轮真的结束了"。
		s.completedRound = round
		// ⚠️ **归纳本轮的共性失败原因** ——
		//
		//	"探测完成，但全是未探测" 要能翻译成
		//	"这一轮 N 个模型都因为 X 失败"。
		//
		//	最常见的真相是**所有模型因同一个原因失败**
		//	（TLS 握手失败 / 出口被封 / DNS 不通）。
		//	只报次数最多的那个 —— 那是"整轮失败"的特征。
		//
		// ⚠️ **必须先读再清零** —— 之前顺序反了，
		//   读到的永远是 0（日志里写着"0 个模型"，实际探了 28 个）。
		s.roundProbedLast = s.roundProbed
		s.roundThrottledLast = s.roundThrottled
		s.roundAvailableLast = s.roundAvailable
		s.roundProbed = 0
		s.roundTotal = 0
		s.roundDone = 0
		s.lastRoundFailure = ""
		s.lastFailureCount = 0
		for reason, count := range s.roundFailureReasons {
			if count > s.lastFailureCount {
				s.lastRoundFailure = reason
				s.lastFailureCount = count
			}
		}
		// ⚠️ **轮次小结** ——
		//
		//	"探测完成" 这句话没有信息量。
		//	用户看到的是"状态全是未探测"，却不知道原因。
		//
		//	把**共性失败原因**和**耗时**说出来，
		//	才能一眼看出"是网络问题"还是"是上游限流"。
		if s.onRound != nil {
			s.onRound(ProbeRoundSummary{
				Round:          round,
				Probed:         s.roundProbedLast,
				Available:      s.roundAvailableLast,
				DurationMs:     time.Since(roundStart).Milliseconds(),
				CommonFailure:  s.lastRoundFailure,
				CommonCount:    s.lastFailureCount,
				ThrottledCount: s.roundThrottledLast,
			})
		}
		s.mu.Unlock()
	}()

	// ⚠️ **手动探测无视"结论还新鲜"和失败退避** ——
	//   用户点了就是想现在重新探一遍。
	targets := s.pickTargets(manual)
	if len(targets) == 0 {
		return
	}

	// ⚠️ **记下本轮总量** —— 前端要显示"正在探测 12/27"。
	//   不知道总量的话，用户只能盯着一个转圈图标干等。
	s.mu.Lock()
	s.roundTotal = len(targets)
	s.roundDone = 0
	s.mu.Unlock()

	// 并发受限地探。
	//
	// ⚠️ **先收集结果，再统一复核** ——
	//   限流是**出口级**的现象，20 个模型各自等 10 秒就是 200 秒。
	//   正确做法：一轮里等一次，然后**一起**再问。
	sem := make(chan struct{}, s.concurrency)
	var wg sync.WaitGroup
	// ⚠️ **键是带车道前缀的模型 id** —— 不同车道的同名模型天然不撞。
	results := make(map[string]ProbeResult, len(targets))
	var mu sync.Mutex
	throttled := make([]probeTarget, 0)
	transient := make([]probeTarget, 0)

	for _, t := range targets {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(model, laneID string, at int) {
			defer wg.Done()
			defer func() { <-sem }()
			r, kind := s.probeOne(ctx, laneID, model, at)
			mu.Lock()
			results[model] = r
			switch kind {
			case probeThrottled:
				throttled = append(throttled, probeTarget{Model: model, Lane: laneID})
			case probeTransient:
				transient = append(transient, probeTarget{Model: model, Lane: laneID})
			}
			mu.Unlock()
			// ⚠️ **推进进度** —— 前端要显示"12/27"。
			s.mu.Lock()
			s.roundDone++
			s.mu.Unlock()
		}(t.Model, t.Lane, round)
	}
	wg.Wait()

	// ⚠️ **主体探完了** —— 从这里开始是"收尾"：
	//   瞬时故障复核、限流延迟复核、落结论。
	//
	//   这段时间可能好几秒（限流复核要等冷却窗口），
	//   前端要能区分"还在探"和"正在收尾"。
	s.mu.Lock()
	s.finalizing = true
	s.mu.Unlock()

	// ⚠️ **两种复核**要**并行**做 ——
	//
	//   瞬时故障复核等 `ProbeTransientDelay`（2 秒），
	//   限流复核等 `ProbeRecheckDelay`（3 秒）。
	//
	//   之前是**串行**的：先等 2 秒探完，再等 3 秒探完 ——
	//   收尾**整整 5 秒**。
	//
	//   但它们等的是**两件不同的事**：
	//     - 瞬时故障：那个端点的**冷启动恢复**
	//     - 限流：这个**出口**的冷却窗口
	//
	//   两件事**互不相干**，没有理由要排队。
	//   并行做完 → 收尾只要 **max(2, 3) = 3 秒**，省 2 秒。
	//
	// ⚠️ **并发安全** ——
	//   两个复核可能写**同一个模型**吗？不能 ——
	//   一个模型在一轮里只会有一种失败性质（`probeKind` 是互斥的）。
	//   但它们**都**写 `results` map —— 所以要加锁。
	var recheckMu sync.Mutex
	writeResult := func(model string, r ProbeResult) {
		recheckMu.Lock()
		results[model] = r
		recheckMu.Unlock()
	}

	var wgRecheck sync.WaitGroup
	wgRecheck.Add(2)
	go func() {
		defer wgRecheck.Done()
		s.recheckTransient(ctx, transient, writeResult)
	}()
	go func() {
		defer wgRecheck.Done()
		lanes := make(map[string]Lane, 2)
		for _, t := range throttled {
			if l, ok := s.lanes.Get(t.Lane); ok {
				lanes[t.Lane] = l
			}
		}
		s.recheckThrottled(ctx, lanes, throttled, writeResult)
	}()
	wgRecheck.Wait()

	// 统一落结论。
	for _, r := range results {
		s.finishProbe(r, true)
	}
}

// ProbeRecheckDelay 是限流结论**再确认**的等待时间。
//
// ⚠️ **为什么是"等一会儿"而不是"立刻再问一次"** ——
//
// 免密车道的配额**按出口 IP 记账**，以秒级波动。
// 上游的一次拒绝只是"这个出口在这一刻"的快照 ——
// **不是这个模型的属性**。
//
// 所以负面结论**不要立刻**再问：
//   - 立刻再问，问的还是**同一个出口的同一刻** ——
//     问十次也是十次 429，白烧配额。
//   - 等几秒再问，出口状态可能已经变了 ——
//     那一刻的成功才说明"模型能用，只是刚才撞墙了"。
//
// ⚠️ 这个"等"跟**限流冷却**（`DefaultQuotaCooldownSec` = 10 秒）
//
//	是同一个量级 —— 因为它们说的是同一件事：
//	**这个出口现在拥挤**。
//
// ⚠️ **整轮只等一次** ——
//
//	逐个模型各等 10 秒的话，20 个模型就是 200 秒。
//	但它们等的是**同一个出口**的同一个冷却窗口 ——
//	所以一轮里等一次，然后**一起**再问，才是对的。
//
// ⚠️ 等 3 秒而不是 10 秒 ——
//
//	免密车道的配额恢复是**秒级**的（实测 retry-after 常常只有几秒），
//	等 10 秒太保守 —— 而且整轮只等一次，代价可以接受。
//	3 秒足够覆盖"挤了一下"的场景。
const ProbeRecheckDelay = 3 * time.Second

// recheckThrottled 对本轮的**限流**结论**延迟再确认一次**。
//
// ⚠️ 只对**限流**复核 ——
//
//	地区封锁 / 出口被风控 / 明确不可用**换出口也没用**
//	（或者根本不是出口问题），再问一次只是白烧额度。
//
// ⚠️ **一次等待，一起再问** —— 限流是**出口级**的现象，
//
//	等一次就够（等的是同一个出口的同一个冷却窗口）。
func (s *ProbeScheduler) recheckThrottled(
	ctx context.Context,
	lanes map[string]Lane,
	throttled []probeTarget,
	write func(model string, r ProbeResult),
) {
	if len(throttled) == 0 {
		return
	}
	// ⚠️ 睡一下再问 —— 问的还是同一出口的同一刻就没意义了。
	select {
	case <-ctx.Done():
		return
	case <-time.After(ProbeRecheckDelay):
	}

	// ⚠️ **并发复核** ——
	//
	//	之前是**逐个串行**探的：20 个限流模型 = 20 次串行探测。
	//	每个探测可能要好几秒（成功要 3~35 秒）——
	//	实测收尾阶段卡了 **42 秒**。
	//
	//	但复核**不占额外的冷却窗口**（等过了就一起问），
	//	所以并发问没有"问同一刻"的问题。
	//
	//	用**同一套并发上限**（`s.concurrency`）—— 别把上游打疼。
	s.recheckConcurrent(ctx, lanes, throttled, write, s.concurrency, func(rr ProbeResult) {
		if rr.State == StateAvailable {
			rr.Detail = "复核成功（首次是限流，等了 " +
				itoa(int(ProbeRecheckDelay/time.Second)) + " 秒就好）"
		}
	})
}

// recheckConcurrent 并发地复核一批模型。
//
// ⚠️ **为什么并发** ——
//
//	复核的对象是一批"限流"的模型，通常有十几个。
//	串行探 = 十几倍的单次探测耗时。
//	实测收尾卡了 42 秒，界面上一动不动。
//
// ⚠️ **并发上限跟主探测一样** —— 别把上游打疼。
func (s *ProbeScheduler) recheckConcurrent(
	ctx context.Context,
	lanes map[string]Lane,
	targets []probeTarget,
	write func(model string, r ProbeResult),
	concurrency int,
	decorate func(ProbeResult),
) {
	if len(targets) == 0 {
		return
	}
	if concurrency <= 0 {
		concurrency = 2
	}
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		l, ok := lanes[t.Lane]
		if !ok || !l.Ready() {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(lane Lane, target probeTarget) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() != nil {
				return
			}
			// ⚠️ 上游只认**裸名** —— 车道前缀是我们自己加的。
			_, bare := ParseModelID(target.Model)
			rr := lane.Probe(ctx, bare)
			// ⚠️ 存**全名**（带前缀）—— 键要唯一。
			rr.Model = target.Model
			if decorate != nil {
				decorate(rr)
			}
			write(target.Model, rr)
		}(l, t)
	}
	wg.Wait()
}

// probeKind 是一个探测结论的**性质**。
//
// ⚠️ 它决定要不要**复核**。
type probeKind int

const (
	// probeFinal 结论就是它 —— 不用复核。
	probeFinal probeKind = iota
	// probeThrottled 限流 —— 要等冷却窗口再问一次。
	probeThrottled
	// probeTransient 瞬时故障 —— **立刻再问一次**。
	//
	// ⚠️ 实测（2026-10-20）：免密车道有模型的端点**间歇性**返回
	//   "Endpoint is unavailable"（失败 500ms，成功却要 9~14 秒）。
	//   单次失败就写"不可用"会**误伤一个能用的模型**。
	probeTransient
)

// recheckTransient 对**瞬时故障**立刻再问一次。
//
// ⚠️ 为什么要复核 ——
//
// 上游的端点会**间歇性**挂一下（实测 6 次里 4 次失败）。
// 单次探测失败就写下负面结论，会把一个**其实能用**的模型
// 从面板上抹掉 —— 用户看到的就是"我的模型怎么变少了"。
//
// 一次成功就恢复成"可用"。这跟"真实流量压过探测"是同一个精神：
// **活着的证据比一次采样硬**。
//
// ⚠️ 只复核**瞬时**故障（传输/服务端/超时/空响应）——
//
//	闸门拒绝、地区封锁、明确不可用**问多少次都一样**，再问是白烧额度。
//
// ProbeTransientDelay 是瞬时故障**复核**前的等待时间。
//
// ⚠️ **为什么要等** ——
//
// 实测（2026-10-20）：那个间歇失败的端点，
// 失败只要 **500ms**，但**恢复要 9~14 秒**（冷启动 + 路由）。
// 立刻再问大概率还是挂的 —— 问了也是白问。
//
// 等 2 秒是个折中：足够让"挤了一下"的情况恢复，
// 又不至于让整轮探测拖太久。
const ProbeTransientDelay = 2 * time.Second

func (s *ProbeScheduler) recheckTransient(
	ctx context.Context,
	targets []probeTarget,
	write func(model string, r ProbeResult),
) {
	if len(targets) == 0 {
		return
	}

	// ⚠️ **等一次，一起再问** ——
	//
	// 之前是**逐个模型各等 2 秒**（串行）：
	// 3 个瞬时故障 = 多等 6 秒，5 个 = 多等 10 秒。
	//
	// 但这些模型等的**是同一件事** ——
	// 那个端点的冷启动恢复。一起等一次就够了。
	//
	// 这跟限流复核（`recheckThrottled`）是同一个道理。
	select {
	case <-ctx.Done():
		return
	case <-time.After(ProbeTransientDelay):
	}

	// ⚠️ **并发复核** —— 理由跟限流复核一样（见 `recheckConcurrent`）。
	lanes := make(map[string]Lane, 2)
	for _, t := range targets {
		if l, ok := s.lanes.Get(t.Lane); ok {
			lanes[t.Lane] = l
		}
	}
	s.recheckConcurrent(ctx, lanes, targets, write, s.concurrency, func(rr ProbeResult) {
		if rr.State == StateAvailable {
			rr.Detail = "复核成功（首次是瞬时故障）"
		}
	})
}

// probeOne 探一个模型，**只探不落结论**。
//
// ⚠️ 返回 (结论, 性质) —— 限流和瞬时故障要等整轮结束后**一起复核**。
//
// ⚠️ `model` 是**带车道前缀**的 id；发给上游时要去掉前缀。
func (s *ProbeScheduler) probeOne(
	ctx context.Context,
	laneID, model string,
	round int,
) (ProbeResult, probeKind) {
	l, ok := s.lanes.Get(laneID)
	if !ok || !l.Ready() {
		return ProbeResult{}, probeFinal
	}
	// ⚠️ 上游只认**裸名** —— 车道前缀是我们自己加的。
	_, bare := ParseModelID(model)
	result := l.Probe(ctx, bare)
	// ⚠️ 结论用**全名**（带前缀）—— 键要唯一。
	result.Model = model
	switch result.State {
	case StateThrottled:
		return result, probeThrottled
	case StateUnknown:
		// ⚠️ **"未探测"可能是瞬时故障** ——
		//   传输/5xx/超时都落到 unknown。
		//   它们值得再问一次（见 recheckTransient）。
		return result, probeTransient
	default:
		return result, probeFinal
	}
}

// finishProbe 把一个探测结论**落盘**（退避记账 + 写回模型行 + 通知回调）。
//
// ⚠️ `inRound` 报告这次探测是不是**批量轮次的一部分**。
//
//	false = 手动单探（ProbeOne）—— 不计入轮次统计，
//	否则"共探测 N 个"会混进不在这一轮里的模型，而且跨轮累加，
//	面板上的数字越滚越大却对不上任何一轮（实测踩过）。
func (s *ProbeScheduler) finishProbe(result ProbeResult, inRound bool) {
	if result.Model == "" {
		return
	}
	s.mu.Lock()
	hook := s.onResult
	// ⚠️ **退避与轮次号只对批量轮次记账** ——
	//   手动单探不该把"上次被探的轮次"刷上去，
	//   否则一次手动探测会让模型在自动轮的分批里被跳过。
	if inRound {
		switch result.State {
		case StateAvailable:
			// ⚠️ **统计探通的个数** ——
			//   小结里要说"16 个可用、8 个失败"，
			//   不能只说"8 个失败"（那会让人以为全挂了）。
			s.roundAvailable++
		}
		// ⚠️ **更新限流比例** —— 给下一轮的自适应批量用。
		// ⚠️ 只算"限流"，不算其它失败 ——
		//   闸门拒绝/模型下架**不是**拥挤，它们不会因为少探就恢复。
		if result.State == StateThrottled {
			s.roundThrottled++
		}
		if s.roundProbed > 0 {
			s.lastThrottleRatio = float64(s.roundThrottled) / float64(s.roundProbed)
		}
		s.roundProbed++
	} else {
		// 手动单探也要清失败计数吗？不 —— 成功已经在上面清了。
		// 这里只保证**不加轮次统计**。
	}
	s.mu.Unlock()

	// ⚠️ **写回模型行** —— 状态、延迟、能力来源。
	// 这不该依赖调用方记得做 —— 探测的结论**就该**体现在清单上。
	// 所以它放在调度器内部。
	s.catalog.ApplyProbe(result)

	// ⚠️ **统计失败原因** —— 只统计批量轮次的，
	// "这一轮 N 个模型因为同一个原因失败"说的必须是**同一轮**的事。
	if inRound {
		s.mu.Lock()
		if result.State != StateAvailable && result.Detail != "" {
			if s.roundFailureReasons == nil {
				s.roundFailureReasons = map[string]int{}
			}
			s.roundFailureReasons[result.Detail]++
		}
		s.mu.Unlock()
	}

	if hook != nil {
		hook(result)
	}
}

// ProbeOne 探一个指定的模型（同步）。
// ⚠️ **这会消耗上游额度** —— 一次真实推理。
// ⚠️ 它**不走退避** —— 手动探测是用户的明确意图，不该被自动策略拦住。
//
// ⚠️ `model` 是**带车道前缀**的 id（`zen/mimo-v2.6-flash-free`）。
func (s *ProbeScheduler) ProbeOne(ctx context.Context, model string) ProbeResult {
	// 找到这个模型在哪条车道。
	laneID := ""
	for _, m := range s.catalog.Models() {
		if m.ID == model {
			laneID = m.Lane
			break
		}
	}
	if laneID == "" {
		// ⚠️ 兼容**裸名** —— 前端可能传没带前缀的。
		laneID, model = ParseModelID(model)
		if laneID == "" {
			return ProbeResult{
				Model: model, State: StateUnknown,
				Detail: "清单里没有这个模型",
				At:     time.Now().UnixMilli(),
			}
		}
		model = FormatModelID(laneID, model)
	}
	l, ok := s.lanes.Get(laneID)
	if !ok || !l.Ready() {
		return ProbeResult{
			Model: model, State: StateUnavailable,
			Detail: "车道未就绪",
			At:     time.Now().UnixMilli(),
		}
	}
	// ⚠️ 上游只认**裸名**。
	_, bare := ParseModelID(model)
	result := l.Probe(ctx, bare)
	// ⚠️ 结论用**全名** —— 键要唯一。
	result.Model = model

	// ⚠️ **不计入轮次统计** —— 手动单探不是批量轮的一部分，
	//   混进去会让"共探测 N 个"跨轮累加（实测踩过）。
	s.finishProbe(result, false)
	return result
}

// PickTargets 选出这一轮要探的模型。
//
// ⚠️ 选择规则：
//
//	① 只探**清单里标记免费**的模型 —— 探测付费模型可能真扣钱
//	② 排除**不参与自动探测**的车道（自定义 / OR）
//	③ **关闭自适应（默认）= 全量** —— 池子里的每个模型都探
//	④ **开启自适应 = 分批** —— 上次探测最久远的优先，每轮最多 batchSize 个
//
// ⚠️ **不再按"结论新鲜 / 失败退避 / 限流冷却"跳过** ——
//
//	那三条都会让每轮的数量随上轮结果漂移，用户看到的就是
//	"一会多一会少，规则不可预期"。现在：
//	- 关闭自适应：数量恒等于池子大小；
//	- 开启自适应：数量恒等于 min(batchSize, 池子大小)（限流时才临时收缩）。
//
// 排序是**确定**的：先按上次被探的轮次（最久远的在前），再按模型 id。
// 所以同一份清单、同样的状态，每轮的选择完全可复现。
func (s *ProbeScheduler) PickTargets() []probeTarget {
	return s.pickTargets(false)
}

// pickTargets 选出这一轮要探的模型。
//
// ⚠️ `manual` 表示这是**用户手动触发**的 —— 手动探测**永远全量**：
//
//	用户点「探测一轮」就是想现在把每个模型都看一遍。
func (s *ProbeScheduler) pickTargets(manual bool) []probeTarget {
	models := s.catalog.Models()

	// 候选池 = **免费** + **参与自动探测的车道**。
	// ⚠️ 用模型 id 去重 —— 防御清单里出现重复行。
	pool := make([]probeTarget, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, m := range models {
		// ⚠️ **只探免费的** —— 探测付费模型可能真扣钱。
		if !m.Free {
			continue
		}
		if seen[m.ID] {
			continue
		}
		// ⚠️ **用用户自己 Key 的车道不参与自动探测** ——
		//
		//	实测（2026-10-20）：一轮 18 个 OR 模型就把当天的
		//	`free-models-per-day` 配额打满了。
		//	那是**用户自己的额度** —— 拿它做每 10 分钟一轮的
		//	全量探测不合适。OR 的可用性由**真实流量**验证。
		if l, ok := s.lanes.Get(m.Lane); ok && !laneAutoProbeable(l) {
			continue
		}
		seen[m.ID] = true
		pool = append(pool, probeTarget{Model: m.ID, Lane: m.Lane})
	}

	if len(pool) == 0 {
		return nil
	}

	// 读状态（不持有 catalog 锁，只短暂持有自己的锁）。
	s.mu.Lock()
	adaptive := s.adaptiveBatch
	for i := range pool {
		pool[i].Picked = s.lastPicked[pool[i].Model]
	}
	s.mu.Unlock()

	// ⚠️ **确定性排序** —— 上次被选中最久远的优先（从没选过 = 0，排最前），
	//   同代次按模型 id。同一份清单、同样的状态 → 每轮顺序完全一致。
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].Picked != pool[j].Picked {
			return pool[i].Picked < pool[j].Picked
		}
		return pool[i].Model < pool[j].Model
	})

	// ⚠️ **关闭自适应（默认）→ 全量** —— 每轮都把池子探完。
	// 手动探测同理：用户要的就是"现在全看一遍"。
	if manual || !adaptive {
		return pool
	}

	// ⚠️ **开启自适应 → 分批**。
	//
	//	按上面的确定性顺序取前 N 个，并**立即记账**（代次 +1）——
	//	下一轮排序时它们会排到后面，于是自然轮到没选过的模型。
	//	所以：相邻轮次**不重复**，覆盖一圈后可预期地收敛（回到开头）。
	s.mu.Lock()
	batch := s.currentBatchLocked(len(pool))
	if batch <= 0 || batch > len(pool) {
		batch = len(pool)
	}
	s.pickGen++
	for _, t := range pool[:batch] {
		s.lastPicked[t.Model] = s.pickGen
	}
	s.mu.Unlock()

	return pool[:batch]
}

// DefaultAdaptiveBatch 是**开启自适应**时每轮的批量上限。
//
// ⚠️ 关闭自适应（默认）时**不使用**它 —— 那时每轮是全量。
const DefaultAdaptiveBatch = 8

// currentBatchLocked 算这一轮**实际**探几个。
//
//	关闭自适应        → 池子多大就探多大（**全量**）
//	开启自适应        → min(batchSize, 池子大小)
//	开启 + 上游越拥挤 → 按上轮限流比例再收缩（至少 minBatch 个）
//
// ⚠️ 这里不能访问 catalog（会死锁 —— catalog 有自己的锁，
//
//	而 PickTargets 可能已经持有它）。所以用**上轮的结果**来算。
//
// ⚠️ 调用方必须**持有锁**。
func (s *ProbeScheduler) currentBatchLocked(poolSize int) int {
	if poolSize <= 0 {
		return 0
	}
	if !s.adaptiveBatch {
		return poolSize
	}
	// 开启自适应：基数是**批量上限**，不是池子大小 ——
	// 否则"自适应"等于没开（池子多大就探多大）。
	base := s.batchSize
	if base <= 0 || base > poolSize {
		base = poolSize
	}
	if s.lastThrottleRatio <= 0 {
		return base
	}
	ratio := s.lastThrottleRatio
	factor := 1.0
	switch {
	case ratio >= 0.75:
		factor = 0.25
	case ratio >= 0.5:
		factor = 0.5
	case ratio >= 0.25:
		factor = 0.75
	}
	batch := int(float64(base) * factor)
	if batch < s.minBatch {
		batch = s.minBatch
	}
	if batch > base {
		batch = base
	}
	return batch
}

// SetAdaptiveBatch 开/关自适应批量。
//
// ⚠️ **关闭 = 每轮全量**；开启 = 按确定性规则分批。
func (s *ProbeScheduler) SetAdaptiveBatch(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adaptiveBatch = on
}

// AdaptiveBatch 返回当前是否开启自适应批量（给状态展示用）。
func (s *ProbeScheduler) AdaptiveBatch() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.adaptiveBatch
}

// probeTarget 是一个探测目标。
type probeTarget struct {
	// Model 是**带车道前缀**的模型 id（`zen/mimo-v2.6-flash-free`）。
	//
	// ⚠️ 用它当键 —— 不同车道的同名模型天然不撞。
	Model string
	Lane  string
	// Picked 是这个模型**上次被选中探测的代次**（0 = 从没选过）。
	// 分批时按它升序排 —— 最久没选中的优先。
	Picked int
}

// ProbeSummary 是一轮探测的统计。
type ProbeSummary struct {
	Probed int
	// Available 是有多少个可用。
	Available int
	// Failed 是有多少个失败。
	Failed int
	// Unknown 是有多少个结论不明。
	Unknown int
}

func (c *Catalog) SummarizeProbes() ProbeSummary {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var s ProbeSummary
	for _, p := range c.probes {
		s.Probed++
		switch p.State {
		case StateAvailable:
			s.Available++
		case StateUnknown:
			s.Unknown++
		default:
			s.Failed++
		}
	}
	return s
}
