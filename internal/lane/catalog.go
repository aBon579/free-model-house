package lane

import (
	"regexp"
	"strings"
)

// **模型清单**与**能力元数据**。
// 核心认识：能力结论有一个**信任序**，低信任的来源**不能覆盖**高信任的来源：
//	实测 (probe)  >  上游声明 (listing)  >  规则/名称启发式 (heuristic)  >  未知
// 而"规则表"是**地板**（基线），不是天花板。
// 一个没匹配任何规则的模型**什么都不声称** ——
// 猜一个 "reasoning: true" 会显示一个假的"思考"徽章。
// 详见 docs/design/00-overview.md §5.7。

// CapabilitySource 是一条能力结论的来源。
type CapabilitySource string

const (
	// CapabilitySourceProbe 是实测出来的 —— 最可信。
	CapabilitySourceProbe CapabilitySource = "probe"
	// CapabilitySourceListing 是上游自己声明的。
	CapabilitySourceListing CapabilitySource = "listing"
	// CapabilitySourceHeuristic 是规则/名称启发式猜的。
	CapabilitySourceHeuristic CapabilitySource = "heuristic"
	// CapabilitySourceUnknown 是没有结论。
	CapabilitySourceUnknown CapabilitySource = "unknown"
)

// ModelInfo 是模型清单里的一行。
type ModelInfo struct {
	ID   string `json:"id"`
	Lane string `json:"lane"`
	// Wire 是它说哪种协议：chat / responses / messages。
	Wire    string `json:"wire"`
	Name    string `json:"name,omitempty"`
	Created int64  `json:"created,omitempty"`

	// Free 标记它是不是免费车道的模型。
	Free bool `json:"free"`
	// Enabled 标记用户有没有把它关掉。
	Enabled bool `json:"enabled"`

	// Vision / AudioInput / FileInput 是输入模态能力。
	Vision     bool `json:"vision"`
	AudioInput bool `json:"audioInput,omitempty"`
	FileInput  bool `json:"fileInput,omitempty"`
	// Reasoning 标记它是否支持思考。
	Reasoning bool `json:"reasoning"`

	// ContextWindow / MaxOutput 是 token 容量。
	ContextWindow int `json:"contextWindow"`
	MaxOutput     int `json:"maxOutput"`

	// CanDisableThinking 标记思考能不能关掉。
	// 关不掉时，整档输出预算要加倍 —— 思考 token 与可见答案
	// **共享**同一个上限。
	CanDisableThinking bool `json:"canDisableThinking"`

	// Efforts 是模型**实际生效**的思考档位。
	Efforts []string `json:"efforts,omitempty"`
	// EffortDefault 是它的默认档位。
	EffortDefault string `json:"effortDefault,omitempty"`

	// Capability 是能力结论的来源。
	Capability CapabilitySource `json:"capability"`

	State  string `json:"state,omitempty"`
	TTFTMs int64  `json:"ttftMs,omitempty"`

	// ProbedAt 是**上次探测的时间**（毫秒时间戳）。
	//
	// ⚠️ **手动和自动探测都记** ——
	//   用户要知道"这个结论是什么时候测的"，
	//   才能判断它还新不新（结论的 TTL 是 10 分钟）。
	//
	// ⚠️ 存**毫秒**，前端按秒显示。
	ProbedAt int64 `json:"probedAt,omitempty"`
}

// capabilityRow 是一行规则。
type capabilityRow struct {
	// match 匹配模型 id。
	match *regexp.Regexp
	// vision / audio / file 是模态能力。
	vision bool
	audio  bool
	file   bool
	// reasoning 标记是否支持思考。
	reasoning bool
	// contextWindow / maxOutput 是容量。
	contextWindow int
	maxOutput     int
	// canDisableThinking 标记思考能否关掉。
	canDisableThinking bool
}

