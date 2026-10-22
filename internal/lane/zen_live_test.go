package lane

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// 这是**真上游**的验证测试。
//
// ⚠️ 默认**跳过** —— 它会消耗免费车道的额度，而且依赖网络。
// 打开它：
//
//	FMH_LIVE=1 go test ./internal/lane/ -run Live -v
//
// 它是 M1 的**验收标准**：一个真实的 200 + 可读的回答。
//
// 判定：
//   - 403 FreeTierError → 指纹规格过期了（看 docs/design/01-zen-fingerprint.md §9）
//   - 429 → 配额用完了（不是我们的 bug）
//   - 200 + 有内容 → 通过

// liveEnabled 报告是否要跑真上游测试。
func liveEnabled() bool {
	v := strings.ToLower(os.Getenv("FMH_LIVE"))
	return v == "1" || v == "true" || v == "yes"
}

// TestLiveZenListing 拉一次真实的模型清单。
func TestLiveZenListing(t *testing.T) {
	if !liveEnabled() {
		t.Skip("设置 FMH_LIVE=1 才会跑真上游测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	lane := NewZenLane(NewSessionMap())
	models, uerr := lane.ListModels(ctx)
	if uerr != nil {
		t.Fatalf("拉清单失败: %v", uerr)
	}
	if len(models) == 0 {
		t.Fatal("清单是空的")
	}
	t.Logf("拿到 %d 个模型", len(models))
	free := 0
	for _, m := range models {
		if m.Free {
			free++
		}
	}
	t.Logf("其中 %d 个在免费车道上", free)

	// 打印前几个，供人工确认。
	for i, m := range models {
		if i >= 8 {
			break
		}
		t.Logf("  %-40s wire=%-9s ctx=%-8d vision=%v reasoning=%v",
			m.ID, m.Wire, m.ContextWindow, m.Vision, m.Reasoning)
	}
}

// TestLiveZenChat 是 M1 的**验收测试**。
//
// 它打一次真实的对话。成功 = 一个 200 + 可读的回答。
func TestLiveZenChat(t *testing.T) {
	if !liveEnabled() {
		t.Skip("设置 FMH_LIVE=1 才会跑真上游测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	lane := NewZenLane(NewSessionMap())

	// 先拿清单，挑一个真正免费的模型。
	//
	// ⚠️ 不能挑清单里的**第一个**模型 —— 清单里混着付费 id，
	// 而付费 id 在没有用户凭据时会回 429/401，看起来像"车道坏了"。
	// ListModels 已经过滤了免费车道，所以这里拿到的都是免费的。
	models, uerr := lane.ListModels(ctx)
	if uerr != nil {
		t.Fatalf("拉清单失败: %v", uerr)
	}
	target := ""
	// FMH_TEST_MODEL 可以指定要测的模型 —— 逐个排查时很有用。
	if forced := strings.TrimSpace(os.Getenv("FMH_TEST_MODEL")); forced != "" {
		target = forced
	}
	for _, m := range models {
		if target != "" {
			break
		}
		if m.Free && m.Wire == "chat" && m.ID != "big-pickle" && m.ID != "beam-free" {
			target = m.ID
		}
	}
	if target == "" {
		t.Skip("清单里没有免费且走 chat 线的模型")
	}
	t.Logf("用模型 %q 测试", target)

	turn := &Turn{
		Model: target,
		Messages: []Message{
			{Role: RoleSystem, Parts: []Part{TextPart("You are a helpful assistant. Answer briefly.")}},
			{Role: RoleUser, Parts: []Part{TextPart("Reply with exactly: FMH-OK")}},
		},
		MaxTokens: 256,
		Seed:      "fmh-live-test-" + target,
	}

	var text strings.Builder
	var reasoning strings.Builder
	var toolCalls int
	usage, finish, uerr := lane.Do(ctx, turn, func(c Chunk) {
		switch c.Kind {
		case ChunkTextDelta:
			text.WriteString(c.Delta)
		case ChunkReasonDelta:
			reasoning.WriteString(c.Delta)
		case ChunkToolCallDelta:
			toolCalls++
		}
	})

	if uerr != nil {
		t.Fatalf("❌ 推理失败: code=%s message=%s", uerr.Code, uerr.Message)
	}
	t.Logf("✅ 成功。finish=%s usage=%+v", finish, usage)
	t.Logf("   回答: %q", text.String())
	if reasoning.Len() > 0 {
		t.Logf("   思考: %q", reasoning.String())
	}
	if toolCalls > 0 {
		t.Logf("   工具调用: %d 次", toolCalls)
	}

	if text.Len() == 0 {
		t.Fatal("回答是空的 —— 指纹可能没完全通过，或模型只返回了思考")
	}
	if !strings.Contains(strings.ToUpper(text.String()), "FMH") {
		t.Logf("⚠ 回答里没有预期的标记（可能模型没照做，不一定是 bug）")
	}
}
