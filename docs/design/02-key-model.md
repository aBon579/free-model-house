# Key 模型

> 状态：已实现（M3）
> 最后更新：2026-10-19

## TL;DR

```
BASEURL: http://<NAS-IP>:8678/v1
API Key: fmh-<40 位 hex>
```

**一个端口**同时提供推理面与管理面。鉴权靠 **Key**，不靠端口。

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
| `/api/*` | 管理 | 会话 Cookie | **只有 NAS 本机的浏览器** |
| `/` | Web UI | 无（登录页本身公开） | 任何人，但管理功能要登录 |
| `/healthz` | 健康检查 | **无** | 任何人 |

### 1.1 为什么管理面在单端口下依然安全？

因为**鉴权是按 Key 做的，不是按端口做的**。

- 管理面需要**管理会话 Cookie**。这个 Cookie 只在 NAS 本机的浏览器里
  （用户用本机 Key 登录一次，或用一次性令牌兑换）。
- 从局域网访问 `/api/*` 时，浏览器里没有那个 Cookie → **401**。

这比"管理面只监听 127.0.0.1"更简单，而且效果一样：
**从 LAN 上你打不开管理功能**。

> ⚠️ 唯一需要注意的是：`/healthz` 是公开的。
> 它只暴露"服务活着吗"和版本号，不暴露任何模型或凭据信息。
> 容器健康检查需要它能工作。

---

## 2. 固定前缀 `fmh-`

用户的要求："项目自己生成一个固定前缀的 API key"。

所以**所有推理 Key 都是这个形状**：

```
fmh-<40 位 hex>
```

- `fmh-` 是固定前缀（4 字符）
- 后面是 20 字节（160 bit 熵）的随机数，hex 编码成 40 字符

举例：

```
fmh-EXAMPLEKEY0000000000000000000000000000
```

### 2.1 两把 Key，同一个前缀

| 名字 | 用途 | 能开哪扇门 |
|---|---|---|
| **本机 Key** | NAS 本机的工具 | 推理面 + **能换管理会话** |
| **局域网 Key** | LAN 里的 agent | **只**能开推理面 |

⚠️ **它们用的是同一个前缀**，但仍然是**两把不同的钥匙**。

为什么不用不同前缀（比如 `fmh-local-` 和 `fmh-lan-`）？

因为前缀是**给人看的**，而隔离要靠**行为**。
用户明确要求"固定前缀"，所以前缀统一；隔离靠：

1. **独立生成** —— 两把钥匙的随机部分不同
2. **独立轮换** —— 轮换一把不影响另一把
3. **启动时校验** —— 若两把字面相同，强制重生成 LAN Key

### 2.2 为什么必须校验"两把不同"？

如果 LAN Key 和本机 Key **字面相同**：

```
LAN Key 泄露 → 攻击者拿到本机 Key
             → 他能从 LAN 上换到管理会话
             → 他能读到上游凭据指纹、轮换你的 Key、改你的配置
```

爆炸半径就**完全没分离**了 —— 分层就白做了。

所以 `Keys.load()` 里有一道保险：

```go
if k.localKey != "" && k.localKey == k.lanKey {
    k.lanKey = store.GenerateKey(KeyPrefix)
    // 存回去
}
```

轮换时也有同样的保险（`RotateLocalKey` / `RotateLanKey`）。

### 2.3 轮换语义

| 操作 | 本机 Key | LAN Key | 管理会话 |
|---|---|---|---|
| 轮换 **LAN Key** | 不变 | **失效** | 不受影响 |
| 轮换 **本机 Key** | **失效** | 不变 | **全部失效**（要重新登录） |

⚠️ 轮换本机 Key 会踢掉所有管理会话 —— 因为持有旧 Key 的人
不该还能进管理面。

---

## 3. 免 key 的真正含义

用户说"让用户免 key 用上免费模型"。

**免的是"上游的 key"，不是"调用 FMH 的 key"。**

```
LAN agent ──[fmh-xxxx]──▶ FMH ──[Bearer public]──▶ OpenCode Zen 免密车道
                        └─[用户自己的 OR Key]──▶ OpenRouter 免费模型
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
- 它**不下发给任何客户端** —— 包括本机 Key 的持有者

### 4.1 密钥丢了怎么办？

`/data/.secret-key` 丢了（或 `FMH_SECRET_KEY` 变了）→
已存的上游凭据解不开 → 那个车道退化成"未配置"。

**服务照常启动**（不会崩），只是要重新录一次 OpenRouter Key。

> ⚠️ 备份 `/data` 时，**`.secret-key` 必须一起备份**。
> 它和 SQLite 在同一个目录里，就是为了避免备份时漏掉。

---

## 5. 一次性登录令牌

管理台的登录有两条路：

### 5.1 用本机 Key 登录（推荐）

管理台首页有个输入框，填本机 Key → 换成一个 8 小时的会话 Cookie。

### 5.2 一次性令牌

服务端生成一个 **10 分钟有效、只能用一次**的令牌，形如
`fmh-boot-…`，通过 URL **fragment** 传递：

```
http://<NAS-IP>:8678/#token=fmh-boot-xxxx
```

⚠️ **用 fragment（`#`）而不是 query（`?`）** ——
fragment 不会发到服务器，所以**不会进访问日志**。

兑换后服务端立刻作废这个令牌，并 302 重定向到 `/`（清掉地址栏）。

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
| LAN Key 泄露 | 轮换 LAN Key 即可 —— 本机 Key、管理会话、上游凭据都不受影响 |
| 本机 Key 泄露 | 轮换本机 Key 会踢掉所有管理会话；上游凭据仍受 AES 保护 |
| 管理 Cookie 泄露 | 8 小时过期 + HttpOnly；轮换本机 Key 可立即作废所有会话 |
| 上游 Key 泄露 | 管理台永不回显；存储加密；只能重新录入 |
| 容器被攻破 | 二进制静态编译、无 shell、非 root；上游凭据虽可解，但**仅限本机** |
| 中间人 | 由用户的 agent 客户端的 TLS 负责（FMH 本身是明文 HTTP，**仅限可信 LAN**） |

### 7.1 明确不做的事

- ❌ 不做 HTTPS（应该放在 NAS 的反向代理后面）
- ❌ 不做 IP 白名单（Key 已经足够；白名单会让"给同事用"变得麻烦）
- ❌ 不记录请求的提示词与回答（只记元数据 —— 见 `docs/design/00-overview.md` §5.9）

---

## 8. 给 agent 的配置片段

### Claude Code

```bash
export ANTHROPIC_BASE_URL="http://<NAS-IP>:8678"
export ANTHROPIC_AUTH_TOKEN="fmh-xxxxxxxx"
```

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
zen/deepseek-v4.1-flash-free
openrouter/deepseek/deepseek-r1:free
```

⚠️ 也接受**裸模型名** —— 网关会自动解析到唯一的车道。
但有重名时（两个车道都有同名模型）必须用全名。

---

## 9. 相关文件

| 文件 | 职责 |
|---|---|
| `internal/server/keys.go` | 钥匙生成、校验、轮换、会话 |
| `internal/server/admin.go` | 管理 API（登录、Key、凭据、设置） |
| `internal/store/vault.go` | 上游凭据的 AES-256-GCM 密封 |
| `internal/store/db.go` | 持久化（Key、凭据、会话映射、用量） |
| `internal/server/server.go` | 单端口 HTTP 服务器与路由 |
