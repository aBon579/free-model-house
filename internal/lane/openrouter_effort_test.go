package lane

import (
	"testing"
)

// ⚠️ 本文件的期望值全部来自**真实上游**（2026-10-19 实测）。
// 上游的实测数据：
//	minimax/minimax-m3.1-flash:free
//	    mandatory=true   efforts=max,xhigh,high,medium,low   default=max
//	liquid/lfm-2.5-2.6b:free
//	nvidia/nemotron-3-super-120b-a12b:free
//	thinkingmachines/inkling:free
//	    mandatory=false  efforts=max,high,medium,low,minimal,none  default=high
//	cohere/north-mini-code:free
// 以及最关键的一条**实测错误**：
//	POST /chat/completions  {"reasoning": {"enabled": false}}
//	→ 400 "Reasoning is mandatory for this endpoint and cannot be disabled."

// newOREffortLane 建一个带档位表的车道。
func newOREffortLane(efforts map[string][]string, mandatory map[string]bool) *OpenRouterLane {
	l := NewOpenRouterLane("sk-or-v1-test")
	l.setEfforts(efforts)
	l.setMandatory(mandatory)
	return l
}

// TestOREffortMandatoryCannotDisable 验证**强制思考的模型不能关掉思考**。
// ⚠️ 这是实测踩过的坑 ——
//
//	发 `reasoning.enabled: false` 会得到 **400**。
//	所以"关掉思考"要**降级成最低档位**。
func TestOREffortMandatoryCannotDisable(t *testing.T) {
	l := newOREffortLane(
		map[string][]string{
			"minimax/minimax-m3.1-flash:free": {"max", "xhigh", "high", "medium", "low"},
		},
		map[string]bool{"minimax/minimax-m3.1-flash:free": true},
	)

	patch := l.effortPatch(&Turn{Model: "minimax/minimax-m3.1-flash:free", Effort: EffortOff})
	if patch == nil {
		t.Fatal("不该是 nil")
	}
	// ⚠️ **绝不能**是 enabled:false —— 那会 400。
	if _, bad := patch["enabled"]; bad {
		t.Fatalf("强制思考的模型不该发 enabled:false: %v", patch)
	}
	// 应该是最低档位。
	if patch["effort"] != "low" {
		t.Fatalf("该降级成最低档位 low，实际 %v", patch)
	}
}

// TestOREffortMandatoryIgnoresNoneLevel 验证**强制思考时 `none` 档也被忽略**。
// ⚠️ 实测：mandatory 模型上 `effort:none` 也返回 **400**。
//
//	所以哪怕模型声明了 `none` 档，mandatory 也要**先**判。
func TestOREffortMandatoryIgnoresNoneLevel(t *testing.T) {
	l := newOREffortLane(
		map[string][]string{
			"mandatory-with-none:free": {"none", "low", "medium"},
		},
		map[string]bool{"mandatory-with-none:free": true},
	)
	patch := l.effortPatch(&Turn{Model: "mandatory-with-none:free", Effort: EffortOff})
	if patch == nil {
		t.Fatal("不该是 nil")
	}
	// ⚠️ 绝不能是 none —— 那会 400。
	if patch["effort"] == "none" {
		t.Fatalf("强制思考的模型不该发 effort:none: %v", patch)
	}
	if _, bad := patch["enabled"]; bad {
		t.Fatalf("强制思考的模型不该发 enabled:false: %v", patch)
	}
	// 该降级成最低的**非 none** 档。
	if patch["effort"] != "low" {
		t.Fatalf("该降级成 low，实际 %v", patch)
	}
}

// TestOREffortMandatoryNoLevels 验证**强制思考且无档位**时干脆不发参数。
func TestOREffortMandatoryNoLevels(t *testing.T) {
	l := newOREffortLane(
		map[string][]string{},
		map[string]bool{"liquid/lfm-2.5-2.6b:free": true},
	)
	patch := l.effortPatch(&Turn{Model: "liquid/lfm-2.5-2.6b:free", Effort: EffortOff})
	if patch != nil {
		t.Fatalf("该是 nil（不硬发 enabled:false），实际 %v", patch)
	}
}

// TestOREffortNoneLevelDisables 验证**声明了 none 档**时用档位表达"关掉"。
func TestOREffortNoneLevelDisables(t *testing.T) {
	l := newOREffortLane(
		map[string][]string{
			"nvidia/nemotron-3-super-120b-a12b:free": {"medium", "low", "none"},
		},
		map[string]bool{},
	)
	patch := l.effortPatch(&Turn{Model: "nvidia/nemotron-3-super-120b-a12b:free", Effort: EffortOff})
	if patch == nil {
		t.Fatal("不该是 nil")
	}
	if patch["effort"] != "none" {
		t.Fatalf("该用 none 档，实际 %v", patch)
	}
	if _, bad := patch["enabled"]; bad {
		t.Fatalf("有 none 档就不该用 enabled:false: %v", patch)
	}
}

