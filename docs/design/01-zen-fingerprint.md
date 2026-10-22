# 免密车道规格：OpenCode Zen 客户端指纹闸门

> 这是 FMH **最关键**的技术文档。上游的免费车道不是"不要 key"，
> 而是"**只接受真正的 OpenCode 客户端**"。这一章把闸门的每一维写清楚，
> 因为**任何一条不满足，整个车队 403**。

本文档的事实来源是**两个独立项目的交叉验证**：

- **OpenCode 官方客户端的源码**（MIT，Copyright (c) 2025 opencode）
  —— `packages/core/src/tool/plugin/read.ts`
  —— `packages/core/src/tool/plugin/shell.ts`
  —— `packages/core/src/session/model-request.ts`
- **FMH 自己的实测**（2026-10-19 ～ 21）

详见 `docs/verification/upstream-conformance.md`。

---

## 0. 闸门总览

| # | 维度 | 不满足的后果 |
|---|---|---|
| 1 | **端点路径** | `403 FreeTierError`（旧的 `/zen/v1/*` 对免费 id 一律 403） |
| 2 | **User-Agent** | `403 FreeTierError`（整串匹配，任何前后缀都不算） |
| 3 | **会话头四件套** | `403 FreeTierError`（缺一即拒） |
| 4 | **会话 ID 编码** | `403 FreeTierError`（形状对但编码不对也拒） |
| 5 | **流式协议** | `403 FreeTierError`（`stream: false` 即拒） |
| 6 | **工具名** | `403 FreeTierError`（必须声明 `read` + `shell`，**大小写精确**） |
| 7 | **`tool_choice`** | 不被拒（实测 200）—— 但 `"none"` 会让模型这一轮不调工具，要清掉 |
| 8 | **凭据** | `Bearer public` —— 共享凭据，不是用户 key |

还有两个**非闸门**但会影响可用性的行为：

- **配额按 session 记账** → 每请求换 session 会立刻 429
- **共享凭据的 429 是"此刻拥挤"** → 冷却要短（10 秒），不能 60 秒

### 闸门**不**比什么（实测，2026-10-22）

| 维度 | 实测结果 |
|---|---|
| **工具 description 的任何一个字** | 全部乱写照样 200 |
| **parameters / schema** | `properties: {}` 或整个省略，都照样 200 |
| **多余的第三个工具** | 照样 200 |
| **`accept`** | `application/json` 也照样 200 |
| **`x-opencode-client`** / **`x-opencode-project`** | 去掉也照样 200 |
| **system 占位提示** | 去掉也照样 200 |

> ⚠️ **测法很关键** ——
> 必须**绕过 FMH 网关直接打上游**。
> 通过网关发的请求会被网关**重写**（加指纹、选流式），
> 测的一直是"FMH 自己的输出"，**不是上游的判定**。

---

## 1. 端点路径

| 模型类型 | 端点 |
|---|---|
| 默认（绝大多数） | `https://opencode.ai/inference/v1/chat/completions` |
| `muse-spark-*` | `https://opencode.ai/inference/v1/responses` |
| `union-alpha` | `https://opencode.ai/inference/v1/messages` |
| 模型清单 | `https://opencode.ai/inference/v1/models` |

> ⚠️ **v1 → v2 迁移**：2026-10-14 前后，免费车道从 `/zen/v1` 移到 `/inference/v1`。
> `/zen/v1/chat/completions` 即使其余全对也返回 403；但 `/zen/v1/models` 与
> `/inference/v1/models` 都仍返回模型清单（所以"清单能拿到"不代表"对话能通"）。

**FMH 的实现**（`internal/lane/zen.go`）：

```go
func ZenEndpointFor(modelID string) string {
    switch {
    case IsMuseSpark(modelID):  return "/inference/v1/responses"
    case IsUnionAlpha(modelID): return "/inference/v1/messages"
    default:                    return "/inference/v1/chat/completions"
    }
}
```

---

## 2. User-Agent

```
opencode/latest/2.0.26/cli
```

**形状规则**：`opencode/<channel>/<version>/<client>`，且 **major ≥ 2**。

**三条硬性约束**（每一条单独违反即 403）：

1. **整串匹配** —— 不能有任何前后缀。
   所以**不能**把自己的归属 UA 拼上去（`... (free-model-house; +https://...)` 就不算了）。
   想保留归属，就放在**别的头**里，或者放在 `x-opencode-project`。
