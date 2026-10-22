# 我们到底在伪造什么

> 这是理解 FMH 的**心智模型**。
> 写给未来的自己，也写给项目的使用者。

---

## 一句话

> 免密车道的上游不是"不要 key"，而是"**只发给真正的 OpenCode 客户端**"。
> 所以我们要伪造的不是**身份**、不是**账号**，而是**客户端的线路形状**。

---

## 一、不是冒充用户，不是偷账号

| 误解 | 事实 |
|---|---|
| "在冒充 OpenCode 用户" | 凭据是字面量 `Bearer public` —— **全网所有人共用一个匿名池** |
| "在偷别人的账号" | 免密车道**根本没有账号体系** |
| "在绕过安全防护" | 它是**产品限制**（免费额度是补贴的），不是安全边界 |

**但它确实是一个限制，而绕过它是灰区。** FMH 照实写明，不美化。见 §5。

---

## 二、要伪造的四样东西

| # | 伪造什么 | 精确形状 | 不伪造的后果 |
|---|---|---|---|
| 1 | **User-Agent** | `opencode/latest/2.0.26/cli`（形状 `opencode/<channel>/<version>/<client>`，major ≥ 2，**整串匹配**） | 403 |
| 2 | **会话头** | `x-opencode-session` / `x-opencode-session-id` / `x-session-affinity` / `x-session-id` 四者**同值** | 403 |
| 3 | **会话 ID 编码** | `ses_` + 12 位 hex 是**降序时间戳** `~(ts_ms<<12 \| counter)`，counter 落在 1..63 | 403 |
| 4 | **工具名** | 必须声明 `read` 与 `shell`（**大小写精确**），两个都要 | 403 |
| 5 | **流式协议** | `stream: true` | 403 |

外加一条硬性禁止：**绝不下发 `tool_choice: "none"`** ——
那会让模型这一轮不调工具。

> ⚠️ **闸门**只比**名字**，**不**比 description 和 schema ——
> 见 [`upstream-conformance.md`](../verification/upstream-conformance.md) §2.6 的实测。
> 这里仍然用官方原文，是**保守**（万一上游以后开始比对），
> 不是**必需**。

---

## 三、为什么这不是"随便写个字符串就行"

上游验证的是**动态的东西**，不是静态字符串。

### 会话 ID：它会解码

上游的私有推理代理**解码**那 12 位 hex，验证它能否还原成一个**新鲜的时间戳**：

| 编码 | 结果 |
|---|---|
| 降序 `~(ms<<12\|seq)`，counter 1..63 | ✅ |
| 升序 `ts << 12` | ❌ |
| sha256 摘要 | ❌ |
| 随机 hex | ❌ |
| counter ≥ 100 | ❌ |

**形状对（`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`）远远不够。**

### 工具定义：用官方原文

v1 时代只要"四个工具名 + 至少一个 property"就行。v2 变成
**`read` + `shell` 两个，名字大小写精确**。

> ⚠️ **实测（2026-10-22）的修正**：
> 之前这里写的是"description 与 schema **逐字一致**" ——
> **那是错的**。把两个工具的描述**全部乱写**、schema 整个省略，
> 照样 200。闸门**只认名字**。
>
> 但用官方原文仍然保留 —— 保守，且描述的是真实语义。

**桩工具 + 占位 property 的老做法已彻底失效**（那会儿是名字都不对）。

### 所以上游在做什么

它在**模拟"真实客户端会怎么行为"** —— 不是查一张静态白名单。
这意味着：**精确复现官方客户端的行为**才是正确策略，而不是猜。

---

## 四、注入的工具是纯指纹，上游执行不了

上游是远程网关 —— 它**跑不了**你本地的 `read` / `bash`。

所以声明它们纯粹是为了证明"我是真客户端"。

**这带来一个反直觉的后果**：因为定义是**真的**（v1 那种"must not be used"的桩描述已失效），
模型会**真的去调**它。实测模型会拿 `read` 回答"看看 /etc 里有什么文件"。

