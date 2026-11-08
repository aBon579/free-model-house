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

### 运行日志页

调用请求 / 探测 / 密钥与认证 / 上游车道 / 系统与设置，五个类目都能在日志页分类查看。每一类都可以单独开关：关掉就不再记录，日志页也不再显示（历史记录仍留在数据库里，重新打开还能看到）。支持清空单个类目或全部。

### 自定义 API Key 可编辑

名称、Base URL、Key、模型清单、上下文长度都可以直接改；保存后该端点的模型池即时增减同步，不需要删掉重建。

### 上下文长度

自定义端点可以填写上下文长度。能探测到模型自身上限时会校验并提示，探测不到时允许填写并给出说明；这个值会作为 `context_window` 展示给 agent。

### 拉取模型更可靠

自动适配「Base URL 已含 /v1」与更长路径、末尾斜杠，以及 Bearer / x-api-key / api-key 三种认证头；请求走 FMH 的出口代理。失败时界面会给出尝试过的地址、状态码与响应片段，不再只有一句「拉取失败」。

### 车道前缀强制锁定

请求带车道前缀的模型（`zen/xxx`、`kilo/xxx`、`openrouter/xxx`，含大小写与首尾空格变体）只在这条车道里解析与调用；该车道没有这个模型就直接报错，不再被其他车道的同名模型顶替。

### 失败日志信息补全

调用失败时也会记录实际命中的「车道」与「上游模型」，并逐条列出每次尝试的车道、模型与结果摘要，排障不用再靠猜。

### Key 标签必填且唯一

新建 Key 必须填标签（≤6 字）；重命名也不能与其他 Key 重名（比较时忽略大小写、归一化首尾空格）。历史遗留的空标签 Key 不强制回填，界面上会提示补一个。

### 自动跨模型调用

原「跨车道回落」更名为「自动跨模型调用」，语义从「换车道」明确为「换模型」，并且改到模型级别：默认关闭，只重试当前模型，重试仍失败就直接报错、一个模型都不换；打开后才自动换模型，顺序仍是 Kilo ↔ Zen（免密、不花你的额度）优先，两条都不行才用 OpenRouter。升级时旧设置会自动迁移，开关不会自己变回关闭。

抖动类失败（连接断开 / 5xx）会先在原地重试（最多 2 次、间隔 250ms），不再立刻换模型；429 配额错误不重试，避免重复烧额度。账号级配额（`Free usage limit reached`）会直接跳到另一条免密车道，模型级配额则不跳。

### 探测目标选择

探测目标的选择变成确定性的：默认（自适应关）每轮全量；开启自适应则每轮最多 8 个、最久未探的优先；手动探测始终全量。不再按「结论新鲜 / 失败退避 / 限流冷却」跳过，每轮数量不再忽多忽少。

## 修复

- 用量页「调用 Key」跟随**实际命中的那把 Key**：同名、无名、前缀相近的多把 Key 不再混在一起
- 自定义端点拉取 `/models` 不再拼成 `/v1/v1/models`

## 文档

- [README](https://github.com/aBon579/free-model-house#readme)
- [NAS 部署指南](https://github.com/aBon579/free-model-house/blob/main/docs/operations/nas-deploy.md)
- [局域网接入](https://github.com/aBon579/free-model-house/blob/main/docs/operations/lan-access.md)
- [更新日志](https://github.com/aBon579/free-model-house/blob/main/CHANGELOG.md)

## 注意

免密车道的上游对客户端有身份限制，本项目通过复现官方客户端的线上行为来满足它——这是灰区，上游随时可能收紧。使用前请读 [SECURITY.md](https://github.com/aBon579/free-model-house/blob/main/SECURITY.md)。