2. **major ≥ 2** —— `opencode/1.18.31` 已被按名字拒掉。
3. **UA 里不能出现某些 token** —— 包含 `deepseek-harness`（大小写不敏感、位置无关）→ 403。
   （踩过这个坑 —— 归属 UA 原本含 `deepseek-harness`，
   被改成 `dsh` 才通过。）

> **维护提示**：这个字符串是**易碎品**。上游随时可能升级版本号。
> 把它放在**一个常量**里（`internal/lane/zen_fingerprint.go` 的 `ZenClientUA`），
> 并配一个回归测试断言它仍符合 `opencode/<channel>/<version>/<client>` 形状。

---

## 3. 会话头四件套

**四个头必须同时存在，且值相同**：

```http
x-opencode-session:    ses_xxxxxxxxxxxxxxxxxxxxxx
x-opencode-session-id: ses_xxxxxxxxxxxxxxxxxxxxxx
x-session-affinity:    ses_xxxxxxxxxxxxxxxxxxxxxx
x-session-id:          ses_xxxxxxxxxxxxxxxxxxxxxx
```

**另外两个头**：

```http
x-opencode-client:  cli
x-opencode-project: global
```

**绝对不能发的头**：

```http
x-opencode-request: ...     # ← v1 专用头，v2 收到即 403
```

> **v1 → v2 的头差异**：
> | | v1 | v2 |
> |---|---|---|
> | UA | `opencode/1.18.31` | `opencode/latest/2.0.26/cli` |
> | 特有头 | `x-opencode-request` | ❌ 不发 |
> | 新增头 | — | `x-session-affinity` / `x-session-id` |

---

## 4. 会话 ID 编码（**最容易做错的一维**）

### 4.1 形状

```
ses_ + 12 位 hex + 14 位 base62
```

正则：`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`

### 4.2 编码（**形状对还不够**）

那 12 位 hex 必须是**降序编码的时间戳**：

```
value = ~(ts_ms << 12 | counter)
hex   = value 的 48 位大端十六进制
```

于是低 12 位是 `0xFFF - counter`。

### 4.3 counter 窗口

上游会**解码**那 12 位 hex，检查里面的 counter：

| counter | 结果 |
|---|---|
| 1 – 63 | ✅ 通过 |
| ≥ 100 | ❌ 403 |
| 随机 hex | ❌ 403 |
| sha256 摘要 | ❌ 403 |
| 升序 `ts << 12` | ❌ 403 |

**为什么随机/哈希不行**：上游的私有推理代理会**解码** session id 的 12 位 hex 前缀，
验证它是否解码为一个**新鲜的**时间戳。sha256 派生出来的前缀永远解不出合法时间戳。

**为什么不能每次请求换 session**：配额按 session 记账。
每个请求一个新 session 会立刻耗尽配额，表现为 429 且 `retry-after` 越来越长。

### 4.4 FMH 的实现

```go
const (
    zenBase62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
    // 上游容忍约一周的新鲜度窗口，六天后重铸留出余量。
    zenSessionRemintAfter = 6 * 24 * time.Hour
)

var zenLastStamp atomic.Int64
var zenSeq       atomic.Int64

// ZenMintSessionID 铸造一个上游接受的会话 ID。
func ZenMintSessionID(nowMS int64) string {
    if nowMS <= 0 { nowMS = time.Now().UnixMilli() }
    // 计数器每毫秒归零 → 突发铸造也不会跑出 1..63 窗口
    prev := zenLastStamp.Swap(nowMS)
    if prev != nowMS { zenSeq.Store(0) }
    seq := zenSeq.Add(1)

    value := ^(uint64(nowMS)<<12 | uint64(seq&0xFFF))
    var raw [6]byte
    for i := 0; i < 6; i++ { raw[i] = byte((value >> (40 - 8*i)) & 0xFF) }
    rnd := make([]byte, 14); rand.Read(rnd)
    return "ses_" + hex.EncodeToString(raw[:]) + base62From(rnd)
}
```

### 4.5 会话稳定性

```
下游会话 seed  →  上游 session id
```

- `seed` 的构造：`client_id + "\x00" + 首条用户消息前 200 字符`
  （取首条消息是为了让同一话题复用同一 session，命中 prompt cache）
- 映射表**持久化到 SQLite**（表 `sessions`），重启后复用，否则 prompt cache 每次冷启动
- 超过 `zenSessionRemintAfter`（6 天）重铸一次，留在上游的新鲜度窗口内
- 表容量上限 4096 条，满了整体重置（它是稳定缓存，不是值得精细淘汰的状态）

---

## 5. 工具指纹闸门

### 5.1 规则

**必须声明 `read` 与 `shell` 两个工具**（名字大小写精确）。

