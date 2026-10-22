package lane

// **统一内部事件模型**（canonical event bus）。
//
// 它是整个网关的中枢：所有上游、所有入站协议都收敛到这一种事件，
// 再由三个 encoder 投影成三种线上格式。
//
// 为什么是"轮辐"而不是"链条"：
// Chat → Responses → Messages 的链式转换每一步都会丢信息
// （Anthropic 的 tool_result 转成 Chat 会变形）。
// 所以是**各自解析进统一形式**，再**各自投影出去**。
//
// 详见 docs/design/00-overview.md §5.1。

// Role 是统一消息的角色。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// PartType 是消息内容块的类型。
type PartType string

const (
	PartText       PartType = "text"
	PartImage      PartType = "image"
	PartAudio      PartType = "audio"
	PartFile       PartType = "file"
	PartToolCall   PartType = "tool-call"
	PartToolResult PartType = "tool-result"
	PartReasoning  PartType = "reasoning"
)

// Part 是一个类型化的内容块。
type Part struct {
	Type PartType
	// Text 是文本内容（text / reasoning）。
	Text string
	// DataURL 是图片的 data URL（image）。
	DataURL string
	// AudioBase64 + AudioFormat 是音频载荷（audio）。
	AudioBase64 string
	AudioFormat string
	// FileName / MediaType / FileBase64 是文件载荷（file）。
	FileName   string
	MediaType  string
	FileBase64 string
	// ToolCallID / ToolName / Arguments 是工具调用（tool-call）。
	ToolCallID string
	ToolName   string
	Arguments  string
	// IsError 标记工具结果是不是错误（tool-result）。
	IsError bool
	// Signature 是思考块的签名（Anthropic 用）。可能为空。
	Signature string
}

func TextPart(text string) Part { return Part{Type: PartText, Text: text} }

func ImagePart(dataURL string) Part { return Part{Type: PartImage, DataURL: dataURL} }

func ToolCallPart(id, name, arguments string) Part {
	return Part{Type: PartToolCall, ToolCallID: id, ToolName: name, Arguments: arguments}
}

func ToolResultPart(callID, text string, isError bool) Part {
	return Part{Type: PartToolResult, ToolCallID: callID, Text: text, IsError: isError}
}

// Message 是一条统一消息。
type Message struct {
	Role  Role
	Parts []Part
}

// TextOf 拼出这条消息里的所有文本。
func (m Message) TextOf() string {
	out := ""
	for _, p := range m.Parts {
		if p.Type == PartText {
			out += p.Text
		}
	}
	return out
}

// ToolDef 是一个可调用工具的声明。
type ToolDef struct {
	Name        string
	Description string
	// Parameters 是 JSON Schema。
	Parameters map[string]any
}

// Usage 是一次调用的 token 计量。
type Usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	Reasoning  int `json:"reasoning,omitempty"`
	CacheRead  int `json:"cacheRead,omitempty"`
	CacheWrite int `json:"cacheWrite,omitempty"`
	Total      int `json:"total,omitempty"`
	// CostUSD 是这次调用花了多少美元（上游报的话）。
	//
	// ⚠️ **只有 OpenRouter 报** —— 免密车道的 usage 里没有这个字段。
	//
	// ⚠️ 用 float64 是因为**钱不能只按整数记** ——
	//   一次免费调用可能是 $0.0000043。累加时四舍五入到分以下（0.000001）。
	//
	// ⚠️ 浮点累加会有误差 —— 所以只在**展示**时格式化，存储时保留原值。
	CostUSD float64 `json:"costUsd,omitempty"`
}

// ChunkKind 是流事件的种类。
type ChunkKind string

const (
	ChunkBlockStart    ChunkKind = "block-start"
	ChunkTextDelta     ChunkKind = "text-delta"
	ChunkReasonDelta   ChunkKind = "reasoning-delta"
	ChunkToolCallDelta ChunkKind = "tool-call-delta"
	ChunkBlockEnd      ChunkKind = "block-end"
	ChunkUsage         ChunkKind = "usage"
	ChunkFinish        ChunkKind = "finish"
)

// BlockType 是块的类型。
type BlockType string

const (
	BlockText      BlockType = "text"
	BlockReasoning BlockType = "reasoning"
	BlockToolCall  BlockType = "tool-call"
)

// FinishReason 是一轮结束的原因。
type FinishReason string

