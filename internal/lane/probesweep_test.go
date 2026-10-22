package lane

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// TestPickTargetsCoversAllModels 验证**一轮探完整个池子**。
//
// ⚠️ 这是用户要的：每 15 分钟**全量**探测一次。
// 之前是"每轮取 N 个" + "10 分钟 TTL 内跳过" ——
// 于是每轮几乎所有模型都被 TTL 跳过，探测形同虚设。
func TestPickTargetsCoversAllModels(t *testing.T) {
	l := &probeLane{id: "zen", fail: map[string]bool{}, state: map[string]ProbeState{}}
	registry := NewRegistry()
	registry.Register(l)
	catalog := NewCatalog()
	models := make([]ModelInfo, 0, 12)
	for i := 0; i < 12; i++ {
		models = append(models, ModelInfo{
			ID: fmt.Sprintf("m%d", i), Lane: "zen", Enabled: true, Free: true,
		})
	}
	catalog.Replace(models)

	s := NewProbeScheduler(catalog, registry, ProbeOptions{
		BatchSize: 4, Concurrency: 2, Interval: 15 * time.Minute,
	})

	// ⚠️ 关键：把 TTL 调到比探测间隔长 ——
	//   这样"结论还新鲜就跳过"那条规则**不会**生效。
	//   （生产环境也是这个关系：15 分钟 > ProbeTTL 10 分钟。）
	targets := s.PickTargets()
	if len(targets) != 12 {
		t.Fatalf("一轮该探完 12 个模型，实际 %d", len(targets))
	}
}

// TestPickTargetsSkipsPaidModels 验证**付费模型不探**（探测会花钱）。
func TestPickTargetsSkipsPaidModels(t *testing.T) {
	l := &probeLane{id: "zen", fail: map[string]bool{}, state: map[string]ProbeState{}}
	registry := NewRegistry()
	registry.Register(l)
	catalog := NewCatalog()
	catalog.Replace([]ModelInfo{
		{ID: "free1", Lane: "zen", Enabled: true, Free: true},
		{ID: "paid1", Lane: "zen", Enabled: true, Free: false},
		{ID: "free2", Lane: "zen", Enabled: true, Free: true},
	})

	s := NewProbeScheduler(catalog, registry, ProbeOptions{
		BatchSize: 10, Concurrency: 2, Interval: 15 * time.Minute,
	})
	targets := s.PickTargets()
	if len(targets) != 2 {
		t.Fatalf("该只探 2 个免费模型，实际 %d", len(targets))
	}
	for _, tgt := range targets {
		if tgt.Model == "paid1" {
			t.Fatal("付费模型不该被探")
		}
	}
}

// TestRunOnceProbesEverything 验证**跑一轮之后每个模型都有结论**。
func TestRunOnceProbesEverything(t *testing.T) {
	l := &probeLane{id: "zen", fail: map[string]bool{}, state: map[string]ProbeState{}}
	registry := NewRegistry()
	registry.Register(l)
	catalog := NewCatalog()
	models := make([]ModelInfo, 0, 8)
	for i := 0; i < 8; i++ {
		models = append(models, ModelInfo{
			ID: fmt.Sprintf("m%d", i), Lane: "zen", Enabled: true, Free: true,
		})
	}
	catalog.Replace(models)

	s := NewProbeScheduler(catalog, registry, ProbeOptions{
		BatchSize: 4, Concurrency: 4, Interval: 15 * time.Minute,
	})
	s.RunOnce(context.Background())

	probed := 0
	for _, m := range catalog.Models() {
		if p := catalog.Probe(m.ID); p.State != StateUnknown {
			probed++
		}
	}
	if probed != 8 {
		t.Fatalf("一轮该探完 8 个模型，实际 %d", probed)
	}
}
