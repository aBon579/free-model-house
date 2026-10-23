# Key 模型

> 状态：已实现（M3；M5 起管理台改为密码登录）
> 最后更新：2026-10-23

## TL;DR

```
推理面   http://<NAS-IP>:8678/v1   +  fmh-<40 位 hex>      ← agent 用这把
管理台   http://<NAS-IP>:8678/     +  密码（初始 admin）    ← 人用密码登录
```

**只有一把推理 Key。** 管理面不再用 Key 换会话 —— 那是旧设计，
现在管理台是**密码登录**（bcrypt 哈希）。

**一个端口**同时提供推理面与管理面，鉴权靠**两种凭据**区分，不靠端口。

> ⚠️ 历史包袱：`docs/verification/` 里 2026-10 的几份验收记录
> 还写着「本机 Key」，那是当时的形态。现在以本文为准。

---

## 1. 单端口

用户的要求是"通过局域网 IP + 端口和 API key 让用户免 key 用上免费模型"，
并且指定了 `http://NAS-IP:8678/v1`。

所以整个服务只监听 **一个端口**：

| 路径 | 面 | 鉴权 | 谁能访问 |
|---|---|---|---|
| `/v1/chat/completions` | 推理 | `fmh-` Key | 局域网任何持有 Key 的人 |
| `/v1/responses` | 推理 | `fmh-` Key | 同上 |
| `/v1/messages` | 推理 | `fmh-` Key | 同上 |
| `/v1/models` | 推理 | `fmh-` Key | 同上 |
| `/v1/messages/count_tokens` | 推理 | `fmh-` Key | 同上 |
| `/api/*` | 管理 | 会话 Cookie | 登录过的人（见 §5） |
| `/` | Web UI | 无（登录页本身公开） | 任何人，但管理功能要登录 |
| `/healthz` | 健康检查 | **无** | 任何人 |

### 1.1 为什么管理面在单端口下依然安全？

因为**两种凭据各管一扇门**：

- **推理面**只认 `fmh-` Key。Key 泄露 → 别人能白嫖你的车道，
  但**读不到**用量之外的东西、**改不了**任何配置。
- **管理面**只认**密码会话**。会话 Cookie 是 `HttpOnly` + 内存态，
  从局域网拿不到（除非他知道密码）。

这比"管理面只监听 127.0.0.1"更简单，而且效果一样。

> ⚠️ **诚实说明**：管理面**没有** IP 白名单，也**没有**登录限流。
> 也就是说，谁能连上 `:8678`，谁就能试密码。要做到"只有本机能进"，
> 用 `FMH_ADDR=127.0.0.1:8678` 只绑本机，或者把它藏在反向代理后面。
> 见 §7.1。

> ⚠️ `/healthz` 是公开的。它只暴露"服务活着吗"和版本号，
> 不暴露任何模型或凭据信息。容器健康检查需要它能工作。

---

## 2. 固定前缀 `fmh-`

用户的要求："项目自己生成一个固定前缀的 API key"。

所以**推理 Key 只有这一种形状**：

```
fmh-<40 位 hex>
```

- `fmh-` 是固定前缀（4 字符）
- 后面是 20 字节（160 bit 熵）的随机数，hex 编码成 40 字符

举例：

```
fmh-EXAMPLEKEY0000000000000000000000000000
```

### 2.1 为什么只要一把 Key？

旧设计里有两把（「本机 Key」能换管理会话，「局域网 Key」只能推理）。
它的初衷是**爆炸半径分离**：LAN Key 泄露不该等于管理台失守。

现在这个目标由**换成密码登录**达成了，而且更彻底：

| | 旧：本机 Key | 新：管理密码 |
|---|---|---|
| 能开管理面 | ✅ | ✅ |
| 泄露后果 | 攻击者进管理台 | 攻击者进管理台 |
| 轮换成本 | 重新发 Key，所有 agent 改配置 | 改密码，**agent 配置一行不改** |
| 和推理面耦合 | 强（同一把凭据体系） | **零**（推理 Key 完全不受影响） |
| 记忆负担 | 两把 Key 要分清 | 一把 Key + 一个密码 |

**关键收益**：改管理密码**踢不掉任何 agent**。
以前轮换「本机 Key」会连带影响管理会话，现在两个体系彻底解耦。

所以 `Keys` 里只剩一个 `lanKey` 字段；`LocalKeyName` 常量已废弃。

### 2.2 Key 从哪来，怎么取

**懒生成**：第一次启动时铸一把，存在 SQLite 里，重启不变。

三个取法（按可靠性排序）：

