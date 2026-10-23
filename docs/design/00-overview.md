# Free Model House — 设计方案 v1.0

> 一个部署在 NAS 上、通过 Web 界面管理、把 **OpenCode Zen 免费车道** 与
> **OpenRouter 免费模型** 聚合成局域网红关的容器化项目。
> 局域网内的 Agent 只需要 `base_url + api_key` 即可免 key 使用免费模型。

> ⚠️ **关于文中的日期**
>
> 本文档记录的实测日期为 **2026-10-19 ～ 2026-10-21**。
>
> 免费车道的上游规格**随时可能变** —— 端点、UA 形状、会话头、工具 schema
> 都是对方定的，不是我们定的。
>
> **以代码为准**：
>
> - 规格字面量 → `internal/lane/zen_fingerprint.go`
> - 规格的回归测试 → `internal/lane/zen_fingerprint_test.go`
>
> 文档里的"实测（2026-10-19）"标注的是**当时**的观测，
> 用来解释"为什么这么写"，不是"现在还是这样"的承诺。

---

## 0. 一句话定位

> **Free Model House（FMH）= NAS 上跑的一个"模型水电煤"网关。**
> 上游接三条免费车道（OpenCode Zen 免密车道 + Kilo 免 key 池 + OpenRouter `:free` 模型），
> 下游只暴露一套标准 OpenAI/Anthropic 兼容 API，
> 局域网里的任何 Agent 用 **NAS 的 IP + 端口 + 一把本地生成的 Key** 接入，
> 不需要、也不接触任何上游凭据。

---

## 1. FMH 的需求定义

### 2.1 功能需求

| 编号 | 需求 |
|---|---|
| F1 | Docker 部署在 NAS（x86_64 / arm64），`docker compose up` 即可用 |
| F2 | Web 管理界面（浏览器访问），管理上游源、模型清单、限流策略、Key、用量 |
| F3 | 上游源 A：**OpenCode Zen 免密车道**（无需任何凭据，靠客户端指纹过闸） |
| F4 | 上游源 B：**OpenRouter 免费模型**（`:free` 后缀 + `openrouter/free` 路由器，用用户的 OR Key） |
| F5 | 统一对外 API：OpenAI Chat Completions / OpenAI Responses / Anthropic Messages + `GET /v1/models` |
| F6 | **LAN 接入**：单端口 `:8678`，用 FMH 签发的 `fmh-…` Key 鉴权 |
| F7 | 上游凭据隔离：OpenRouter Key 只存在网关内，**永不**下发给 LAN 客户端 |
| F8 | 限流自动切换：某模型 429 → 自动换同车道下一个可用模型，响应头标注实际服务的模型 |
| F9 | 能力感知路由：带图/音频/文件的请求不落到纯文本模型上 |
| F10 | 思考档位（effort）映射：客户端的 reasoning 意图 → 上游实际能强制执行的参数 |
| F11 | 用量看板：按模型/客户端/日期统计 token、请求数、失败数、实际落点模型 |
| F12 | 单模型体检：可用性 + 首字延迟探测，结果持久化，重启后仍在 |
| F13 | 单一配置源：所有设置持久化在 `/data`，`docker restart` 不丢 |

### 2.2 非功能需求

| 编号 | 需求 | 理由 |
|---|---|---|
| N1 | 镜像 < 100MB | NAS 存储与拉取慢 |
| N2 | 冷启动 < 3s | NAS 用户随手用 |
| N3 | 常驻内存 < 200MB | 多用户共享 NAS |
| N4 | 单二进制或单进程 | NAS 用户不想管多容器 |
| N5 | 无 CDN 依赖 | 内网离线可用 |
| N6 | 零构建步骤 | `docker compose up` 就能跑 |

---

## 2. 技术选型：单一语言还是多语言交叉？

### 3.1 结论：**后端 Go 单语言 + 前端 TypeScript（编译期）**，运行时是一个 Go 进程 + 静态文件

这是**"双语言但非交叉编码"**：Go 独占后端全部逻辑，TS 只写前端并在构建期编译成静态资源。
**不存在** Go↔TS 之间的运行时协作（没有 gRPC、没有 WASM、没有 FFI、没有共享内存）。

### 3.2 为什么后端选 Go

对比两个原始项目：

| 维度 | FMH 需求 |
|---|---|
| 镜像体积 | `FROM scratch` + CGO off → **~15MB** | node:22-alpine → ~130MB | ✅ Go 完胜（N1） |
| 冷启动 | ~50ms | ~400ms | ✅ Go |
| 常驻内存 | ~30MB | ~120MB | ✅ Go（N3） |
| 交叉编译 NAS 架构 | ✅ `CGO_ENABLED=0 GOOS=linux GOARCH=arm64` 一条命令 | 需为每个架构构建 node_modules | ✅ Go |
| 并发流式代理 | goroutine + `io.Copy` 天然合适 | 事件循环也可，但 CPU 密集（JSON 重写）易成瓶颈 | ✅ Go |
| 单二进制无依赖 | ✅ | ❌ 需 node_modules 或打包器 | ✅ Go（N4） |
| 静态编译到 scratch | ✅ | ❌ | ✅ |

**决定性理由**：这是**要长期挂在 NAS 上、给全家多台 Agent 共用**的基础设施。
镜像小、冷启动快、跨架构零成本、内存占用低 —— 这四条直接决定 NAS 用户的实际体验。
FMH 选 Go 是因为**要跑在 NAS 上**。

### 3.3 为什么前端用 TypeScript

- 管理台用 React 19 + Vite 7 + Tailwind 4。
- 管理台是典型的**表单 + 表格 + 实时图表**，TS 的类型收益在这个场景最大。
- **构建期产物**：Vite 编译成静态文件，**打进 Go 的 `embed.FS`**，最终镜像里只有一个 Go 二进制 + 内嵌静态资源。
  - 用户不需要 Node.js；CI 只需要一次 `npm ci && vite build`。
  - 前端调后端一律走同源 `/api/*`（JSON over HTTP），**不与 Go 共享任何类型运行时**。

### 3.4 为什么**不**多语言交叉编码

明确排除以下方案，并说明理由：

| 备选 | 为什么不选 |
|---|---|
| Go 后端 + Rust 加速的 JSON/SSE 重写 | 收益极小。上游 SSE 解析是 IO-bound，Go 的 `bufio.Scanner` + `json.Decoder` 足够；引入 Rust 需要 bindgen/cgo，破坏"单二进制 + scratch" |
| Go 后端 + WASM 插件（跑第三方渠道适配器） | 过度设计。FMH 只需两个上游源，不需要插件生态 |
| Go 后端 + Python 脚本做"能力标注" | 改为**纯规则 + 实测**，不引入 Python 运行时 |
| 全 TypeScript（Node） | 镜像大 5-8×，冷启动慢 8×，NAS 拉取痛苦 |
| 全 Go（含服务端渲染 HTML） | 管理台交互复杂（实时看板、模型搜索、虚拟滚动），纯 Go 模板会写得很痛苦，因此走 React |

**判据**：多语言交叉只在"**运行时**需要两种语言互相调用"时才值得。
本项目**唯一**的跨语言边界是**构建期**（TS → 静态资源），这是零成本的。

### 3.5 依赖清单（刻意精简）

**后端（Go）**
- `net/http` —— HTTP 服务 + 路由（标准库 `ServeMux` 在 Go 1.22+ 已支持方法与路径参数，够用）
- `net/http/httputil` —— **反向代理**（LAN 中继用，见 §5.4）
- `encoding/json` —— 协议编解码
- `golang.org/x/net/http2` —— 上游 HTTP/2（免费车道对 HTTP/1.1 与 HTTP/2 无差别，但 HTTP/2 复用连接省 TLS 握手）
- `github.com/mattn/go-sqlite3` → 改用 **`modernc.org/sqlite`**（纯 Go，无 CGO！）—— 存用量、探测结果、限流账本
  - ⚠️ 不用 `mattn/go-sqlite3`：它需要 CGO，会破坏静态编译。
- 无 Web 框架。裸 `net/http` + `mux.HandleFunc`。

**前端**
- React 19 + TypeScript 5 + Vite 7 + Tailwind 4 + TanStack Query（数据获取）+ TanStack Virtual（模型长列表虚拟滚动）
- `lucide-react` 图标
- **零 CDN**：所有字体/图标/脚本本地打包

---

## 3. 总体架构

