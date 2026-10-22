package lane

import (
	"context"
	"testing"
	"time"
)

// probeStubLane 是一个**立刻失败**的假车道。
//
// ⚠️ 用它来验证"轮次小结的数字对不对" ——
//
//	失败快，所以一轮几毫秒就完（真实场景要几十秒）。
type probeStubLane struct {
	id     string
	ready  bool
	state  ProbeState
	detail string
	calls  []string
}

func (p *probeStubLane) ID() string  { return p.id }
func (p *probeStubLane) Ready() bool { return p.ready }
func (p *probeStubLane) Status() LaneStatus {
	return LaneStatus{ID: p.id, Name: p.id, Ready: p.ready}
}
func (p *probeStubLane) ListModels(ctx context.Context) ([]ModelInfo, *Error) {
	return nil, &Error{Code: CodeServer, Message: "stub"}
}
func (p *probeStubLane) Probe(ctx context.Context, model string) ProbeResult {
	p.calls = append(p.calls, model)
	return ProbeResult{
		Model:  model,
		State:  p.state,
		Detail: p.detail,
		At:     time.Now().UnixMilli(),
	}
}
func (p *probeStubLane) Do(ctx context.Context, turn *Turn, emit func(Chunk)) (*Usage, FinishReason, *Error) {
	return nil, "", &Error{Code: CodeServer, Message: "stub"}
}

// TestRoundSummaryCountsCorrectly 验证**轮次小结的数字对**。
//
// ⚠️ 这是一个**真 bug** 的回归测试：
//
//	之前的代码在轮次结束时**先清零再读取**，
//	于是小结里永远写着"0 个模型" ——
//	而实际探了 28 个。用户完全被误导。
func TestRoundSummaryCountsCorrectly(t *testing.T) {
	catalog := NewCatalog()
	registry := NewRegistry()

	stub := &probeStubLane{
		id:     ZenLaneID,
		ready:  true,
		state:  StateAvailable,
		detail: "",
	}
	registry.Register(stub)

	catalog.Replace([]ModelInfo{
		{ID: FormatModelID(ZenLaneID, "m1"), Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: FormatModelID(ZenLaneID, "m2"), Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: FormatModelID(ZenLaneID, "m3"), Lane: ZenLaneID, Enabled: true, Free: true},
	})

	s := NewProbeScheduler(catalog, registry, ProbeOptions{
		BatchSize:   3,
		Concurrency: 1,
		Interval:    time.Hour,
	})
	s.SetAdaptiveBatch(false)

	var summary ProbeRoundSummary
	got := make(chan ProbeRoundSummary, 1)
	s.SetRoundHook(func(sum ProbeRoundSummary) { got <- sum })

	s.RunOnceManual(context.Background())

	select {
	case summary = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("轮次没有结束")
	}

	if summary.Probed == 0 {
		t.Fatalf("小结说探了 0 个模型，实际至少探了 %d 个", len(stub.calls))
	}
	if summary.Probed != len(stub.calls) {
		t.Fatalf("小结说 %d 个，实际 %d 个", summary.Probed, len(stub.calls))
	}
	if summary.Available != len(stub.calls) {
		t.Fatalf("小结说可用 %d 个，实际 %d 个", summary.Available, len(stub.calls))
	}
	if summary.DurationMs < 0 {
		t.Fatalf("耗时不该是负数: %d", summary.DurationMs)
	}
}

// TestRoundSummaryReportsCommonFailure 验证**共性失败原因**能被归纳出来。
//
// ⚠️ 这是"探测完成但全是未探测"的诊断依据。
func TestRoundSummaryReportsCommonFailure(t *testing.T) {
	catalog := NewCatalog()
	registry := NewRegistry()

	stub := &probeStubLane{
		id:     ZenLaneID,
		ready:  true,
		state:  StateUnknown,
		detail: "open /etc/ssl/certs/ca-certificates.crt: permission denied",
	}
	registry.Register(stub)

	catalog.Replace([]ModelInfo{
		{ID: FormatModelID(ZenLaneID, "m1"), Lane: ZenLaneID, Enabled: true, Free: true},
		{ID: FormatModelID(ZenLaneID, "m2"), Lane: ZenLaneID, Enabled: true, Free: true},
	})

	s := NewProbeScheduler(catalog, registry, ProbeOptions{
		BatchSize:   2,
		Concurrency: 1,
		Interval:    time.Hour,
	})
	s.SetAdaptiveBatch(false)

	got := make(chan ProbeRoundSummary, 1)
	s.SetRoundHook(func(sum ProbeRoundSummary) { got <- sum })

	s.RunOnceManual(context.Background())

	var summary ProbeRoundSummary
	select {
	case summary = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("轮次没有结束")
	}

	if summary.CommonFailure == "" {
		t.Fatal("该归纳出共性失败原因")
	}
	if summary.CommonCount != 2 {
		t.Fatalf("共性原因该占 2 个，实际 %d", summary.CommonCount)
	}
	if summary.Available != 0 {
		t.Fatalf("该没有可用的，实际 %d", summary.Available)
	}
}
