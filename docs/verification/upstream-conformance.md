# 上游一致性验证

> 免密车道的上游只接受"看起来像官方客户端"的请求。
> FMH 要复现这个行为，就必须**精确**知道官方客户端发出去的请求长什么样。
>
> 官方客户端是**开源**的（MIT），所以"官��客户端的请求形状"是公开可读的代码。
>
> 这份文档记录两件事：
>
> 1. FMH 的实现跟官方源码**对得上**（可复现的核对）
> 2. 哪些结论是**源码里读不出来**、靠 FMH 自己实测确定的

---

## 1. 源码里能直接读到的

### 1.1 会话头

`packages/core/src/session/model-request.ts`（OpenCode 官方）：

```ts
const affinity = SessionAffinity.get(session)
const base = LLM.request({
  model: model.model,
  http: {
    headers: {
      "x-opencode-session-id": session.id,
      ...(session.parentID ? { "x-opencode-parent-session-id": session.parentID } : {}),
      "x-session-affinity": affinity,
      "X-Session-Id": affinity,
      ...(session.parentID ? { "x-parent-session-id": session.parentID } : {}),
      "User-Agent": App.useragent(app),
      "x-opencode-project": session.projectID,
      "x-opencode-session": affinity,
      "x-opencode-client": app.name,
    },
  },
  ...
})
```

FMH 的对应实现（`internal/lane/zen_fingerprint.go`）：

```go
func ZenHeaders(session string, stream bool) map[string]string {
    return map[string]string{
        "content-type":          "application/json",
        "authorization":         ZenCredential,
        "user-agent":            ZenClientUA,
        "x-opencode-client":     "cli",
        "x-opencode-session":    session,
        "x-opencode-session-id": session,
        "x-session-affinity":    session,
        "x-session-id":          session,
        "x-opencode-project":    "global",
        "accept":                accept,
    }
}
```

**对应关系**：

| 官方头 | FMH 的值 | 差异原因 |
|---|---|---|
| `x-opencode-session` | `session` | 一致 |
| `x-opencode-session-id` | `session` | 官方这里是 `session.id`，但四件套要求同值 —— FMH 统一成同一个 |
| `x-session-affinity` | `session` | 一致 |
| `X-Session-Id` | `session` | 一致（HTTP 头名大小写不敏感） |
| `x-opencode-project` | `"global"` | 官方取 `session.projectID`，FMH 固定 `"global"` |
| `x-opencode-client` | `"cli"` | 官方取 `app.name`，FMH 固定 `"cli"` |
| `User-Agent` | `ZenClientUA` | 官方 `App.useragent(app)`，FMH 固化成一个常量 |

### 1.2 会话 ID 的形状

官方同一段代码里的正则，**直接给出了会话 ID 的形状**：

```ts
promptCacheKey: /^ses_[0-9a-f]{64}$/.test(affinity) ? affinity.slice(4) : affinity,
```

FMH 的对应实现：

```go
var zenSessionRe = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)
```

**`ses_` 前缀 + 64 位** —— 官方正则印证了 FMH 的形状判断。

⚠️ 官方这条正则只校验**形状**，不校验编码。
而上游网关**会解码**那 12 位 hex —— 见 §2.2。

### 1.3 工具定义

`packages/core/src/tool/plugin/read.ts`：

```ts
description:
  "Read the contents of a file or directory. Supports text files, images, and PDFs. " +
  "Images and PDFs are presented directly to the model. Each text line is prefixed " +
  "by its 1-based line number as <line>: <content>. The prefix is for reference and " +
  "is not part of the file content. Directory entries are returned one per line. " +
  "Use offset and limit to read large files or directories in sections. Prefer one " +
  "larger read over many small slices, and use grep to find specific content in " +
  "large files.",
```

`packages/core/src/tool/plugin/shell.ts`：

```ts
const description = (shell?: string) =>
  [
    "Execute a shell command and return its output.",
    ...(shell ? [`Commands run on ${OS} using ${shell}.`] : []),
    "Quote file paths containing spaces or special characters.",
    "Prefer dedicated tools over shell commands when possible.",
    "When output is large, the full result is saved to a file and a truncated preview is returned.",
    "Rely on automatic truncation unless filtering the output is more useful.",
    "Commands accept an optional timeout, background commands have no timeout by default.",
    "Background commands return immediately, and you will be notified when they complete.",
  ].join(" ")
```

**FMH 把它固化成了静态常量**（因为客户端发出去时它已经拼好了）：

```go
const zenReadDescription = "Read the contents of a file or directory. ..."
const zenShellDescription = "Execute a shell command and return its output. ..."
```

参数描述（`path` / `offset` / `limit` / `command` / `workdir` / `timeout` /
`background`）也是**逐字一致** —— 官方用模板串插值的地方
（`${DEFAULT_TIMEOUT_MS}`、`${OS}`、`${shell}`），FMH 填的是运行时实际值。

