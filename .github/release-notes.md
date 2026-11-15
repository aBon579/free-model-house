局域网免费模型聚合网关。把 OpenCode Zen / Kilo / OpenRouter 的免费模型聚合成一个 OpenAI / Anthropic 兼容接口，跑在 NAS 上给局域网里的 Agent 用。这一版加上了**用自己的账号接入的上游车道**（Qoder / Qoder 中国版 / TRAE）。

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

## 0.1.3 的新东西

### Credit 车道：用自己的账号接入

新增 `credit` 车道，管理你自己的 Qoder、Qoder 中国版、TRAE 账户：按渠道独立的账户列表、余额与资源包、活动清单与每日领取。账号里的模型对外写成 `credit/<提供方>/<上游模型>`，和免密车道一样直接接进 Agent。不同渠道的额度单位不合并、不换算 —— 这里的「1,000」和那里的「1,000」不是一种东西。

加账号一律走浏览器授权，只有你自己登录得到的账号才收；凭据按账号密封存进数据目录，界面和接口只显示指纹。

### 账号级每日自动领取

每个账号单独填一个领取时刻（UTC+8），留空就是这个号不自动领。到点只按活动清单领取一次，失败当天不重试，并且不会发送任何推理请求 —— 自动领取不花你的调用额度。旧的「全部渠道」总开关会自动迁移成各账号的时刻，升级完不会出现你没要求的领取。

### Qoder 的「每日签到」要单独送一份设备身份

上游在这对请求头缺失时**不下发「每日签到」这一项活动，也不给任何错误码**，症状就是「点了没反应、余额不涨」。这对值是 Qoder 客户端在你自己电脑上算出来的，登录链路（浏览器授权 / 刷新凭据）都不会返回它，而网关跑在 NAS 上没有客户端。

所以这一版把它做成显式的一步：

1. 在装有 Qoder 客户端的电脑上双击 `tools/qoder-machine-identity.cmd`（就一个文件，PowerShell 正文在它自己的下半段里）。它默认在桌面生成 `qoder-machine-identity.json`，实际落在哪个目录以脚本打印的那一行为准。
2. 在这台电脑的浏览器里打开 FMH → Credit 页 →「Qoder 设备身份」→「导入」，选这个文件保存。原值只在提交那一次经过网络，之后界面只回显指纹。

网关存的是**全局一份**：它是设备级的，与登录哪个账号无关。账号凭据里本来就成对带着这两个值的，那个账号用自己的。身份会随客户端升级或重新生成而换新，领不到时先在电脑上重跑一次脚本再导入，不必怀疑账号。

领取结果分成三种说法，不再混成一句「签到请求已完成」：缺设备身份 / 清单里没有可领项 / 上游记着今天已领。

### 领取失败不再牵连推理

领取失败按错误类型分档暂停自动领取：凭据类问题长期停，套餐上限 12 小时，限流 1 分钟，上游故障 10 分钟；上游只要正常应答过一次，档位就自动清除。这套暂停只作用于自动领取，推理车道的模型可用性与各 Key 的限流档位不受影响。

### 其他

- 进 Credit 页或切换渠道时自动补一发余额查询，不用手点「刷新列表」。
- 备份导出 / 导入带上设备身份与各账号的领取时刻；导出仍然要求显式 `confirm=true`，并在运行日志留痕 —— 那份文件里是完整凭据，请自己保管好。

### 摘掉 ZCode 渠道

ZCode 的领取依赖上游验证码补验：token 一次性、只能由人来解，自动化路径始终不稳，整条通道连同前端开关一起删除。库里已无适配器的 ZCode 账号会在装载时清掉并写进启动日志 —— 密封保存的凭据不该静默消失。

## 修复

- TRAE 的授权链接不再在第一屏报「登录失败」：`client_id` 曾被当成每次登录现编的随机值发出去，而授权页第一屏就按它判决客户端，连输手机号那一屏都到不了。
- TRAE 用量缺席按「未消耗」计算（上游没下发就是 0，不再显示成未知），余额按分类与总数聚合展示。

## 文档

- [README](https://github.com/aBon579/free-model-house#readme)
- [NAS 部署指南](https://github.com/aBon579/free-model-house/blob/main/docs/operations/nas-deploy.md)
- [局域网接入](https://github.com/aBon579/free-model-house/blob/main/docs/operations/lan-access.md)
- [更新日志](https://github.com/aBon579/free-model-house/blob/main/CHANGELOG.md)

## 注意

免密车道的上游对客户端有身份限制，本项目通过复现官方客户端的线上行为来满足它——这是灰区，上游随时可能收紧。使用前请读 [SECURITY.md](https://github.com/aBon579/free-model-house/blob/main/SECURITY.md)。
