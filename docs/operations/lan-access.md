# 局域网接入指南

> 面向**最终用户**：你在 NAS 上跑着 FMH，现在想让家里/办公室的 Agent 用上免费模型。

---

## 0. 三步走

```
① 打开管理台 http://<NAS-IP>:8678/ → 用「本机 Key」登录
② 「API 接入」页 → 复制 Base URL 和「局域网 Key」
③ 把配置喂给你的 Agent
```

---

## 1. 拿到接入信息

管理台 → **API 接入**，你会看到：

```
Base URL:  http://192.168.31.5:8678/v1
API Key:   fmh-3f9a2b8c...
```

| 项 | 说明 |
|---|---|
| **Base URL** | NAS 的局域网 IP + `8678` + `/v1`。管理台会列出**所有**检测到的网卡 IPv4，挑你实际用的那个 |
| **API Key** | 局域网 Key。点「显示」才可见，点「复制」直接进剪贴板 |
| **端口** | `8678`，跟管理面**共用**。改端口要改 `FMH_ADDR` 环境变量并重启 |

### ⚠️ 两个 Key 都是 `fmh-` 前缀 + 40 位 hex

| Key | 用途 | 能不能给 Agent |
|---|---|---|
| **本机 Key** | 登录管理台 + 本机推理 | ❌ **别给** —— 它能进管理台 |
| **局域网 Key** | LAN 里的 Agent 用 | ✅ 给这个 |

它们是两个**独立的值**，不是同一个。
本机 Key 在管理台「服务设置」里能看到指纹。

---

## 2. 单端口设计

推理面和管理面**共用 8678**：

| 路径 | 鉴权 | 用途 |
|---|---|---|
| `/v1/*` | Key | 推理（OpenAI / Anthropic 协议） |
| `/api/*` | 会话 Cookie | 管理面 |
| `/` | 无（只是页面） | 管理台的 HTML |
| `/healthz` | 无 | 健康检查 |

⚠️ **管理路由和推理路由在同一端口上** ——
   所以**管理面靠会话 Cookie 鉴权**，不靠"换个端口就隔离"。

⚠️ 「局域网推理」开关**默认是开的**。
关掉后，**非本机来源**的推理请求会返回 403
（`局域网推理已关闭 —— 只能在 NAS 本机使用`）。
本机 Key 不受影响。

---

## 3. 接进各个 Agent

### Claude Code

```bash
export ANTHROPIC_BASE_URL="http://192.168.31.5:8678/v1"
export ANTHROPIC_AUTH_TOKEN="fmh-xxxxxxxx"
```

⚠️ Claude Code 拼的是 `{BASE_URL}/v1/messages`，
   所以 `ANTHROPIC_BASE_URL` 要**带 `/v1`**。

### OpenAI 兼容（OpenCode / Cline / Aider）

```bash
export OPENAI_BASE_URL="http://192.168.31.5:8678/v1"
export OPENAI_API_KEY="fmh-xxxxxxxx"
```

Python SDK：

```python
from openai import OpenAI
client = OpenAI(base_url="http://192.168.31.5:8678/v1", api_key="fmh-xxxxxxxx")
```

### OpenCode（`opencode.json`）

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "fmh": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Free Model House",
      "options": { "baseURL": "http://192.168.31.5:8678/v1" },
      "models": {
        "zen/minimax-m3.1-flash-free": { "name": "MiniMax M3.1 Flash" }
      }
    }
  }
}
```

### Cline / Roo Code

```json
{
  "apiProvider": "openai",
  "openAiBaseUrl": "http://192.168.31.5:8678/v1",
  "openAiApiKey": "fmh-xxxxxxxx"
}
```

### Aider

```yaml
# ~/.aider.conf.yml
model: openrouter/zen/minimax-m3.1-flash-free
openai-api-base: http://192.168.31.5:8678/v1
openai-api-key: fmh-xxxxxxxx
```

### curl

```bash
curl http://192.168.31.5:8678/v1/chat/completions \
  -H "Authorization: Bearer fmh-xxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"zen/minimax-m3.1-flash-free",
       "messages":[{"role":"user","content":"你好"}]}'
```

---

## 4. 模型名怎么写

模型名是 `车道/模型`：

| 写法 | 含义 |
|---|---|
| `zen/minimax-m3.1-flash-free` | 免密车道的模型 |
| `openrouter/deepseek/deepseek-chat-v3-0324:free` | OpenRouter 的模型 |

⚠️ **OpenRouter 的模型 id 里本来就有斜杠** ——
   按**第一个**斜杠切分，所以是 `openrouter/` + `deepseek/deepseek-chat-v3-0324:free`。

不带车道前缀也能用（比如 `minimax-m3.1-flash-free`），
FMH 会按清单里的归属解析。⚠️ 但**同名歧义**时会有问题，
所以**建议总是写全**。

完整清单看管理台「模型清单」页，或者：

```bash
curl http://192.168.31.5:8678/v1/models \
  -H "Authorization: Bearer fmh-xxxxxxxx"
```

---

## 5. 三种协议

| 协议 | 路径 | 典型客户端 |
|---|---|---|
| OpenAI Chat | `/v1/chat/completions` | Cline / Aider / 多数 SDK |
| OpenAI Responses | `/v1/responses` | OpenAI SDK 的 `responses.create` |
| Anthropic Messages | `/v1/messages` | Claude Code |

无 `/v1` 的简写也支持（`/chat/completions` 等），
方便某些只拼相对路径的客户端。

---

## 6. 排障

### 401

Key 不对，或者用了**本机 Key**却从别的机器调。

### 403 `局域网推理已关闭`

管理台「服务设置」→ 打开「局域网推理」。

### 403 `FreeTierError`

免密车道的闸门拒绝。可能是：
- 上游改了指纹规格 → 管理台「上游源」页 → **开始诊断**
- 出口 IP 被风控 → 换个出口，或开「自动跨模型调用」

### 404 on `/v1/models`

路径写错了。正确的是 `/v1/models`（带 `/v1`）。

### 429

上游限流。FMH 会**自动换模型重试**（同一个候选链里）。
如果持续 429，说明整个免密车道的配额都被你自己占满了 ——
   看管理台「模型清单」页的冷却倒计时。

### 连接超时

```bash
# 先在 NAS 本机测
curl -s http://127.0.0.1:8678/healthz && echo OK
```

本机通、局域网不通 → **防火墙**问题，不是 FMH 的问题。

---

## 7. 安全

### 已经做了的

- 管理面**同源严格**（不开放 CORS）
- 管理会话用 HttpOnly Cookie，8 小时过期
- 上游凭据用 **AES-256-GCM** 密封，只在 NAS 上
- 管理面**不回显**上游凭据明文，只显示指纹

### 建议你自己加的

1. **防火墙限制来源** —— NAS 上限制 `:8678` 只接受特定网段。
2. **别把 8678 暴露到公网** —— 要公网用就套反向代理 + TLS。
3. **定期轮换局域网 Key** —— 管理台「服务设置」里有「轮换」。
4. **备份 `/data`** —— 里面有密钥和 Key，丢了就解不开凭据。

---

## 8. 相关的文档

- [`nas-deploy.md`](./nas-deploy.md) —— 怎么装上去
- [`../../design/03-api-surface.md`](../../design/03-api-surface.md) —— 完整的 API 表面
- [`../../design/02-key-model.md`](../../design/02-key-model.md) —— Key 分层与威胁模型
