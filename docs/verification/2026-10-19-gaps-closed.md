# 剩余不足的一次性解决 —— 实现与验证

> ⚠️ **术语说明**：本文记录的是 **2026-10-19** 那次排查，
> 当时的开关叫「跨车道回落」。
> 后来它被改名为「**自动跨模型调用**」，语义也从"换车道"
> 明确成了"**换模型**"（关闭时只重试当前模型，一个模型都不换）。
> 详见 `docs/design/00-overview.md` §5.4.3。


> 日期：2026-10-19
> 状态：已完成
> 测试：196 个全过（+80）· 端到端 30/30 · 50 并发 100/100

---

## 0. 起点

上一轮结束时剩下 7 项不足：

| 项 | 状态 |
|---|---|
| 断流续写没在真实截断上验证过 | ⚠️ |
| OpenRouter 车道没跑过真实推理 | ⚠️ |
| 跨车道回落没端到端验证 | ⚠️ |
| 移动端 UI 没在真机验过 | ⚠️ |
| 没有并发压测（50 并发） | ⚠️ |
| 没在 arm64 上实际构建过 | ✅ **不需要**（用户的 NAS 是 x86） |
| 上游变更没有自动检测 | ⚠️ |

这一轮全部处理完。**过程中抓到了 8 个真 bug**（其中 2 个严重）。

---

## 1. OpenRouter 车道跑通了

### 1.1 Key 的本质（先回答用户的问题）

**调用 OpenRouter 推理必须用你自己的 Key。** 这不是 FMH 的设计选择，
是 OpenRouter 的规则 —— 它的"免费模型"（`:free`）只是**特价档**，
对你免费，但**必须绑定账户**（没有 Key 直接 `401 User not found.`）。

**但免密车道不需要任何 Key** —— 用的是共享的 `Bearer public`。

所以 FMH 给你两个选择：

| | 需要你的 Key？ | 额度 | 稳定性 |
|---|---|---|---|
| **免密车道**（默认开） | ❌ | 共享的，高峰期限流 | 可能整段被 IP 风控 |
| **OpenRouter**（可选） | ✅ | 你自己的 | 稳，但要用你的免费额度 |

**不配 OpenRouter 完全不影响使用** —— 那条车道就是"未配置"状态，
不参与路由。

### 1.2 实测（真实上游，用真实 Key）

```
/api/v1/key          → 200  limit=$0.01  usage=$0  is_free_tier=true
/api/v1/models       → 469 个模型，18 个免费
chat/completions     → 200  content="OR-OK"
```

通过**网关**（不是直连上游）：

```
POST /v1/chat/completions  model=openrouter/minimax/minimax-m3.1-flash:free
→ 200, 1.9s, content="OR-OK"
```

**Anthropic 协议 + OpenRouter**：

```
POST /v1/messages
→ 200  type=message  stop=end_turn
   thinking 'We need answer exactly OR-MSG likely. Need comply.'
   text 'OR-MSG'
```

### 1.3 抓到的 4 个 bug

#### ① **mandatory 模型会 400**（严重）

OpenRouter 的 `reasoning` 字段有 `mandatory` 标志。实测矩阵：

| 模型类型 | `effort:none` | `enabled:false` | `effort:low` |
|---|---|---|---|
| `mandatory=false` | ✅ 200，reasoning_tokens=0 | ✅ 200，reasoning_tokens=0 | ✅ 200 |
| `mandatory=true` | ❌ **400** | ❌ **400** | ✅ 200 |

> 400 的原文：`"Reasoning is mandatory for this endpoint and cannot be disabled."`

FMH 原来的 `effortPatch` 无条件对 "none/off" 发 `enabled: false`
—— 对 mandatory 模型就是**必然 400**。

**修法**：加 `mandatory` 追踪表，三分支处理：

```
① mandatory=true        → 降级成最低档位（不发任何"关掉"的语义）
② 声明了 none 档        → 发 effort:none
③ 都不满足              → 发 enabled:false
```