const (
	FinishStop      FinishReason = "stop"
	FinishToolCalls FinishReason = "tool-calls"
	FinishMaxTokens FinishReason = "max-tokens"
	FinishAborted   FinishReason = "aborted"
	FinishError     FinishReason = "error"
)

// Chunk 是一个归一化的流片段。
type Chunk struct {
	Kind  ChunkKind
	Index int
	// BlockType 只在 block-start / block-end 时有意义。
	BlockType BlockType
	// Text 是块的完整文本（block-end 时），或增量文本。
	Text  string
	Delta string
	ID    string
	Name  string
	// Arguments 是工具参数的增量。
	Arguments string
	// Usage 只在 ChunkUsage 时有意义。
	Usage *Usage
	// Finish 只在 ChunkFinish 时有意义。
	Finish FinishReason
}

// UsageRecord 是一次请求的用量记录。
//
// ⚠️ **"请求"与"回合"必须分开记**：
//
//	Requests  = 物理上游请求数（含故障转移与续写）
//	Turns     = 用户实际发起的轮次
//
// 没有这个区分，"断流续写 3 次"会被算成 3 个请求 —— 用量看板会骗人。
type UsageRecord struct {
	// At 是时间（epoch ms）。
	At int64 `json:"at"`
	// Day 是本地自然日（YYYY-MM-DD）。
	Day   string `json:"day"`
	Lane  string `json:"lane"`
	Model string `json:"model"`
	// ActualModel 是**实际服务**的模型 —— 故障转移后可能与请求的不同。
	ActualModel string `json:"actualModel"`
	Client      string `json:"client"`
	Requests    int    `json:"requests"`
	Turns       int    `json:"turns"`
	// Attempts 是尝试次数（含故障转移）。
	Attempts int `json:"attempts"`
	// InputTokens / OutputTokens / ReasoningTokens 是 token 计量。
	InputTokens     int `json:"inputTokens"`
	OutputTokens    int `json:"outputTokens"`
	ReasoningTokens int `json:"reasoningTokens"`
	// Failed 标记这一轮最终失败。
	Failed int `json:"failed"`
	// Recovered 标记这一轮被续写救回来。
	Recovered int `json:"recovered"`
	// CostUSD 是这次请求花的美元。
	//
	// ⚠️ **只有 OpenRouter 报** —— 免密车道永远是 0。
	CostUSD float64 `json:"costUsd"`
}

// Turn 是一次归一化后的推理请求。
type Turn struct {
	// Model 是**上游**模型 id（不含车道前缀、不含档位后缀）。
	Model    string
	Lane     string
	Messages []Message
	Tools    []ToolDef
	// MaxTokens 是输出预算；<= 0 表示由上游决定。
	MaxTokens int
	// Effort 是思考档位（模型声明过的）。
	Effort string
	Stream bool
	// ClientID 是调用方的标识（从 Key 解析出来的）。
	ClientID string
	// Seed 是会话种子 —— 用来绑定稳定的**上游** session。
	Seed string
	// EffortPatch 是模型自己声明的思考参数补丁（OpenRouter 那种 reasoning 对象）。
	EffortPatch map[string]any
	// SuppressTools 表示**不向下游声明调用方的工具**。
	//
	// ⚠️ 只在续写时用 —— 我们只想让模型把话说完，
	// 不想让它又去调工具（那会产生新的调用，客户端会困惑）。
	SuppressTools bool
}

// Needs 描述一次请求对模型能力的要求。
type Needs struct {
	Image bool
	Audio bool
	File  bool
	Tools bool
	// PromptTokens 是对提示词长度的粗估。
	PromptTokens int
}

// SummarizeNeeds 从统一消息里推出这次请求需要什么能力。
//
// 图片按 1K token 估（一张图大致就是这个量级），文本按 4 字符/token 估。
func SummarizeNeeds(msgs []Message, hasTools bool) Needs {
	n := Needs{Tools: hasTools}
	chars := 0
	for _, m := range msgs {
		for _, p := range m.Parts {
			switch p.Type {
			case PartText, PartReasoning:
				chars += len(p.Text)
			case PartImage:
				n.Image = true
				n.PromptTokens += 1024
			case PartAudio:
				n.Audio = true
				n.PromptTokens += 1024
			case PartFile:
				n.File = true
				n.PromptTokens += 1024
			case PartToolCall:
				chars += len(p.Arguments) + len(p.ToolName)
			case PartToolResult:
				chars += len(p.Text)
			}
		}
	}
	n.PromptTokens += chars / 4
	return n
}