```
┌──────────────────────────────────────────────────────────────────────┐
│                         NAS 主机 (Docker)                            │
│                                                                       │
│  ┌─────────────────────────────────────────────────────────────────┐ │
│  │  free-model-house 容器  (单一 Go 二进制, 内嵌前端静态资源)      │ │
│  │                                                                  │ │
│  │  ┌──────────────┐   ┌──────────────┐   ┌────────────────────┐  │ │
│  │  │ :8678 管理面  │   │ :8678 推理面  │   │ :8678 Web UI      │  │ │
│  │  │ /api/*       │   │ /v1/*        │   │ /                 │  │ │
│  │  │ 会话 Cookie  │   │ fmh-* Key    │   │ (静态页)           │  │ │
│  │  │              │   │              │   │                    │  │ │
│  │  └───────┬──────┘   └──────┬───────┘   └─────────┬───────────┘  │ │
│  │          │                 │                      │              │ │
│  │          └─────────┬───────┴──────────────────────┘              │ │
│  │                    ▼                                             │ │
│  │         ┌─────────────────────┐   ┌──────────────────────────┐   │ │
│  │         │  Gateway Core      │   │  Registry / Catalog      │   │ │
│  │         │  ├ 鉴权 (Key 分层)  │   │  ├ 模型清单              │   │ │
│  │         │  ├ 规范化 (Canonical)│  │  ├ 能力元数据            │   │ │
│  │         │  ├ 路由 & 故障转移  │   │  ├ 探测结果持久化        │   │ │
│  │         │  └ 用量计量        │   │  └ 限流账本              │   │ │
│  │         └──────────┬──────────┘   └──────────────────────────┘   │ │
│  │                    │                                             │ │
│  │         ┌──────────▼──────────┐   ┌──────────────────────────┐   │ │
│  │         │   Lane 层 (源适配器) │   │  SQLite (纯 Go, 挂 /data) │   │ │
│  │         │  ┌────────────────┐  │   └──────────────────────────┘   │ │
│  │         │  │ OpenCode Zen  │  │  ← 免密：指纹 + 铸造会话       │ │
│  │         │  │  (免密车道)     │  │                                  │ │
│  │         │  ├────────────────┤  │   ┌──────────────────────────┐   │ │
│  │         │  │ OpenRouter    │  │   │  Web UI (React, 内嵌)     │   │ │
│  │         │  │ (:free + 路由) │  │   └──────────────────────────┘   │ │
│  │         │  └────────────────┘  │                                  │ │
│  │         └──────────┬──────────┘                                  │ │
│  └────────────────────┼─────────────────────────────────────────────┘ │
└───────────────────────┼──────────────────────────────────────────────┘
                        │ HTTPS (出网)
        ┌───────────────┴────────────────┐
        ▼                                ▼
┌────────────────────┐        ┌──────────────────────┐
│  opencode.ai       │        │  openrouter.ai       │
│  /inference/v1/*   │        │  /api/v1/*           │
│  (免密, 靠指纹)     │        │  (用户的 OR Key)      │
└────────────────────┘        └──────────────────────┘

局域网 Agent (Claude Code / OpenCode / Cline / 自研 agent …)
        │  POST http://192.168.31.5:8678/v1/chat/completions
        │  Authorization: Bearer fmh-xxxxxxxx
        ▼
   ✅ 不需要 OpenRouter Key，不需要 OpenCode 账号
```

---

## 4. 核心机制设计

### 5.1 统一内部事件模型（Canonical Event Bus）

**这是整个网关的中枢。** 所有上游、所有入站协议都收敛到一种事件：

```go
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

type Chunk struct {
    Kind      ChunkKind
    Index     int      // 块序号（并行工具调用靠它区分）
    BlockType string   // text | reasoning | tool-call
    Text      string
    Delta     string
    ID        string   // tool call id
    Name      string   // tool name
    Args      string   // 工具参数增量
    Usage     *Usage
    Finish    string   // stop | tool-calls | max-tokens | aborted
}
```

统一消息模型（一条消息可以有多段内容）：

```go
type Message struct {
    Role  Role    // system | user | assistant | tool
    Parts []Part  // TextPart | ImagePart | AudioPart | FilePart | ToolCallPart | ToolResultPart
}
```

**三个 Encoder** 把 Chunk 流投影成三种线上协议：

| Encoder | 输出 | 目标客户端 |
|---|---|---|
| `ChatEncoder` | `chat.completion.chunk` | Cline / Aider / OpenAI SDK / 绝大多数工具 |
| `ResponsesEncoder` | `response.output_text.delta` 等 | OpenAI Codex / 新 SDK |
| `MessagesEncoder` | `content_block_delta` / `message_start` … | **Claude Code**（Anthropic Messages） |

**为什么必须有三线**：Claude Code 只会说 Anthropic Messages；Codex 只会说 Responses；
普通 agent 只会说 Chat Completions。一个网关只支持一种，就有一半用户接不进来。

**"工具调用块"的关键细节**（踩过的坑）：
`tool_calls[].index` 是线上协议的累加器 key；**没有 index 的**（Claude 线常见）
必须用 `call_id` 区分，否则并行工具调用的 id 会互相覆盖，被合并成一个块。

**Encoder 层的两个额外职责**：

1. **压制占位工具**：注入的 `read`/`shell` 绝不能出现在给客户端的事件里。
   做法是在 **Encoder 层**压制（而不是在上游层）——
   把这些块整个吞掉，`tool_calls` 的 index 从 0 重新编号。
2. **思考内容的双写**：Chat 线上同时写 `reasoning` 和 `reasoning_content`。

**Encoder 层还要处理"降级到另一个模型"**：故障转移发生时，
同一个 SSE 流要能继续 —— 下游已经收到的块保持不变，
只是后续的块来自另一个模型。这要求 Encoder 持有"已提交状态"，
而不是无状态的纯函数。

### 5.2 Lane 层：源适配器

每个上游源实现同一个接口：

```go
type Lane interface {
    // ID 形如 "zen" / "openrouter"
    ID() string
    // ListModels 返回该源当前的可用模型 + 能力元数据
    ListModels(ctx context.Context) ([]ModelInfo, error)
    // Do 发起一次推理，产出规范化 Chunk 流
    Do(ctx context.Context, req *Turn) (*Usage, string, *UpstreamError, error)
    // Classify 把上游错误映射到统一错误码
    Classify(status int, body string, retryAfter int) *UpstreamError
}
```

#### 5.2.1 OpenCode Zen Lane（免密）

这是**最难也最核心**的一环。必须精确复刻 §1.1 ① 的四维闸门。

**a) 指纹头构造**（按 §1.1 ① 的规格实现）

```go
const ClientUA = "opencode/latest/2.0.26/cli"

func ZenHeaders(session, requestID string, stream bool) map[string]string {
    accept := "*/*"
    if stream { accept = "text/event-stream" }
    return map[string]string{
        "content-type":          "application/json",
        "authorization":         "Bearer public",  // 共享凭据，非用户 key
        "user-agent":            ClientUA,         // 整串匹配，前后缀都不行
        "x-opencode-client":     "cli",
        "x-opencode-session":    session,
        "x-opencode-session-id": session,
        "x-session-affinity":    session,
        "x-session-id":          session,
        "x-opencode-project":    "global",
        "accept":                accept,
        // 注意：绝不发 x-opencode-request（v1 专用头，v2 收到即 403）
    }
}
```

**b) 会话 ID 铸造（降序编码）**

关键点：**不是 sha256，不是随机，必须是能被上游解码出"新鲜时间戳"的降序编码。**

```go
// MintSessionId 生成上游接受的会话 ID：
//   ses_ + 12 位 hex(降序时间戳) + 14 位 base62(随机)
// 降序编码 = ~(ts_ms << 12 | counter)，低 12 位落在 0xfc0-0xfff（counter 1..63）
func MintSessionId(nowMS int64, counter uint16) string {
    value := ^(uint64(nowMS)<<12 | uint64(counter&0xFFF))
    var raw [6]byte
    for i := 0; i < 6; i++ { raw[i] = byte((value >> (40 - 8*i)) & 0xFF) }
    rnd := make([]byte, 14); rand.Read(rnd)
    return "ses_" + hex.EncodeToString(raw[:]) + base62From(rnd)
}
```

**会话稳定性**：一个下游会话 → 一个上游 session，持久化到 SQLite。
`seed` 取 `client_id + 首条用户消息前 200 字符`。
**过期重铸**：上游容忍约一周的新鲜度窗口，所以 6 天后重铸一次。

**c) 工具指纹注入 + 别名还原**

```go
// ZenFingerprintTools 是闸门要求的两个工具名（大小写精确匹配）。
// ⚠️ 名字是关键 —— 改名就 403。
//   description 用官方原文（保守，实测上游当前不比对）。
var ZenFingerprintTools = []string{"read", "shell"}
```

处理流程：

