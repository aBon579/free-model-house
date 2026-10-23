# M3–M5 验收记录

> ⚠️ **术语说明（补记）**：本文写于 **2026-10-19** ——
> 当时管理台还是「用本机 Key 换会话」，之后这一层已改成**密码登录**
> （单 Key + bcrypt 密码，见 `docs/design/02-key-model.md`）。
> 文中出现的「本机 Key / LAN Key」是**当时**的形态，保留不改以忠实记录；
> **要以当前实现为准，请看设计文档，不要照本文配置。**
>
> ⚠️ **术语说明**：本文记录的是 **2026-10-19** 那次排查，
> 当时的开关叫「跨车道回落」。
> 后来它被改名为「**自动跨模型调用**」，语义也从"换车道"
> 明确成了"**换模型**"（关闭时只重试当前模型，一个模型都不换）。
> 详见 `docs/design/00-overview.md` §5.4.3。


> 日期：2026-10-19
> 版本：0.1.0
> 方法：真实上游 + 无头浏览器

---

## 1. 端到端验收（30 项全过）

在**全新的数据目录**上启动，真实打上游。

```
=== 1. Key 模型 ===
  PASS  本机 Key 有 fmh- 前缀
  PASS  LAN Key 有 fmh- 前缀
  PASS  两把 Key 不同
  PASS  主体 40 位 hex

=== 2. 鉴权 ===
  PASS  无 Key → 401
  PASS  错 Key → 401
  PASS  本机 Key → 200
  PASS  LAN Key → 200
  PASS  模型数 > 25

=== 3. 管理面隔离 ===
  PASS  LAN Key 不能进管理面 → 401
  PASS  无会话不能进管理面 → 401
  PASS  LAN Key 不能登录 → 401
  PASS  本机 Key 能登录 → 200

=== 4. 健康检查与 UI ===
  PASS  healthz 无鉴权 → 200
  PASS  UI 首页 → 200 HTML
  PASS  SPA 回退 → 200

=== 5. 真实上游推理（免密车道）===
  PASS  非流式 → 200
  PASS  内容含 FMH-OK
  PASS  usage 有 prompt_tokens
  PASS  X-FMH-Lane 头存在
        耗时 1.6s  usage={'completion_tokens': 94, 'prompt_tokens': 966, 'total_tokens': 1060}

=== 6. 流式 ===
  PASS  流式 → 200
  PASS  Content-Type 是 SSE
  PASS  流式内容含 1..5
  PASS  流以 [DONE] 结束
        assembled='1\n2\n3\n4\n5'

=== 7. Anthropic 协议 ===
  PASS  /v1/messages → 200
  PASS  Anthropic 形状正确

=== 8. token 计数（本地，不花额度）===
  PASS  count_tokens → 200
  PASS  标注了是估算

=== 9. 错误处理 ===
  PASS  缺 model → 400
  PASS  错误是 OpenAI 形状

========================================================
  通过 30 / 失败 0
========================================================
```

---

## 2. 关键实测数据

| 项 | 值 |
|---|---|
| 免密车道模型数 | 10 |
| OpenRouter 免费模型数 | 18 |
| 合计 | 28 |
| 非流式推理延迟 | 1.6s |
| 首字延迟 | ~1.2s |

---

## 3. Web UI 实测（无头 Edge + playwright-core）

### 3.1 页面渲染

| 页面 | 结果 |
|---|---|
| 快速接入 | ✅ Base URL、Key、6 个工具的配置片段 |
| 模型清单 | ✅ 26 行（虚拟滚动）、4 个筛选器 |
| 用量 | ✅ 总量卡片、曲线、按模型表格 |
| 上游源 | ✅ 两条车道、凭据指纹、跨车道回落开关 |
| 服务设置 | ✅ 4 组设置、Key 轮换、关于 |

### 3.2 能力徽章的三态

从 DOM 的 class 读出来的实测分布：

```
counts: { support: 37, unsupported: 11, unknown: 8 }
```

抽样核对（与 OpenRouter 上游的 `input_modalities` 完全一致）：

