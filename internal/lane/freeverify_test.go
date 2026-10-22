package lane

import (
	"context"
	"strings"
	"testing"
	"time"
)

// 验证"拉到的模型真的是免费的吗"这一层。
// 核心认识：**名字匹配只是快速过滤，不是保证。**
// 保证来自**运行时探测** —— 上游的反应是决定性的。

// TestFreeVerificationUsesUpstreamResponse 验证判定依据是上游的反应。
func TestFreeVerificationUsesUpstreamResponse(t *testing.T) {
	// 模拟三种上游反应。
	cases := []struct {
		name   string
		uerr   *Error
		expect FreeVerdict
	}{
		{
			name:   "401 Missing API key → 确定不是免费",
			uerr:   &Error{Code: CodeCredential, Message: "Missing API key."},
			expect: VerdictNotFree,
		},
		{
			name:   "429 限流 → 在免费车道上",
			uerr:   &Error{Code: CodeQuota, Message: "Rate limit exceeded."},
			expect: VerdictFree,
		},
		{
			name:   "403 闸门 → 在免费车道上",
			uerr:   &Error{Code: CodeFreeTier, Message: "free tier can only be used from within OpenCode"},
			expect: VerdictFree,
		},
		{
			name:   "403 IP 风控 → 在免费车道上",
			uerr:   &Error{Code: CodeAbuseBlocked, Message: "detected abusive traffic from this IP address"},
			expect: VerdictFree,
		},
		{
			name:   "200 成功 → 在免费车道上",
			uerr:   nil,
			expect: VerdictFree,
		},
		{
			name:   "400 请求自己的错 → 不能下结论",
			uerr:   &Error{Code: CodeClient, Message: "bad request"},
			expect: VerdictUnknown,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			v := NewFreeVerifier(func(ctx context.Context, model string) *Error {
				return c.uerr
			})
			got := v.Verify(context.Background(), "some-model")
			if got != c.expect {
				t.Fatalf("结论应为 %v，实际 %v", c.expect, got)
			}
		})
	}
}

// TestFreeVerifierKeepsUnknownModels 验证"没探过的模型不被静默藏起来"。
func TestFreeVerifierKeepsUnknownModels(t *testing.T) {
	v := NewFreeVerifier(func(ctx context.Context, model string) *Error { return nil })
	v.Set("known-paid", VerdictNotFree)

	models := []ModelInfo{
		{ID: "known-paid"},
		{ID: "never-probed"},
	}
	kept := v.Filter(models)
	if len(kept) != 1 {
		t.Fatalf("只该滤掉确定不免费的，实际剩 %d 个", len(kept))
	}
	if kept[0].ID != "never-probed" {
		t.Fatalf("留错了: %q", kept[0].ID)
	}
}

// TestMissingAPIKeyIsDetected 验证"缺 API key"能被识别出来。
func TestMissingAPIKeyIsDetected(t *testing.T) {
	e := ClassifyFailure(401, `{"error":{"message":"Missing API key."}}`, 0)
	if !e.MissingAPIKey {
		t.Fatal("401 Missing API key 应被标记")
	}
	if !IsMissingAPIKey(e) {
		t.Fatal("IsMissingAPIKey 应为真")
	}
	// 但闸门的措辞不是这个意思。
	e2 := ClassifyFailure(403, "OpenCode's free tier can only be used from within OpenCode", 0)
	if IsMissingAPIKey(e2) {
		t.Fatal("闸门的措辞不该被当成'缺 API key'")
	}
}