```
1. 遍历调用方 tools：
   - 名字不是 read/shell（小写精确匹配）→ 原样保留
   - 名字是 read/shell → 改名为 <name>_client[N]，记入 renameMap
   - 同名重复声明 → 只留第一个（上游按重复拒）
2. 注入缺失的 read/shell 官方定义
3. 同步改写：历史消息里的 tool_call 名、tool_choice 指向的名字
4. 追加系统提示："Do not call the `read` or `shell` tools; they are placeholders..."
   （v2 注入的是真定义，不提示模型就会真去调）
5. 兜底：删除 tool_choice == "none"（那会让模型这一轮不调工具）
6. 响应侧：用 renameMap 把名字还原成调用方的
```

**大小写变体（`Read` / `Bash`）不算闸门工具，原样透传** —— 上游接受它们与注入项并存。
（按小写归并会把调用方的 `Read` 改成 `read`，正好撞上"闸门只认小写精确名"的判定。）

**d) 端点选择**

| 模型 | 端点 | 说明 |
|---|---|---|
| 绝大多数 | `/inference/v1/chat/completions` | 默认 |
| `muse-spark-*` | `/inference/v1/responses` | Responses 线 |
| `union-alpha` | `/inference/v1/messages` | Anthropic 线 |

> ⚠️ 旧的 `/zen/v1/*` 对免费 id 一律 403。

**e) 上线前必须跑"指纹回归测试"**

这是本项目**最不能省**的部分。FMH 要有一套 Go 测试：

```
TestFingerprintToolDefinitions   官方定义逐字不变
TestFingerprintAliasing          冲突工具改名 + 历史同步 + 响应还原
TestSessionIDEncoding            降序编码 + counter 在 1..63 窗口
TestGatewayHeaders               四个会话头同值、UA 整串、无 x-opencode-request
TestToolChoiceNeverNone          任何路径都不会发出 tool_choice:"none"
TestEndpointSelection            三种 wire 的端点映射
```

### 5.2.2 OpenRouter Lane（`:free`）

比 Zen 简单得多：**标准 OpenAI 兼容**，唯一的特殊性是：

1. **鉴权**：`Authorization: Bearer <OR Key>`（用户自己在管理台填）。FMH 存它，**绝不下发**。
2. **免费模型识别**：
   - 模型 id 以 `:free` 结尾（如 `deepseek/deepseek-r1:free`）
   - 或使用 OpenRouter 的免费路由器 `openrouter/free`
   - 或上游在 `/models` 里标 `pricing.prompt == "0"` 且 `pricing.completion == "0"`
   > 三条判据都要用 —— 只靠 `:free` 后缀会漏掉新上架的免费模型。
3. **能力元数据**：OpenRouter 的 `/api/v1/models` 带有 `context_length`、`top_provider.max_completion_tokens`、
   `architecture.input_modalities`、`supported_parameters`（含 `reasoning`）。
   这些直接喂给 §5.4 的能力路由。
4. **限流**：OpenRouter 免费档是按 key 限速（`200 req/min`、`50/day` 视账户而定），
   429 时 `Retry-After` 明确 —— 冷却直接用 `Retry-After`，不走 10 秒默认。

**为什么 OpenRouter 也做成 Lane 而不是特例**：它和 Zen 共享模型清单、能力元数据、
限流账本、故障转移链。**做成特例就会有两套路由、两套限流、两套看板。**

### 5.3 规范化层（Canonicalizer）

把三种入站协议（Chat / Responses / Messages）统一成 `[]Message + []ToolDef`：

```go
type Turn struct {
    Model      string
    Messages   []Message
    Tools      []ToolDef
    MaxTokens  int
    Effort     string   // light | balanced | deep | (模型声明的档位)
    Stream     bool
    ClientID   string   // 从 Key 解析出的客户端标识
    Seed       string   // 会话种子（用于上游 session 绑定）
}
```

**翻译是"轮辐"而不是"链条"**：
两种入站方言（OpenAI Chat、OpenAI Responses、Anthropic Messages）**各自**解析进
统一形式；车道再把统一形式**投影**到该模型需要的上游 wire。
**不是** Chat → Responses → Messages 的链式转换 ——
链式转换每一步都会丢信息（Anthropic 的 `tool_result` 到 Chat 会变形）。

#### 5.3.1 规范化时的三个坑

**① 系统提示要合并**

Chat 线的 `messages[0].role=="system"`、Responses 线的 `instructions`、
Messages 线的 `system` 三个位置都要收进 `Message{Role:system}`。
指纹提示也注入到这里。

**② 工具结果配对必须在每次发送前修复**

Anthropic 线的 `tool_result` 必须与前一轮的 `tool_use` 有对应 ID。
**孤立的 tool_result 和悬空的 tool_call 都要删掉**：

```
悬空的 tool_call（模型调了但没结果）→ 删
孤立的 tool_result（结果没有对应调用）→ 删
名字为空的 tool_call → 删
```

**为什么必须修 —— 这是一个"毒丸"机制**：

> 一个重放的、没有匹配的工具调用会被上游 400 拒绝，
> 而这个 400 会**污染整个会话** —— 之后**每一轮**都失败。
>
> 因为它留在**持久化的历史**里。不删掉它，这个会话就废了。

名字为空的 tool_call 更毒：它到达宿主时是 `Error: unknown tool ""` ——
一个**已经被回答过**的毒丸（模型已经回复了），配对修复会把它**永远留在历史里**。
（这是已知的上游行为）

**③ `max_tokens` 必填**

Anthropic 线不填会 400。OpenAI 线不填则由上游决定 —— 但对免费车道，
**不填等于把档位控制权交出去**。FMH 应该总是显式给一个值
（由档位推导，见 §5.8）。

#### 5.3.2 工具定义的归一化

一个工具定义在三种 wire 上有三种形状，但**只有一个**归一化形式：

```go
type ToolDef struct {
    Name        string
    Description string
    Parameters  map[string]any   // JSON Schema
}
```

出站时按 wire 渲染：

| wire | 形状 |
|---|---|
| chat | `{"type":"function","function":{name,description,parameters}}` |
| responses | `{"type":"function",name,description,parameters,"strict":false}` |
| messages | `{name,description,input_schema}` |

#### 5.3.3 出站渲染的额外约束

- **Claude 线**：`system` 是独立顶层字段；连续同角色回合要合并；
  `tool_result` 要领起它所在的 user 回合；只保留 PDF，其他媒体丢弃。
- **Chat 线**：`stream_options: {include_usage: true}` 才会带回 usage。
- **Responses 线**：`instructions` 是系统提示；`input` 是扁平数组。

### 5.4 路由与故障转移

#### 5.4.1 候选链构造

```
1. 请求的模型 ID → 解析出 Lane + 上游模型 ID
   - "zen/deepseek-v4.1-flash-free"  → Lane=zen
   - "openrouter/deepseek/deepseek-r1:free" → Lane=openrouter
   - 无前缀 → 按模型清单里的归属猜测；猜不到 → 默认 zen
2. 能力过滤（硬约束）：
   请求含 image → 候选必须 Vision==true
   请求含 audio → 候选必须 AudioInput==true
   请求含 file   → 候选必须 FileInput==true
   ⚠️ "未知能力"视为通过但排在已知能力之后（软过滤）
3. 上下文长度过滤（软）：
   estimated_prompt_tokens > applyHeadroom(context_window) → 跳过（除非没有别的候选）
4. 排序（candidateKey 思路）：
   可用性(探测状态) > 已知延迟(TTFT) > 目录顺序
5. 请求的模型永远排第一 —— 故障转移只在它"拒绝且尚未吐字"时才启用
```

**`applyHeadroom` — 实测容量是下界，不是上界**

探测是用"逐步加大直到被拒"的方式做的，所以真实上限落在
"最后一次成功的尺寸"与"首次被拒的尺寸"之间。记下来的数字是**下界**。
所以路由过滤要用 `context_window × 1.5`：

```go
// applyHeadroom 把实测容量放宽 1.5 倍。
// ⚠️ 0 表示"没测过" —— 此时不要发明数字，
//   让未知模型的默认值（131072/32768）继续负责。
func applyHeadroom(n int) int {
    if n <= 0 { return 0 }
    return n * 3 / 2
}
```

**理由**：上游才是裁判。候选被静默丢弃的代价是**整个请求失败**；
让一个略微超标的 prompt 真的发出去，让上游来判，代价只是一次 400。

#### 5.4.2 故障转移循环

```go
for _, cand := range candidates {
    usage, finish, uerr, err := lane.Do(ctx, turn, cand)
    if err == nil { return ... }
    if !uerr.Switchable {           // CLIENT_ERROR / INVALID_CREDENTIAL → 不换
        return ..., uerr
    }
    if uerr.Code == CodeQuota {
        throttle.MarkThrottled(cand.Model, uerr.RetryAfter)  // 短冷却
        continue                    // 换下一个（绝不重试同一模型）
    }
    if uerr.Code == CodeRegion || uerr.Code == CodeFreeTier {
        // 地域/闸门：Zen 车道整体不可用 → 直接跳到 OpenRouter 车道
        continue
    }
    // TRANSPORT / TIMEOUT / SERVER → 换下一个
}
```

