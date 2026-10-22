# 上游行为实测记录

> 本文件记录 FMH 开发期间对上游的**实测**。
> 每条都标注了**验证方式**和**日期**，以及**怎么重新验证**。
>
> ⚠️ 这些事实是**易碎品**。上游随时可能改。
> 规格类事实的**权威副本**在 `docs/design/01-zen-fingerprint.md`；
> 本文件是**证据**。

---

## 2026-10-14 — v1 → v2 的四维闸门

**验证方式**：TLS 中间人（自签 CA，`NODE_EXTRA_CA_CERTS`）抓官方客户端的真实请求，
再做**逐字节重放**做单变量对照（每次只删/改一项，记录 200/403）。

| 项目 | v1 | v2 |
|---|---|---|
| UA | `opencode/1.18.31` | `opencode/latest/2.0.26/cli` |
| 请求头 | 带 `x-opencode-request` | **不带**；改带 `x-session-affinity` / `x-session-id` |
| 工具闸门 | `bash`/`glob`/`grep`/`read`，各带至少一个 property | **`read` + `shell`**，名字大小写精确 |
| `tool_choice` | 无工具时补 `none` | 不被拒（实测 200），但 `none` 会让模型不调工具 |

**会话头**：v2 要求 `x-opencode-session-id` 与 `x-opencode-session` **同时存在**。
去掉其中一个 → 403。

**工具闸门的矩阵实验**：

```
bash 单独                     → 403
read 单独                     → 403
bash + read，schema 为空       → 403
bash + read，各带一个 property → 200
Read / BASH（大小写变体）        → 403
```

**UA 里的禁用 token**：含 `deepseek-harness`（大小写不敏感、位置无关）→ 403。

**端点迁移**：聊天端点移到 `/inference/v1/chat/completions`。
`/zen/v1/chat/completions` 即使其余全对也 403。
但 `/inference/v1/models` 与 `/zen/v1/models` **都仍返回清单**。

> ⚠️ **这意味着"清单能取到"不代表"对话能通"**。
> 冒烟测试必须打一次真实的对话。

---

## 2026-10-14 — 配额按 session 计费

**现象**：每请求换一个 session id → 立刻 429，且 `retry-after` 越来越长。

**结论**：一个下游会话必须绑定**一个稳定**的上游 session。

**旁证**：多出口场景下实测 720M 输入 token、**零缓存命中** ——
每次换出口把会话打散了，prompt cache 全废。

---

## 2026-10-16 — 共享凭据的 429 是"此刻拥挤"

**测量**：92 次记录的限流事件里，**最短的一次只持续 0.6 秒**；
`space-bunny-free` 和 `longcat-2.5-preview-free` 都在两分钟内重新 200。

**结论**：无 `Retry-After` 时的冷却设为 **10 秒**，不是 60 秒。
冷却过长会让面板显示"不可用"而模型其实好好的
（因为候选链会跳过被限流的模型）。

**另一条**：**重试一个被拒的请求不消耗配额** ——
所以便宜的方向就是短冷却。

---

## 2026-10-17 — 会话 ID 的**编码**也要对

**现象**：`ses_` + 12 hex + 14 base62 的**形状**完全正确，仍然 403。

**原因**：上游的私有推理代理会**解码**那 12 位 hex，
验证它解出一个**新鲜的**时间戳。

**接受的编码**：`~(ts_ms << 12 | counter)`，counter 在 **1..63**。

| 编码 | 结果 |
|---|---|
| 降序 `~(ms<<12\|seq)`，counter 1..63 | ✅ |
| 升序 `ts << 12` | ❌ |
| sha256 摘要 | ❌ |
| 随机 hex | ❌ |
| counter ≥ 100 | ❌ |

**对照实验**：同一出口 IP 下，官方 2.0.32 CLI 的**逐字节相同**的请求通过，
而一个新鲜的升序 id 403 —— 所以这是**身份校验**，不是 IP 限速。

**上游容忍的新鲜度窗口**：约一周。所以重铸周期取 6 天。

---

## 2026-10-XX — 思考参数**被接受但被忽略**

**测量**：三种拼写各采样三次 ——
`reasoning_effort`、`thinking.budget_tokens`、`enable_thinking`、`thinking_budget`
**全部被接受然后被忽略**：`low` 产生的推理 token 反而**多于** `xhigh`，
而且未知字段偶尔引发上游 503。

**唯一真正生效的控制是 `max_tokens`**，
而推理量直接跟随它（64 token 的上限把推理从 ~397 拉到 ~63）。

**推论**：effort 档位必须实现为**输出预算**，不能是一个"提示"。

**强制思考的模型**：`mimo-v2.6-flash-free` 一天 92 次调用里，
**82% 的输出 token 是推理**。8192 的上限只剩 ~1500 给答案，
长回合约每三次就有一次以 `length` 结束。
→ 不可关闭思考的模型，整档上限 **×2**。

---

## 2026-10-XX — 空闲 vs 总时长

