package lane

import (
	"testing"
	"time"
)

// TestEvidenceDowngradesNegativeProbe 验证**更新的成功证据**让负面结论降级。
//
// ⚠️ 这是用户报的那个问题的核心：
//
//	探测撞上出口风控 → 写"被风控"
//	用户手动调通了   → 留下成功证据
//	下一轮探测又撞墙 → 应该**降级成"未探测"**，而不是又写"被风控"
//
// 降级（而不是直接写"可用"）的理由：
//
//	证据只说"**刚才**是通的"，不说明"**现在**是通的" ——
//	而 unknown 的语义正好是"没有可靠结论"，
//	并且它**不会**把模型从路由里摘掉。
func TestEvidenceDowngradesNegativeProbe(t *testing.T) {
	c := NewCatalog()
	c.Replace([]ModelInfo{{ID: "m1", Lane: ZenLaneID}})

	// ① 探测先给一个负面结论。
	c.ApplyProbe(ProbeResult{
		Model:  "m1",
		State:  StateAbuseBlocked,
		Detail: "出口被风控",
		At:     time.Now().UnixMilli(),
	})
	if got := c.Models()[0].State; got != string(StateAbuseBlocked) {
		t.Fatalf("前置条件不对：应是 abuse-blocked，实际 %q", got)
	}

	// ② 真实流量成功 → 留下证据。
	//
	// ⚠️ 时序说明：探测结论的 `At` 是**探测开始**的时间，
	//   不是结束时间 —— 一次探测可能要跑十几秒。
	//   所以"探测进行中真的通了"这种证据**必须**能推翻它。
	//   这里模拟的正是这个场景：
	//   探测开始（记 At）→ 探测途中真实流量成功（记证据）→ 探测返回负面结论。
	verdictAt := time.Now().UnixMilli()
	time.Sleep(5 * time.Millisecond)
	c.NoteEvidence("m1")

	// ③ 探测返回负面结论 —— 但结论的 At **早于**证据。
	c.ApplyProbe(ProbeResult{
		Model:  "m1",
		State:  StateAbuseBlocked,
		Detail: "出口被风控",
		At:     verdictAt,
	})

	got := c.Models()[0].State
	if got == string(StateAbuseBlocked) {
		t.Fatalf("负面结论该被证据降级，实际还是 %q", got)
	}
	if got != string(StateUnknown) {
		t.Fatalf("应该降级成 unknown（未探测），实际 %q", got)
	}
	if p := c.Probe("m1"); p.Detail == "" {
		t.Fatal("降级后该带上说明")
	}
}

// TestOlderEvidenceDoesNotCount 验证**比结论旧**的证据不作数。
//
// ⚠️ 探测之前就有的成功，证明不了探测那一刻的情况。
func TestOlderEvidenceDoesNotCount(t *testing.T) {
	c := NewCatalog()
	c.Replace([]ModelInfo{{ID: "m1", Lane: ZenLaneID}})

	c.NoteEvidence("m1")
	verdictAt := time.Now().UnixMilli() + 1000 // 结论比证据新

	c.ApplyProbe(ProbeResult{
		Model: "m1",
		State: StateAbuseBlocked,
		At:    verdictAt,
	})

	if got := c.Models()[0].State; got != string(StateAbuseBlocked) {
		t.Fatalf("旧证据不该作数，实际 %q", got)
	}
}

// TestExpiredEvidenceDoesNotCount 验证**过期的证据**不作数。
//
// 否则一次成功就永久有效，模型真坏掉了也发现不了。
func TestExpiredEvidenceDoesNotCount(t *testing.T) {
	c := NewCatalog()
	c.Replace([]ModelInfo{{ID: "m1", Lane: ZenLaneID}})

	c.NoteEvidence("m1")
	// 手动把证据推到过去。
	c.mu.Lock()
	c.evidence["m1"] = time.Now().Add(-EvidenceTTL - time.Minute).UnixMilli()
	c.mu.Unlock()

	c.ApplyProbe(ProbeResult{
		Model: "m1",
		State: StateAbuseBlocked,
		At:    time.Now().UnixMilli(),
	})

	if got := c.Models()[0].State; got != string(StateAbuseBlocked) {
		t.Fatalf("过期证据不该作数，实际 %q", got)
	}
}

// TestPositiveProbeNotDowngraded 验证**正面结论**不受证据影响。
func TestPositiveProbeNotDowngraded(t *testing.T) {
	c := NewCatalog()
	c.Replace([]ModelInfo{{ID: "m1", Lane: ZenLaneID}})

	c.NoteEvidence("m1")
	time.Sleep(5 * time.Millisecond)
	c.ApplyProbe(ProbeResult{
		Model: "m1",
		State: StateAvailable,
		At:    time.Now().UnixMilli(),
	})

	if got := c.Models()[0].State; got != string(StateAvailable) {
		t.Fatalf("正面结论该写进去，实际 %q", got)
	}
}

// TestUnknownNotDowngraded 验证"未探测"不算负面结论。
func TestUnknownNotDowngraded(t *testing.T) {
	c := NewCatalog()
	c.Replace([]ModelInfo{{ID: "m1", Lane: ZenLaneID}})

	c.NoteEvidence("m1")
	c.ApplyProbe(ProbeResult{
		Model: "m1",
		State: StateUnknown,
		At:    time.Now().UnixMilli(),
	})

	if got := c.Models()[0].State; got != string(StateUnknown) {
		t.Fatalf("未探测不该被降级，实际 %q", got)
	}
}

// TestProbeStateNegative 验证 Negative 的判定。
func TestProbeStateNegative(t *testing.T) {
	negative := map[ProbeState]bool{
		StateAvailable:    false,
		StateUnknown:      false,
		StateThrottled:    true,
		StateRegionBlock:  true,
		StateAbuseBlocked: true,
		StateUnavailable:  true,
	}
	for st, want := range negative {
		if got := st.Negative(); got != want {
			t.Errorf("%q.Negative() = %v，期望 %v", st, got, want)
		}
	}
}

// TestProbeMaxTokens 验证探测的输出预算**够模型吐出答案**。
//
// ⚠️ 实测踩过的坑：原来用 16，模型把 16 个 token 全烧在 reasoning 上，
//
//	一个可见 token 都没吐 → finish=max-tokens。
//	那样探测根本测不到"能不能出答案"，首字延迟也会被算成整个请求的耗时。
func TestProbeMaxTokens(t *testing.T) {
	if probeMaxTokens < 128 {
		t.Fatalf("探测预算 %d 太小 —— 模型会全烧在 reasoning 上，测不到输出", probeMaxTokens)
	}
	turn := lane_probe_turn("m1")
	if turn.MaxTokens != probeMaxTokens {
		t.Fatalf("探测请求的 max_tokens = %d，期望 %d", turn.MaxTokens, probeMaxTokens)
	}
}