**三条铁律**（每条都对应一类真实事故）：

| 规则 | 理由 |
|---|---|
| **429 绝不重试同一模型** | 配额按 session 记账，重试 = 再烧一次。**换模型是唯一诚实的杠杆** |
| **一旦已向客户端吐过内容，绝不换模型** | 换了会内容重复/拼接错乱 |
| **请求自身的错（4xx）绝不换** | 重放同样的 body 只会得到同样的拒绝，换模型是浪费 |

**按失败类别分配重试预算**：
一个全局预算会让重试**静默退化成只试一次** —— 93 秒后报 502，而健康的出口闲置着。

```
配额类拒绝（快）：  总预算 40s
慢故障（传输/5xx）： 总预算 210s   （实测每次尝试 30-90s）
```

#### 5.4.3 自动跨模型调用

Zen 车道全灭 → 落到 OpenRouter 的 `:free` 模型（如果用户配了 OR Key 且开启了回落）。
**默认关闭**（会消耗用户自己的 OR 额度），可在管理台打开。**单向**（不会反向回落）。

#### 5.4.4 响应头标注实际模型

这个做法对调试极有价值：

```
X-FMH-Model:      deepseek-v4.1-flash-free      ← 实际服务的模型
X-FMH-Lane:       zen
X-FMH-Fallback:   true                          ← 是否发生了故障转移
X-FMH-Request-Id: ...
```

> `X-FMH-Model` 是**排查问题的第一手线索**：
> "我请求的 deepseek，为什么账单上是 glm？" —— 看这个头。

### 5.5 Key 分层模型（**用户需求的核心**）

这是 FMH 最重要的产品设计。**三把钥匙，永不相交：**

| Key | 格式 | 作用域 | 谁能拿到 | 谁能用 |
|---|---|---|---|---|
| **管理会话** | HttpOnly Cookie（8h） | 管理面 `/api/*` | 只有 NAS 本机浏览器 | 改设置、看用量、管 Key |
| **本机推理 Key** | `fmh-<40 hex>` | 推理面 | NAS 本机 | 全部推理接口 + **能换管理会话** |
| **LAN 推理 Key** | `fmh-<40 hex>` | 推理面 | **局域网 Agent 用户** | 只开推理面 |

⚠️ **两把推理 Key 共用同一个前缀**，但它们是**两个独立的值**。
   隔离靠"独立生成 / 独立轮换 / 启动时校验字面不同"，
   不靠前缀 —— 详见 `02-key-model.md` §2.1。

**用户侧体验**：

```bash
# 局域网内任意机器
export OPENAI_BASE_URL=http://192.168.31.5:8678/v1
export OPENAI_API_KEY=fmh-xxxxxxxxxxxxxxxx

# Claude Code
export ANTHROPIC_BASE_URL=http://192.168.31.5:8678/v1
export ANTHROPIC_AUTH_TOKEN=fmh-xxxxxxxxxxxxxxxx
```

**上游凭据的处理**（关键）：

- OpenRouter Key 存在 FMH 的 SQLite（`secrets` 表）里，**只在本机解密使用**。
- 写进 `settings.json` / SQLite 时用 AES-GCM 加密（密钥来自环境变量 `FMH_SECRET_KEY`，
  首次启动生成并写入 `/data/.secret-key`，`chmod 0600`）。
- **LAN 客户端永远拿不到它**：`/v1/models` 与所有推理响应里都不回显上游 Key。
- 管理台的"上游凭据"输入框**只显示指纹**（`…abcd`），永不回显全文。
  （这个原则必须写进代码注释）

**Key 轮换**：

- 轮换推理 Key → 旧 Key 立即失效；管理会话与上游凭据不受影响。
- 修改管理密码 → 所有管理会话作废；推理 Key 与 agent 配置完全不受影响。
- 轮换 OpenRouter Key → 只影响 OpenRouter 车道。

**访问控制**：

- ⚠️ **单端口** —— 推理面和管理面共用 `:8678`。
  隔离**不靠端口**，靠**鉴权**：
  - `/api/*` 必须要**管理会话 Cookie**（密码登录换来的）
  - `/v1/*` 必须要 **Key**
  - 两者**互不通用** —— 推理 Key 打 `/api/*` 会 401
- 「局域网推理」开关**默认开启**。关掉后**非本机来源**的推理请求返回 403；
  本机来源不受影响。
  - 判据是**请求来源**，不是端口 —— 因为只有一个端口。
- 管理面**同源严格**（不开放 CORS），会话 Cookie 8 小时过期。
- 想要更强的隔离（管理面只绑 loopback）：
  设 `FMH_ADDR=127.0.0.1:8678`，然后用 NAS 的反向代理只暴露推理路径，
  或者走 SSH 隧道 `ssh -L 8678:127.0.0.1:8678 user@nas`。

**可选增强（v1.1）**：LAN Key 支持"绑定客户端 IP"与"只读/禁用工具"两种策略。
不在 v1.0，但接口先留好。

### 5.6 限流与冷却

```go
type ThrottleNote struct {
    ThrottledAt   int64
    CooldownUntil int64
    LastOK        int64
    Episodes      []ThrottleEpisode  // 最近的配额耗尽窗口
}
```

**冷却时长策略**（关键设计点）：

| 上游 | 默认冷却 | 理由 |
|---|---|---|
| Zen 免密车道 | **10 秒** | 共享凭据 `Bearer public`，429 = 此刻拥挤。实测最短只持续 0.6s，冷却 60s 会让面板误报"不可用" |
| OpenRouter | 用 `Retry-After` | 免费档的 429 明确带重试时间（可能几小时） |
| 有 `Retry-After` | 用 `Retry-After`，上限 2 小时 | |

**冷却到期自动复活**（LiteLLM 风格，不用黑名单）：冷却结束即回到候选池，
由下一次真实请求或探测来确认它是否真的恢复。
另外保留一个"复活账本"：某模型通过某出口成功过一次，就清掉它在该出口的失败记录
（真实流量比任何探测样本都硬）。

### 5.7 能力元数据与探测

#### 5.7.1 四级信任序

（`modalityCapsOf` 的做法）

```
实测 (live probe)  >  上游声明 (provider listing)  >  规则/名称启发式  >  未知
```

**注意不对称性**：视觉能力**只能升级不能降级**。
一次 AI 标注或规则猜测说"这个模型不支持视觉"，不应该把一次实测确认的视觉能力抹掉。

**能力表是地板，不是天花板**：一张规则表（正则匹配模型名）提供基线，
实测与上游声明在其上叠加。未匹配任何规则的模型**什么都不声称** ——
猜一个 `reasoning: true` 会显示一个假的"思考"徽章。

**容量只填不改**：`openref` 从公开目录（OpenRouter 的 `/api/v1/models`、
`models.dev`）拿容量时，写的是 **CapsOnly** 行 —— 只有容量，没有任何模态声明。
而且**只在别的来源都没说话时**才写。

**同名歧义要埋掉而不是猜**：两个厂商的行归一化后撞到同一个基础 id 时，
**删掉这个键并永久埋掉**（第三个行不许把它复活）。

#### 5.7.2 探测（probe）

- 对每个模型发一个**最小**请求（`messages:[{role:user,content:"ping"}]`）。
- 记录：可用性状态 + **首字延迟 (TTFT)** + 探测时间。
- **首字延迟是选路的关键信号**：一个 30 秒才出字的模型，配上 200s 超时的 agent 会直接失败。

⚠️ **`max_tokens` 不能太小（实测踩过的坑）**：

原来用 `max_tokens:16`。实测某个模型把 16 个 token **全烧在 reasoning 上**，
一个可见 token 都没吐，于是撞上 `max_tokens` 截断（`finish = max-tokens`）。

后果有三个，都很严重：

1. 探测**测不到"能不能出答案"** —— 它只测到"能不能开始思考"；
2. `finish=max-tokens` 和"上游拒绝"在结果上混在一起；
3. 首字延迟被算成整个请求的耗时（实测 14~40 秒），
   把这个模型在排序里压到最后。

现在用 `max_tokens:256`（实测甜点：reasoning 11~17 + 输出 14~26，
够模型先思考再吐答案，又不像真实请求那样烧配额）。

**只有"指名了模型"的拒绝才算在模型头上**：

| 情况 | 结论 | 理由 |
|---|---|---|
| 地域封锁 | `region-blocked` | 明确是地域问题 |
| 配额 | `throttled` | 明确是配额 |
| 闸门锁定 | `unavailable` | 明确 |
| 400 / 404 / 422 | `unavailable` | **路由拒绝** —— 明确点名了这个模型 |
| 消息里点名了模型（"no such model"） | `unavailable` | 除非状态码 ≥ 500 |
| 5xx | **`unknown`** | **不是模型的判决** |
| 配额 / 传输故障 | **`unknown`** | 不是模型的判决 |

