package lane

import (
	"encoding/json"
	"testing"
)

// 清单信封解析

// TestParseListingAcceptsEverySeenShape 验证解析器接受所有见过的形状。
//
// 上游改一次响应格式，解析就该崩一次 ——
// 健壮的解析器接受所有形状，而不是赌一种。
func TestParseListingAcceptsEverySeenShape(t *testing.T) {
	shapes := []struct {
		name    string
		payload string
		want    []string
	}{
		{
			name:    "data + 对象",
			payload: `{"object":"list","data":[{"id":"a-free"},{"id":"b-free"}]}`,
			want:    []string{"a-free", "b-free"},
		},
		{
			name:    "models 键",
			payload: `{"models":[{"id":"a-free"}]}`,
			want:    []string{"a-free"},
		},
		{
			name:    "items 键",
			payload: `{"items":[{"id":"a-free"}]}`,
			want:    []string{"a-free"},
		},
		{
			name:    "data + 纯字符串",
			payload: `{"data":["a-free","b-free"]}`,
			want:    []string{"a-free", "b-free"},
		},
		{
			name:    "裸数组 + 对象",
			payload: `[{"id":"a-free"}]`,
			want:    []string{"a-free"},
		},
		{
			name:    "裸数组 + 纯字符串",
			payload: `["a-free","b-free"]`,
			want:    []string{"a-free", "b-free"},
		},
	}
	for _, s := range shapes {
		t.Run(s.name, func(t *testing.T) {
			got := ParseListingIDs([]byte(s.payload))
			if len(got) != len(s.want) {
				t.Fatalf("期望 %v，实际 %v", s.want, got)
			}
			for i := range s.want {
				if got[i] != s.want[i] {
					t.Fatalf("期望 %v，实际 %v", s.want, got)
				}
			}
		})
	}
}

func TestParseListingKeepsDeclaredCapacity(t *testing.T) {
	// 上游**当前**不声明容量，但将来可能声明。
	// 解析器要能接住，声明的值比本地规则表更可信。
	payload := `{"data":[{"id":"a-free","context_length":123456,"max_output_tokens":7890,"name":"A"}]}`
	rows := ParseListing([]byte(payload))
	if len(rows) != 1 {
		t.Fatalf("应有 1 行，实际 %d", len(rows))
	}
	if rows[0].ContextWindow != 123456 {
		t.Errorf("上下文窗口不对: %d", rows[0].ContextWindow)
	}
	if rows[0].MaxOutput != 7890 {
		t.Errorf("最大输出不对: %d", rows[0].MaxOutput)
	}
	if rows[0].Name != "A" {
		t.Errorf("名字不对: %q", rows[0].Name)
	}
}

func TestParseListingSkipsGarbage(t *testing.T) {
	payload := `{"data":[{"id":""},{"no_id":true},{"id":"ok-free"},123,null]}`
	got := ParseListingIDs([]byte(payload))
	if len(got) != 1 || got[0] != "ok-free" {
		t.Fatalf("应只保留 ok-free，实际 %v", got)
	}
}

// id 归一化

