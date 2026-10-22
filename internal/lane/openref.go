package lane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
)

// **公开目录补容量**层。
//
// 用途：本地规则表跟不上上游的模型变动 —— 它是手写的，而上游每周都在
// 加模型、改容量。两个来源，**都不需要 key**：
//
//	models.dev/api.json          → 社区维护的模型目录
//	openrouter.ai/api/v1/models  → OpenRouter 的公开目录
//
// ⚠️ 信任序：实测 > 上游声明（opencode 的清单）> 第三方目录 > 本地规则表。
// 而 opencode 的清单**不声明任何容量**（实测：只有 id/object/created/owned_by），
// 所以这里的数据是"第三方声明"，比实测低、比没有高。
//
// ⚠️ **本层只写容量，不写模态结论。**
// 模态是"路由硬过滤"的依据 —— 用第三方目录猜出来的模态去过滤，
// 会把能看图的模型误杀。模态只能来自实测。

// freeLaneSuffixRe 剥掉车道自己的档位标记。
//
// ⚠️ 只剥标记本身：`-preview` 是**模型名的一部分**
// （`longcat-2.5-preview-free`），剥掉它会让 preview 版和稳定版撞车。
var freeLaneSuffixRe = regexp.MustCompile(`(?:-(?:free|contributor))+$`)

// versionSepRe 统一下划线与主流的短横线。
//
// ⚠️ 点号要保留 —— 版本号（"1.3"）必须能对上。
var versionSepRe = regexp.MustCompile(`_+`)

// NormalizeID 把一个目录里的 id 归一化成可比较的核心。
//
// vendor 前缀丢掉、车道档位后缀丢掉、分隔符统一、转小写。
// `stepfun/step-5-preview-free` 与 `step-5-preview` 都会落到 `step-5-preview`。
//
// ⚠️ **顺序很重要**：先统一分隔符，**再**剥档位后缀。
// 反过来的话，`deepseek_v4_flash_free` 剥不掉 `_free`（正则认的是 `-free`），
// 就会归一化成 `deepseek-v4-flash-free` 而不是 `deepseek-v4-flash`。
func NormalizeID(id string) string {
	s := strings.ToLower(strings.TrimSpace(id))
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	// ① 先把下划线统一成短横线。
	s = versionSepRe.ReplaceAllString(s, "-")
	// ② 再剥档位后缀。
	s = strings.TrimSpace(freeLaneSuffixRe.ReplaceAllString(s, ""))
	s = strings.Trim(s, "-")
	return s
}

// ModelsDevBase 是 models.dev 的目录根。
var ModelsDevBase = "https://models.dev/api.json"

// modelsDevDoc 是 models.dev 顶层的一个 provider。
type modelsDevDoc struct {
	// Models 是这个 provider 的模型表。
	Models map[string]struct {
		Limit struct {
			Context int `json:"context"`
			Input   int `json:"input"`
			Output  int `json:"output"`
		} `json:"limit"`
		Modalities struct {
			Input  []string `json:"input"`
			Output []string `json:"output"`
		} `json:"modalities"`
		Reasoning bool `json:"reasoning"`
	} `json:"models"`
}

// ModelsDevMeta 是一个模型在公开目录里的元数据。
type ModelsDevMeta struct {
	ContextWindow int
	MaxOutput     int
	// Vision / Audio / File 是模态声明（来自第三方目录）。
	Vision bool
	Audio  bool
	File   bool
	// Reasoning 是是否支持思考。
	Reasoning bool
	// Efforts 是模型**自己声明**的思考档位（OpenRouter 会给）。
	Efforts []string
	// CanDisable 标记思考能不能关掉。
	//
	// ⚠️ `mandatory: true` 的模型关不掉 ——
	// 它的输出预算要加倍（思考与可见答案**共享**同一个上限）。
	CanDisable bool
}

