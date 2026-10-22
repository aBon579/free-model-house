# 安全策略

## 报告漏洞

**请不要开公开 issue。** 公开 issue 会让漏洞细节立刻可见。

请用 GitHub 的 **Private vulnerability reporting**
（仓库 → `Security` → `Report a vulnerability`）。

如果该功能没开，请发邮件给维护者，标题写 `[security]`。

**请包含**：

- 受影响版本
- 复现步骤
- 影响范围（谁能拿到什么）
- 你的 NAS 部署方式（`docker compose` / 手动二进制）

我会在 **7 天内**回应，至少先确认收到。

## 这个项目的安全模型

先说清楚**边界** —— 这比列漏洞更重要。

### 它**不是**为公网设计的

FMH 是一个**局域网**网关：

- 默认**不做 HTTPS**（应该放在 NAS 的反向代理后面）
- **不做 IP 白名单**
- 推理 Key 是**长期有效**的 bearer token

**把它直接暴露到公网 = 把你的免费模型额度送人。**

正确的做法：

```
公网 ──[HTTPS 反向代理 + 认证]──▶ NAS 内网 ──▶ FMH :8678
```

### 管理台

- 密码登录，**首次部署强制改掉**默认密码（`admin`）
- 改完密码会**作废所有管理会话**
- 会话 Cookie 是 `HttpOnly` + `SameSite=Lax`
- **故意不设 `Secure`** —— NAS 的 Web UI 常常是 `http://192.168.x.x`，
  设了 `Secure` 会让浏览器直接丢弃 Cookie

### 凭据存储

| | 处理 |
|---|---|
| OpenRouter Key | AES-256-GCM 加密存盘 |
| 加密密钥 | 来自 `FMH_SECRET_KEY`，未设置时自动生成在 `/data/.secret-key` |
| 管理台回显 | **只显示指纹**（`sk-or-v1-…abcd`），永不显示明文 |
| 下发给客户端 | **永不下发** |

**加密密钥丢失 = 已存的上游凭据无法解密**（只能重录）。
所以备份 `/data` 时要**连 `.secret-key` 一起**。

### 数据记录

**只记元数据** —— 模型、token 数、时间、客户端标识。

**不记**提示词、回答、请求内容。

## 已知的、故意不做的事

这些不是漏洞，是**有意的设计取舍**：

| 不做 | 原因 |
|---|---|
| HTTPS | 应该由反向代理负责 |
| IP 白名单 | 局域网场景不需要 |
| 多出口代理池 | 复杂度高，用户量小 |
| 自动升级 | NAS 上的自动更新容易出事 |
| 速率限制的分布式实现 | 单进程网关不需要 |

## 免密车道的隐私

**这条车道不是匿名的。**

- 你的请求会发到上游，**带你的出口 IP**
- 上游**可能记录** prompt 与回答
- Kilo 官方明确写了：*"Auto Free may route your requests to providers that log
  prompts and outputs and use them to improve their services."*

**不要用它处理密码、密钥、个人身份信息或商业机密。**

需要保密就用你自己的付费 API，或只用 OpenRouter 车道。

## 依赖安全

`vendor/` **提交进仓库** —— 镜像在离线 NAS 上构建，不联网拉依赖。

```bash
go mod tidy && go mod vendor
```

升级依赖时请一并提交 `go.mod`、`go.sum`、`vendor/`。

**已知**：SQLite 走 `github.com/ncruces/go-sqlite3`（纯 Go WASM 实现），
不需要 CGO，也没有原生代码。
