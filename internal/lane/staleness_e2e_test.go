package lane

import (
	"testing"
	"time"
)

// TestStaleProbeShowsUnknown 验证**过期的结论在 Models() 里读作"未探测"** ——
// 不参与自动探测的车道（OR）没人来刷新结论，
// 过了探测间隔后面板必须显示"未探测"，而不是挂着几天前的"可用"。
func TestStaleProbeShowsUnknown(t *testing.T) {
	c := NewCatalog()
	// ⚠️ 先设 staleness 再套结论 —— 与真实启动顺序一致
	//   （SetProbeStaleness 在 restoreState 之后，但都在服务前）。
	c.SetProbeStaleness(15 * time.Minute)
	c.Replace([]ModelInfo{{ID: "openrouter/m1", Lane: OpenRouterLaneID, Free: true}})

	// 3 天前的"可用"。
	old := time.Now().Add(-72 * time.Hour).UnixMilli()
	c.ApplyProbe(ProbeResult{Model: "openrouter/m1", State: StateAvailable, At: old})

	got := c.Models()[0]
	if got.State != string(StateUnknown) {
		t.Fatalf("过期结论该显示未探测，实际 %q", got.State)
	}
	// ProbedAt 保留 —— 区分"过期"与"从没探过"。
	if got.ProbedAt != old {
		t.Fatalf("ProbedAt 该保留旧时间，实际 %d", got.ProbedAt)
	}
}

// TestFreshProbeStaysAvailable 验证**间隔内的结论不被抹** ——
// 过期判定不能把刚探出来的状态也抹掉。
func TestFreshProbeStaysAvailable(t *testing.T) {
	c := NewCatalog()
	c.SetProbeStaleness(15 * time.Minute)
	c.Replace([]ModelInfo{{ID: "zen/m1", Lane: ZenLaneID, Free: true}})
	c.ApplyProbe(ProbeResult{Model: "zen/m1", State: StateAvailable, At: time.Now().UnixMilli()})

	if got := c.Models()[0].State; got != string(StateAvailable) {
		t.Fatalf("间隔内的结论不该被抹，实际 %q", got)
	}
}

// TestProbeSchedulerIntervalIsWallClock 钉住"下一轮 = 本轮开始 + 间隔"的语义。
// 用户报的问题：设定 15 分钟、体感总超时 —— 因为之前是"跑完再等整个间隔"，
// 一轮 80s 就把周期拉成 16 分 20 秒。
func TestProbeSchedulerIntervalIsWallClock(t *testing.T) {
	s := NewProbeScheduler(NewCatalog(), NewRegistry(), ProbeOptions{Interval: time.Minute})
	roundStart := time.Now()
	next := s.NextFireAt(roundStart)
	want := roundStart.Add(time.Minute)
	if !next.Equal(want) {
		t.Fatalf("下一轮触发点该是 本轮开始+间隔（%s），实际 %s", want, next)
	}
}