| 途径 | 命令 | 说明 |
|---|---|---|
| 启动日志 | `docker logs free-model-house \| head` | **唯一**在容器起不来时还能用的途径 |
| 子命令 | `docker exec free-model-house /fmh key` | 容器在运行时可用；`fmh key lan` 等价 |
| 管理台 | 「服务设置」→ 显示完整 Key | 默认只回显指纹，要显式 `?reveal=1` |

⚠️ 不想让明文 Key 进日志（会被收进日志系统、贴到工单）？
设 `FMH_HIDE_KEYS=1`，启动日志就只打指纹（`fmh-…ce8`）。

### 2.3 轮换语义

```
POST /api/keys/rotate   { "which": "lan" }
```

| 操作 | 推理 Key | 管理会话 | agent 配置 |
|---|---|---|---|
| 轮换推理 Key | **旧 Key 立即失效** | **不受影响** | 要改成新 Key |
| 修改管理密码 | 不变 | **全部失效**（要重新登录） | **完全不受影响** |

⚠️ 轮换推理 Key 是**在线**的：不用重启，`Keys.RotateLanKey()` 直接换内存里的值并落库。
旧 Key 下一次请求就是 401。

---

## 3. 免 key 的真正含义

用户说"让用户免 key 用上免费模型"。

**免的是"上游的 key"，不是"调用 FMH 的 key"。**

```
LAN agent ──[fmh-xxxx]──▶ FMH ──[Bearer public]──▶ OpenCode Zen 免密车道
                        ├─[无需凭据]────────────▶ Kilo 免 key 池
                        └─[用户自己的 OR Key]───▶ OpenRouter 免费模型
```

- **调用方**要有一把 `fmh-` Key —— 它只是"FMH 的门禁卡"，
  零成本、零额度、不绑定任何付费账户。
- **上游**的免密车道用的是共享的 `Bearer public` —— 不需要用户注册。
- OpenRouter 车道**需要用户自己的 Key**（要在管理台录入），
  因为它的免费额度是绑在账户上的。

所以"免 key"对用户来说意味着：

> 不用去 OpenCode 注册、不用绑卡、不用为每次调用付费。
> 拿一把 FMH 的 Key 就能用。

---

## 4. 上游凭据永不离开本机

这是整个模型里最重要的一条。

**OpenRouter 的 API Key**：

- 录入时用 **AES-256-GCM** 加密（`internal/store/vault.go`）
- 密钥来自 `FMH_SECRET_KEY` 环境变量；没设就在 `/data/.secret-key`
  生成一个（0600 权限）
- 密文存 SQLite；**明文只在网关进程内存里**
- 管理台**只回显指纹**（`sk-or-v1-…abcd`），**永不回显全文**
- 它**不下发给任何客户端** —— 推理面拿不到，管理台也只看到指纹

### 4.1 密钥丢了怎么办？

`/data/.secret-key` 丢了（或 `FMH_SECRET_KEY` 变了）→
已存的上游凭据解不开 → 那个车道退化成"未配置"。

**服务照常启动**（不会崩），只是要重新录一次 OpenRouter Key。

> ⚠️ 备份 `/data` 时，**`.secret-key` 必须一起备份**。
> 它和 SQLite 在同一个目录里，就是为了避免备份时漏掉。

---

## 5. 管理台登录（密码）

### 5.1 流程

```
打开 http://<NAS-IP>:8678/
  → 输入密码（初始 admin）
  → 会话带上 mustChangePassword 标记
  → 立刻弹「设置新密码」，改成 ≥8 个字符才放行
  → 改完所有会话作废，包括自己 → 用新密码重登
```

| 项 | 值 | 备注 |
|---|---|---|
| 初始密码 | `admin` | 系统给定，不校验长度 |
| 最短长度 | **8 个字符** | 按**字符数**算，不是字节（中文 8 个字才算 8） |
| 存储 | bcrypt，cost **12** | 只存哈希，不存明文 |
| 设置键 | `admin_password_hash` / `admin_password_changed` | SQLite 设置表 |
| 改密要求 | 先验**当前密码** | 别人借了你的浏览器也改不了 |

⚠️ **为什么允许用初始密码登录**：直接拒绝会让人一头雾水（"我哪来的密码？"）。
允许登录但**强制先改**，路径最短。

⚠️ **改完密码会踢掉所有人**，包括当前浏览器 —— 前端要引导重新登录。

### 5.2 忘了密码怎么办

**目前没有 `fmh passwd` 之类的重置命令。** 两条路：

1. 删掉 SQLite 设置表里的 `admin_password_hash` 与 `admin_password_changed`
   两行 → 下次启动重新铸初始密码 `admin`（仍需改密）。
2. 换一个空的 `FMH_DATA` 目录重新初始化 —— **已存的上游凭据会一起丢**。

> 📌 待办：补一个 `fmh passwd -set <新密码>` 子命令。这是目前最明显的易用性缺口。