| 模型 | 图 | 思 |
|---|---|---|
| `openrouter/cohere/north-mini-code:free` | 不支持 | 支持 |
| `openrouter/liquid/lfm-2.5-2.6b:free` | 不支持 | 支持 |
| `openrouter/poolside/laguna-xs-2.1:free` | 不支持 | 支持 |
| `openrouter/openrouter/free` | 支持 | **不支持** |
| `zen/beam-free` | **未测量** | **未测量** |
| `zen/jev-1.13-free` | **未测量** | **未测量** |
| `zen/minimax-m3.1-flash-free` | **未测量** | **未测量** |

> ⚠️ 注意 `zen/` 下的"未测量"——
> 免密车道的清单**不声明**能力，规则表也匹配不上，
> 所以诚实地标成"未测量"而不是猜一个。

### 3.3 暗色主题

切换正常，CSS 变量全套覆盖。

---

## 4. 这一轮抓到的 3 个真 bug

都是**只有真正跑起来才会暴露**的问题。

### Bug 1：非流式永远返回空内容

**症状**：`content: ''`，但 `usage` 正常。

**原因**：非流式路径从 `turn.Messages` 读答案 ——
但那是**请求**，里面没有模型的回答。答案在 chunk 流里。

**修法**：加 `internal/server/collect.go`，从 chunk 重建 assistant 消息。

**测试**：加了 `TestGatewayNonStreamReturnsContent` 与
`TestGatewayNonStreamAnthropicHasContent`。
⚠️ 这个 bug 单元测试测不出来 —— 因为它读的是"请求"，而请求里当然没答案。
**是端到端测试抓出来的。**

### Bug 2：LAN Key 不能调推理接口

**症状**：`/v1/models` 用 LAN Key → 401。

**原因**：网关只认本机 Key。那是**双端口**时代的假设 ——
单端口下，LAN 里的 agent 拿的就是 LAN Key，
而它们访问的正是同一个 `:8678`。

**修法**：推理面用 `MatchAny`（两把都放行），
但**管理面登录仍然只认本机 Key**（`MatchLocal`）。

> 这个区分才是有意义的：它保证 LAN Key 泄露时，
> 攻击者能白嫖额度，但**进不了管理面**。

### Bug 3："实际服务"列误报切换

**症状**：`⚠ 实际由 minimax-m3.1-flash-free 应答` ——
但请求的就是这个模型。

**原因**：请求模型存的是全名 `zen/minimax-m3.1-flash-free`，
实际模型存的是裸名 `minimax-m3.1-flash-free`。
格式不同 → 比较失败 → 误报。

**修法**：两边都用 `FormatModelID(lane, model)`。

---

## 5. 另外两个 UI 错误

| 错误 | 修法 |
|---|---|
| 免密车道卡片显示"未就绪"、模型数 18 | `Sources.tsx` 里 zen 卡片误用了 `orLane`，改成 `zenLane` |
| 所有模型的能力徽章都画成实心 | 已知"不支持"要画删除线、"未测量"要画虚线空白 —— 之前三种都渲染成实心 |

---

## 6. 单元测试

```
internal/lane    ok  135 个测试
internal/server  ok
internal/store   ok

gofmt: 全部干净
go vet: 干净
```

---

## 7. 还没验证的（诚实清单）

| 项 | 为什么没验 |
|---|---|
| **断流续写的真实触发** | 需要一个真会截断的模型。逻辑有 11 个单元测试覆盖，但没端到端跑过 |
| **OpenRouter 真实推理** | 没有 OR Key。清单拉取 ✅、配额查询 ✅（`401 "User not found."` 证明端点要 Key），推理 ❌ |
| **跨车道回落** | 需要"免密被 IP 封 + OR 有 Key"的组合场景 |
| **自动探测的定时调度** | 定时器还没接上 |
| **移动端 UI** | 响应式断点写了，没在真机上验过 |
| **并发压测** | 没测过 50 并发 |
| **限流账本重启恢复** | `saveThrottle` 在跑，没验证过恢复 |
