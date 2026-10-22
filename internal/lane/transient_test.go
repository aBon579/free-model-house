package lane

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// flakyLane 是一个"第一次失败、第二次成功"的假车道。
type flakyLane struct {
	id string
	// failFirst 决定前 N 次探测失败。
	failFirst int
	mu        probeLaneMu
	calls     int
}

type probeLaneMu struct {
	mu sync.Mutex
}

func (f *flakyLane) ID() string  { return f.id }
func (f *flakyLane) Ready() bool { return true }
func (f *flakyLane) ListModels(ctx context.Context) ([]ModelInfo, *Error) {
	return nil, nil
}
func (f *flakyLane) Status() LaneStatus { return LaneStatus{ID: f.id, Ready: true} }
func (f *flakyLane) Do(ctx context.Context, turn *Turn, emit func(Chunk)) (*Usage, FinishReason, *Error) {
	return nil, FinishError, &Error{Code: CodeServer, Message: "not implemented"}
}

func (f *flakyLane) Probe(ctx context.Context, model string) ProbeResult {
	f.mu.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.mu.Unlock()
	if n <= f.failFirst {
		// 模拟"端点间歇性挂一下"。
		return ProbeResult{
			Model: model, State: StateUnknown,
			Detail: "Upstream request failed: Endpoint is unavailable.",
			At:     time.Now().UnixMilli(),
		}
	}
	return ProbeResult{Model: model, State: StateAvailable, At: time.Now().UnixMilli()}
}

// TestTransientFailureIsRechecked 验证**瞬时故障会被复核**。
//
// ⚠️ 实测（2026-10-20）：免密车道有模型的端点**间歇性**返回
//
//	"Endpoint is unavailable"（失败 500ms，成功却要 9~14 秒）。
//
//	单次探测失败就写"不可用"会**误伤一个能用的模型** ——
//	用户看到的就是"我的模型怎么变少了"。
func TestTransientFailureIsRechecked(t *testing.T) {
	l := &flakyLane{id: "zen", failFirst: 1}
	registry := NewRegistry()
	registry.Register(l)
	catalog := NewCatalog()
	catalog.Replace([]ModelInfo{
		{ID: "m1", Lane: "zen", Enabled: true, Free: true},
	})

	s := NewProbeScheduler(catalog, registry, ProbeOptions{
		BatchSize: 4, Concurrency: 1, Interval: 15 * time.Minute,
	})
	s.RunOnce(context.Background())

	p := catalog.Probe("m1")
	if p.State != StateAvailable {
		t.Fatalf("瞬时故障复核后该是 available，实际 %q（%s）", p.State, p.Detail)
	}
}

// TestFinalFailureIsNotRechecked 验证**确定性失败不复核**。
//
// ⚠️ 闸门拒绝、地区封锁、明确不可用**问多少次都一样** ——
//
//	再问是白烧额度。
func TestFinalFailureIsNotRechecked(t *testing.T) {
	// ⚠️ "地区受限"是**确定性**的失败 —— 不该复核。
	l := &probeLane{
		id:    "zen",
		fail:  map[string]bool{},
		state: map[string]ProbeState{"m1": StateRegionBlock},
	}
	registry := NewRegistry()
	registry.Register(l)
	catalog := NewCatalog()
	catalog.Replace([]ModelInfo{
		{ID: "m1", Lane: "zen", Enabled: true, Free: true},
	})

	s := NewProbeScheduler(catalog, registry, ProbeOptions{
		BatchSize: 4, Concurrency: 1, Interval: 15 * time.Minute,
	})
	s.RunOnce(context.Background())

	probed := l.probedModels()
	if len(probed) != 1 {
		t.Fatalf("地区受限不该被复核，实际探了 %d 次：%v", len(probed), probed)
	}
	p := catalog.Probe("m1")
	if p.State != StateRegionBlock {
		t.Fatalf("该保持 region-blocked，实际 %q", p.State)
	}
}

// TestFullSweepMarksEverything 验证**一轮之后每个模型都有结论**。
func TestFullSweepMarksEverything(t *testing.T) {
	l := &probeLane{id: "zen", fail: map[string]bool{}, state: map[string]ProbeState{}}
	registry := NewRegistry()
	registry.Register(l)
	catalog := NewCatalog()
	models := make([]ModelInfo, 0, 6)
	for i := 0; i < 6; i++ {
		models = append(models, ModelInfo{
			ID: fmt.Sprintf("m%d", i), Lane: "zen", Enabled: true, Free: true,
		})
	}
	catalog.Replace(models)

	s := NewProbeScheduler(catalog, registry, ProbeOptions{
		BatchSize: 4, Concurrency: 3, Interval: 15 * time.Minute,
	})
	s.RunOnce(context.Background())

	for _, m := range catalog.Models() {
		if p := catalog.Probe(m.ID); p.State == StateUnknown {
			t.Fatalf("模型 %s 该有结论", m.ID)
		}
	}
}
