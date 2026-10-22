package lane

import (
	"testing"
	"time"
)

// TestLaneLockAllRefused 验证**全被拒 = 车道被锁**。
//
// ⚠️ 这是用户报的"显示被风控但我能调"的**另一半** ——
//
// 免密车道的可用性是**网络路径的属性**。
// 当**所有**模型都被同一种理由拒绝时，
// 正确的结论是"这个出口用不了整条车道"，
// 而不是"这 N 个模型分别坏了"。
func TestLaneLockAllRefused(t *testing.T) {
	c := NewCatalog()
	ids := []string{"a", "b", "c", "d"}
	models := make([]ModelInfo, 0, len(ids))
	for _, id := range ids {
		models = append(models, ModelInfo{ID: id, Lane: ZenLaneID})
	}
	c.Replace(models)

	// 全部被风控。
	for _, id := range ids {
		c.ApplyProbe(ProbeResult{
			Model: id,
			State: StateAbuseBlocked,
			At:    time.Now().UnixMilli(),
		})
	}

	lock := c.LaneLockOf(ZenLaneID)
	if !lock.Locked {
		t.Fatal("全部被风控时该判为车道被锁")
	}
	if lock.State != StateAbuseBlocked {
		t.Fatalf("状态应是 abuse-blocked，实际 %q", lock.State)
	}
	if lock.Count != 4 || lock.Total != 4 {
		t.Fatalf("计数不对：count=%d total=%d", lock.Count, lock.Total)
	}
	if lock.Reason == "" {
		t.Fatal("该带上理由")
	}
}

// TestLaneLockNotAllRefused 验证**有一个可用就不算车道被锁**。
func TestLaneLockNotAllRefused(t *testing.T) {
	c := NewCatalog()
	models := []ModelInfo{
		{ID: "a", Lane: ZenLaneID},
		{ID: "b", Lane: ZenLaneID},
		{ID: "c", Lane: ZenLaneID},
	}
	c.Replace(models)

	now := time.Now().UnixMilli()
	c.ApplyProbe(ProbeResult{Model: "a", State: StateAbuseBlocked, At: now})
	c.ApplyProbe(ProbeResult{Model: "b", State: StateAbuseBlocked, At: now})
	c.ApplyProbe(ProbeResult{Model: "c", State: StateAvailable, At: now})

	if lock := c.LaneLockOf(ZenLaneID); lock.Locked {
		t.Fatal("有一个可用就不该判为车道被锁")
	}
}

// TestLaneLockMixedReasons 验证**理由不一致不算车道被锁**。
//
// 理由不一致 = 各模型各自的问题，不是车道的问题。
func TestLaneLockMixedReasons(t *testing.T) {
	c := NewCatalog()
	models := []ModelInfo{
		{ID: "a", Lane: ZenLaneID},
		{ID: "b", Lane: ZenLaneID},
		{ID: "c", Lane: ZenLaneID},
	}
	c.Replace(models)

	now := time.Now().UnixMilli()
	c.ApplyProbe(ProbeResult{Model: "a", State: StateAbuseBlocked, At: now})
	c.ApplyProbe(ProbeResult{Model: "b", State: StateRegionBlock, At: now})
	c.ApplyProbe(ProbeResult{Model: "c", State: StateAbuseBlocked, At: now})

	if lock := c.LaneLockOf(ZenLaneID); lock.Locked {
		t.Fatal("理由不一致不该判为车道被锁")
	}
}

// TestLaneLockNeedsEnoughSamples 验证**样本太少不下结论**。
//
// 只探了 1 个就被拒，不能说"整条车道被锁"。
func TestLaneLockNeedsEnoughSamples(t *testing.T) {
	c := NewCatalog()
	c.Replace([]ModelInfo{{ID: "a", Lane: ZenLaneID}})
	c.ApplyProbe(ProbeResult{
		Model: "a",
		State: StateAbuseBlocked,
		At:    time.Now().UnixMilli(),
	})

	if lock := c.LaneLockOf(ZenLaneID); lock.Locked {
		t.Fatal("只探了一个模型不该判为车道被锁")
	}
}

// TestLaneLockThrottleIsNotALock 验证**限流不算车道被锁**。
//
// 限流是**暂时**的，而且是全局配额 —— 不是"这个出口不行"。
func TestLaneLockThrottleIsNotALock(t *testing.T) {
	c := NewCatalog()
	models := []ModelInfo{
		{ID: "a", Lane: ZenLaneID},
		{ID: "b", Lane: ZenLaneID},
		{ID: "c", Lane: ZenLaneID},
	}
	c.Replace(models)

	now := time.Now().UnixMilli()
	for _, id := range []string{"a", "b", "c"} {
		c.ApplyProbe(ProbeResult{Model: id, State: StateThrottled, At: now})
	}

	if lock := c.LaneLockOf(ZenLaneID); lock.Locked {
		t.Fatal("限流不该判为车道被锁")
	}
}

// TestLaneLockIgnoresStaleProbes 验证**过期结论不计入**。
func TestLaneLockIgnoresStaleProbes(t *testing.T) {
	c := NewCatalog()
	models := []ModelInfo{
		{ID: "a", Lane: ZenLaneID},
		{ID: "b", Lane: ZenLaneID},
		{ID: "c", Lane: ZenLaneID},
	}
	c.Replace(models)

	// 全部是**过期**的负面结论。
	old := time.Now().Add(-2 * ProbeTTL).UnixMilli()
	for _, id := range []string{"a", "b", "c"} {
		c.ApplyProbe(ProbeResult{Model: id, State: StateAbuseBlocked, At: old})
	}

	lock := c.LaneLockOf(ZenLaneID)
	if lock.Locked {
		t.Fatal("过期结论不该判为车道被锁")
	}
	if lock.Total != 0 {
		t.Fatalf("过期结论不该计入样本数，实际 %d", lock.Total)
	}
}