> ⚠️ 描述与 schema **不**需要逐字一致 —— 实测（2026-10-22）乱写也 200。
> 这里用官方原文是**保守**（万一上游以后开始比对），不是**必需**。

> **v1 → v2 的变化**：
> | | v1 | v2 |
> |---|---|---|
> | 工具 | `bash` / `glob` / `grep` / `read` 四个，各带至少一个 property | **`read` + `shell` 两个，名字精确** |
> | 桩工具 | 允许"占位 schema + 任意 property" | **不允许**（v2 比对定义本身） |

### 5.2 官方定义（**逐字，不可改写**）

```go
var ZenFingerprintTools = []string{"read", "shell"}

var zenReadDescription = "Read the contents of a file or directory. Supports text files, images, and PDFs. Images and PDFs are presented directly to the model. Each text line is prefixed by its 1-based line number as <line>: <content>. The prefix is for reference and is not part of the file content. Directory entries are returned one per line. Use offset and limit to read large files or directories in sections. Prefer one larger read over many small slices, and use grep to find specific content in large files."

var zenReadParameters = map[string]any{
    "type": "object",
    "properties": map[string]any{
        "path":   map[string]any{"type": "string", "description": "File or directory to read"},
        "offset": map[string]any{"type": "integer", "minimum": 0, "description": "The line or directory entry to start reading from (1-based)"},
        "limit":  map[string]any{"type": "integer", "minimum": 0, "description": "The maximum number of lines or directory entries to read (defaults to and capped at 2000)"},
    },
    "required":             []any{"path"},
    "additionalProperties": false,
}

var zenShellDescription = "Execute a shell command and return its output. Commands run on Linux using bash. Quote file paths containing spaces or special characters. Prefer dedicated tools over shell commands when possible. When output is large, the full result is saved to a file and a truncated preview is returned. Rely on automatic truncation unless filtering the output is more useful. Commands accept an optional timeout, background commands have no timeout by default. Background commands return immediately, and you will be notified when they complete."

var zenShellParameters = map[string]any{
    "type": "object",
    "properties": map[string]any{
        "command":    map[string]any{"type": "string", "description": "Shell command string to execute"},
        "workdir":    map[string]any{"type": "string", "description": "Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead."},
        "timeout":    map[string]any{"type": "integer", "minimum": 0, "description": "Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands. Background commands have no timeout by default."},
        "background": map[string]any{"type": "boolean", "description": "Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use the '&' at the end of the command when using this parameter. You will be notified when it completes. DO NOT poll for completion."},
    },
    "required":             []any{"command"},
    "additionalProperties": false,
}
```

> ⚠️ **不要为了"更好看"改写这些字符串。**
> 用官方原文最稳 —— 万一上游以后开始比对，也不会炸。
> （实测当前**不**比对，但保守没坏处。）
> 把它们放在一个文件里，配一个断言"这些字符串没被改过"的测试。

### 5.3 调用方工具冲突的处理

调用方（Claude Code 等）可能自己有 `read` / `shell` 工具，但定义不同。
**三个错误做法**：

| 做法 | 为什么错 |
|---|---|
| 直接覆盖调用方的工具 | 毁掉调用方的能力 |
| 把调用方的定义改写成官方定义 | 调用方的工具语义变了（它可能真的是 `Read` 文件） |
| 把调用方的 `Read`（大写）按小写归并成 `read` | 正好撞上"闸门只认小写精确名"的判定 → 403 |

**正确做法：别名 + 响应侧还原**

```
1. 遍历调用方 tools：
   - 名字不是 read/shell（小写精确匹配）→ 原样保留
   - 名字是 read/shell → 改名为 <name>_client[N]，记入 renameMap
   - 同名重复声明 → 只留第一个（上游按重复拒）
2. 注入缺失的 read/shell 官方定义（按当前 wire 的形状）
3. 同步改写：
   - 历史消息里的 tool_call 名（否则模型会把自己上一轮的 read 误认为注入的那个）
   - tool_choice 指向的名字
4. 追加系统提示："Do not call the `read` or `shell` tools; they are placeholders
   that do nothing. Use the other tools you have been given instead."
5. 兜底：删除 tool_choice == "none"
6. 响应侧：用 renameMap 把名字还原成调用方的
```

**为什么必须加第 4 步的提示**：v2 注入的是**真工具定义**（v1 那种"must not be used"的桩描述已失效），
模型会真的去调它。实测模型会拿 `read` 去回答"看看 /etc 里有什么文件"。