// capabilityRows 是能力规则表。
// ⚠️ 这些数字是**实测**或**官方公布**的，不是猜的。
// 但它们是**地板** —— 实测结论可以覆盖它们，反之不行。
var capabilityRows = []capabilityRow{
	{match: regexp.MustCompile(`^mimo.*v2\.6`), vision: true, reasoning: true, contextWindow: 1048576, maxOutput: 131072},
	{match: regexp.MustCompile(`^mimo.*v2\.5`), vision: true, reasoning: true, contextWindow: 1048576, maxOutput: 131072},
	{match: regexp.MustCompile(`^mimo`), vision: true, reasoning: true, contextWindow: 262144, maxOutput: 131072, canDisableThinking: true},
	{match: regexp.MustCompile(`^muse.?spark`), vision: true, reasoning: true, contextWindow: 1048576, maxOutput: 943718, canDisableThinking: true},
	{match: regexp.MustCompile(`^step`), reasoning: true, contextWindow: 1000000, maxOutput: 64000, canDisableThinking: true},
	{match: regexp.MustCompile(`^nemotron.*super.?vl`), vision: true, reasoning: true, contextWindow: 1048576, maxOutput: 262144, canDisableThinking: true},
	{match: regexp.MustCompile(`^nemotron`), reasoning: true, contextWindow: 128000, maxOutput: 32768, canDisableThinking: true},
	{match: regexp.MustCompile(`^ling`), reasoning: true, contextWindow: 262144, maxOutput: 32768, canDisableThinking: true},
	{match: regexp.MustCompile(`^space.?bunny`), vision: true, reasoning: true, contextWindow: 1048576, maxOutput: 65536, canDisableThinking: true},
	{match: regexp.MustCompile(`^union`), vision: true, contextWindow: 262144, maxOutput: 131072, canDisableThinking: true},
	{match: regexp.MustCompile(`^deepseek.*v4\.1`), vision: true, reasoning: true, contextWindow: 1048576, maxOutput: 262144, canDisableThinking: true},
	{match: regexp.MustCompile(`^deepseek`), reasoning: true, contextWindow: 1048576, maxOutput: 65536, canDisableThinking: true},
	{match: regexp.MustCompile(`^ll.?alpha`), vision: true, reasoning: true, contextWindow: 1048576, maxOutput: 131072, canDisableThinking: true},
	{match: regexp.MustCompile(`^glm`), reasoning: true, contextWindow: 524288, maxOutput: 262144, canDisableThinking: true},
	{match: regexp.MustCompile(`^longcat`), vision: true, reasoning: true, contextWindow: 1048576, maxOutput: 131072, canDisableThinking: true},
	{match: regexp.MustCompile(`^fledge`), reasoning: true, contextWindow: 131072, maxOutput: 32768, canDisableThinking: true},
}

// applyHeuristicCapabilities 用规则表给一个模型补上基线能力。
// ⚠️ 未匹配任何规则的模型**什么都不声称**。
func applyHeuristicCapabilities(info *ModelInfo, modelID string) {
	base := baseModelID(modelID)
	for _, row := range capabilityRows {
		if !row.match.MatchString(base) {
			continue
		}
		info.Vision = row.vision
		info.AudioInput = row.audio
		info.FileInput = row.file
		info.Reasoning = row.reasoning
		info.CanDisableThinking = row.canDisableThinking
		if row.contextWindow > 0 {
			info.ContextWindow = row.contextWindow
		}
		if row.maxOutput > 0 {
			info.MaxOutput = row.maxOutput
		}
		info.Capability = CapabilitySourceHeuristic
		return
	}
	// 没匹配上 —— 什么都不声称。
	info.Capability = CapabilitySourceUnknown
}

// baseModelID 剥掉档位后缀与车道前缀。
func baseModelID(modelID string) string {
	s := strings.TrimSpace(modelID)
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 1 && s[len(s)-1] == ')' {
		for i := len(s) - 2; i >= 0; i-- {
			if s[i] == '(' {
				return strings.TrimSpace(s[:i])
			}
		}
	}
	return s
}

// CapabilityKnown 报告能力结论是否**可信**。
// ⚠️ 空字符串也当作"未知" —— 因为一个手工构造的 ModelInfo
// （测试、导入的清单）可能没设这个字段。
// "未知"意味着：模态结论**只是猜测**，不能用来硬过滤。
func (m ModelInfo) CapabilityKnown() bool {
	switch m.Capability {
	case CapabilitySourceProbe, CapabilitySourceListing, CapabilitySourceHeuristic:
		return true
	default:
		// CapabilitySourceUnknown 或空串 → 未知。
		return false
	}
}

// EstimateTokens 估算一个请求体的 token 数。
// ⚠️ **这是启发式估算，不是精确计数** ——
// 而且它**必须被如实标注**（返回里带 fmh_estimated: true）。
// 为什么要本地估？Anthropic 的 /v1/messages/count_tokens 是**免费**的，
// 但那是 Anthropic 的服务 —— 我们的上游是别家，
// 调上游去数 token 会**消耗一次真实推理**。
// 所以：本地估，标明是估算。
// 经验系数：英文约 4 字符/token，中文约 1.5 字符/token。
// 这里用一个保守的混合值 —— 宁可高估（让调用方留余量），
// 不要低估（那会导致请求被上游拒绝，而本地看不出原因）。
func EstimateTokens(body map[string]any) int {
	total := 0
	total += countTextTokens(body["messages"])
	total += countTextTokens(body["input"])
	total += countTextTokens(body["system"])
	total += countTextTokens(body["instructions"])
	total += countTextTokens(body["prompt"])
	if s, ok := body["prompt"].(string); ok {
		total += countTextTokens(s)
	}
	// 工具定义也占 token。
	if tools, ok := body["tools"].([]any); ok {
		for _, t := range tools {
			total += 64 // 每个工具的固定开销
			if m, ok := t.(map[string]any); ok {
				total += countTextTokens(m["description"])
				if fn, ok := m["function"].(map[string]any); ok {
					total += countTextTokens(fn["description"])
					total += countTextTokens(fn["name"])
				}
			}
		}
	}
	return total
}