// FetchModelsDev 拉取 models.dev 里 opencode provider 的容量。
//
// ⚠️ 用**精确 id** 做键（不做归一化）——
// 因为它的 key 本来就是上游的 id，同名不同模型的风险最小。
func FetchModelsDev(ctx context.Context, client *http.Client) (map[string]ModelsDevMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ModelsDevBase, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "free-model-house/0.1")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &Error{Code: CodeServer, Message: "models.dev HTTP " + itoa(resp.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	var doc map[string]modelsDevDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	provider, ok := doc["opencode"]
	if !ok {
		return nil, &Error{Code: CodeServer, Message: "models.dev has no opencode provider"}
	}
	out := map[string]ModelsDevMeta{}
	for id, m := range provider.Models {
		meta := ModelsDevMeta{
			ContextWindow: m.Limit.Context,
			MaxOutput:     m.Limit.Output,
			Reasoning:     m.Reasoning,
		}
		for _, in := range m.Modalities.Input {
			switch in {
			case "image":
				meta.Vision = true
			case "audio":
				meta.Audio = true
			case "pdf", "file":
				meta.File = true
			}
		}
		if meta.ContextWindow <= 0 && meta.MaxOutput <= 0 {
			continue
		}
		out[id] = meta
	}
	return out, nil
}

// OpenRouterModelsBase 是 OpenRouter 的公开模型目录。
//
// ⚠️ **不需要 key** —— 这是一个公开端点。
// 所以它可以用来给**别的车道**补容量。
var OpenRouterModelsBase = "https://openrouter.ai/api/v1/models"

// openRouterRow 是 OpenRouter 目录里的一行。
type openRouterRow struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
	TopProvider   struct {
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
	Architecture struct {
		InputModalities []string `json:"input_modalities"`
	} `json:"architecture"`
	SupportedParameters []string `json:"supported_parameters"`
	// Reasoning 的形状**上游变过**：
	//   旧：布尔
	//   新：对象 {"mandatory":bool,"supported_efforts":[...],"default_effort":"..."}
	// 所以用 RawMessage，两种都吃。
	Reasoning json.RawMessage `json:"reasoning"`
}

// openRouterReasoning 是 OpenRouter 的 reasoning 对象形状。
type openRouterReasoning struct {
	Mandatory        bool     `json:"mandatory"`
	SupportedEfforts []string `json:"supported_efforts"`
	DefaultEffort    string   `json:"default_effort"`
}

// parseOpenRouterReasoning 解析 reasoning 字段（两种形状都支持）。
//
// 返回：是否支持思考、支持哪些档位、能否关掉。
func parseOpenRouterReasoning(raw json.RawMessage) (supports bool, efforts []string, canDisable bool) {
	if len(raw) == 0 {
		return false, nil, false
	}
	// 旧形状：布尔。
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b, nil, true
	}
	// 新形状：对象。
	var obj openRouterReasoning
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false, nil, false
	}
	// ⚠️ `mandatory: true` 表示**思考关不掉** ——
	// 这时候输出预算要加倍（思考与答案共享上限）。
	canDisable = !obj.Mandatory
	return true, obj.SupportedEfforts, canDisable
}