⚠️ **而且 mandatory 必须先判** —— 实测 mandatory 模型上 `effort:none`
**也**是 400。原来的写法（先查 none 档再查 mandatory）会漏掉这一种。

#### ② **"关掉思考"的意图在到达 lane 层之前就丢了**（严重）

```go
// 原来
case "minimal", "low", "none", "off", "disabled", "light":
    return "light"      // ← "关掉思考"变成了"最轻的思考"
```

`NormalizeEffort` 把 `none`/`off`/`disabled` 全部映射成 `light`。
理由是"免密车道做不到真的关掉思考"。

**但那把意图也一起丢了** —— OpenRouter 车道**能**关掉，
而它拿到的永远是 `light`，于是"关掉思考"这个请求**静默失效**。

**修法**：引入独立档位 `EffortOff`，保留意图到 lane 层，
让每条车道自己决定能不能做到：

```
免密车道  → 做不到，给最紧的预算（且**不加倍** —— 加倍正好相反）
OpenRouter → 真的关掉
```

#### ③ **`reasoning_tokens` 永远拿不到**

OpenRouter 的 usage 形状是**嵌套**的：

```json
{"completion_tokens": 40, "completion_tokens_details": {"reasoning_tokens": 39}}
```

顶层**没有** `reasoning_tokens` —— 只读顶层会永远得到 0。

而且 Chat encoder **整个把思考 token 丢了**。

**修法**：解码器两个地方都读；encoder 按 OpenAI 的标准形状输出
`usage.completion_tokens_details.reasoning_tokens`。

实测：`{"completion_tokens": 21, "completion_tokens_details": {"reasoning_tokens": 20}, ...}` ✅

#### ④ **额度从来没查过**

`FetchQuota` 实现了但**从来没被调用** —— 界面上"额度"那一栏永远是空的。

**修法**：清单刷新时查一次；**刚存好 Key 立刻查一次**
（用户最想看的就是"我还剩多少"，等 30 分钟太久）。

---

## 2. 跨车道回落：**发现并修复一个严重 bug**

### 2.1 bug：开关关了**照样**会碰 OpenRouter

原来的守卫写在这里：

```go
// 尝试之后
if !r.allowCrossLane && isCrossLaneSwitch(cand.Lane) {
    continue
}
```

问题有两个：

1. 它在**尝试之后**才判断 —— 那个候选**已经被调过了**，
   用户的额度**已经消耗**。
2. 它比的是"**上一个失败候选**的车道"，不是"**这个候选**的车道"。

实测（修复前）：

```
候选[0] zen/model-a     ← 请求的模型
候选[1] zen/model-b
候选[2] openrouter/model-c

结果: Lane=openrouter  Model=openrouter/model-c
zen 调用: [zen/model-a]
or  调用: [openrouter/model-c]     ← ⚠️ 开关是关的！
```

**跨车道回落默认关闭**（因为它花用户自己的 OR 额度），
但这个 bug 让开关**形同虚设**。

**修法**：改成**尝试之前**判断，而且比的是"这个候选的车道"
vs"**请求原本要的车道**"：

```go
if !r.allowCrossLane && cand.Lane != originLane {
    continue    // 换个车道 —— 但用户没许可
}
```

### 2.2 顺带理清了一个错误分类

`401 Missing API key` 在免密车道上有**特殊含义**：
上游发现 `Bearer public` **不适用于这个模型** —— 也就是**这个模型不免费**。

那换模型**是有用的**（车道里别的模型可能还是免费的），
所以它必须是**模型级**（`ScopeProvider`），不是**凭据级**（`ScopeAccount`）。

**修法**：用 `Error.MissingAPIKey` 标志做区分 ——
它有值 → 模型级、可切换；没值 → 凭据级、立即返回。

### 2.3 8 个新测试