**为什么 5xx 不能算数**：`Service Unavailable` 是**反向代理的理由短语**。
按它匹配会把一个**正在工作**的模型从选择器里摘掉。

**这个不对称是刻意的**：

> 一个过期的选择器条目的代价 = 一次可重试的失败回合。
> 一个因为抖动而消失的模型的代价 = 用户要经历一整轮看不见的重新探测。
>
> 前者便宜得多。

**流内拒绝**（200 但帧里带 `error`）也要解析：
`model is unavailable|not supported|invalid model` → 不可用；
`not available in your country` → 地域封锁。

**判据有时效**（`stateTTL = 10 分钟`）：
过期后读作 `unknown` —— 模型**仍然留在清单里、仍然可路由**，
直到一次新探测来续上这个断言。**一次坏样本不该让模型藏一整轮。**

##### 5.7.2.1 出口是网络路径的属性（不是模型）

⚠️ **这是整个探测设计里最容易搞错的一点。**

免密车道的可用性是**出口**的属性。上游的原话：

> "OpenCode's free models aren't available from your current network because
> we've **detected abusive traffic from this IP address**. This isn't related
> to your account. If you're on a VPN or proxy, turn it off and try again"

所以：

| 事实 | 推论 |
|---|---|
| 配额按**出口 IP** 记账，以**秒级**波动 | 上游的一次拒绝只是"这个出口在这一刻"的快照，**不是模型的属性** |
| 地区的封锁也是出口的属性 | "地区受限"和"出口被风控"改出口才有用，改模型没用 |
| 用户开了 VPN 就换了一个出口 | **能用的模型集合跟着出口变** —— 面板必须能显示当前出口 |

⚠️ **逐个模型标"出口被风控"是误导** —— 用户会去逐个换模型，
而真正要换的是出口。所以有两条：

**① 出口检测**（`internal/lane/egress.go`）

三个 IP 回显服务（ipify / ipinfo / ipapi）**互为备份**，
第一个答就算。检测**失败不报错** —— 拿不到出口信息绝不能影响网关可用性。

⚠️ **出口变了要立刻重探** —— 因为旧的结论是**旧出口**的结论。

**② 车道级封锁**（`Catalog.LaneLockOf`）

当**所有**模型都被**同一种理由**拒绝时，正确的结论是
"**这个出口用不了整条车道**"，而不是"这 N 个模型分别坏了"。

判据（三条都要满足）：

1. 这一轮**探过至少 3 个**模型 —— 只探了 1 个不能下"整条被锁"的结论；
2. **全部**被拒（没有一个可用）；
3. 理由**一致** —— 全是"出口被风控"，或全是"地区受限"。

⚠️ **限流不算车道被锁** —— 限流是**暂时**的，而且是全局配额，
不是"这个出口不行"。

##### 5.7.2.2 真实流量与探测对质

```
真实请求成功 → 留下一份带时间戳的证据
探测写负面结论时 → 先看有没有「比结论更新的」成功证据
                 有 → 负面结论降级成 unknown（不是 available）
```

⚠️ **为什么降级成 `unknown` 而不是 `available`**：

- 证据只说"**刚才**是通的"，不说明"**现在**是通的"；
- `available` 的语义是"探测验证过可用" —— 真实流量没资格写这个；
- `unknown` 的语义正好是"没有可靠结论"，
  而且它**不会**把模型从路由里摘掉（排序上排在 available 之后、负面之前）。

⚠️ **证据要比结论**新**才作数** —— 探测结论的时间戳是**探测开始**的时间
（一次探测要跑十几秒），所以"探测进行中真的通了"这种证据**必须**能推翻它。
探测之前就有的成功证明不了探测那一刻的情况。

⚠️ **证据会过期**（10 分钟）—— 否则一次成功就永久有效，
模型真坏掉了也发现不了。

**限流结论延迟复核**：

限流是**暂时**的，所以负面结论**不要立刻**再问 ——
立刻再问，问的还是**同一个出口的同一刻**，问十次也是十次 429。
等 10 秒（= 限流冷却窗口）再问一次，那一刻的成功才说明
"模型能用，只是刚才撞墙了"。

⚠️ 只对**限流**复核 —— 地区封锁 / 出口风控 / 明确不可用
**换出口也没用**（或者根本不是出口问题），再问一次只是白烧额度。

**探测的三个工程约束**：

1. **逐个探测**（不是并发打一轮）—— 面板上模型一个一个亮起来。
   并且**一轮必须覆盖全部模型** —— 半轮会让结论过时。
2. **全被限流时进入指数退避**（30 分钟起，每轮 ×2，上限 120 分钟）——
   **配额墙不能被探测撞穿**。用户自己的请求不受影响。
3. **探测消耗上游额度** → 默认**关**；并发上限默认 2（因为"这条车道按 session
   计配额，429 带**越来越长**的 retry-after"）；与真实流量用**分离的令牌桶**限速。

**全被拒时全部保留**：如果一轮下来**所有**模型都是 `unavailable`，
那**全部保持可见** —— 这几乎肯定是上游出问题了，不是模型集体消失。

**TTFT 平滑**：用最近 N 次的**中位数**（不是平均）。
免费车队的排队尖峰会把平均值毁掉 —— 一个 1.3 秒的模型平均后能变成 19.7 秒。

#### 5.7.3 清单刷新：只增不减

**铁律：一次失败的清单拉取绝不能让清单变小。**

```
1. 旧清单按车道切分
2. 每个源只替换自己那一片，且只在拉取成功且非空时替换
3. 全都空了 → 整轮是 no-op（保留旧清单）
```

**为什么这条这么重要**：模型清单是**下游一切**的输入 ——
`/v1/models` 返回它、路由从它挑候选、能力过滤按它判。
清单一缩水，用户看到模型凭空消失。

**冷启动重试**：失败的拉取**只在该源一条缓存都没有时**额外给一次机会（3 秒后）。
运行期的一次抖动不花钱（缓存还在），冷启动的抖动会让这个源消失半小时。

### 5.8 思考档位（Effort）映射

这是**极易做错**的一环。实测结论：
免费车道**接受但忽略** `reasoning_effort` / `thinking.budget_tokens` / `enable_thinking`，
**唯一真正生效的是 `max_tokens`**，而推理 token 量直接跟随它。

所以 FMH 的 effort 语义是**"输出预算"**，不是"提示词里的建议"：

| 档位 | 含义 | 实现 |
|---|---|---|
| `light` | 精简 | `max_tokens = 2048` |
| `balanced` | 均衡（默认） | `max_tokens = 8192` |
| `deep` | 深思 | `max_tokens = 模型的 maxOutput` |

**预算的优先级**（`BudgetFor`）：

```
1. 档位的天花板   —— 永远生效
2. 模型容量（×1.5 headroom）—— 作为 deep 档的天花板
3. 客户端显式的 max_tokens —— 压过上面两者
4. 配置的兜底值   —— 只约束"未知能力"的模型
```

**第 4 条是个陷阱**：曾经兜底值会从上方压住**所有**模型，
于是一个实测 256K 的模型只答了 32K。兜底值只该管未知模型。

**档位解析顺序**：

```
模型名后缀 "(light|balanced|deep)"  >  客户端的 reasoning_effort  >  配置的默认值
```

Anthropic 的 `thinking.budget_tokens` 要**分桶**到同样这三档。

**"关掉思考"的语义**：这条车道**做不到**真的关掉思考。
所以 `none` / `off` / `disabled` / `minimal` 都要映射到 `light`（最短思考），
**不能**映射到一个不存在的"无思考"档 —— 那样会骗用户。

**强制思考的模型要加倍**：思考 token 与可见答案**共享同一个输出上限**。
实测某个模型 82% 的输出 token 是推理 —— 那 8192 的上限只剩 ~1500 给答案，
长回合约每三次就 `length` 截断。所以对"不可关闭思考"的模型，整档上限 ×2。
（`ALWAYS_THINKING_FACTOR = 2`）

**OpenRouter 车道不同**：它用的是 OpenRouter 统一的 `reasoning: { effort: "..." }` 对象，
是真档位。可以各自声明自己的 effort 菜单，在模型清单里分开呈现。

### 5.9 用量与看板

**计量点**（一个请求要记 4 类数据）：

```
requests        总请求数
user_turns     用户回合数（不含自动重试/续写）
physical_reqs   物理上游请求数（含重试、续写、故障转移）
tokens_in/out   按实际服务的模型计
failures        按错误码计
recoveries      断流续写/截断续写救回来的次数
```

`physical_reqs` 与 `user_turns` 分开是必须的：没有这个区分，
"断流续写 3 次"会被算成 3 个请求，用量看板会骗人。

