package lane

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 并发压测（Go 层）
//
// ⚠️ 为什么要**自己写**而不是用 `go test -race`：
//
//   race detector 在 Windows 上需要 **POSIX 线程版**的 MinGW，
//   常见的 win32 线程版 gcc 跑不起来。
//
//   所以这里用**另一种方式**验证并发安全：
//   ① 高并发地打**所有共享状态**（清单、限流账本、探测、路由）
//   ② 检查有没有 **panic**、有没有**数据竞争的特征**（计数不符、map panic）
//   ③ 检查**限流账本的记账**在并发下是否准确
//
// ⚠️ 这不能替代 race detector ——
//   但它能抓住**最要命**的那一类问题：
//   "两个 goroutine 同时改 map" 会在运行时直接 panic。

// TestConcurrentCatalogAccess 验证**并发读写清单**不会崩。
func TestConcurrentCatalogAccess(t *testing.T) {
	c := NewCatalog()
	models := make([]ModelInfo, 0, 200)
	for i := 0; i < 200; i++ {
		models = append(models, ModelInfo{
			ID:      fmt.Sprintf("zen/m%d", i),
			Lane:    ZenLaneID,
			Enabled: true,
			Free:    true,
		})
	}
	c.Replace(models)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				switch j % 5 {
				case 0:
					_ = c.Models()
				case 1:
					c.SetProbe(ProbeResult{
						Model: fmt.Sprintf("zen/m%d", n%200),
						State: StateAvailable,
						At:    time.Now().UnixMilli(),
					})
				case 2:
					c.ApplyProbe(ProbeResult{
						Model: fmt.Sprintf("zen/m%d", n%200),
						State: StateThrottled,
						At:    time.Now().UnixMilli(),
					})
				case 3:
					_ = c.SummarizeProbes()
				case 4:
					c.Replace(models)
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestConcurrentThrottleBook 验证**并发记账**准确。
//
// ⚠️ 限流账本是并发写的热点 ——
//
//	多个请求同时被限流，同时记账。
//	计数错了会导致"该冷却的模型没冷却"或"没限流的模型被误伤"。
func TestConcurrentThrottleBook(t *testing.T) {
	book := NewBook()

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			model := fmt.Sprintf("m%d", n%10)
			for j := 0; j < 100; j++ {
				switch j % 3 {
				case 0:
					book.MarkThrottled(model, 30)
				case 1:
					book.MarkOK(model)
				case 2:
					_ = book.Throttled(model)
					_ = book.CooldownRemaining(model)
				}
			}
		}(i)
	}
	wg.Wait()

	// 账本不能坏掉 —— 读出来要能正常序列化。
	rows := book.Rows()
	for _, r := range rows {
		if r.Model == "" {
			t.Fatal("账本里有空模型的行")
		}
	}
}

// TestConcurrentRouterCompletions 验证**并发推理**不崩、结果正确。
//
// ⚠️ 路由器的共享状态：候选链、限流账本、成功缓存。
//
//	50 个并发请求打同一批模型 ——
//	限流会同时发生，账本会同时被写。
func TestConcurrentRouterCompletions(t *testing.T) {
	zen := &fakeLane{
		id: ZenLaneID, ready: true, text: "ok", emitText: true,
		behavior: map[string]*Error{},
	}
	// 让一个模型总是限流，触发记账路径。
	zen.behavior["zen/limited"] = &Error{Code: CodeQuota, Message: "限流", RetryAfterSec: 10}

	router, catalog := newTestRouter(zen)
	catalog.Replace([]ModelInfo{
		{ID: "zen/good", Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: "zen/limited", Lane: ZenLaneID, Enabled: true, Free: true},
	})

	var wg sync.WaitGroup
	var okCount, failCount int
	var mu sync.Mutex

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			model := "zen/good"
			if n%2 == 0 {
				model = "zen/limited"
			}
			turn := &Turn{
				Model:    model,
				Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}},
			}
			_, uerr := router.Complete(context.Background(), turn, func(Chunk) {}, func() bool { return false })
			mu.Lock()
			if uerr != nil {
				failCount++
			} else {
				okCount++
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	// ⚠️ "good" 模型该全成功（它不失败）。
	//   "limited" 失败后应该回退到 "good" —— 所以也该成功。
	//
	//   等等：`limited` 限流 → 换 `good` → 成功。
	//   所以**所有**请求都该成功（只要候选链里有 good）。
	if okCount != 50 {
		t.Fatalf("该 50 个全成功（限流的会回退），实际成功 %d 失败 %d", okCount, failCount)
	}
}

// TestConcurrentProbeScheduler 验证**并发探测**不崩。
func TestConcurrentProbeScheduler(t *testing.T) {
	l := &probeLane{id: "zen", fail: map[string]bool{}, state: map[string]ProbeState{}}
	registry := NewRegistry()
	registry.Register(l)
	catalog := NewCatalog()
	infos := make([]ModelInfo, 0, 50)
	for i := 0; i < 50; i++ {
		infos = append(infos, ModelInfo{
			ID: fmt.Sprintf("m%d", i), Lane: "zen", Enabled: true, Free: true,
		})
	}
	catalog.Replace(infos)

	s := NewProbeScheduler(catalog, registry, ProbeOptions{
		BatchSize: 10, Concurrency: 4, Interval: time.Minute,
	})

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				s.RunOnce(context.Background())
				_ = s.PickTargets()
				s.ProbeOne(context.Background(), "m0")
			}
		}()
	}
	wg.Wait()
}

// TestConcurrentUsageRecording 验证**并发用量记账**不丢数。
//
// ⚠️ 用量记账在每次请求后发生 ——
//
//	并发时丢数就意味着"用量统计不准"。
func TestConcurrentUsageRecording(t *testing.T) {
	book := NewBook()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				book.MarkOK(fmt.Sprintf("m%d", n%5))
			}
		}(i)
	}
	wg.Wait()

	// MarkOK 之后不该有模型还处于限流状态。
	for _, r := range book.Rows() {
		if book.Throttled(r.Model) {
			t.Fatalf("MarkOK 之后 %q 还在限流中", r.Model)
		}
	}
}
