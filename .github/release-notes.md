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

## 0.1.1 的新东西

### 多 Key（Key Hub）

每台设备一把独立 Key：命名标签（≤6 字）、独立启停、独立限流档位（light / standard / deep）。旧版的单 Key 升级后自动迁入为「主 Key」，现有 Agent 配置无需任何改动。

新增两层限流：网关级档位（off / light / standard / deep）优先级最高，非 off 时所有 Key 强制跟随；切回 off 后各 Key 的档位设置自动恢复生效。

### 出口代理

上游流量可经你指定的代理转发（HTTP / HTTPS / SOCKS5，全局单入口）。换出口 IP 即换上游对你的配额与地区判定；开关即点即生效，检测异步执行、任何状态下都可刷新出口。

### 用量改进

365 天热力图（按容器宽度自适应）；模型明细按最近调用时间排序，最新在用的一眼置顶。

## 修复

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
