局域网免费模型聚合网关。把 OpenCode Zen / Kilo / OpenRouter 的免费模型聚合成一个 OpenAI / Anthropic 兼容接口，跑在 NAS 上给局域网里的 Agent 用。

## 安装

```bash
mkdir fmh && cd fmh
curl -O https://raw.githubusercontent.com/aBon579/free-model-house/main/docker-compose.yml
docker compose up -d
```

不用装 Go 或 Node —— 镜像有 `linux/amd64` 与 `linux/arm64`，Docker 按你的架构自动选择。

镜像地址：

```
ghcr.io/abon579/free-model-house:latest
```

## 0.1.2 的新东西

### 自定义 API Key 可编辑

名称、Base URL、Key、模型清单、上下文长度都可以直接改；保存后该端点的模型池即时增减同步，不需要删掉重建。

### 上下文长度

自定义端点可以填写上下文长度。能探测到模型自身上限时会校验并提示，探测不到时允许填写并给出说明；这个值会作为 `context_window` 展示给 agent。

### 拉取模型更可靠

自动适配「Base URL 已含 /v1」与更长路径、末尾斜杠，以及 Bearer / x-api-key / api-key 三种认证头；请求走 FMH 的出口代理。失败时界面会给出尝试过的地址、状态码与响应片段，不再只有一句「拉取失败」。

## 修复

- 用量页「调用 Key」跟随**实际命中的那把 Key**：同名、无名、前缀相近的多把 Key 不再混在一起
- 自定义端点拉取 `/models` 不再拼成 `/v1/v1/models`
- 探测轮次按墙钟对齐：下一轮 = 本轮开始 + 间隔，不再随轮次耗时漂移
- 「探测结论」只统计最近一轮，不再混入历史结论
- 探测结论超过设定间隔后面板自动回到「未探测」
- 打码状态下复制 Key 得到的是完整值（明文不进 DOM）
- 关闭的车道不再出现在模型清单与 `/v1/models`
- 出口检测改异步两段式，不再被浏览器超时掐断
- 回显源缺 country 函数导致的崩溃（已加回归测试）

## 文档

- [README](https://github.com/aBon579/free-model-house#readme)
- [NAS 部署指南](https://github.com/aBon579/free-model-house/blob/main/docs/operations/nas-deploy.md)
- [局域网接入](https://github.com/aBon579/free-model-house/blob/main/docs/operations/lan-access.md)
- [更新日志](https://github.com/aBon579/free-model-house/blob/main/CHANGELOG.md)

## 注意

免密车道的上游对客户端有身份限制，本项目通过复现官方客户端的线上行为来满足它——这是灰区，上游随时可能收紧。使用前请读 [SECURITY.md](https://github.com/aBon579/free-model-house/blob/main/SECURITY.md)。