// countTextTokens 估算一段内容的 token 数。
func countTextTokens(raw any) int {
	switch v := raw.(type) {
	case string:
		return textTokens(v)
	case []any:
		total := 0
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				total += countTextTokens(m["content"])
				total += countTextTokens(m["text"])
			}
		}
		return total
	case map[string]any:
		total := 0
		for _, val := range v {
			total += countTextTokens(val)
		}
		return total
	}
	return 0
}

// textTokens 估算一段文本的 token 数。
// ⚠️ 中文与英文的密度差别很大 —— 一律按"字符数/2"算，
// 对中文偏保守（偏高），对英文偏低（偏低）。
// 但**高估的代价远小于低估**：
// 高估 → 调用方多留余量，安全；
// 低估 → 请求被上游拒绝，而本地看不出原因。
func textTokens(s string) int {
	if s == "" {
		return 0
	}
	// 每 2 个字符 1 个 token（对中英混合保守）。
	n := len([]rune(s))
	return (n + 1) / 2
}

// ApplyCapabilityTags 用一个更高信任的结论覆盖规则表。
// ⚠️ **视觉能力只能升级不能降级** —— 一次猜测不该抹掉一次实测确认的能力。
// 容量按字段合并：量到了窗口但没量输出上限的来源，不该丢掉另一个来源的输出上限。
func ApplyCapabilityTags(info *ModelInfo, tags CapabilityTags) {
	if tags.Vision != nil && *tags.Vision && !info.Vision {
		info.Vision = true
	}
	if tags.Audio != nil {
		info.AudioInput = *tags.Audio
	}
	if tags.File != nil {
		info.FileInput = *tags.File
	}
	if tags.Reasoning != nil {
		info.Reasoning = *tags.Reasoning
	}
	if tags.ContextWindow > 0 {
		info.ContextWindow = tags.ContextWindow
	}
	if tags.MaxOutput > 0 {
		info.MaxOutput = tags.MaxOutput
	}
	if tags.Source != "" {
		info.Capability = tags.Source
	}
}

// CapabilityTags 是一组能力结论。
type CapabilityTags struct {
	Vision        *bool
	Audio         *bool
	File          *bool
	Reasoning     *bool
	ContextWindow int
	MaxOutput     int
	Source        CapabilitySource
}

// SatisfiesNeeds 报告一个模型能不能承接这次请求。
// ⚠️ **"没测过"不等于"不能"。**
// 这是模态过滤最容易搞错的地方。三种来源要区别对待：
//
//	实测 / 上游声明  → **硬过滤**（我们知道它不行）
//	规则表匹配到了     → **硬过滤**（表是实测出来的，可信）
//	规则表**没**匹配到 → **软过滤**（我们只是不知道，不该断言它不行）
//
// 用"没测过"当"不能"去硬过滤，会把能看图的模型误杀 ——
// 而误杀的代价是**整个请求失败**。
func SatisfiesNeeds(info ModelInfo, needs Needs) bool {
	// 能力未知 → 软过滤（通过，但排序时靠后）。
	if !info.CapabilityKnown() {
		return true
	}
	if needs.Image && !info.Vision {
		return false
	}
	if needs.Audio && !info.AudioInput {
		return false
	}
	if needs.File && !info.FileInput {
		return false
	}
	return true
}

// ApplyHeadroom 把实测容量放宽 1.5 倍。
// ⚠️ 0 表示"没测过" —— 此时**不要发明数字**，
// 让未知模型的默认值继续负责。
// 为什么放宽：实测容量是**下界**。探测是"逐步加大直到被拒"，
// 真实上限落在最后一次成功与首次被拒之间。
// 路由过滤用 1.5 倍，让略微超标的 prompt 真的发出去 ——
// 上游才是裁判，候选被静默丢弃的代价是**整个请求失败**。
func ApplyHeadroom(n int) int {
	if n <= 0 {
		return 0
	}
	return n * 3 / 2
}