**两级日志**：

- **access** —— 每个**客户端请求**一行（响应完成时写）
- **upstream** —— 每个**物理尝试**一行（包括客户端从未看到的故障转移）

**存储**：SQLite（`modernc.org/sqlite`，纯 Go）。按天聚合 + 按模型明细。
**只存元数据，不存提示词与回答**（隐私）。

**请求 ID 双向对齐**：
接受客户端自带的 `X-Request-Id` / `X-Client-Request-Id`（这样客户端日志与网关日志能对上），
没有就自己铸一个，并在任何 handler 写响应之前就回显出去。

---

### 5.10 断流保护（三重兜底）

免费上游的质量参差，流的完整性是硬需求。

#### 5.10.1 "提交点"规则：什么算"已经回答了"

这是流式代理**最容易做错**的一处。故障转移的前提是"还没吐字"，
但**什么算"字"**需要精确：

| 事件 | 算不算提交？ |
|---|---|
| `text-delta`（可见文本） | ✅ **算** |
| `tool-call-delta`（工具调用） | ✅ **算** |
| `reasoning-delta`（思考） | ❌ **不算** |

**为什么思考不算**：这是"想了整整一分钟，然后撞上配额墙"——
**这恰恰是最值得故障转移的情形**。如果思考 delta 算提交，
模型就会在一个根本不会给出答案的回合上死磕。

**实现**：Encoder 持有一个 `committed bool`。
在 `committed == false` 期间，输出被**暂存**（不是丢弃）；
一旦出现第一个"答案承载"的 delta，就 `commit()` ——
暂存的内容一次性刷出，HTTP 200 才真正提交。

**在提交之前发生的上游失败，可以作为真正的 HTTP 状态码返回**
（429 + `Retry-After`、502 + 分类错误）。
一旦提交，失败只能以流内错误事件的形式传递。

> 这也解释了为什么 `deferStreamResponse` 那种"延迟写头"的技巧是必需的：
> **不延迟写头，200 一开始就发出去了**，后面再想返回 429 也来不及。

> ⚠️ **实现时最容易踩的坑（真的踩过）**：
>
> 光把"提交点"的判断写对**不够**。
> `encoder.Write(chunk)` 会**立刻**写 SSE 帧，而写帧会触发 `WriteHeader(200)`。
> 所以即使你的判断是"`reasoning-delta` 不算提交"，
> 只要你**先调 `encoder.Write` 再判断**，思考内容照样会抢先提交。
>
> 正确顺序：
>
> ```go
> if !committed && c.Kind == ChunkReasonDelta {
>     pending = append(pending, c)   // 缓冲，不写
>     return
> }
> if !committed {
>     committed = true
>     writer.commit()                // 现在才写头
>     for _, p := range pending { encoder.Write(p) }  // 先倒出思考
>     pending = nil
> }
> encoder.Write(c)
> ```
>
> 缓冲**不能变成丢弃** —— 客户端（尤其 Claude Code）依赖思考内容。
> 顺序也要保持：思考在前，正文在后。

#### 5.10.2 空闲检测（不是总时长限制）

**关键区别**：上游流可能合法地跑很久。限制**总时长**会砍掉健康的长回答。
要限制的是**沉默**。

```go
// streamIdleTimeout 是"多久没有任何新字节"的阈值。
// 每次读到 chunk 就重置 —— 它限制的是沉默，不是长度。
const streamIdleTimeout = 90 * time.Second
```

**为什么是 90 秒**：实测健康的回合每几秒给一个 chunk、7-62 秒完成；
而一次卡死的回合沉默超过 5 分钟，把整个重试预算耗光，
导致**什么都没重试**，客户端只看到自己的取消。

#### 5.10.2 三种续写形态（各只续一次）

| 形态 | 判据 | 动作 |
|---|---|---|
| **纯思考被切断** | 有 reasoning、无 text、无 tool、无 finish_reason | 一次 checkpoint 请求：注入恢复指令 + 已有的 reasoning JSON |
| **静默停止** | 有正常 finish_reason，但只有 thinking | 同上 |
| **max_tokens 截断** | finish == `max_tokens`，有部分 text | 把已得的部分作为 assistant 回合回喂 + 续写指令，带上**剩余预算** |

**不做**的：中途切断且已有 text 答案 → **不续写**。
那是"这一轮就是这么长"的属性，重发会再付一次同样的五分钟。

**续写请求的两个约束**：

1. **续写拿到的是本轮剩余的时间预算**，不是一个新的完整窗口。
   （否则总时长无界）
2. **续写请求里绝不能带 `tool_choice:"none"`** ——
   那会让模型这一轮不调工具。
   压制手段回到续写指令正文里本来就有的 "Do not call tools"。

#### 5.10.3 SSE 心跳与取消传播

**SSE 心跳**：长时间无字节时向下游发 SSE 注释行 `:\n\n`，防止中间代理掐断连接。

**取消传播**：客户端断开 → `context.Cancel` → 立即中止上游请求，**不**留下继续跑的 goroutine。
**三个信号**共享一条幂等的 abort 路径：

```go
req.once("aborted", abort)   // 客户端提前断开
socket.once("close", abort)  // socket 关了
res.once("close", abort)     // 响应关了
```

> 非流式端点在完成前不写响应头，所以 `ServerResponse#close` 可能来得太晚
> （Linux 上尤其明显）。`req.aborted` 与 socket 暴露得更早，三个都要挂。

#### 5.10.4 先延迟提交响应头

**这是流式代理最容易犯的错误**：一开始就写 `200 OK` + `Content-Type: text/event-stream`，
然后上游才告诉你它被限流了 —— 这时你只能往流里塞一个错误事件，
而客户端的退避逻辑看不到 `429` 和 `Retry-After`，只会当成一次普通的失败。

**正确做法**：**把 `message_start` / role 前言延迟到第一个内容字节**。
在此之前，上游的 429 + `Retry-After` 还能作为**真正的 HTTP 429** 返回，
agent 读到它才知道该退避多久。一旦发出了第一个字节，200 就已提交，
失败就只能以流内错误事件的形式传递。

```go
// 伪代码
if !sse.flushed {
    // 上游错误还能走真正的 HTTP 状态码
    return writeHTTPError(w, status, body, retryAfter)
}
sse.flushed = true
// 已经提交，只能流内报错
```

---

### 5.11 上游 HTTP 传输层

**共享一个 `http.Transport`**，参数是实测调出来的：

```go
transport := &http.Transport{
    ResponseHeaderTimeout: 90 * time.Second,  // 接了 TCP 却永不回应的出口
    MaxIdleConns:          128,
    MaxIdleConnsPerHost:   128,               // Go 默认 2 → 6 个并发里 4 个每轮重做 TLS
    IdleConnTimeout:       90 * time.Second,
    TLSHandshakeTimeout:   15 * time.Second,
    ExpectContinueTimeout: 1 * time.Second,
    DialContext: (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
}
```

**`ResponseHeaderTimeout: 90s` 的理由**：一个接受 TCP 然后永不回应的出口，
会让请求**完全没有截止时间**。90 秒清掉了最慢的实测冷启动（62 秒），
同时仍能砍掉卡死的出口。

**并发上限**：全局信号量 `maxConcurrentUpstream = 6`。
免费车道是共享资源，无界并发会把上游打挂。

**响应体形状嗅探（不信 Content-Type）**：

```go
// 上游有时用 application/json 的 Content-Type 回答流式请求。
// 所以形状比头可信。
func sniffBody(head []byte) string  // "empty" | "sse" | "json" | "unknown"
```

读前 4 KiB 判定形状，然后把读过的字节 `Unread` 回流里，让解码器看到完整的流。

**SSE 解析**：丢注释行、丢 `[DONE]`、合并 usage 帧、帧内 `error` 视为分类过的失败。
usage 要同时认两种拼法：

| 来源 | 拼法 |
|---|---|
| OpenAI | `prompt_tokens` / `completion_tokens` / `cache_read_input_tokens` |
| Anthropic | `message.usage.input_tokens` |

**思考内容的四种拼法**：上游可能用 `reasoning` / `reasoning_content` / `reasoning_text` / `thinking`。
**只读第一个非空的那个** —— 有些网关**同时**用两个名字报同一段文本，
全读会把块算重，用量统计会翻倍。

**思考内容的双写**：OpenAI Chat 线上要**同时**写 `reasoning` 和 `reasoning_content`。
只写一个会让半个生态静默丢失思考内容。

**"完成"与"被切断"要靠终止帧区分，不能靠超时**：

```
终止帧：response.completed / response.incomplete / response.failed / response.done
```

只认 `response.completed` 会让 `response.incomplete`
（一个撞到输出上限的回合）看起来像被切断的流 → **重试一个已经完成的回合**。

**重试策略的一个关键例外**：