| 测试 | 验证 |
|---|---|
| `TestCrossLaneFallsBackWhenZenBlocked` | 免密被封 → 真的回落到 OR |
| `TestCrossLaneDisabledDoesNotFallBack` | **开关关了绝不碰 OR** |
| `TestCrossLaneModelLevelFailureStillTriesSameLane` | 限流是模型级的 → 试完同车道 |
| `TestCrossLaneErrorPreservesRetryAfter` | 最终错误保留 `Retry-After` |
| `TestCrossLaneSkipsLaneOnGateRejection` | 闸门拒绝 → 同车道只试一个 |
| `TestCrossLaneCredentialErrorIsModelLevel` | "这个模型不免费" → 换模型有用 |
| `TestCrossLaneAccountCredentialErrorDoesNotSwitch` | key 配错了 → 立刻返回 |
| `TestCrossLaneBothLanesFail` | 两条车道都挂 → 如实报错 |

### 2.4 端到端实测

```
跨车道回落关（默认）: 指名 zen/... → 200，走免密车道
跨车道回落开:          指名 openrouter/... → 200，走 OR 车道

用量统计（两条车道分开记，实际模型也对）：
  openrouter/minimax/minimax-m3.1-flash:free  请求=8  实际=openrouter/…
  zen/minimax-m3.1-flash-free                 请求=3  实际=zen/…
```

---

## 3. 移动端 UI：**从"裁断"修到"能用"**

### 3.1 实测发现的问题

先说结论：**侧栏的响应式是好的**（<1024px 变成抽屉），
但**模型清单的表格在手机上是被裁掉的**。

```
375px：容器宽 341px，内容宽 810px  ⚠️ 右边 5 列完全看不见
768px：容器宽 718px，内容宽 810px  ⚠️ 还是超
```

而且是**裁断**，不是横向滚动 —— 用户根本不知道右边还有东西。

### 3.2 修法：窄屏换卡片布局

用 `grid-template-areas` 重排（不是 `display:none` —— 那会**丢信息**）：

```
桌面（≥1024px）              窄屏（<1024px）
┌────┬────┬────┬────┐       ┌────────────────────┐
│模型│车道│能力│状态│       │ 模型名          [🔍]│
├────┼────┼────┼────┤       ├────────────────────┤
│首字│服务│    │操作│       │ 车道 图 思 状态      │
└────┴────┴────┴────┘       ├────────────────────┤
                            │ 首字 1.0s  实际服务   │
                            └────────────────────┘
```

行高也要跟着变（40px → 78px），否则虚拟滚动会**让卡片互相压住**。

### 3.3 实测（三个断点）

```
@375px  行高=78px  行间重叠=0 ✅  内容 341px / 容器 341px
@768px  行高=78px  行间重叠=0 ✅  内容 718px / 容器 718px
@1600px 行高=40px  行间重叠=0 ✅  7 列原样，一字未改
```

信息**一个不少**（车道/能力/状态/首字/实际服务全在）。

### 3.4 其它页

「用量」页的表格本来就有 `overflow-x-auto`，能横向滚动，**可用**。

---

## 4. 并发压测

### 4.1 端到端（50 并发，真实上游）

```
=== 非流式 (50 并发) ===
  总耗时 10.8s  吞吐 4.6 req/s
  成功 50/50  状态码分布: {200: 50}
  延迟 p50=5.48s  p90=8.00s  p99=8.69s  max=8.69s

=== 流式 (50 并发) ===
  总耗时 8.9s  吞吐 5.6 req/s
  成功 50/50  状态码分布: {200: 50}
  延迟 p50=5.07s  p90=8.13s  p99=8.86s  max=8.86s
```

**100/100 全过，零错误。**

⚠️ 延迟偏高（p50 5 秒）是**上游**排队造成的，
不是网关 —— 50 个并发打共享的免密配额，上游就是会排队。

### 4.2 Go 层并发测试（5 个）

`go test -race` 在 Windows 上需要 **POSIX 线程版**的 MinGW，
本机只有 win32 线程版（跑不了）。所以改用另一种方式验证：

