package lane

import (
	"testing"
	"time"
)

// TestReplacePreservesProbeState 验证**清单刷新后探测结论还在**。
//
// ⚠️ 这是一个真实踩过的坑：
//
//	清单刷新 → `Replace` 换掉模型行 → 新行是"干净"的
//	→ 状态 / 延迟全没了 → 前端显示"全部未知"
//	→ 而探测结论其实好好地存在 `probes` 里
//
// 所以 `Replace`/`Merge` 必须把结论**套回**新行。
func TestReplacePreservesProbeState(t *testing.T) {
	c := NewCatalog()
	c.Replace([]ModelInfo{
		{ID: "m1", Lane: "zen", Enabled: true, Free: true},
		{ID: "m2", Lane: "zen", Enabled: true, Free: true},
	})
	c.SetProbe(ProbeResult{
		Model: "m1", State: StateAvailable, TTFTMs: 1234,
		At: time.Now().UnixMilli(),
	})
	c.ApplyProbe(ProbeResult{
		Model: "m1", State: StateAvailable, TTFTMs: 1234,
		At: time.Now().UnixMilli(),
	})

	// 刷新清单（模拟 30 分钟一次的刷新）。
	c.Replace([]ModelInfo{
		{ID: "m1", Lane: "zen", Enabled: true, Free: true},
		{ID: "m2", Lane: "zen", Enabled: true, Free: true},
		{ID: "m3", Lane: "zen", Enabled: true, Free: true},
	})

	for _, m := range c.Models() {
		if m.ID != "m1" {
			continue
		}
		if m.State != string(StateAvailable) {
			t.Fatalf("刷新后状态丢了: %q", m.State)
		}
		if m.TTFTMs != 1234 {
			t.Fatalf("刷新后延迟丢了: %d", m.TTFTMs)
		}
	}
}

// TestMergeAppliesProbeToNewModel 验证**合并进来的新模型**也套用结论。
func TestMergeAppliesProbeToNewModel(t *testing.T) {
	c := NewCatalog()
	c.Replace([]ModelInfo{{ID: "m1", Lane: "zen", Enabled: true}})
	c.SetProbe(ProbeResult{
		Model: "m2", State: StateThrottled,
		At: time.Now().UnixMilli(),
	})

	// m2 是新进来的（之前探过）。
	c.Merge([]ModelInfo{{ID: "m2", Lane: "zen", Enabled: true}})

	for _, m := range c.Models() {
		if m.ID != "m2" {
			continue
		}
		if m.State != string(StateThrottled) {
			t.Fatalf("新模型的探测结论没套上: %q", m.State)
		}
	}
}

// TestSetProbeDoesNotTouchModelRows 验证 `SetProbe` **不动**模型行。
//
// ⚠️ 这是有意的 —— `SetProbe` 只是记结论，
//
//	`ApplyProbe` 才写模型行。
//	两者语义不同，混用会出意外。
func TestSetProbeDoesNotTouchModelRows(t *testing.T) {
	c := NewCatalog()
	c.Replace([]ModelInfo{{ID: "m1", Lane: "zen", Enabled: true}})
	c.SetProbe(ProbeResult{
		Model: "m1", State: StateAvailable,
		At: time.Now().UnixMilli(),
	})
	for _, m := range c.Models() {
		if m.State != "" {
			t.Fatalf("SetProbe 不该改模型行，实际 %q", m.State)
		}
	}
}

// TestApplyProbeSetsCapabilitySource 验证**探测是最高信任来源**。
func TestApplyProbeSetsCapabilitySource(t *testing.T) {
	c := NewCatalog()
	c.Replace([]ModelInfo{
		{ID: "m1", Lane: "zen", Enabled: true, Capability: CapabilitySourceHeuristic},
	})
	c.ApplyProbe(ProbeResult{
		Model: "m1", State: StateAvailable,
		At: time.Now().UnixMilli(),
	})
	for _, m := range c.Models() {
		if m.Capability != CapabilitySourceProbe {
			t.Fatalf("能力来源该是 probe，实际 %q", m.Capability)
		}
	}
}