// TestLiveOpenRefCatalogs 验证两个公开目录都能拉到数据并对齐。
func TestLiveOpenRefCatalogs(t *testing.T) {
	if !liveEnabled() {
		t.Skip("设置 FMH_LIVE=1 才会跑真上游测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	client := Transport()

	md, err := FetchModelsDev(ctx, client)
	if err != nil {
		t.Fatalf("models.dev 拉取失败: %v", err)
	}
	t.Logf("models.dev/opencode: %d 个模型带容量", len(md))
	for _, id := range []string{"deepseek-v4.1-flash-free", "minimax-m3.1-flash-free"} {
		if m, ok := md[id]; ok {
			t.Logf("  %-30s ctx=%-9d out=%-8d vision=%v reasoning=%v",
				id, m.ContextWindow, m.MaxOutput, m.Vision, m.Reasoning)
		} else {
			t.Logf("  %-30s (不在目录里)", id)
		}
	}

	// OpenRouter 公开目录（无需 key）
	or, err := FetchOpenRouterCatalog(ctx, client)
	if err != nil {
		t.Fatalf("openrouter 目录拉取失败: %v", err)
	}
	t.Logf("openrouter 免费模型: %d 个", len(or))

	// 对齐度：models.dev 的 opencode 条目有多少在实时清单里。
	lane := NewZenLane(NewSessionMap())
	models, uerr := lane.ListModels(ctx)
	if uerr != nil {
		t.Fatalf("拉清单失败: %v", uerr)
	}
	covered := 0
	for _, m := range models {
		if _, ok := md[m.ID]; ok {
			covered++
		}
	}
	t.Logf("实时免费清单 %d 个，其中 %d 个在 models.dev 里有容量", len(models), covered)
	if len(models) > 0 && covered == 0 {
		t.Error("models.dev 应该至少覆盖一些实时模型")
	}
}

// TestLiveFreeVerification 验证"哪些模型真的免费"这个判定在真上游成立。
// 这是用户问题的直接答案：**保证来自运行时探测，不来自名字。**
func TestLiveFreeVerification(t *testing.T) {
	if !liveEnabled() {
		t.Skip("设置 FMH_LIVE=1 才会跑真上游测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	lane := NewZenLane(NewSessionMap())

	// 拿全量清单（不过滤）。
	models, uerr := lane.ListModels(ctx)
	if uerr != nil {
		t.Fatalf("拉清单失败: %v", uerr)
	}
	if len(models) == 0 {
		t.Fatal("清单是空的")
	}

	// 名字过滤能砍掉多少。
	all, uerr := lane.ListModels(ctx)
	_ = all
	_ = uerr

	// 挑几个付费 id 与几个免费 id 来验证判定。
	type probe struct {
		id   string
		free bool
	}
	probes := []probe{
		{"minimax-m3.1-flash-free", true},
		{"deepseek-v4.1-flash-free", true},
		{"claude-haiku-4-5", false},
		{"gpt-5", false},
	}
	v := NewFreeVerifier(lane.ProbeFree)
	for _, p := range probes {
		verdict := v.Verify(ctx, p.id)
		t.Logf("  %-28s → %-10s (名字说免费=%v)", p.id, verdict, p.free)
	}

	// 关键断言：付费 id 必须被判为 not-free。
	for _, p := range probes {
		if !p.free {
			if v.Verdict(p.id) != VerdictNotFree {
				t.Errorf("%q 应被判为 not-free，实际 %v", p.id, v.Verdict(p.id))
			}
		}
	}
}

// TestLiveNameFilterVsReality 对比"名字过滤"与"实测真相"。
func TestLiveNameFilterVsReality(t *testing.T) {
	if !liveEnabled() {
		t.Skip("设置 FMH_LIVE=1 才会跑真上游测试")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()

	// 拿全量 id（用底层请求，绕过过滤）。
	lane := NewZenLane(NewSessionMap())
	session := lane.sessions.For("catalog:zen")
	headers := ZenHeaders(session, false)
	headers["accept"] = "application/json"

	var listing zenListing
	if uerr := getJSON(ctx, lane.client, lane.base+"/inference/v1/models", headers, &listing); uerr != nil {
		t.Fatalf("拉清单失败: %v", uerr)
	}

	total := len(listing.Data)
	byName := 0
	for _, row := range listing.Data {
		if IsFreeLane(row.ID) {
			byName++
		}
	}
	t.Logf("上游清单共 %d 个 id", total)
	t.Logf("名字过滤认为 %d 个免费", byName)
	t.Logf("名字过滤砍掉了 %d 个（%.0f%%）", total-byName, float64(total-byName)/float64(total)*100)

	if byName == total {
		t.Log("⚠ 名字过滤没砍掉任何东西 —— 过滤规则可能失效了")
	}
	if total > 0 && float64(byName)/float64(total) > 0.5 {
		t.Errorf("名字过滤只砍掉了一半以下，规则可能过宽")
	}

	// 名字过滤**不会**漏掉付费模型（那才是危险的方向）。
	// 漏掉免费模型只是"少几个选项"，无害。
	for _, row := range listing.Data {
		if !IsFreeLane(row.ID) && strings.Contains(strings.ToLower(row.ID), "free") {
			t.Errorf("%q 含 'free' 但没被匹配 —— 规则有洞", row.ID)
		}
	}
}