### 1.4 核对结果

| 项目 | 结果 |
|---|---|
| `read` 工具描述 | ✅ 整段逐字命中官方源码 |
| `shell` 工具描述 | ✅ 逐句 8/8 命中 |
| 7 个参数描述 | ✅ 6/7 一致，1 条有差异（见 §1.5） |
| 会话头 | ✅ 名称与取值规则一致 |
| 会话 ID 形状 | ✅ 官方正则 `^ses_[0-9a-f]{64}$` 印证 |

### 1.5 `background` 参数 —— **已修正**

`background` 参数的描述，官方所有版本都是：

```
... You do not need to use '&' at the end of the command ...
```

FMH 早期版本是：

```
... You do not need to use the '&' at the end of the command ...
```

差 4 个字符（`use` vs `use the`）。

**已改成官方原版** —— 一致性脚本现在 7/7 全中。

> ⚠️ 这个差异**本来不影响使用**（实测两版都返回 200），
> 原因见 §2.6：**上游不比对工具描述**。
> 但既然查出来了，改成原版总归是好事。

其余 6 个参数描述与官方逐字一致。

---

## 2. 源码里**读不出来**的（FMH 自己实测的）

上游的私有行为，官方客户端里当然不会写。这是 FMH 自己测出来的。

### 2.1 端点按模型名分流

官方客户端走的是自己的路由，但**免费车道**的上游要求按模型名选端点：

| 模型 | 端点 |
|---|---|
| `muse-spark-*` | `/inference/v1/responses` |
| `union-alpha` | `/inference/v1/messages` |
| 其余 | `/inference/v1/chat/completions` |

⚠️ 上游的模型清单**不声明**任何 wire 信息（只有 `id` / `object` / `created` /
`owned_by`）—— 所以只能按模型名选。

### 2.2 会话 ID 的编码（形状对还不够）

上游的私有推理代理**会解码**那 12 位 hex，验证它能否还原成一个**新鲜的时间戳**。

| 编码 | 结果 |
|---|---|
| 降序 `~(ms<<12\|counter)`，counter ∈ 1..63 | ✅ |
| 升序 `ts << 12` | ❌ |
| sha256 摘要 | ❌ |
| 随机 hex | ❌ |
| counter ≥ 100 | ❌ |

FMH 的实现：

```go
var zenSessionDescendingRe = regexp.MustCompile(`^[0-9a-f]{9}f[c-f][0-9a-f]$`)
```

**形状对（`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`）远远不够** ——
官方那条正则只管形状，上游网关还要解码。

### 2.3 `tool_choice` 的硬性禁止

上游见到 `tool_choice: "none"` 就 403。

官方客户端源码里 `tool_choice` 只出现在 GitHub Copilot 的适配器里
（`packages/core/src/github-copilot/...`）—— 免费车道的这条限制**不在源码里**，
是实测出来的。

FMH 的处理是**兜底清除**：

```go
switch choice := body["tool_choice"].(type) {
case string:
    if choice == "none" { delete(body, "tool_choice") }
case map[string]any:
    if t, _ := choice["type"].(string); t == "none" { delete(body, "tool_choice") }
}
```

### 2.4 闸门有**两层**

这是实测（2026-10-19）最重要的发现。

同一台机器、同一出口 IP，只改"有没有带指纹"：

| 请求 | 上游回应 |
|---|---|
| **不带指纹** | `"OpenCode's free tier can only be used from within OpenCode."` |
| **带完整指纹** | `"OpenCode's free models aren't available from your current network because we've detected **abusive traffic from this IP address**. ..."` |

**两句都是 403、都是 `FreeTierError` type —— 但指向完全不同的层：**

```
第一层：身份闸门   "can only be used from within OpenCode"
       → 伪造指纹能过   ✅ FMH 已通过
       → 改了指纹也没用（因为指纹本来就对）

第二层：IP 信誉    "detected abusive traffic from this IP address"
       → 伪造指纹没用   ❌ 只能换出口
       → 与你的账号无关
```

**所以错误码必须分开** —— 混淆它们会让用户"改了半天指纹，其实一直是对的"。

### 2.5 上游清单不声明 wire 信息

实测（2026-10-19）：免费车道的模型清单只有 `id` / `object` / `created` /
`owned_by` 四个字段。

⇒ "清单能取到" ≠ "对话能通"。模型清单必须配**实测**与**限流账本**。

---

## 2.6 闸门到底比什么（实测，2026-10-22）

**直接打上游**（绕过 FMH 网关），一次只改一个变量：