```
可重试：EMPTY_RESPONSE / SERVER / TIMEOUT / TRANSPORT
不可重试：RATE_LIMIT
```

**为什么 429 不能自动重试**：这条车道的 429 带**越来越长**的 retry-after。
两次自动重试会把一个配额墙变成**三道墙** —— 同一个回合，付了三次钱。

---

## 5. 目录结构

```
Free Mdel House/
├── README.md                     # 项目说明 + 快速开始
├── LICENSE                       # MIT
├── docker-compose.yml            # 部署入口
├── Dockerfile                    # 多阶段构建 → 单二进制
├── .dockerignore
├── .env.example                  # FMH_SECRET_KEY / 端口 / 时区
├── go.mod
├── Makefile                      # 开发便利（build/test/lint/dev）
│
├── cmd/
│   └── fmh/
│       └── main.go               # 入口：配置加载 → 服务启动 → 信号处理
│
├── internal/
│   ├── config/                   # 配置加载与校验（env + DB 覆盖）
│   ├── server/                   # HTTP 装配、路由表、中间件
│   │   ├── router.go             #   路由表
│   │   ├── auth.go               #   Key 分层鉴权中间件
│   │   ├── cors.go
│   │   ├── logging.go            #   结构化日志 + 请求 ID
│   │   └── admin.go              #   /api/* 管理接口
│   │
│   ├── gateway/                  # 网关核心
│   │   ├── gateway.go            #   主编排：入站 → 规范化 → 路由 → 出站 → 编码
│   │   ├── canonical.go          #   统一 Message / ToolDef / Turn
│   │   ├── events.go             #   Chunk 事件定义
│   │   ├── encoder_chat.go       #   → OpenAI Chat
│   │   ├── encoder_responses.go  #   → OpenAI Responses
│   │   ├── encoder_messages.go   #   → Anthropic Messages
│   │   ├── router.go             #   候选链构造与排序
│   │   └── errors.go             #   统一错误码与分类
│   │
│   ├── lane/                     # 上游源适配器
│   │   ├── lane.go               #   Lane 接口 + 公共类型
│   │   ├── zen.go                #   OpenCode Zen 免密车道
│   │   ├── zen_fingerprint.go    #   ⚠️ 指纹/会话/工具闸门（核心）
│   │   ├── zen_test.go
│   │   ├── openrouter.go         #   OpenRouter :free 车道
│   │   ├── openrouter_free.go    #   免费模型识别
│   │   ├── sse.go                #   SSE 解析（上行）
│   │   └── classify.go           #   错误分类
│   │
│   ├── catalog/                  # 模型清单与能力元数据
│   │   ├── catalog.go            #   合并（Zen + OR + 规则表）
│   │   ├── capability.go         #   四级信任序
│   │   ├── probe.go              #   探测与 TTFT
│   │   └── ttft.go               #   滑动窗口延迟
│   │
│   ├── throttle/                 # 限流账本
│   │   └── throttle.go           #   短冷却、复活、账本
│   │
│   ├── keys/                     # Key 分层
│   │   ├── manager.go            #   生成/校验/轮换/指纹
│   │   └── vault.go              #   上游凭据 AES-GCM 加解密
│   │
│   ├── store/                    # 持久化
│   │   ├── db.go                 #   SQLite 打开、迁移
│   │   ├── models.go             #   模型/元数据表
│   │   ├── usage.go              #   用量表
│   │   ├── sessions.go           #   会话映射表
│   │   └── throttle.go           #   限流表
│   │
│   ├── relay/                    # LAN 中继
│   │   └── relay.go              #   httputil.ReverseProxy + Key 替换
│   │
│   ├── ui/                       # 内嵌前端
│   │   └── embed.go              #   //go:embed all:dist
│   │
│   └── logx/                     # 日志
│       └── logx.go
│
├── web/                          # 前端源码（TS）
│   ├── package.json
│   ├── vite.config.ts
│   ├── tsconfig.json
│   ├── index.html
│   └── src/
│       ├── main.tsx
│       ├── api/                  # 生成的 API 客户端
│       ├── pages/
│       │   ├── Overview.tsx      #  概览
│       │   ├── Models.tsx        #  模型清单（虚拟滚动 + 搜索筛选）
│       │   ├── Access.tsx        #  API 接入（Key / 地址 / 示例）
│       │   ├── Usage.tsx         #  用量看板
│       │   ├── Sources.tsx       #  上游源管理
│       │   └── Settings.tsx      #  服务设置
│       └── components/
│
├── docs/
│   ├── design/                   # 设计文档
│   │   ├── 00-overview.md
│   │   ├── 01-architecture.md
│   │   ├── 02-zen-fingerprint.md # ⚠️ 免密车道规格
│   │   ├── 03-key-model.md
│   │   └── 04-api-surface.md
│   ├── operations/
│   │   ├── deployment.md
│   │   ├── lan-access.md         # 局域网接入指南（各 Agent 配置片段）
│   │   └── troubleshooting.md
│   └── upstream-notes.md          # 上游行为实测记录
│
└── scripts/
    ├── dev.sh / dev.ps1
    └── probe-live.sh              # 手动打真上游的连通性脚本
```

---

## 6. Docker 部署

### 7.1 Dockerfile（多阶段 → 单二进制 → scratch）

> ⚠️ 下面是**当前实际使用的**版本。
> 完整可运行的副本在仓库根目录的 `Dockerfile`。
> 部署步骤见 [`../operations/nas-deploy.md`](../operations/nas-deploy.md)。

```dockerfile
# ── Stage 1: 前端构建 ──────────────────────────────
FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build            # → /internal/server/web/dist（见下方⚠️）

# ── Stage 2: Go 构建 ───────────────────────────────
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /internal/server/web/dist ./internal/server/web/dist
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" -o /out/fmh ./cmd/fmh

# ── Stage 3: 运行时 ────────────────────────────────
FROM scratch
COPY --from=build /out/fmh /fmh
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
VOLUME ["/data"]
USER root
ENV FMH_ADDR=":8678" FMH_DATA="/data"
EXPOSE 8678
ENTRYPOINT ["/fmh"]
```

⚠️ **两个容易踩的坑**：

**① 前端产物的路径。**
`vite.config.mjs` 的 `outDir` 是 `../internal/server/web/dist`，
相对 vite 的 root（本阶段 `/web`）解析 → 实际输出到
**`/internal/server/web/dist`**，不是 `/web/dist`。
写错 `COPY --from=web` 的源路径会**让构建直接失败**。
所以 Dockerfile 里有一行自检：

```dockerfile
RUN test -f /internal/server/web/dist/index.html \
    || (echo "❌ 前端产物不在预期位置" && exit 1)
```

**② 运行阶段为什么是 root。**
bind mount 的宿主目录属主是 root:root 0755，
容器若以 nobody（65534）启动就**改不了属主、也写不进去** → 首次启动失败。

正确做法跟 PostgreSQL / Redis 官方镜像一样：
以 root 启动 → 程序把 `/data` 交给 nobody → `setuid` 降权 → 全程非特权。

**镜像大小**：Go 二进制 ~11.7MB + 前端 dist ~400KB + CA 证书 ~200KB ≈ **~15MB**。
对比 Node 方案的 ~130MB —— 这是选 Go 的最直接收益。

⚠️ `modernc.org/sqlite` 是纯 Go，能进 `scratch`；`mattn/go-sqlite3` 需要 CGO，**不能**。

### 7.2 docker-compose.yml

> ⚠️ 下面是**当前实际使用的**版本。
> 完整可运行的副本在仓库根目录的 `docker-compose.yml`。

```yaml
services:
  fmh:
    build: .
    image: free-model-house:0.1.0
    container_name: free-model-house
    restart: unless-stopped
    ports:
      - "8678:8678"    # 推理面 + 管理面（单端口）
    volumes:
      - ./data:/data
    environment:
      FMH_ADDR: "${FMH_ADDR:-:8678}"
      FMH_DATA: "${FMH_DATA:-/data}"
      FMH_SECRET_KEY: "${FMH_SECRET_KEY:-}"
      TZ: "${TZ:-Asia/Shanghai}"
    healthcheck:
      test: ["CMD", "/fmh", "healthcheck"]
      interval: 30s
      timeout: 5s
      retries: 3
```

### 7.3 NAS 特别注意

| 项 | 处理 |
|---|---|
| **架构** | `docker buildx build --platform linux/amd64,linux/arm64` |
| **UID/GID** | scratch 镜像默认 root。NAS 上建议 `user: "1026:100"`（群晖默认用户）并 `chown` 数据目录 |
| **时区** | 挂载 `/etc/localtime` 或设 `TZ` 环境变量 |
| **host 网络** | 部分 NAS（如群晖）推荐 `network_mode: host`，避免 Docker 端口映射在局域网内的 hairpin 问题 |
| **反向代理** | 若要在管理面加 HTTPS，FMH 只绑 loopback + NAS 的反向代理（Synology 反向代理 / nginx proxy manager） |
| **自动更新** | Watchtower 或 NAS 自带的容器更新（**不建议**自动更新指纹逻辑 —— 上游闸门一改就 403，需要人工确认） |