| 测试 | 验证 |
|---|---|
| `TestConcurrentCatalogAccess` | 50 goroutine 并发读写清单不崩 |
| `TestConcurrentThrottleBook` | 并发记账不乱 |
| `TestConcurrentRouterCompletions` | 并发推理结果正确 |
| `TestConcurrentProbeScheduler` | 并发探测不崩 |
| `TestConcurrentUsageRecording` | 并发记账不丢数 |

**`-count=20` 跑 20 遍，全过。**

⚠️ 这**不能替代** race detector，但能抓住最要命的那类问题
（"两个 goroutine 同时改 map" 会直接 panic）。

---

## 5. 断流续写：**端到端验证 + 修复一个 bug**

### 5.1 造了一个"会截断的上游"

真实上游不会稳定地截断，所以造了一个假服务器：

- 第一次请求 → 吐一半文本，**不给 finish_reason，直接断开**
- 第二次请求（带 `__fmh_continue` 工具调用）→ 从断点继续，正常结束

### 5.2 抓到的 bug：续写发给了**错误的模型**

```go
// 原来
func (g *Gateway) continueStream(ctx, turn *lane.Turn, ...) {
    l, ok := g.lanes.Get(turn.Lane)      // ← turn 里的车道
    ...
    Model: turn.Model,                     // ← turn 里的模型
}
```

但 `turn` 是**请求里的**模型，而**故障转移可能换过模型/车道**：

```
用户要 zen/model-a → 实际是 openrouter/model-b 应答的
→ 续写却发给了 zen/model-a
```

后果：续写的内容**接不上**，还浪费一次上游调用。

**修法**：用路由结果里的 `result.Lane` / `result.Model`
（那才是**实际服务**的模型）。

### 5.3 端到端验证结果

```
上游被调用 2 次 ✅
第二次请求带 __fmh_continue 工具调用 ✅
客户端拿到的答案：
  "这是一段很长的答案的前半部分，用来测试断流续写。这是后半部分，应该接在前半后面。"
```

**完整、无重复。**（"前半部分"只出现 1 次）

### 5.4 一个假警报

第一次跑失败时，输出里出现了 `ef bf bd`（U+FFFD 替换字符）。

**排查后确认是测试服务器自己的锅** —— 它按 5 **字节**切中文，
把 3 字节的 UTF-8 字符切成了两半，`json.Marshal` 把非法字节替换成 U+FFFD。

**不是网关的 bug**（真实上游的每个 SSE 帧都是完整 JSON 文档）。
但这个排查过程值得记下来：**看到乱码先怀疑自己的测试代码**。

---

## 6. 上游变更自动检测（新建）

### 6.1 为什么需要

免密车道的闸门**只给一句话**：`403 FreeTierError`。

也就是说上游**改了规格**时，症状是"所有模型同时不可用"，
而根因有 **6 种可能**（端点 / UA / 会话头 / 会话 ID / 工具定义 / tool_choice），
错误体**一个都不告诉你**。

更糟的是它**静悄悄地**坏：没有告警、没有日志，
客户端只看到"网关没模型了"。

### 6.2 设计：合规监视器（`internal/lane/conformance.go`）

**定期用**一个**模型探一次**（30 分钟一次，比模型探测长），
然后**分类**：

| 上游的反应 | 结论 | 为什么这么分 |
|---|---|---|
| 200 | **上游正常** | 闸门还认我们 |
| 429 限流 | **上游正常**（只是限流） | ⚠️ 限流**不是**规格问题 |
| 403 FreeTierError | **闸门拒绝** | 规格变了，或出口被封 |
| 网络错误 | **探不到上游** | 不是规格问题 |
| "模型不可用" | **换一个模型再试** | ⚠️ 模型下架 ≠ 闸门坏了 |

⚠️ **最后一条是实测踩出来的**：第一次跑时挑到了
`liquid/lfm-2.5-2.6b:free`，上游说"Model is unavailable"，
但其它模型完全正常 —— 一次就下结论会得到**假的警报**。

**挑模型也要讲究**：优先挑**探测结论是"可用"**的，
而不是"没探测过的"（后者可能挑到刚下架的）。