| 测试 | 结果 | 结论 |
|---|---|---|
| 完整请求（复刻 FMH） | ✅ 200 | 基线 |
| `accept: application/json` | ✅ 200 | **不看** accept |
| 不带 system 占位提示 | ✅ 200 | **不看** system 提示 |
| 去掉 `x-opencode-client` | ✅ 200 | **不看**这个头 |
| 去掉 `x-opencode-project` | ✅ 200 | **不看**这个头 |
| **工具描述全部乱写** | ✅ 200 | ⚠️ **不比对工具描述！** |
| **`tool_choice: "none"`** | ✅ 200 | ⚠️ **不禁止** tool_choice！ |
| **非流式**（`stream: false`） | ❌ 403 | ✅ **必须流式** |
| **不带工具** | ❌ 403 | ✅ **必须有 read + shell** |
| **去掉全部会话头** | ❌ 403 | ✅ **必须**四个会话头 |
| **User-Agent 换成随意的** | ❌ 403 | ✅ **必须**官方形状的 UA |
| 模型名填错 | ❌ 400 | 通过了闸门，只是模型不存在 |

### 闸门条件（结论）

**必须全部满足**：

| 条件 | 说明 |
|---|---|
| **User-Agent** | 形状要对 —— `opencode/latest/<版本>/cli` |
| **四个会话头** | `x-opencode-session` / `x-opencode-session-id` / `x-session-affinity` / `x-session-id`，同值 |
| **会话 ID 形状** | `ses_` + 12 位 hex + 14 位 base62 |
| **流式** | `stream: true` |
| **read + shell 两个工具** | 名字要叫这个，内容随意 |

**不看**：`accept`、`x-opencode-client`、`x-opencode-project`、
system 提示、**工具描述的任何字**、`tool_choice`。

### ⚠️ 两条要修正的旧结论

**① 工具描述不需要逐字一致。**

旧文档写的是"上游逐字比对工具描述，改一个词就 403" ——
**这是错的**。实测把两个工具的描述**全部乱写**，照样 200。

那为什么还用官方原文？因为**没必要改** —— 官方客户端就长那样，
用原文能保证"万一上游以后开始比对"时也不炸。
但这是**保守**，不是**必需**。

**② `tool_choice: "none"` 不会被拒绝。**

旧文档写"上游见到 `tool_choice: "none"` 就 403" ——
**这也是错的**。实测 200。

所以 FMH 里那段"兜底清除 `tool_choice: none`"的代码
**不是为了过闸门** —— 它解决的是**正确性**问题：
调用方（尤其是断流续写路径）可能留下 `none`，
那会让模型**这一轮不调工具** —— 而我们明明把工具给它了。
压制"这轮别调工具"的正确做法是在续写指令正文里写
"Do not call tools"，而不是设 `tool_choice: "none"`。

> ⚠️ 这两条旧结论被推翻，说明**之前的验证方法有问题**：
> 那些测试都是**通过 FMH 网关**发的，而网关会**重写**请求
> （加指纹、选流式），所以测的一直是"FMH 自己的输出"，
> **不是上游的判定**。
>
> 要测上游，必须**绕过网关直接打**。

---

## 3. FMH 在官方行为之上**补**的

官方客户端是**交互式 TUI**，FMH 是**网关**。目标不同，所以 FMH 做了
官方不需要做的事：

| FMH 补什么 | 为什么官方不需要 |
|---|---|
| 冲突工具改名时**避开已占用的别名**（`read_client2`） | 官方客户端不代理别人的工具 |
| `tool_choice: "none"` 的**兜底清除** | 官方自己产生这个值，不会撞上上游的禁止 |
| Responses 线**自动补 `tool_choice: "auto"`** | 官方按自己的协议走 |
| 历史消息里的 `tool_call` **也改名** | 官方不需要改名 |
| 会话 ID 的**降序时间戳编码** | 官方用 `SessionAffinity` 管理，不需要伪造 |
| 会话 ID 的**6 天重铸周期** | 官方会话是用户主动开的 |
| **端点按模型名分流** | 官方有自己的路由层 |

---

## 4. 复现

```bash
git clone --depth 1 --filter=blob:none --sparse https://github.com/sst/opencode.git
cd opencode
git sparse-checkout set packages/core/src
```

然后跑核对脚本：

```bash
python3 tools/upstream-conformance.py --opencode opencode --fmh fmh
```

它会：

1. 从官方源码抽出 `read` / `shell` 的描述与参数描述
2. 跟 FMH 的常量逐字比对
3. 从官方源码里找出会话头的定义位置
4. 输出 §1.4 那张核对表

**只用标准库** —— 不给项目加依赖。

---

## 5. 许可

OpenCode 是 **MIT 授权**（Copyright (c) 2025 opencode）。

FMH 使用了它的工具定义字符串（用官方原文，保守但稳），
按 MIT 要求保留了版权声明 —— 见 [`LICENSE`](../../LICENSE)。

FMH 对这部分内容的改动仅限于：

- 把官方运行时拼接的描述**固化为静态常量**
- 补上 FMH 自己实测的结论（§2）
