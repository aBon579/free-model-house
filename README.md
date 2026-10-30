<p align="center">
  <img src="fmhface.webp" alt="Free Model House" width="900">
</p>

<h1 align="center">Free Model House</h1>

<p align="center">
  局域网免费模型聚合网关 —— 一个端口，一把 Key，接通全部免费模型
</p>

<p align="center">
  <a href="https://github.com/aBon579/free-model-house/releases"><img src="https://img.shields.io/github/v/release/aBon579/free-model-house?style=flat-square&label=Release" alt="Release"></a>
  <a href="https://github.com/aBon579/free-model-house/actions/workflows/ci.yml"><img src="https://img.shields.io/github/actions/workflow/status/aBon579/free-model-house/.github/workflows/ci.yml?branch=main&style=flat-square&label=CI" alt="CI"></a>
  <a href="LICENSE"><img src="https://img.shields.io/github/license/aBon579/free-model-house?style=flat-square" alt="License"></a>
  <img src="https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat-square&logo=go" alt="Go">
  <img src="https://img.shields.io/badge/platform-amd64%20%7C%20arm64-lightgrey?style=flat-square" alt="Platform">
</p>

<p align="center">

```bash
mkdir fmh && cd fmh
curl -O https://raw.githubusercontent.com/aBon579/free-model-house/main/docker-compose.yml
docker compose up -d
```

</p>

---

## 它做什么

```
你的 Agent ──[fmh-xxx]──▶ FMH ──[Bearer public]──▶ OpenCode Zen 免密车道（zen）
                         ├─[无需凭据]──────▶ Kilo 免 key 池（kilo）
                         └─[你自己的 OR Key]▶ OpenRouter 免费模型（openrouter）
```

| 对谁 | 提供什么 |
|---|---|
| **调用方** | 一个 OpenAI / Anthropic 兼容的 API，一把固定前缀的 Key |
| **上游** | 一个符合官方客户端线上行为的请求，加上故障转移 |
| **运维者** | 一个 Web 控制台：哪个模型现在能用、上次是谁应答的、为什么 |

免费模型不是"接上就能用"的——FMH 把四件事做在了一层：

| 障碍 | 处理 |
|---|---|
| 免费车道只认官方客户端指纹 | 复现官方客户端的线上行为 |
| 全局共享配额，高峰大面积限流 | 跨模型自动故障转移（可关） |
| 付费模型和免费模型混在一个清单 | 三级信任链 + `401` 探针判定 |
| 上游流会断，客户端拿到残缺答案 | 断流续写 |

## 三条上游车道

|  | zen | kilo | openrouter |
|---|---|---|---|
| 需要你的 Key | ❌ 共享匿名池 | ❌ | ✅ 要 |
| 花谁的钱 | 全网共享 | 上游提供 | 你自己的额度 |
| 默认 | ✅ 开启 | ✅ 开启 | 不配 Key 不参与路由 |
| 参与自动探测 | ✅ | ✅ | ❌（会花你的额度） |
| 故障转移目标 | — | ✅ 互为备胎 | 需手动开启 |

免密车道的可用性**跟着出口 IP 走**——被风控时整条车道失效。两条免密车道互为出口无关的备胎；出口代理页可以让全部上游流量经你指定的代理转发，换出口即换配额与地区判定。

## 快速开始

**① 部署**（见顶部命令框，或[自己构建](#构建)）—— amd64 / arm64 自动选镜像。

**② 拿 Key** —— 启动日志直接打印，或：

```bash
docker exec free-model-house /fmh key
```

**③ 接进你的 Agent**：

```bash
export ANTHROPIC_BASE_URL="http://<NAS-IP>:8678/v1"
export ANTHROPIC_AUTH_TOKEN="fmh-你的key"
```

模型名带车道前缀：

```
zen/minimax-m3.1-flash-free
openrouter/deepseek/deepseek-chat-v3-0324:free
```

**④ 登录管理台** `http://<NAS-IP>:8678/` —— 初始密码 `admin`，首次登录强制改密。多把 Key、限流档位、出口代理、探测设置都在面板里。

## 主要功能

- **多 Key（Key Hub）** —— 每台设备一把：命名标签、独立启停、独立限流档位（light / standard / deep）
- **两层限流** —— 网关级档位（off / light / standard / deep）优先级最高；回 off 后各 Key 档位自动恢复
- **出口代理** —— HTTP / HTTPS / SOCKS5，全局单入口，即点即生效
- **自动探测** —— 按设定间隔全量探一遍免费模型，结论过期间隔自动回到"未探测"；限流比例自适应批量
- **自动跨模型调用** —— Kilo ↔ Zen 优先（不花钱），OpenRouter 需显式开启
- **断流续写** —— 上游流断了把已产出内容接回去重试
- **用量** —— 365 天热力图、按模型明细、最近调用置顶；只记元数据，不记提示词与回答

## 配置

环境变量只有这些，其余都在管理台里改：

| 变量 | 默认 | 说明 |
|---|---|---|
| `FMH_ADDR` | `:8678` | 监听地址 |
| `FMH_DATA` | `/data` | 数据目录（**必须持久化**） |
| `FMH_SECRET_KEY` | 自动生成 | 凭据加密密钥；变更后已存凭据无法解密 |
| `FMH_HIDE_KEYS` | `0` | `1` = 启动日志只打 Key 指纹 |
| `TZ` | `Asia/Shanghai` | 时区，影响按天用量切分 |

## 构建

```bash
git clone https://github.com/aBon579/free-model-house && cd free-model-house
docker compose up -d --build
```

三阶段 Dockerfile（node → golang → scratch），镜像约 14 MB。容器以 root 启动后自行降权到 nobody。

推 `v*` tag 自动构建 amd64 + arm64 推 GHCR 并创建 Release。

## 文档

| 文档 | 内容 |
|---|---|
| [设计总览](docs/design/00-overview.md) | 架构、技术选型、目录结构 |
| [免密车道规格](docs/design/01-zen-fingerprint.md) | 客户端指纹闸门与回归清单 |
| [Key 模型](docs/design/02-key-model.md) | Key 与登录模型、端口布局、威胁模型 |
| [API 契约](docs/design/03-api-surface.md) | 路由表、三协议细节、错误码 |
| [UI 方案](docs/design/04-ui.md) | 视觉语言、触屏适配 |
| [NAS 部署指南](docs/operations/nas-deploy.md) | 构建、配置、验证、排障、备份 |
| [局域网接入](docs/operations/lan-access.md) | 各客户端配置片段 + 排障 |
| [上游一致性验证](docs/verification/upstream-conformance.md) | 免密车道实现与官方客户端的对照 |

## 参与贡献

```bash
go test ./...            # 后端
cd web && npm ci && npm run build   # 前端
```

## 许可证

[MIT](LICENSE)

---

> **边界说明**：免密车道是上游提供的免费产品档位，有客户端身份限制。本项目复现官方客户端的线上行为来满足它——这属于灰区，上游随时可能收紧；使用前请阅读 [SECURITY.md](SECURITY.md)。OpenRouter 的免费额度绑定你自己的账户，FMH 不碰你的付费额度，除非你显式开启跨模型调用。