### 6.3 界面

「上游源」页顶部一条**状态灯**：

```
● 上游正常    big-pickle    [2026-10-19 23:49]  [立即检查]
```

红灯时下面多一条说明：

```
⚠ 上游闸门拒绝 —— 免密车道的闸门只给一句 403 FreeTierError，不告诉你哪一条不对。
  常见原因：① 上游改了指纹规格 —— 改 internal/lane/zen_fingerprint.go；
            ② 出口 IP 被风控 —— 换个出口，或开「跨车道回落」用 OpenRouter 兜底。
```

### 6.4 API

```
GET  /api/conformance   → {state, label, model, detail, checkedAt, intervalSec}
POST /api/conformance   → 立即检查一次（会消耗一次上游推理）
```

### 6.5 10 个测试

覆盖：闸门拒绝 / 限流不误报 / 网络不误报 / 成功 / 挑限流最少的 /
不探付费模型 / 模型级失败会换模型重试 / 没有备选时如实报未知 /
回调触发 / 汇总。

### 6.6 实测

```
POST /api/conformance
→ 200, 0.7s
  状态: ok
  模型: big-pickle
  说明: 上游正常（只是限流）     ← ⚠️ 限流被正确区分了
```

UI：`dotClass = "status-dot … bg-[var(--ok)]"`，无 JS 错误。

---

## 7. 汇总：这一轮抓到的 8 个 bug

| 严重度 | bug | 影响 |
|---|---|---|
| **严重** | 跨车道回落的开关**形同虚设** | 开关关了照样消耗用户自己的 OR 额度 |
| **严重** | mandatory 模型发 `enabled:false` → **400** | OpenRouter 的一部分模型完全不可用 |
| **严重** | `NormalizeEffort` 把"关掉思考"归并成 `light` | "关掉思考"的请求**静默失效** |
| 中 | 续写发给了**请求的**模型而不是**实际服务的**模型 | 故障转移后接不上，白费一次调用 |
| 中 | `reasoning_tokens` 全部丢失 | 客户端看不到思考占了多少 token |
| 中 | `FetchQuota` 从没被调用 | 额度栏永远空白 |
| 中 | 移动端表格被**裁断** | 375px 下 5 列完全看不见 |
| 低 | `401 Missing API key` 被当成凭据级错误 | 换模型没用，白烧一整轮候选 |

另外新增的**能力**：

- 上游合规监视器（自动检测上游改规格）
- 窄屏卡片布局
- 50 并发压测 + 5 个 Go 层并发测试
- 断流续写的端到端验证

---

## 8. 现在的不足

| 项 | 状态 |
|---|---|
| **`go test -race` 跑不了** | ⚠️ Windows 上需要 POSIX 线程版 MinGW，本机只有 win32 线程版。已用 5 个并发测试 + `-count=20` 代替，但不是完全等价 |
| **合规监视器的"闸门拒绝"没在真故障下验证过** | ⚠️ 逻辑有 10 个测试覆盖，但**没有真的让上游 403 过**（不能人为让上游拒绝） |
| **跨车道回落没在"免密真被 IP 封"下验证过** | ⚠️ 用假车道模拟了 `EGRESS_BLOCKED`，真实 IP 封禁没遇到过 |
| **OpenRouter 的 `usage.cost` 没透出** | ⚠️ 上游给了 `cost: 0`，FMH 没显示"这次花了多少钱" |
| **移动端没在真机验过** | ⚠️ 用 375px 模拟器验了 DOM，没有真实手机 |
| **没有 arm64 构建** | ✅ **不需要**（用户的 NAS 是 x86） |
| **上游变更检测只有"闸门层面"** | ⚠️ 能发现"闸门坏了"，发现不了"工具定义变了"这类**静默降级** |
| **合规探测间隔固定 30 分钟** | ⚠️ 不能在设置里改 |
| **没有限速/配额的自适应** | ⚠️ 探测的批量大小是固定的，不会根据限流情况自动调 |
