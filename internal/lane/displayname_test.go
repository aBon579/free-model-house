package lane

import "testing"

// 车道名前缀是**给 agent 挑模型用的** —— 模型选择列表里只有这一个字符串，
// 少了前缀就分不清同名的模型是谁家的（Kilo 与 OpenRouter 的免费 id 完全重叠）。

func TestKiloDisplayNameHasLanePrefix(t *testing.T) {
	got := kiloDisplayName(kiloRow{ID: "xiaomi/mimo-v2.6-flash", Name: "xiaomi/mimo-v2.6-flash (free)"})
	want := "Kilo · mimo-v2.6-flash"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// ⚠️ 上游的厂商前缀与 `(free)` 后缀仍然要剥掉 —— 加车道名不等于把噪音搬回来。
func TestKiloDisplayNameStillStripsNoise(t *testing.T) {
	got := kiloDisplayName(kiloRow{ID: "glm-4.7-free", Name: "glm-4.7 (free)"})
	want := "Kilo · glm-4.7"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// 上游没给名字时用 id 顶上，不能发一个光秃秃的 `Kilo · `。
func TestKiloDisplayNameFallsBackToID(t *testing.T) {
	got := kiloDisplayName(kiloRow{ID: "some-model"})
	want := "Kilo · some-model"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

// OpenRouter 只补车道名：上游自己写的 `厂商: 型号` 原样留着。
func TestOpenRouterDisplayNameHasLanePrefix(t *testing.T) {
	row := openRouterCatalogRow{
		ID:   "nvidia/nemotron-3-super-120b-a12b:free",
		Name: "NVIDIA: Nemotron 3 Super 120B (free)",
	}
	got := openRouterDisplayName(row)
	want := "OpenRouter · NVIDIA: Nemotron 3 Super 120B (free)"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestOpenRouterDisplayNameFallsBackToID(t *testing.T) {
	got := openRouterDisplayName(openRouterCatalogRow{ID: "qwen/qwen3-coder:free"})
	want := "OpenRouter · qwen/qwen3-coder:free"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// 两个字段都没有 —— 宁可没有名字，也不要一个只剩前缀的字符串。
	if openRouterDisplayName(openRouterCatalogRow{}) != "" {
		t.Fatalf("空行不该造出前缀名")
	}
}