func TestNormalizeID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"deepseek-v4.1-flash-free", "deepseek-v4.1-flash"},
		{"stepfun/step-5-preview-free", "step-5-preview"},
		{"step-5-preview", "step-5-preview"},
		{"muse-spark-1.3-contributor-free", "muse-spark-1.3"},
		{"longcat-2.5-preview-free", "longcat-2.5-preview"},
		{"deepseek_v4_flash_free", "deepseek-v4-flash"},
	}
	for _, c := range cases {
		if got := NormalizeID(c.in); got != c.want {
			t.Errorf("%q → %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeIDKeepsPreview(t *testing.T) {
	// ⚠️ `-preview` 是模型名的一部分 —— 剥掉它会让 preview 版和稳定版撞车。
	if NormalizeID("longcat-2.5-preview-free") == NormalizeID("longcat-2.5-free") {
		t.Error("preview 版与稳定版不应归一化到同一个键")
	}
}

// 容量应用

func TestApplyCapacityRespectsTrustOrder(t *testing.T) {
	// 实测来源的结论**不能**被第三方目录覆盖。
	info := ModelInfo{ContextWindow: 999, MaxOutput: 111, Capability: CapabilitySourceProbe}
	ApplyCapacity(&info, CapacityMeta{
		ContextWindow: 200000,
		MaxOutput:     32000,
		Source:        CapabilitySourceListing,
	})
	if info.ContextWindow != 999 || info.MaxOutput != 111 {
		t.Fatalf("实测来源不该被覆盖: %+v", info)
	}
}

func TestApplyCapacityFromListingWinsOverHeuristic(t *testing.T) {
	// 第三方目录/上游声明**可以**覆盖本地规则表。
	info := ModelInfo{ContextWindow: 1048576, MaxOutput: 262144, Capability: CapabilitySourceHeuristic}
	ApplyCapacity(&info, CapacityMeta{
		ContextWindow: 200000,
		MaxOutput:     32000,
		Source:        CapabilitySourceListing,
	})
	if info.ContextWindow != 200000 || info.MaxOutput != 32000 {
		t.Fatalf("应被覆盖: %+v", info)
	}
	if info.Capability != CapabilitySourceListing {
		t.Fatalf("来源标记应更新: %v", info.Capability)
	}
}

func TestApplyCapacityDoesNotWriteModalityFromThirdParty(t *testing.T) {
	// ⚠️ 模态结论**只有实测来源**才生效。
	// 路由的模态过滤是硬约束 —— 用猜出来的结论过滤会误杀模型。
	info := ModelInfo{Capability: CapabilitySourceHeuristic}
	ApplyCapacity(&info, CapacityMeta{
		ContextWindow: 200000,
		MaxOutput:     32000,
		Source:        CapabilitySourceListing,
		Vision:        true,
		HasModality:   true,
	})
	if info.Vision {
		t.Fatal("第三方目录的模态结论不该写进模型")
	}
}

func TestApplyCapacityFromProbeWritesModality(t *testing.T) {
	info := ModelInfo{Capability: CapabilitySourceHeuristic}
	ApplyCapacity(&info, CapacityMeta{
		ContextWindow: 200000,
		MaxOutput:     32000,
		Source:        CapabilitySourceProbe,
		Vision:        true,
		HasModality:   true,
		Reasoning:     true,
	})
	if !info.Vision || !info.Reasoning {
		t.Fatalf("实测来源的模态结论应该生效: %+v", info)
	}
}

func TestMergeCatalogBuriesConflicts(t *testing.T) {
	// ⚠️ 同名不同模型 —— **宁可留空，也不要猜**。
	// 挑错了会让一个模型带着别人的容量进路由。
	strong := map[string]ModelsDevMeta{
		"deepseek-v4.1-flash": {ContextWindow: 200000, MaxOutput: 32000},
	}
	weak := map[string]ModelsDevMeta{
		"deepseek-v4.1-flash": {ContextWindow: 1048576, MaxOutput: 262144},
		"only-in-weak":        {ContextWindow: 1000, MaxOutput: 100},
	}
	merged, conflicts := MergeCatalog(strong, weak)
	if !conflicts["deepseek-v4.1-flash"] {
		t.Error("冲突应被标记")
	}
	if _, ok := merged["deepseek-v4.1-flash"]; ok {
		t.Error("冲突的键应被埋掉")
	}
	if _, ok := merged["only-in-weak"]; !ok {
		t.Error("不冲突的应保留")
	}
}

func TestApplyCatalogToModels(t *testing.T) {
	models := []ModelInfo{
		{ID: "deepseek-v4.1-flash-free", ContextWindow: 1048576, MaxOutput: 262144, Capability: CapabilitySourceHeuristic},
		{ID: "unknown-model-free", ContextWindow: 111, MaxOutput: 222, Capability: CapabilitySourceHeuristic},
	}
	catalog := map[string]ModelsDevMeta{
		"deepseek-v4.1-flash-free": {ContextWindow: 200000, MaxOutput: 32000},
	}
	applied := ApplyCatalog(models, catalog)
	if len(applied) != 1 || applied[0] != "deepseek-v4.1-flash-free" {
		t.Fatalf("应用摘要不对: %v", applied)
	}
	if models[0].ContextWindow != 200000 {
		t.Fatalf("容量没被应用: %d", models[0].ContextWindow)
	}
	if models[1].ContextWindow != 111 {
		t.Fatal("不匹配的模型不该被动")
	}
}

// OpenRouter 免费判定

func TestIsOpenRouterFree(t *testing.T) {
	free := []string{
		"deepseek/deepseek-r1:free",
		"qwen/qwen3-coder:free",
		// ⚠️ 只有 openrouter/free 是免费的。
		// 实测（2026-10-19）：openrouter/auto、openrouter/fusion 等的 pricing 是 -1，
		// 不是 0 —— 它们是**按路由到的模型计费**的，不能当免费用。
		"openrouter/free",
	}
	for _, id := range free {
		if !IsOpenRouterFree(id) {
			t.Errorf("%q 应被识别为免费", id)
		}
	}
	paid := []string{
		"openai/gpt-4.1",
		"anthropic/claude-sonnet-4-5",
		"meta-llama/llama-3.3-70b-instruct",
		// 这些不是免费路由 —— 它们按实际路由到的模型计费。
		"openrouter/auto",
		"openrouter/auto-beta",
		"openrouter/fusion",
	}
	for _, id := range paid {
		if IsOpenRouterFree(id) {
			t.Errorf("%q 不该被识别为免费", id)
		}
	}
}

func TestModelsDevMetaShape(t *testing.T) {
	// 确认我们的解析能吃下 models.dev 的实际结构。
	payload := `{
	  "opencode": {
	    "models": {
	      "deepseek-v4.1-flash-free": {
	        "limit": {"context": 200000, "output": 32000},
	        "modalities": {"input": ["text","image"], "output": ["text"]},
	        "reasoning": true
	      }
	    }
	  }
	}`
	var doc map[string]modelsDevDoc
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	m, ok := doc["opencode"].Models["deepseek-v4.1-flash-free"]
	if !ok {
		t.Fatal("没找到模型")
	}
	if m.Limit.Context != 200000 || m.Limit.Output != 32000 {
		t.Fatalf("容量解析不对: %+v", m.Limit)
	}
	if len(m.Modalities.Input) != 2 {
		t.Fatalf("模态解析不对: %+v", m.Modalities)
	}
}
