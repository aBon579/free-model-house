# 对外 API 契约

FMH 对外暴露三套标准推理协议 + 一个模型清单。所有响应遵循各自上游的原生格式，
以保证最大兼容性。

---

## 1. 路由总表

| 方法 | 路径 | 鉴权 | 协议 | 说明 |
|---|---|---|---|---|
| GET | `/v1/models` | ✅ | — | 模型清单 + 能力元数据 |
| POST | `/v1/chat/completions` | ✅ | OpenAI Chat | 流式 / 非流式 |
| POST | `/v1/responses` | ✅ | OpenAI Responses | 流式 / 非流式 |
| POST | `/v1/messages` | ✅ | Anthropic Messages | 流式 / 非流式 |
| POST | `/v1/messages/count_tokens` | ✅ | Anthropic | 本地估算 |
| GET | `/health` | 本机免 / LAN 要 | — | 存活 |
| GET | `/admin` | 会话 | — | 管理台 SPA |
| ANY | `/api/*` | 会话 或 本机 Key | — | 管理 API |

**别名**：所有 `/v1/*` 推理路径同时接受**无 `/v1` 前缀**的形式
（`/chat/completions`、`/models`、`/messages`…），兼容老客户端。

### 1.1 管理 API 清单

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/login` | 用本机 Key 换会话 Cookie |
| GET | `/api/status` | 总体状态（车道、模型数、版本、**出口 IP**、**车道级封锁**） |
| GET | `/api/models` | 模型清单 + 探测状态 + 冷却倒计时 |
| GET | `/api/lanes` | 车道详情 + OpenRouter 额度 |
| GET | `/api/usage` | 用量统计（按模型聚合，含"实际服务"） |
| GET | `/api/keys` / POST `/api/keys/rotate` | 局域网 Key 轮换 |
| GET/POST | `/api/secrets` | 上游凭据（只回显指纹） |
| GET/POST | `/api/settings` | 服务设置（LAN 档位、自动跨模型调用、探测节奏） |
| GET | `/api/probes` | 探测结论 + 统计 |
| POST | `/api/probes/run` | 手动探测（带 `model` 同步；不带则整轮异步） |
| **GET** | **`/api/conformance`** | **上游合规状态** |
| **POST** | **`/api/conformance`** | **立即做一次合规检查**（消耗一次上游推理） |
| **POST** | **`/api/diagnostics`** | **指纹诊断** —— 定位上游改了哪一条（最多 7 次请求） |

⚠️ `/api/probes/run` 与 `/api/conformance` **会消耗上游额度**，
界面上必须写明这一点。

⚠️ `/api/diagnostics` 会发 **最多 7 次请求**（1 基线 + 6 变体）。
变体被拒时是 403，**不花钱**；基线通过时那 1 次是真实推理，**花钱**。
所以只在**闸门拒绝**时才跑 —— 管理台把它放在拒绝横幅里。

**CORS**：`/v1/*` 允许跨域（浏览器里的 harness 能用）；
`/admin`、`/api/*` **同源严格**（不开放 CORS）。

**预检**：`OPTIONS` 不鉴权，直接返回 204 + CORS 头
（浏览器无法在预检里带 key；且预检不消耗任何额度）。

---

## 2. `GET /v1/models`

### 2.1 标准字段 + 扩展字段

```json
{
  "object": "list",
  "data": [
    {
      "id": "zen/deepseek-v4.1-flash-free",
      "object": "model",
      "created": 1760000000,
      "owned_by": "free-model-house",

      "context_window": 1048576,
      "max_output_tokens": 262144,
      "input_modalities": ["text", "image"],
      "reasoning": true,
      "supported_reasoning_levels": ["light", "balanced", "deep"],
      "capability_source": "probe",

      "lane": "zen",
      "state": "available",
      "ttft_ms": 1840,
      "upstream_model": "deepseek-v4.1-flash-free"
    }
  ]
}
```

### 2.2 扩展字段说明

| 字段 | 类型 | 含义 |
|---|---|---|
| `context_window` | int | 上下文窗口。**可能不准** —— 看 `capability_source` |
| `max_output_tokens` | int | 最大输出。思考 token 与它**共享**额度 |
| `input_modalities` | string[] | `["text"]` / `["text","image"]` / `["text","image","audio","file"]` |
| `reasoning` | bool | 是否支持思考 |
| `supported_reasoning_levels` | string[] | 该模型**实际生效**的档位。空的 = 不支持 |
| `capability_source` | string | `probe` > `listing` > `heuristic` > `unknown` —— **结论的可信度** |
| `lane` | string | `zen` / `openrouter` |
| `state` | string | `available` / `throttled` / `region_blocked` / `unavailable` / `unknown` |
| `ttft_ms` | int | 最近的首字延迟（平滑值）。**选模型的关键信号** |
| `upstream_model` | string | 实际发给上游的模型 id |

### 2.3 为什么 `capability_source` 很重要

Agent 需要知道**这个结论有多可靠**。一个 `heuristic` 来源的"支持视觉"可能是猜的；
一个 `probe` 来源的是实测过的。Agent 可以据此决定要不要冒险。

（这样能力元数据**从仪表盘爬取变成标准接口的一等公民**。）

### 2.4 模型 ID 命名

| 形式 | 含义 |
|---|---|
| `zen/<model>` | 走 OpenCode Zen 免密车道 |
| `openrouter/<model>` | 走 OpenRouter |
| `<model>` | 无前缀 —— 按清单里的归属解析；解析不到默认走 `zen` |

上游模型 id 里含 `/`（如 `deepseek/deepseek-r1:free`），
所以用**第一个** `/` 分隔车道与模型：`openrouter/deepseek/deepseek-r1:free`
→ lane=`openrouter`, model=`deepseek/deepseek-r1:free`。

### 2.5 思考模型的档位变体

对于 `reasoning: true` 的模型，额外暴露档位变体，让客户端的模型选择器能直接选：

```json
{ "id": "zen/deepseek-v4.1-flash-free (light)", ... }
{ "id": "zen/deepseek-v4.1-flash-free (deep)",  ... }
```

档位也可以通过请求体的 `reasoning_effort` / `thinking` 传入 —— 两条路都支持。

---

## 3. `POST /v1/chat/completions`

**完全兼容 OpenAI Chat Completions**，支持 `stream: true`。

### 3.1 FMH 的扩展：模型 ID 后缀

```json
{ "model": "zen/deepseek-v4.1-flash-free (deep)", "messages": [...] }
```

后缀 `(light)` / `(balanced)` / `(deep)` / `(high)` 等会被解析成档位，
并从实际发给上游的模型 id 中剥离。

### 3.2 FMH 的扩展：思考意图

```json
{
  "model": "zen/deepseek-v4.1-flash-free",
  "reasoning_effort": "high",
  "messages": [...]
}
```

`low`/`minimal`/`none` → `light`；`medium` → `balanced`；
`high`/`xhigh`/`max` → `deep`。

### 3.3 响应头

| 头 | 含义 |
|---|---|
| `X-FMH-Model` | **实际服务的模型**（故障转移后可能与请求的不同） |
| `X-FMH-Lane` | `zen` / `openrouter` |
| `X-FMH-Fallback` | `true` 表示发生了故障转移 |
| `X-FMH-Request-Id` | 请求追踪 ID（同时出现在日志里） |
| `X-FMH-Upstream-Latency-Ms` | 首字延迟（若已测得） |

> `X-FMH-Model` 是**排查问题的第一手线索**：
> "我请求的 deepseek，为什么账单上是 glm？" —— 看这个头。

---

## 4. `POST /v1/responses`

OpenAI Responses 协议（Codex / 新 SDK 用）。

**注意**：这一线的请求体结构与 Chat 线差别较大（`input` 数组、`instructions` 字段、
`tools` 是 flat 形状），FMH 内部会规范化后再走统一流程。

**注意**：这一线在调用方未给 `tool_choice` 时会补 `"auto"`（官方行为），
其他线不补。

---

## 5. `POST /v1/messages`

Anthropic Messages 协议（**Claude Code 用这个**）。

### 5.1 请求

```json
{
  "model": "zen/deepseek-v4.1-flash-free",
  "max_tokens": 8192,
  "system": "You are ...",
  "messages": [
    { "role": "user", "content": "hello" },
    { "role": "user", "content": [{ "type": "image", "source": {...} }] }
  ],
  "tools": [
    { "name": "read", "description": "...", "input_schema": {...} }
  ],
  "stream": true
}
```

### 5.2 事件序列（流式）

```
event: message_start
data: {"type":"message_start","message":{...}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{...}}

event: message_stop
data: {"type":"message_stop"}
```

思考内容走 `thinking_delta`，工具调用走 `input_json_delta`。

### 5.3 `stop_reason` 映射

| 上游 | Anthropic |
|---|---|
| `stop` | `end_turn` |
| `tool_calls` | `tool_use` |
| `max_tokens` | `max_tokens` |
| （上游流被截断） | `max_tokens` |

---

## 6. `POST /v1/messages/count_tokens`

Claude Code 会调用它做预算判断。FMH **不带 tokenizer**，用启发式估算。

```json
// 请求
{ "model": "zen/deepseek-v4.1-flash-free", "messages": [...] }

// 响应
{ "input_tokens": 1234 }
```

### 6.1 估算方法

```
估算 ≈ ceil(总字符数 / 3.6) + 消息数 × 4 + 工具 schema 字符数 / 3.6
```

`3.6` 是英语 + 代码混合文本的经验字符/token 比。

### 6.2 必须诚实

- 响应里附加一个非标准字段标明这是估算：

```json
{ "input_tokens": 1234, "fmh_estimated": true, "fmh_method": "heuristic-3.6" }
```

- **绝不能谎称精确**。Claude Code 只用它做预算判断，几个百分点的误差完全可接受；
  但如果模型在撒谎（比如返回一个假的小数字让 Claude Code 以为够用），就会出问题。
- 估算**不调用上游**，不消耗额度。

---

## 7. 错误格式

### 7.1 OpenAI 形状

```json
{
  "error": {
    "message": "upstream refused: rate limited",
    "type": "rate_limit_error",
    "code": "upstream_rate_limit",
    "param": null
  }
}
```

### 7.2 Anthropic 形状（`/v1/messages`）

```json
{
  "type": "error",
  "error": {
    "type": "rate_limit_error",
    "message": "upstream refused: rate limited"
  }
}
```

### 7.3 FMH 的错误码扩展

| `code` | HTTP | 含义 | 客户端该做什么 |
|---|---|---|---|
| `upstream_free_tier_blocked` | 502 | **上游闸门拒绝**（可能上游改了指纹规格） | 重试无用；上报给 FMH 维护者 |
| `upstream_rate_limit` | 429 | 上游限流/配额 | 退避重试；FMH 内部会自动换模型 |
| `upstream_region_blocked` | 502 | 地域封锁 | 换出口/换模型 |
| `upstream_credential` | 502 | 上游凭据无效（**不是你的 key 错了**） | 检查 FMH 的上游源配置 |
| `upstream_client_error` | 400 | 请求自身有问题 | 修请求；FMH 不会换模型 |
| `upstream_transport` | 502 | 网络/传输失败 | 可重试 |
| `upstream_timeout` | 504 | 超时 | 可重试 |
| `upstream_empty_response` | 502 | 上游返回空 | 可重试 |
| `no_model_available` | 503 | 所有候选都不可用 | 稍后重试 |
| `invalid_api_key` | 401 | **你的** FMH Key 错了 | 换 Key |
| `model_not_found` | 404 | 模型不存在或被禁用 | 查 `/v1/models` |

> **`upstream_free_tier_blocked` 必须独立成码**。
> 如果把它归到 `invalid_api_key` / `upstream_credential`，
> 客户端会以为"是我的 key 错了"，而且**会停掉故障转移**。
> 上游一改规格，整个网关就卡死。
> （这个坑踩过 —— 要写清楚注释。）

---

## 8. CORS

| 路径 | CORS |
|---|---|
| `/v1/*` | 允许所有来源（`Access-Control-Allow-Origin: *`） |
| `/health` | 允许 |
| `/admin`, `/api/*` | **同源严格**，不返回 CORS 头 |

允许的头：`Authorization, Content-Type, X-Api-Key, X-FMH-Request-Id`
允许的方法：`GET, POST, OPTIONS`

---

## 9. 版本与兼容

- FMH 的**对外契约是 OpenAI / Anthropic 的标准**，随时间稳定。
- 上游的模型清单会变（新模型、模型下线）—— 这是**期望行为**，
  客户端应通过 `/v1/models` 发现，而不是硬编码。
- 若上游改了模型 id，FMH 在 `/v1/models` 里**如实反映**，不做别名映射
  （除了车道前缀，那是 FMH 自己的命名空间）。