**测量**：健康回合每几秒给一个 chunk、7-62 秒完成。
一次卡死的回合沉默**超过 5 分钟**，把整个 210 秒重试预算耗光 ——
结果**什么都没重试**，客户端只看到自己的取消。

**结论**：限制**沉默**（90 秒无字节），不限制**总时长**。
每次读到 chunk 就重置计时器。

---

## 2026-10-XX — 工具配对是"毒丸"

**现象**：一个被中断的回合在持久化历史里留下悬空的 tool_call。
重放它 → 上游 `400 [invalid_request_error]`，而且
**这个会话之后每一轮都失败**（它住在历史里）。

**推论**：每次发送前**必须**做配对修复：
删掉悬空的 tool_call、孤立的 tool_result、名字为空的 tool_call。

---

## 2026-10-XX — 5xx 不能算在模型头上

**现象**：`Service Unavailable` 是**反向代理的理由短语**。
按它匹配把一个**正在工作**的模型从选择器里摘掉了。

**结论**：5xx / 配额 / 传输故障 → 结论保持 `unknown`，**不改变成员资格**。
只有**指名了模型**的拒绝（400/404/422、或消息里点了模型名）才记在模型头上。

**对照的不对称**：
- 过期条目 → 一次可重试的失败回合
- 抖动导致的消失 → 用户经历一整轮看不见的重新探测

前者便宜得多。

---

## 2026-10-XX — 响应体的形状比 Content-Type 可信

**现象**：上游用 `application/json` 的 Content-Type 回答**流式**请求。

**结论**：读前 4096 字节**嗅探形状**（`sse` / `json` / `empty` / `unknown`），
并且把嗅探到的字节**replay** 回流里（不吞掉、不缓冲）。

---

## 2026-10-XX — 429 不能自动重试

**现象**：这条车道的 429 带**越来越长**的 retry-after。
两次自动重试把一个配额墙变成**三道墙** —— 同一个回合付了三次。

**结论**：可重试 = `EMPTY_RESPONSE` / `SERVER` / `TIMEOUT` / `TRANSPORT`；
**429 走"换模型"，不走"重试"**。

---

## 怎么重新验证

```bash
# 1. 拿清单
curl -s https://opencode.ai/inference/v1/models | jq '.data | length'

# 2. 真的打一次对话（这是唯一的验收标准）
#    带上完整的指纹：UA、四个会话头、铸造的 session id、read/shell 工具定义

# 3. 观察响应头里的实际服务模型
curl -i ... | grep -i x-fmh-model
```

**单变量对照法**（找出是哪一条闸门在拒绝）：

```
1. 从一份已知能通过的真实请求出发
2. 每次只改/删一项
3. 记录 200/403
4. 变体矩阵：bash 单独 / read 单独 / 空 schema / 各带一个 property / 大小写变体
```

---

## 2026-10-19 — OpenRouter 的思考档位语义（真实 Key 实测）

**验证方式**：真实 Key 直接调 `https://openrouter.ai/api/v1`，逐项对照。

### 1. 额度与清单

```
GET /api/v1/key    → 200  limit=$0.01  usage=$0  is_free_tier=true
GET /api/v1/models → 469 个模型，18 个免费
```

> ⚠️ **没带 Key 时 `/models` 也能列**（不需要认证），
> 但 `/key`（查额度）和 `/chat/completions`（推理）**必须**带 Key。
>
> 这印证了"OpenRouter 的免费模型只是特价档，不是免 key"。

### 2. `reasoning` 的三种形状

```json
"reasoning": {"mandatory": true,  "supported_efforts": ["max","xhigh","high","medium","low"], "default_effort": "max"}
"reasoning": {"mandatory": true,  "supported_efforts": [], "default_effort": null}
"reasoning": {"mandatory": false, "supported_efforts": ["medium","low","none"], "default_effort": "medium"}
"reasoning": {"mandatory": false, "supported_efforts": [], "default_effort": null}
```

18 个免费模型里：
**2 个** `mandatory=true`、**3 个**有档位、**1 个**的档位里含 `none`。

### 3. ⚠️ **关掉思考的实测矩阵**（最重要的一条）

| 模型类型 | `effort:none` | `enabled:false` | `effort:low` |
|---|---|---|---|
| `mandatory=false` | ✅ 200，`reasoning_tokens=0` | ✅ 200，`reasoning_tokens=0` | ✅ 200 |
| `mandatory=true` | ❌ **400** | ❌ **400** | ✅ 200 |

400 的原文：

```json
{"error":{"message":"Reasoning is mandatory for this endpoint and cannot be disabled.","code":400}}
```

> ⚠️ **两个坑**：
>
> ① `mandatory=true` 的模型**不能**发任何"关掉"的语义 ——
>    连 `effort:none` 也会 400。
>    所以 `mandatory` 必须**先**判。
>
> ② `mandatory=false` 但**没声明档位**的模型，
>    发 `effort:high` 是 200，但**没有任何效果**
>    （`reasoning_tokens` 和"不发"时差不多）——
>    所以对这类模型**不发** effort 更诚实。

### 4. `usage` 的形状