**提示词措辞有讲究**：
- 写成"这些工具**不可用**"会让模型连**调用方自己的** `Read`/`Bash` 也一起拒绝。
- 写成"这些是**占位符、请用别的工具**"才既压住注入项、又不误伤调用方的工具。

### 5.4 三种 wire 的工具形状

| wire | 形状 |
|---|---|
| Chat | `{"type":"function","function":{"name","description","parameters"}}` |
| Responses（flat） | `{"type":"function","name","description","parameters","strict":false}` |
| Messages（claude） | `{"name","description","input_schema"}` |

Responses 线的工具带 `strict: false`（与官方 CLI 一致）。

---

## 6. `tool_choice`

**绝不下发 `tool_choice: "none"`** ——
那会让模型这一轮不调工具，而我们明明把工具给它了。

- 调用方给了 `tool_choice` 且不是 `none` → 原样带（但被改名的要重定向到别名）
- 调用方没给：
  - Chat 线 → **不发**
  - Responses 线 → 发 `"auto"`（官方行为）
  - Messages 线 → 不发

**续写请求特别容易踩坑**：断流续写时通常会想"这轮别调工具"，
但那必须靠**续写指令正文**里的 "Do not call tools" 来压制，
**不能**靠 `tool_choice: "none"`。

---

## 7. 凭据

```http
authorization: Bearer public
```

**`public` 是共享凭据** —— 免费车道的所有用户共用这一个。
它不是用户的 key，也不代表任何个人身份。

推论：
- 免费车道的 429 反映的是"全局此刻拥挤"，不是"你的模型坏了"
- 冷却要**短**（10 秒）
- 隐私上更要小心：所有人的 prompt 都流经同一个池子

---

## 8. 回归测试清单

FMH 必须有这一套测试（`internal/lane/zen_test.go`）：

```go
TestZenClientUAShape          // 匹配 opencode/<channel>/<version>/<client>，major>=2
TestZenClientUAHasNoPrefix    // 整串，无前后缀
TestZenHeadersFourSessionIDs  // 四个会话头存在且同值
TestZenHeadersNoV1Request     // 不含 x-opencode-request
TestZenSessionIDShape         // ^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$
TestZenSessionIDDescending    // 12 位 hex 匹配 ^[0-9a-f]{9}f[c-f][0-9a-f]$
TestZenSessionIDCounterWindow // counter 1..63 通过
TestZenSessionStability       // 同一 seed → 同一 session
TestZenFingerprintDefsVerbatim// 官方定义逐字未改
TestZenFingerprintAliasing    // 冲突工具改名 + 历史同步 + tool_choice 重定向
TestZenFingerprintCaseVariant // `Read`/`Bash` 原样透传，不被归并
TestZenFingerprintDuplicate   // 同名重复只留第一个
TestZenNeverEmitsToolChoiceNone
TestZenResponsesAutoChoice    // Responses 线补 auto，Chat 线不补
TestZenEndpointSelection      // 三种 wire 的端点映射
TestZenPlaceholderNote        // 注入后必带占位符提示
```

---

## 9. 上游变更时的应对

上游已经改过至少两次（v1→v2、`/zen/v1`→`/inference/v1`）。再次变更时：

1. **症状**：所有模型同时 403，且错误体是 `FreeTierError`。
2. **不要**把它分类成"凭据错误"（`INVALID_CREDENTIAL`）——
   那会让客户端以为"你的 key 错了"，并且**停掉故障转移**，让整个系统卡死。
   要有独立的 `FREE_TIER_BLOCKED` 码。
3. **排查顺序**：
   - 端点路径对不对？
   - UA 整串对不对？
   - 四个会话头齐不齐？
   - 会话 ID 的降序编码对不对？
   - `read`/`shell` 的定义逐字对不对？
   - `tool_choice` 是不是 `"none"`？
4. **改动必须集中在 `internal/lane/zen_fingerprint.go` 一个文件**，
   并让上面的测试全部通过。

---

## 10. 指纹诊断器（**别猜**）

上面 6 步是**人工**的 —— 每改一处就要重启、等上游、试一次。
6 个维度 = 6 轮往返，每轮几分钟。

FMH 有个**自动**的定位器：**单变量对照**。

### 10.1 原理

从一份"**已知能通过**"的请求出发，**每次只改一项**，看它是不是被拒。

```
基线   ── 200 ──▶ 说明只坏了一项（或没坏）
     └─ 403 ──▶ 说明坏了多项，或坏了全局维度（端点/UA）

基线通过后：
  改端点   ── 200 ──▶ 上游不管端点
          └─ 403 ──▶ ⚠️ 上游在管端点
  改 UA     ── 200 ──▶ 上游不管 UA
          └─ 403 ──▶ ⚠️ 上游在管 UA
  …（6 项）
```