⇒ 必须补一句系统提示压住它：

```
Do not call the `read` or `shell` tools; they are placeholders that do nothing.
Use the other tools you have been given instead.
```

**措辞有讲究**：
- 写成"这些工具**不可用**" → 模型会连**调用方自己的** `Read`/`Bash` 也一起拒绝
- 写成"这些是**占位符、请用别的工具**" → 既压住注入项、又不误伤调用方的工具

实测证据（`minimax-m3.1-flash-free`，2026-10-19）：
思考过程原文 —— *"The user wants exactly \"FMH-OK\". I should reply with that. **No tool calls needed.**"*

它认得这两个工具，但选择不用。

---

## 五、⚠️ 闸门有**两层**，伪造只能过第一层

这是实测（2026-10-19）最重要的发现。

同一台机器、同一出口 IP，只改"有没有带指纹"：

| 请求 | 上游回应 |
|---|---|
| **不带指纹** | `"OpenCode's free tier can only be used from within OpenCode."` |
| **带完整指纹** | `"OpenCode's free models aren't available from your current network because we've detected **abusive traffic from this IP address**. This isn't related to your account. If you're on a VPN or proxy, turn it off and try again..."` |

**两句都是 403、都是 `FreeTierError` type —— 但指向完全不同的层：**

```
第一层：身份闸门   "can only be used from within OpenCode"
       → 伪造指纹能过   ✅ FMH 已通过
       → 改了指纹也没用（因为指纹本来就对）

第二层：IP 信誉    "detected abusive traffic from this IP address"
       → 伪造指纹没用   ❌ 只能换出口
       → 与你的账号无关
```

### 这个区别决定了三件事

**① 错误码必须分开**

```go
CodeFreeTier     Code = "FREE_TIER_BLOCKED"  // 身份闸门：改指纹有用
CodeAbuseBlocked Code = "EGRESS_BLOCKED"     // IP 风控：改指纹没用，换出口有用
```

混淆它们会让你"改了半天指纹，其实一直是对的"。

**② 必须有第二条车道**

IP 被风控时，**整条免密车道**都不可用 —— 换模型救不了。

实测（同一时刻）：10 个免费模型里 **6 个 RATE_LIMIT、1 个 REGION_BLOCKED、
只有 1 个能答**。

⇒ 只有一个免密车道的网关，IP 一被风控就死了。
⇒ **OpenRouter 车道是必需项，不是可选项**。

**③ "清单能拉到" ≠ "能用"**

上游清单 85 个模型，**免费车道只有 10 个**，而当下能答的只有 1 个。
模型清单必须配**实测**与**限流账本**。

---

## 六、对 FMH 架构的直接后果

| 后果 | 设计 |
|---|---|
| 指纹规格是**易碎品** | 全部集中在 `internal/lane/zen_fingerprint.go` **一个文件**，配回归测试锁住字面量 |
| 上游改规格 | 只改那一个文件；测试全绿才能发版 |
| 闸门拒绝时的错误提示 | 必须说"上游闸门/风控变了"，**不能**说"你的 key 错了" |
| 两条车道 | 免密（`zen`）+ OpenRouter（`openrouter`），共享路由/限流/看板 |
| 自动跨模型调用 | 模型失败 → 自动换（默认关；切到 OpenRouter 会消耗用户自己的额度） |

---

## 七、诚实声明（FMH 必须写进 README 与 UI）

- FMH 是**客户端/网关**，不是模型服务方。所有输出由第三方上游生成。
- **免费不等于无限，也不等于私有。** 额度由上游单方面提供，随时可能限流、收费或下线。
- 免密车道的 prompt **可能被上游记录**。请勿发送密码、密钥、个人身份信息或商业机密。
- 上游对免费车道的使用设有**客户端限制**，FMH 通过复现官方客户端的线路行为来满足它。
  这是一个**灰区**，使用者应自行判断是否符合其服务条款。
- 生产与敏感场景请使用你自己的付费 API。
- FMH 与任何上游厂商无隶属或背书关系。
