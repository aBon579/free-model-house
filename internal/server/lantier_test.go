package server

import (
	"sync"
	"testing"
	"time"
)

// TestLanTierParse 验证档位解析。
func TestLanTierParse(t *testing.T) {
	cases := map[string]LanTier{
		"light":    LanTierLight,
		"standard": LanTierStandard,
		"deep":     LanTierDeep,
	}
	for in, want := range cases {
		if got := ParseLanTier(in); got != want {
			t.Errorf("ParseLanTier(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestLanTierParseLegacy 验证**旧档位能平滑迁移**。
//
// ⚠️ 用户升级后数据库里还存着旧值 —— 不能变成"未知"，
//
//	也不能让局域网突然用不了。
func TestLanTierParseLegacy(t *testing.T) {
	cases := map[string]LanTier{
		// 旧的"限量"（限流）→ 最紧的一档。
		"limited": LanTierLight,
		// 旧的"全开"（不限量）→ 深度档。
		"lan": LanTierDeep,
		// 旧的"关闭"→ 最紧的一档。
		"off": LanTierLight,
		// 认不出来 → 标准档（**不是**最紧的）——
		// 免得升级后局域网突然用不了。
		"":        LanTierStandard,
		"garbage": LanTierStandard,
	}
	for in, want := range cases {
		if got := ParseLanTier(in); got != want {
			t.Errorf("ParseLanTier(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestNoOffTier 验证**没有"完全关闭"档**。
//
// ⚠️ 用户明确要求去掉"局域网内 Agent 完全用不了"这个选项 ——
//
//	这个网关就是给局域网里的 agent 用的。
func TestNoOffTier(t *testing.T) {
	for _, tier := range LanTierAll {
		if tier == "off" {
			t.Fatal("档位列表里不该有 off")
		}
	}
	if len(LanTierAll) != 3 {
		t.Fatalf("该有 3 档，实际 %d", len(LanTierAll))
	}
}

// TestTierLimits 验证**每档的默认限额**。
func TestTierLimits(t *testing.T) {
	perMin, conc := TierLimits(LanTierLight)
	if perMin <= 0 || conc <= 0 {
		t.Fatalf("轻量档该有限额，实际 %d/%d", perMin, conc)
	}
	perMin, conc = TierLimits(LanTierStandard)
	if perMin <= 0 || conc <= 0 {
		t.Fatalf("标准档该有限额，实际 %d/%d", perMin, conc)
	}
	// 深度档 = 不限量。
	perMin, conc = TierLimits(LanTierDeep)
	if perMin != 0 || conc != 0 {
		t.Fatalf("深度档该不限量，实际 %d/%d", perMin, conc)
	}
	// ⚠️ 档位要**递增**（越深越宽松）。
	light, _ := TierLimits(LanTierLight)
	standard, _ := TierLimits(LanTierStandard)
	if light >= standard {
		t.Fatalf("轻量档该比标准档紧：%d vs %d", light, standard)
	}
}

// TestLimiterStartsFull 验证令牌桶**初始是满的**。
//
// ⚠️ 这是实测踩过的坑：桶初始 tokens=0 时，
//
//	刚切到限流档时第一个请求就被拒（429），
//	用户看到的是"刚设置就 429"。
func TestLimiterStartsFull(t *testing.T) {
	l := newLimiter(10, 0)
	tokens, _ := l.stats()
	if tokens < 10 {
		t.Fatalf("桶初始应该满（10），实际 %.1f", tokens)
	}
	if !l.allow() {
		t.Fatal("第一个请求应该被放行")
	}
}

// TestLimiterEnforcesPerMinute 验证每分钟上限。
func TestLimiterEnforcesPerMinute(t *testing.T) {
	l := newLimiter(3, 0)
	allowed := 0
	for i := 0; i < 10; i++ {
		if l.allow() {
			allowed++
		}
	}
	if allowed != 3 {
		t.Fatalf("3 次配额应该只放行 3 个，实际 %d", allowed)
	}
}

// TestLimiterUnlimited 验证 perMinute<=0 时不限流。
func TestLimiterUnlimited(t *testing.T) {
	l := newLimiter(0, 0)
	for i := 0; i < 1000; i++ {
		if !l.allow() {
			t.Fatalf("不限流时第 %d 个请求被拒了", i)
		}
	}
}

// TestLimiterConcurrency 验证并发上限。
func TestLimiterConcurrency(t *testing.T) {
	l := newLimiter(0, 2)
	if !l.allow() || !l.allow() {
		t.Fatal("前两个应该拿到名额")
	}
	if l.allow() {
		t.Fatal("第三个不该拿到名额（并发上限 2）")
	}
	l.release()
	if !l.allow() {
		t.Fatal("释放一个之后应该能再拿一个")
	}
}

// TestLimiterConcurrent 验证并发下的正确性。
func TestLimiterConcurrent(t *testing.T) {
	l := newLimiter(0, 10)
	var wg sync.WaitGroup
	var mu sync.Mutex
	active := 0
	maxActive := 0

	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !l.allow() {
				return
			}
			mu.Lock()
			active++
			if active > maxActive {
				maxActive = active
			}
			mu.Unlock()

			time.Sleep(2 * time.Millisecond)

			mu.Lock()
			active--
			mu.Unlock()
			l.release()
		}()
	}
	wg.Wait()

	if maxActive > 10 {
		t.Fatalf("并发峰值 %d 超过上限 10", maxActive)
	}
}

// TestLimiterRefills 验证令牌会随时间补充。
func TestLimiterRefills(t *testing.T) {
	// 60 次/分钟 = 1 次/秒。
	l := newLimiter(60, 0)
	for i := 0; i < 60; i++ {
		if !l.allow() {
			t.Fatalf("第 %d 个请求被拒了", i)
		}
	}
	if l.allow() {
		t.Fatal("桶空之后不该再放行")
	}
	// 等 1.2 秒 → 应该补回 1 个令牌。
	time.Sleep(1200 * time.Millisecond)
	if !l.allow() {
		t.Fatal("1.2 秒后应该补回至少 1 个令牌")
	}
}