```json
{"prompt_tokens": 206, "completion_tokens": 40, "total_tokens": 246,
 "cost": 0, "is_byok": false,
 "prompt_tokens_details": {"cached_tokens": 195, ...},
 "cost_details": {...},
 "completion_tokens_details": {"reasoning_tokens": 39, "image_tokens": 0, "audio_tokens": 0}}
```

> ⚠️ **顶层没有 `reasoning_tokens`** ——
> 只读顶层会永远得到 0。
> 必须读 `completion_tokens_details.reasoning_tokens`。

**流式响应里也有这个字段**（在最后一帧的 `usage` 里），
所以流式和非流式都能拿到。

### 5. 各档位的实际效果（`minimax/minimax-m3.1-flash:free`）

```
effort=low     → 200  reasoning_tokens=0
effort=medium  → 200  reasoning_tokens=0
effort=high    → 200  reasoning_tokens=10
effort=xhigh   → 200  reasoning_tokens=16
```

> ⚠️ `low` 和 `medium` 实测都是 0 —— **低档位可能直接不思考**。
> 别把"档位生效"和"一定思考"混为一谈。

### 6. 怎么重新验证

```bash
KEY=sk-or-v1-...

# 额度
curl -s -H "Authorization: Bearer $KEY" https://openrouter.ai/api/v1/key

# 某个模型的 reasoning 声明
curl -s https://openrouter.ai/api/v1/models | jq '.data[] | select(.id|endswith(":free")) | {id, reasoning}'

# 档位矩阵（换模型名重跑）
for EFF in none low medium high xhigh; do
  curl -s -X POST https://openrouter.ai/api/v1/chat/completions \
    -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
    -d "{\"model\":\"minimax/minimax-m3.1-flash:free\",
         \"messages\":[{\"role\":\"user\",\"content\":\"2+2?\"}],
         \"max_tokens\":200,\"reasoning\":{\"effort\":\"$EFF\"}}" \
    | jq '{err:.error.message, rt:.usage.completion_tokens_details.reasoning_tokens}'
done
```

---

## 2026-10-19 — 免密车道的模型级失败

**验证方式**：自动探测调度器对 10 个免费模型各探一次。

| 模型 | 状态 | 上游原话 |
|---|---|---|
| `minimax-m3.1-flash-free` | available | — |
| `deepseek-v4.1-flash-free` | throttled | `Rate limit exceeded. Please try again later.` |
| `big-pickle` | throttled | 同上 |
| `beam-free` | **region-blocked** | `This model is not available in your country` |

> ⚠️ **`region-blocked` 是模型级的**，不是车道级的 ——
> 换同车道的其它模型**有用**。
> 而 `EGRESS_BLOCKED`（IP 风控）才是车道级的，换模型**完全没用**。
>
> 这两种在 `FREE_TIER_BLOCKED` 与 `EGRESS_BLOCKED` 下是**不同的错误码**，
> 路由器据此决定"要不要换车道"。

---

## 2026-10-19 — 指纹诊断实测：**只有 UA 被管**

**验证方式**：FMH 自己的单变量对照诊断器（`/api/diagnostics`）打真实上游。
基线 + 6 个变体，每个变体**只改一项**。

| 变体 | 结果 | 上游原话 |
|---|---|---|
| 基线（不改任何东西） | **200** | — |
| 端点路径 → `/zen/v1/chat/completions` | 200 | — |
| **User-Agent → `opencode/latest/1.0.0/cli`** | **426** | `OpenCode 1.18.0 or newer is required to use the free tier` |
| 会话头（去掉 `x-opencode-session-id`） | 200 | — |
| 会话 ID 编码（打乱降序） | 200 | — |
| `read`/`shell` 工具定义（改写描述） | 200 | — |
| `tool_choice` → `"none"` | 200 | — |

**推论**：

1. ⚠️ **当前上游只检查 UA**。
   文档里的 6 条规矩**仍然要照做** ——
   上游随时可能加严，而且我们**无法预知**它会加严哪一条。
   "现在没被管"≠"可以乱来"。

2. ⚠️ **失败状态码是 426，不是 403**。
   上游用 `426 Upgrade Required` 表达"你的客户端版本太老"。
   这跟 `403 FreeTierError` 是**不同的失败**，但两者都归 `FREE_TIER_BLOCKED`。

3. ⚠️ 诊断器**必须用生产同款构造**才能得到可信结论 ——
   详见 `docs/design/01-zen-fingerprint.md` §10.2。

**重新验证**：

```bash
curl -X POST http://<NAS-IP>:8678/api/diagnostics \
  -H 'Content-Type: application/json' \
  -b 'fmh_session=<管理会话 cookie>' \
  -d '{"model":"minimax-m3.1-flash-free"}'
```

---

## 记录格式约定

后续实测请按这个格式追加：

```markdown
## YYYY-MM-DD — 一句话结论

**测量方式**：（TLS 中间人 / 逐字节重放 / 对照实验 / 生产观察）

**结论**：

| 变体 | 结果 |
|---|---|

**推论**：对 FMH 的设计意味着什么。
```