// TestOREffortDisableWithoutNoneLevel 验证**没有 none 档但不强制的**用 enabled:false。
func TestOREffortDisableWithoutNoneLevel(t *testing.T) {
	l := newOREffortLane(
		map[string][]string{"cohere/north-mini-code:free": {}},
		map[string]bool{},
	)
	patch := l.effortPatch(&Turn{Model: "cohere/north-mini-code:free", Effort: EffortOff})
	if patch == nil {
		t.Fatal("不该是 nil")
	}
	if patch["enabled"] != false {
		t.Fatalf("该发 enabled:false，实际 %v", patch)
	}
}

// TestOREffortSnapsToSupported 验证**档位会收敛到模型声明过的**。
func TestOREffortSnapsToSupported(t *testing.T) {
	l := newOREffortLane(
		map[string][]string{
			"nvidia/nemotron-3-super-120b-a12b:free": {"medium", "low", "none"},
		},
		map[string]bool{},
	)
	// deep → high，但 high 不在声明里 → 收敛到最近的。
	patch := l.effortPatch(&Turn{Model: "nvidia/nemotron-3-super-120b-a12b:free", Effort: "deep"})
	if patch["effort"] != "medium" {
		t.Fatalf("该收敛到 medium，实际 %v", patch["effort"])
	}
}

// TestOREffortUnifiedLevelsMap 验证**统一档位 → OpenRouter 档位**的映射。
func TestOREffortUnifiedLevelsMap(t *testing.T) {
	l := newOREffortLane(
		map[string][]string{
			"m": {"max", "xhigh", "high", "medium", "low"},
		},
		map[string]bool{},
	)
	cases := map[string]string{
		"light":    "low",
		"balanced": "medium",
		"deep":     "high",
	}
	for in, want := range cases {
		patch := l.effortPatch(&Turn{Model: "m", Effort: in})
		if patch["effort"] != want {
			t.Errorf("%s → %v，期望 %s", in, patch["effort"], want)
		}
	}
}

// TestOREffortKnownNoLevelsIsConservative 验证**已知不支持档位**的模型不发 effort。
// ⚠️ "已知"和"未知"要分开：
//
//	已知（在上表里但档位为空）→ 上游明确说了"我不支持档位" → 不发
//	未知（不在上表里）          → 我们还没探到 → 发（上游会忽略，无害）
func TestOREffortKnownNoLevelsIsConservative(t *testing.T) {
	// ⚠️ 显式放一个**空档位**的条目 → 标记为"已知"。
	l := newOREffortLane(
		map[string][]string{"cohere/north-mini-code:free": {}},
		map[string]bool{},
	)
	patch := l.effortPatch(&Turn{Model: "cohere/north-mini-code:free", Effort: "deep"})
	if patch != nil {
		t.Fatalf("已知不支持档位时该不发，实际 %v", patch)
	}
}

// TestOREffortUnknownModelSendsEffort 验证**未知模型**照发统一档位。
// 实测：上游接受 `effort`（200，只是被忽略）。
func TestOREffortUnknownModelSendsEffort(t *testing.T) {
	l := newOREffortLane(map[string][]string{}, map[string]bool{})
	patch := l.effortPatch(&Turn{Model: "never-seen-model", Effort: "deep"})
	if patch == nil {
		t.Fatal("未知模型该照发档位")
	}
	if patch["effort"] != "high" {
		t.Fatalf("deep 该映射成 high，实际 %v", patch)
	}
}

// TestLowestEffortSkipsNone 验证**降级时不会选中 none**。
// ⚠️ "none" 是"关掉"，不是"最低档" ——
//
//	用它来"降级"等于把强制思考的模型变成了关掉思考（会 400）。
func TestLowestEffortSkipsNone(t *testing.T) {
	if got := lowestEffort([]string{"none", "low", "medium"}); got != "low" {
		t.Fatalf("该是 low，实际 %q", got)
	}
	if got := lowestEffort([]string{"none"}); got != "" {
		t.Fatalf("只有 none 时该返回空，实际 %q", got)
	}
	if got := lowestEffort([]string{"max", "xhigh", "high"}); got != "high" {
		t.Fatalf("该是 high，实际 %q", got)
	}
}

// TestBudgetForEffortOff 验证**"关掉思考"用最紧的预算，且不加倍**。
func TestBudgetForEffortOff(t *testing.T) {
	// 思考关不掉的模型。
	info := ModelInfo{MaxOutput: 32768, Reasoning: true, CanDisableThinking: false}

	// ⚠️ "关掉思考"时**不加倍** —— 加倍正好相反。
	got := BudgetFor(EffortOff, info, 0, DefaultEffortBudget)
	if got != 2048 {
		t.Fatalf("该是 2048（最紧预算，不加倍），实际 %d", got)
	}
	// 对照：普通 light 档会加倍。
	light := BudgetFor("light", info, 0, DefaultEffortBudget)
	if light != 2048*2 {
		t.Fatalf("light 档该加倍成 4096，实际 %d", light)
	}
}