这跟 M1 阶段人工验证指纹用的是**同一套方法** ——
只不过把那 6 轮往返压缩成了 1 次。

### 10.2 实现

`internal/lane/diagnostics.go` 的 `ZenLane.Diagnose(ctx, model)`。

⚠️ **必须用生产同款构造**：

| 环节 | 为什么 |
|---|---|
| `ZenHeaders(session, true)` | 手写会漏 `x-opencode-client` / `x-opencode-project` → 基线就不通 |
| `ZenApplyFingerprint(body, style)` | 保证注入的 `read`/`shell` 名字精确 |
| `stream: true` | 生产路径一直是流式 |
| `ZenEndpointFor` / `ZenWireFor` | 端点与 wire 按模型名选，不能写死 |

⚠️ **变体必须在 `ZenApplyFingerprint` 之后施加** ——
   因为它会把 `tool_choice: "none"` **删掉**（那会让模型这一轮不调工具）。
   先改后注入 = 变体被撤销，测出来永远是"通过"。

### 10.3 局限（**必须如实说**）

单变量对照的前提是 **同时只有一个维度坏了**。

- **基线就不通** → 上游改的是**多处**，或者改的是端点/UA 这种全局维度。
  这时候**不能**说"是第 X 项坏了"，只能说"不止一处"。
  这时必须人工对照本文档逐项核。

⚠️ **更要紧的是：基线不通还有别的原因，不能一律当成"上游改了规格"。**

| 基线的失败 | 含义 | 该做什么 |
|---|---|---|
| **429 限流** | 上游在挤，**不是在拒绝我们** | **等几分钟再跑** —— 现在跑 7 次请求全是 429，纯浪费 |
| **传输层错误** | 网断了 / 出口被封 | 查网络，不是规格问题 |
| **EGRESS_BLOCKED** | 出口 IP 被风控 | 换出口，或开自动跨模型调用 |
| **403 / 426** | **真的被闸门拒** | 这才是要诊断的情况 |

⚠️ **把 429 说成"上游改了一处"是误导** ——
   用户会去改指纹文件，而那**完全没有用**。

⚠️ 真实触发场景：用户刚点完「立即检查」（一次真实推理），
   紧接着点「开始诊断」，就吃到 429 了。
   所以诊断器**必须**区分这两种情况。

### 10.4 成本

每次诊断 = **1（基线）+ 6（变体）= 7 次请求**。

- 变体被拒时是 403，**不花钱**。
- 基线通过时那 1 次是真实推理，**花钱**。

所以：**只在闸门拒绝时才跑**。管理台把它放在"上游闸门拒绝"的横幅里，
不放在常规操作区。

### 10.5 实测（2026-10-19，真实上游）

```
基线通过: True (HTTP 200)
结论: 上游在管的是：**User-Agent** —— 改这一项会被拒，其余随便改都通过。
  端点路径          通过    data: {"id":"07258cb125ea067db1538222bc9b4dff",...
  User-Agent       被拒426  OpenCode 1.18.0 or newer is required to use the free
  会话头            通过    ...
  会话 ID 编码       通过    ...
  read/shell 工具定义  通过    ...
  tool_choice      通过    ...
```

⚠️ **注意状态码是 426，不是 403** ——
   上游用 `426 Upgrade Required` 表达"你的客户端版本太老"。
   这跟 `403 FreeTierError` 是**不同的失败**，但两者都归到 `FREE_TIER_BLOCKED`。

⚠️ **结论要如实读**：
   "只有 UA 被管"**不等于**"其它维度可以乱来"——
   它只说明**当前**上游只检查 UA。
   文档里的 6 条规矩仍然要照做，因为上游随时可能加严。

### 10.6 用法

```bash
# 管理台：上游源页 → 闸门拒绝横幅 → 开始诊断
curl -X POST http://<NAS-IP>:8678/api/diagnostics \
  -H 'Content-Type: application/json' \
  -b 'fmh_session=<管理会话 cookie>' \
  -d '{"model":"minimax-m3.1-flash-free"}'
```

返回：

```json
{
  "ok": true,
  "baselineOk": true,
  "baselineStatus": 200,
  "conclusion": "上游在管的是：**User-Agent** …",
  "results": [
    {"name":"ua","label":"User-Agent","ok":false,"status":426,
     "detail":"OpenCode 1.18.0 or newer is required to use the free tier"}
  ],
  "durationMs": 7900
}
```