### 5.3 遗留：一次性登录令牌

`internal/server/keys.go` 里还留着 `NewBootstrapToken` / `/api/bootstrap`
（10 分钟有效、`#token=` fragment 传递、兑换后 302 清地址栏）。
它原本是"用 Key 换会话"时代的免输入登录方案。

**现在生产代码里没有任何地方生成它**，只有测试在用。
留着是为了将来做"扫码/链接登录"，别把它当功能。

---

## 6. 会话

| 属性 | 值 |
|---|---|
| 存储 | **内存**（重启即失效） |
| 有效期 | 8 小时 |
| Cookie 名 | `fmh_session` |
| HttpOnly | ✅ |
| SameSite | `Lax` |
| Secure | ❌ —— 见下 |

⚠️ **不设 `Secure`** —— NAS 的 Web UI 常常是 `http://192.168.x.x`，
设了 `Secure` 会导致浏览器直接丢弃 Cookie，表现为"登录了但还是回到登录页"。

⚠️ 会话在内存里，所以**重启服务会踢掉所有人**。
对一个自托管的网关来说这是可接受的（也是更安全的默认值）。

---

## 7. 威胁模型

| 威胁 | 防护 |
|---|---|
| LAN 上有人扫端口 | 没有 Key 一律 401；Key 是 160 bit 熵 |
| 推理 Key 泄露 | 管理台「轮换」一下：旧 Key 立即失效，**管理会话和 agent 之外的东西都不受影响** |
| 管理密码泄露 | 改密码 → **所有会话立即作废**；推理 Key 不用动 |
| 管理 Cookie 泄露 | 8 小时过期 + HttpOnly；改密码可立即作废所有会话 |
| 有人在爆破管理密码 | ⚠️ **目前没有限流** —— 所以管理面只该暴露在可信网络里（见 §1.1、§7.1） |
| 上游 Key 泄露 | 管理台永不回显全文；存储加密；只能重新录入 |
| 容器被攻破 | 二进制静态编译、无 shell、非 root；上游凭据虽可解，但**仅限本机** |
| 中间人 | 由用户的 agent 客户端的 TLS 负责（FMH 本身是明文 HTTP，**仅限可信 LAN**） |

### 7.1 明确不做的事

- ❌ 不做 HTTPS（应该放在 NAS 的反向代理后面）
- ❌ 不做 IP 白名单（Key + 密码已经够；白名单会让"给同事用"变麻烦）
- ❌ 不做登录限流（**已知缺口**；如果管理面要暴露到不可信网络，先在反代上做）
- ❌ 不记录请求的提示词与回答（只记元数据 —— 见 `docs/design/00-overview.md` §5.9）

---

## 8. 给 agent 的配置片段

### Claude Code

```bash
export ANTHROPIC_BASE_URL="http://<NAS-IP>:8678/v1"   # ⚠️ 必须带 /v1
export ANTHROPIC_AUTH_TOKEN="fmh-xxxxxxxx"
```

⚠️ Claude Code 会自己拼 `/v1/messages`，所以 `ANTHROPIC_BASE_URL` **要带 `/v1`**，
写成 `http://<NAS-IP>:8678` 会 404。

### OpenCode / Cline / Continue（OpenAI 兼容）

```json
{
  "provider": {
    "fmh": {
      "npm": "@ai-sdk/openai-compatible",
      "options": {
        "baseURL": "http://<NAS-IP>:8678/v1",
        "apiKey": "fmh-xxxxxxxx"
      }
    }
  }
}
```

### Aider

```bash
export OPENAI_API_BASE="http://<NAS-IP>:8678/v1"
export OPENAI_API_KEY="fmh-xxxxxxxx"
```

### 模型名

用 `车道/模型` 的形式：

```
zen/minimax-m3.1-flash-free
kilo/deepseek-v3:free
openrouter/deepseek/deepseek-r1:free
```

⚠️ 也接受**裸模型名** —— 网关会自动解析到唯一的车道。
但有重名时（两个车道都有同名模型）必须用全名。

---

## 9. 相关文件

| 文件 | 职责 |
|---|---|
| `internal/server/keys.go` | 推理 Key 的生成、校验、轮换；管理会话 |
| `internal/server/password.go` | 管理密码的加载 / 校验 / 修改（bcrypt） |
| `internal/auth/password.go` | 密码策略：最短长度、哈希、初始密码 |
| `internal/server/admin.go` | 管理 API（登录、Key、凭据、设置） |
| `internal/store/vault.go` | 上游凭据的 AES-256-GCM 密封 |
| `internal/store/db.go` | 持久化（Key、凭据、用量） |
| `internal/server/server.go` | 单端口 HTTP 服务器与路由 |