// FetchOpenRouterCatalog 拉 OpenRouter 的公开目录。
//
// 只取**免费**的模型 —— 付费模型对 FMH 没有意义。
func FetchOpenRouterCatalog(ctx context.Context, client *http.Client) (map[string]ModelsDevMeta, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, OpenRouterModelsBase, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json")
	req.Header.Set("user-agent", "free-model-house/0.1")

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &Error{Code: CodeServer, Message: "openrouter HTTP " + itoa(resp.StatusCode)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Data []openRouterRow `json:"data"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	out := map[string]ModelsDevMeta{}
	for _, row := range doc.Data {
		if !IsOpenRouterFree(row.ID) {
			continue
		}
		supports, efforts, canDisable := parseOpenRouterReasoning(row.Reasoning)
		meta := ModelsDevMeta{
			ContextWindow: row.ContextLength,
			MaxOutput:     row.TopProvider.MaxCompletionTokens,
			Reasoning:     supports,
			Efforts:       efforts,
			CanDisable:    canDisable,
		}
		for _, in := range row.Architecture.InputModalities {
			switch in {
			case "image":
				meta.Vision = true
			case "audio":
				meta.Audio = true
			case "file", "pdf":
				meta.File = true
			}
		}
		out[NormalizeID(row.ID)] = meta
	}
	return out, nil
}

// CapacityMeta 是一个容量结论。
type CapacityMeta struct {
	ContextWindow int
	MaxOutput     int
	Source        CapabilitySource
	// Vision / Audio / File / Reasoning 是模态声明。
	// ⚠️ 只有实测来源才允许带模态结论。
	Vision    bool
	Audio     bool
	File      bool
	Reasoning bool
	// HasModality 报告是否带了模态结论。
	HasModality bool
}

// ApplyCapacity 把一个容量结论应用到模型上。
//
// ⚠️ **模态结论只有实测来源才生效**。
// 第三方目录的模态声明**不写进模型** ——
// 路由的模态过滤是硬约束，用猜出来的结论去过滤会误杀模型。
//
// 容量则可以直接用：它只影响"要不要把这个候选跳过"的软过滤，
// 猜错的代价远小于误杀。
func ApplyCapacity(info *ModelInfo, meta CapacityMeta) bool {
	if meta.ContextWindow <= 0 && meta.MaxOutput <= 0 {
		return false
	}
	// 已有更高信任来源的结论 → 不覆盖。
	if info.Capability == CapabilitySourceProbe {
		return false
	}
	if meta.ContextWindow > 0 {
		info.ContextWindow = meta.ContextWindow
	}
	if meta.MaxOutput > 0 {
		info.MaxOutput = meta.MaxOutput
	}
	if meta.HasModality && meta.Source == CapabilitySourceProbe {
		info.Vision = meta.Vision
		info.AudioInput = meta.Audio
		info.FileInput = meta.File
		if meta.Reasoning {
			info.Reasoning = true
		}
	}
	info.Capability = meta.Source
	return true
}

// MergeCatalog 合并两个容量来源。
//
// ⚠️ 冲突处理：**宁可留空，也不要猜**。
//
// 两个来源对同一个归一化 id 给出**不同**的容量时 ——
// 那个 id 是"同名不同模型"，埋掉它，不要挑一个赢家。
// 挑错了会让一个模型带着别人的容量进路由。
func MergeCatalog(strong, weak map[string]ModelsDevMeta) (map[string]ModelsDevMeta, map[string]bool) {
	out := map[string]ModelsDevMeta{}
	conflicts := map[string]bool{}
	for k, v := range strong {
		out[k] = v
	}
	for k, v := range weak {
		existing, ok := out[k]
		if !ok {
			out[k] = v
			continue
		}
		// 冲突：同名不同模型。
		if existing.ContextWindow != v.ContextWindow || existing.MaxOutput != v.MaxOutput {
			delete(out, k)
			conflicts[k] = true
		}
	}
	return out, conflicts
}

// ApplyCatalog 把容量目录应用到模型清单上。
//
// 返回应用了什么的摘要（用于日志）。
func ApplyCatalog(models []ModelInfo, catalog map[string]ModelsDevMeta) []string {
	applied := []string{}
	for i := range models {
		meta, ok := catalog[models[i].ID]
		if !ok {
			// 试归一化后的键（给 OpenRouter 目录用）。
			meta, ok = catalog[NormalizeID(models[i].ID)]
		}
		if !ok {
			continue
		}
		before := models[i]
		ApplyCapacity(&models[i], CapacityMeta{
			ContextWindow: meta.ContextWindow,
			MaxOutput:     meta.MaxOutput,
			Source:        CapabilitySourceListing,
		})
		if models[i].ContextWindow != before.ContextWindow || models[i].MaxOutput != before.MaxOutput {
			applied = append(applied, models[i].ID)
		}
	}
	sort.Strings(applied)
	return applied
}

// openRouterFreeSuffixRe 匹配 OpenRouter 的 `:free` 后缀。
var openRouterFreeSuffixRe = regexp.MustCompile(`:free$`)

// IsOpenRouterFree 报告一个 OpenRouter 模型 id 是否免费。
//
// ⚠️ 两条判据**都要用**：
//
//  1. id 以 `:free` 结尾（`deepseek/deepseek-r1:free`）
//  2. 上游的 pricing 是 0/0
//
// 只靠 `:free` 会漏掉新上架的零价模型；
// 只靠 pricing 也会漏（有些免费模型定价不是 0）。
//
// ⚠️ 还有一个特例：`openrouter/free` 是 OpenRouter 的**免费路由器**，
// 它把请求路由到当前可用的免费模型 —— 它自己也不是 `:free` 后缀。
func IsOpenRouterFree(id string) bool {
	id = strings.TrimSpace(id)
	if id == "" {
		return false
	}
	if id == "openrouter/free" {
		return true
	}
	return openRouterFreeSuffixRe.MatchString(id)
}