---

## 7. Web 管理界面设计

**技术**：React 19 + TS + Vite 7 + Tailwind 4 + TanStack Query + TanStack Virtual。
**设计目标**：NAS 用户在手机浏览器上也能用（响应式），且**所有资源本地化**（无 CDN）。

### 8.1 页面

| 页面 | 内容 |
|---|---|
| **概览** | 服务状态、LAN 接入地址（含检测到的所有网卡 IPv4）、模型数、近 7 天 token 热力图、上游健康 |
| **模型清单** | 搜索 / 按车道·能力·状态筛选、虚拟滚动、复制模型 ID、**单模型体检**（可用性 + TTFT）、启用/禁用 |
| **API 接入** | Base URL、Key 显示/隐藏/复制/轮换、**各 Agent 的配置片段一键复制**、curl 示例 |
| **用量统计** | 按模型/客户端/日期的 token 与请求数、失败与恢复次数、实际落点模型 |
| **上游源** | 源启停、凭据录入（只显示指纹）、模型清单刷新、偏好配置 |
| **服务设置** | LAN 开关与端口、路由策略、限流冷却参数、探测开关与间隔、输出上限、思考档位默认 |

### 8.2 关键交互

- **"复制接入配置"** 是最核心的动作 —— 一键复制 `base_url` + `key` + 环境变量片段。
  这是"用户免 key 用上免费模型"这句话的落点。
- **模型体检** 按钮：点一下就发一个 `ping`，显示 TTFT 与结果。**要显式警告"这会消耗上游额度"**。
- **未保存提示**：设置页离开前提示（踩过"后台轮询覆盖用户草稿"的坑）。
- **实时刷新**：状态每 15 秒轮询一次；**读取状态不发推理请求**（探测和测试才发）。

### 8.3 管理面访问控制

管理面靠**鉴权**隔离（会话 Cookie），不靠端口 —— 因为只有**一个**端口。

想要更强的隔离（管理面完全不暴露到 LAN）：

1. **只绑 loopback**（`FMH_ADDR=127.0.0.1:8678`）+ SSH 端口转发：
   ```bash
   ssh -L 8678:127.0.0.1:8678 user@nas
   ```
2. **NAS 反向代理 + 认证**（在 NAS 上配，FMH 不感知）——
   只把 `/v1/*` 暴露出去，管理路径拦在代理层。

**不做**的事：不在 FMH 里自建"管理密码 + 远程暴露" —— 那等于自己写一个 Web 应用做认证，
既不安全又增加攻击面。

---

## 8. 对外 API 契约

### 9.1 推理接口

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| GET | `/v1/models` | ✅ | 模型清单 + 能力元数据 |
| POST | `/v1/chat/completions` | ✅ | OpenAI Chat（流式/非流式） |
| POST | `/v1/responses` | ✅ | OpenAI Responses（流式/非流式） |
| POST | `/v1/messages` | ✅ | Anthropic Messages（流式/非流式） |
| POST | `/v1/messages/count_tokens` | ✅ | 本地估算（无需 tokenizer） |
| GET | `/health` | ❌（本机面） / ✅（LAN 面） | 存活 |
| GET | `/admin` | 会话 | 管理台 SPA |
| `/api/*` | | 会话（密码登录） | 管理 API |

**同时提供无 `/v1` 前缀的别名**（`/chat/completions` 等），兼容老客户端。

### 9.2 `/v1/models` 的扩展字段

标准 OpenAI 字段之外，附加（这是让 Agent 自主决策的关键）：

```json
{
  "id": "zen/deepseek-v4.1-flash-free",
  "object": "model",
  "owned_by": "free-model-house",
  "context_window": 1048576,
  "max_output_tokens": 262144,
  "input_modalities": ["text", "image"],
  "reasoning": true,
  "supported_reasoning_levels": ["light", "balanced", "deep"],
  "capability_source": "probe",
  "lane": "zen",
  "state": "available",
  "ttft_ms": 1840
}
```

`capability_source` 让调用方知道这个结论**有多可靠**（实测 > 上游声明 > 规则 > 未知）。

### 9.3 count_tokens 的本地估算

没有 tokenizer 也能给出**有用的**估算（Claude Code 会调它）：

```
估算 ≈ ceil(总字符数 / 3.6) + 消息数 * 4 + 工具 schema 字符数 / 3.6
```

**明确标注"这是估算"**（响应用 OpenAI 的 `input_tokens` 字段，外加一个非标准字段标注方法）。
Claude Code 只用它做预算判断，误差可接受；**绝不能谎称精确**。

---

## 9. 实施路线

| 阶段 | 交付 | 验收标准 |
|---|---|---|
| **M0 骨架** | Go 服务 + 路由表 + Key 分层 + SQLite + Docker 构建 | `docker compose up` 后 `/health` 200，镜像 < 30MB |
| **M1 Zen 免密车道** | 指纹 + 会话铸造 + 工具闸门 + 三线 encoder + `/v1/models` | **一条 200 的真实对话**（这是最关键的一步） |
| **M2 OpenRouter 车道** | `:free` 识别 + 能力元数据 + 限流 | 用 OR Key 打通一个 `:free` 模型 |
| **M3 路由与容错** | 候选链 + 故障转移 + 限流账本 + 探测 | 手动关掉一个模型能自动切换 |
| **M4 Web UI** | 六个页面 + 接入配置一键复制 | 手机浏览器可用 |
| **M5 LAN 接入** | 单端口 + Key 轮换 + 来源判定 | 另一台机器用 `fmh-` Key 跑通 |
| **M6 加固** | 用量看板 + 断流三重兜底 + 错误分类完善 | 压测与故障注入 |
| **M7 文档** | 部署指南 + 各 Agent 接入片段 | 新用户 10 分钟内跑通 |

### 10.1 风险与对策

| 风险 | 影响 | 对策 |
|---|---|---|
| **上游闸门再变**（已发生 2 次：v1→v2、`/zen/v1`→`/inference/v1`） | 全部 403 | ① 把闸门规格集中在**一个文件**（`zen_fingerprint.go`），改只改一处 ② 闸门回归测试 ③ 上游失败时**明确报"上游闸门变更"而不是"你的 key 错了"**（`CodeFreeTier`） |
| **免费额度被耗尽** | 局域网用户全挂 | ① 限流账本 + 短冷却 ② 故障转移 ③ 看板可见 ④ 文档写清"免费不等于无限" |
| **OpenRouter 免费档限速**（~50 req/day） | 体验差 | ① 与 Zen 车道**分层**：默认走 Zen，OR 作为回落 ② OR Key 只在需要时消耗 ③ 面板显示剩余预期 |
| **NAS 架构**（arm64/mips） | 镜像跑不起来 | 多架构 buildx；mips 不支持（Go 不官方支持，Node 也不支持）—— 文档注明 |
| **上游提示词被记录** | 隐私 | 文档显著位置声明；不提供"把上游设为可信"开关 |
| **上游条款变化导致封号** | 停服 | 明确免责声明：本项目**不承诺**任何上游的可用性 |

### 10.2 合规与免责（必须写进 README 与 UI）

- FMH 是**客户端/网关**，不是模型服务方；所有输出由第三方上游生成。
- **免费不等于无限，也不等于私有**。免费额度由上游单方面提供，随时可能限流、收费或下线。
- 上游可能记录 prompt（Kilo 池明确声明会记录并用于改进）。
- **不要通过本项目发送密码、密钥、个人身份信息、商业机密**。
- 使用各渠道即表示同意其服务条款与用量政策。
- FMH 与任何上游厂商无隶属或背书关系。

---

## 10. 立即可执行的第一步

**先证明免密车道能通**（M1 的核心），再写其他所有东西。

```bash
git clone <repo> && cd "Free Mdel House"
mkdir -p internal/lane && touch internal/lane/zen_fingerprint.go
```

第一步要写的三个文件：

1. `internal/lane/zen_fingerprint.go`
   —— `MintSessionId` / `ZenHeaders` / `ApplyZenFingerprint`（别名 + 注入 + 还原）
2. `internal/lane/zen_test.go`
   —— §4.2.1 的六个回归测试
3. `cmd/fmh/main.go` 的最小版
   —— 起一个 HTTP 服务，`POST /v1/chat/completions` 打到 `opencode.ai/inference/v1/chat/completions`

**判定标准只有一条**：拿到一个真实的 200 + 可读的模型回答。
在这一条达成之前，不要写 UI、不要写看板、不要写 OpenRouter 车道。


