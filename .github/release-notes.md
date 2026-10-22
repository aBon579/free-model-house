## 安装

```bash
mkdir fmh && cd fmh
curl -O https://raw.githubusercontent.com/aBon579/free-model-house/main/docker-compose.yml
docker compose up -d
```

不需要装 Go 或 Node，Docker 会按你的架构（amd64 / arm64）自动选镜像。

镜像地址：

```
ghcr.io/abon579/free-model-house:latest
```

## 这是什么

把三条上游的免费模型聚合成一个 **OpenAI / Anthropic 兼容接口**，
跑在局域网里给 Agent 用：

| 车道 | 说明 |
|---|---|
| `zen` | OpenCode Zen 免密车道（共享凭据，需要客户端指纹） |
| `kilo` | Kilo 免 key 池（无需凭据，标准 OpenAI 兼容） |
| `openrouter` | OpenRouter 免费模型（用你自己的 Key） |

模型 ID 带车道前缀（`zen/xxx`、`kilo/xxx`、`openrouter/xxx`）。

## 主要能力

- **自动探测**：定期发最小请求确认可用性，记录首字延迟
- **自动跨模型调用**：模型失败时按 `Kilo ↔ Zen` → `OpenRouter` 顺序切换
  （默认关闭；关闭时只重试当前模型，一个模型都不换）
- **上游合规监视器**：闸门被拒时明确告诉你是哪一类问题
- **断流续写**：上游流中断时把已产出的内容接回去并重试
- **三级信任链**：用 `401` 探针实测判定免费，不靠模型名猜
- **`X-FMH-Model` 响应头** + 管理台「实际服务」列 —— 故障转移不再静默

## 管理台

单端口 `:8678` 同时提供推理面（`/v1/*`）与管理面（`/api/*`、`/`）。
密码登录、模型清单、用量热力图、亮暗主题、手机适配。

## 文档

- [README](https://github.com/aBon579/free-model-house#readme)
- [更新日志](https://github.com/aBon579/free-model-house/blob/main/CHANGELOG.md)
- [NAS 部署指南](https://github.com/aBon579/free-model-house/blob/main/docs/operations/nas-deploy.md)

## 注意

免密车道的上游对客户端有身份限制，本项目通过复现官方客户端的线上行为来满足它 ——
这是一个**灰区**，上游随时可能收紧。使用前请读 [SECURITY.md](https://github.com/aBon579/free-model-house/blob/main/SECURITY.md)。
