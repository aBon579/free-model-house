# NAS 部署指南

> 面向**最终用户**：你要把 FMH 装在自己的 NAS 上，给局域网里的 Agent 提供免费模型。

---

## 0. 三分钟速览

```bash
# ① 把项目拷到 NAS（见 §1）
ssh user@nas-ip "mkdir -p ~/fmh"

# ② 进去构建
ssh user@nas-ip "cd ~/fmh && docker compose up -d --build"

# ③ 看日志，确认 Key 和管理台初始密码
ssh user@nas-ip "cd ~/fmh && docker compose logs -f fmh"
```

启动成功后：

| 用途 | 地址 |
|---|---|
| **管理台**（浏览器打开） | `http://<NAS-IP>:8678/` |
| **推理面**（Agent 用） | `http://<NAS-IP>:8678/v1` |
| **Agent 的 API Key** | `fmh-…`（管理台「API 接入」页复制） |

⚠️ **单端口** —— 推理面和管理面共用 8678。
   所以「局域网推理」开关**默认是开的**，不用另外配。

⚠️ **只有一把推理 Key**（`fmh-` 前缀 + 40 位 hex），管理台用**密码**登录：

| 凭据 | 用途 |
|---|---|
| **局域网 Key**（`fmh-…`） | 给 Agent 调用推理面用 |
| **管理密码** | 浏览器登录管理台（初始 `admin`，首次登录强制改） |

两者是**两套独立的东西**：Key 泄露不影响管理台，改密码也不影响任何 Agent 配置。
⚠️ Key 仍然只发给你信任的设备 —— 拿到它就能白嫖你的免费车道。

---

## 1. 把项目放到 NAS 上

### 要上传哪个？

**`Free Mdel House` 这一个文件夹**，整个传。

它包含 Docker 构建需要的一切：

```
Free Mdel House/
├── Dockerfile          ← 构建脚本
├── .dockerignore
├── docker-compose.yml  ← 编排
├── .env.example
├── go.mod / go.sum     ← Go 依赖
├── cmd/fmh/            ← 入口
├── internal/           ← 后端源码
├── web/                ← 前端源码（+ package.json / package-lock.json）
├── docs/  README.md    ← 文档（不影响构建）
└── vendor/             ← 依赖源码（离线构建用）
```

⚠️ **不要**单独传 `internal/` 或 `web/` —— 缺了 Dockerfile 就没法构建。

### 怎么传

**方式 A：git clone（推荐）**

```bash
git clone https://github.com/aBon579/free-model-house
cd free-model-house
```

`node_modules` 由镜像里的 `npm ci` 安装，`data/` 是本机运行时数据 —— 都不需要手动传。

**方式 B：用镜像**

不想在 NAS 上构建的话，直接 `docker compose up -d` 拉 GHCR 上的镜像。

⚠️ **不需要在 NAS 上装 Go 或 Node** ——
   Dockerfile 里的三个阶段（node → golang → scratch）全在容器里跑。

---

## 1.5 镜像源

需要能拉 `node:22-alpine` 和 `golang:1.26-alpine` 两个基础镜像。

⚠️ 如果 NAS 网络拉不动（比如 fnOS 的 mirror 缺认证头导致 401），
   可以在一台能联网的机器上先构建好镜像，再传过去：

```bash
# 有网的机器
docker build -t fmh:latest .
docker save -o fmh.tar fmh:latest

# NAS
docker load -i fmh.tar
docker compose up -d
```

📖 历史背景见 [`fnos-401.md`](./fnos-401.md)。

### ⚠️ 完全不用 Docker 的话（不依赖任何镜像源）

```bash
./scripts/build-linux.sh amd64    # → 11.6 MB 纯静态二进制
scp fmh-linux-amd64 user@<NAS-IP>:~/
# NAS 上：
FMH_ADDR=":8678" FMH_DATA="$HOME/fmh-data" ~/fmh-linux-amd64
```

---

## 2. 配置

```bash
cp .env.example .env
vi .env
```

`.env` 里**只有 4 个变量是程序真的读的**：

| 变量 | 默认 | 说明 |
|---|---|---|
| `FMH_ADDR` | `:8678` | 监听地址。**只绑本机**就写 `127.0.0.1:8678` |
| `FMH_DATA` | `/data` | 数据目录。**必须持久化** |
| `FMH_SECRET_KEY` | 空 | 凭据加密密钥。留空会自动生成到 `/data/.secret-key` |
| `TZ` | `Asia/Shanghai` | 时区。影响「按天」用量统计的切分点 |

⚠️ **不要设那些程序不认的变量** ——
   写了也没效果（比如 `FMH_LISTEN`、`FMH_PROBE_INTERVAL_MIN`）。

### 关于 `FMH_SECRET_KEY`

- **留空** → 首次启动自动生成 32 字节随机密钥，写进 `/data/.secret-key`。
- **手动设** → 用这个命令生成：
  ```bash
  openssl rand -base64 32
  ```
  结果填进去。

⚠️ **这个密钥丢了，存进去的 OpenRouter Key 就解不开了**（只能重新录）。
   所以要么用 `FMH_SECRET_KEY` 并自己备份，要么**确保 `/data` 目录有备份**。

---

## 3. 启动

```bash
docker compose up -d --build
```

### ⚠️ 首次构建要几分钟

三个阶段都要下东西（node 镜像 ~180MB、golang 镜像 ~800MB）。

### ⚠️ 架构要选对

`docker-compose.yml` **不写死架构** —— Docker 会自动用宿主机的。

- **x86_64 NAS** → 默认就行
- **arm64 NAS** → 也能直接跑（Go 静态编译，scratch 镜像）

⚠️ 前提是 NAS 能拉取对应架构的 node/golang 基础镜像。
   如果镜像源只镜像了 amd64，构建会失败 ——
   那时候再给 compose 加 `platform: linux/amd64` 强制拉。

### 看日志

```bash
docker compose logs -f fmh
```

应该看到：

```
fmh  数据库 /data/fmh.db
fmh  局域网 Key fmh-EXAMPLEKEY0000000000000000000000000000
fmh  管理台初始密码：admin（首次登录后必须修改）
fmh  [zen] 清单 9 个模型
fmh  [kilo] 清单 17 个模型
fmh  [openrouter] 清单 18 个模型
fmh  监听 :8678
```

⚠️ **日志里是完整 Key** —— 直接复制就能用；同一屏还会告诉你管理台初始密码。

⚠️ 为什么明文进日志：
   启动日志是**唯一**可靠的取 Key 途径 ——
   `docker exec free-model-house /fmh key` 要求容器**正在运行**；
   日志只给指纹的话，首次部署就卡住了。

⚠️ 不想让明文进日志（会被收进日志系统、贴到工单）：
   设 `FMH_HIDE_KEYS=1` 就只打指纹。

其他取法：

```bash
docker exec free-model-house /fmh key          # 打印这把 Key（明文）
docker exec free-model-house /fmh key lan      # 同上，历史别名
docker exec free-model-house /fmh key both     # 同上，历史别名（只剩一把）
```

⚠️ 管理台登录后也能在「服务设置」页点「显示完整 Key」查看 ——
默认只给指纹，明文要**主动**点一下。

---

## 4. 验证

### 容器活着

```bash
docker compose ps
# STATUS 应该显示 healthy
```

### 健康检查

```bash
docker compose exec fmh /fmh healthcheck && echo OK
```

### 命令行子命令

| 命令 | 作用 |
|---|---|
| `fmh` | 正常启动 |
| `fmh healthcheck` | 健康检查（容器 healthcheck 用） |
| `fmh version` | 打印版本 |
| `fmh key` | 打印**完整**推理 Key |
| `fmh key lan` | 同上（历史别名） |
| `fmh key both` | 同上（历史别名，现在只剩一把） |

⚠️ `fmh key` 打印的是**明文** —— 它只读数据目录，不启动服务，
   所以能在容器里跑（`scratch` 里没有 shell，路径要绝对）：

```bash
docker exec free-model-house /fmh key
```

⚠️ **安全权衡**：能访问数据目录的人本来就能读到数据库里的明文 Key，
   所以这个命令**没有扩大**攻击面 —— 只是把"要手动翻 SQLite"变成一条命令。

### 推理面通

```bash
# 换成你的局域网 Key
curl -s http://<NAS-IP>:8678/v1/models \
  -H "Authorization: Bearer fmh-你的key" | head -c 300
```

应该返回模型清单。

⚠️ 推理面只认**这一把** `fmh-` Key —— 不再分本机/局域网。

### 管理台能打开

浏览器打开 `http://<NAS-IP>:8678/`，用**密码**登录（初始 `admin`，会强制你先改）。

---

## 5. 接入 Agent

### Claude Code

```bash
export ANTHROPIC_BASE_URL="http://<NAS-IP>:8678/v1"
export ANTHROPIC_AUTH_TOKEN="fmh-你的key"
```

⚠️ Claude Code 拼的是 `{BASE_URL}/v1/messages`，
   所以 `ANTHROPIC_BASE_URL` 要**带 `/v1`**。

### OpenAI 兼容客户端（OpenCode / Cline / Aider）

```json
{
  "baseURL": "http://<NAS-IP>:8678/v1",
  "apiKey": "fmh-你的key"
}
```

### curl 试一把

```bash
curl http://<NAS-IP>:8678/v1/chat/completions \
  -H "Authorization: Bearer fmh-你的key" \
  -H "Content-Type: application/json" \
  -d '{"model":"zen/minimax-m3.1-flash-free",
       "messages":[{"role":"user","content":"你好"}]}'
```

### 三种协议都支持

| 协议 | 路径 | 典型客户端 |
|---|---|---|
| OpenAI Chat | `/v1/chat/completions` | Cline / Aider / 多数 SDK |
| OpenAI Responses | `/v1/responses` | OpenAI SDK 的 `responses.create` |
| Anthropic Messages | `/v1/messages` | Claude Code |

三种路径的**无 `/v1` 简写**也支持（`/chat/completions` 等），
方便某些只拼相对路径的客户端。

---

## 6. 常见问题

### 构建失败：`401 Unauthorized`（拉镜像）

```
failed to solve: node:22-alpine: failed to resolve source metadata for
docker.io/library/node:22-alpine: ... 401 Unauthorized
```

⚠️ **这是 NAS 的镜像源问题，不是 FMH 的 bug** ——
   fnOS 默认的 `docker.fnnas.com` 需要认证但 Docker 不传认证头。

📖 完整解决步骤见 [`fnos-401.md`](./fnos-401.md)。

最快的两条路：

```bash
# ① 换镜像源（fnOS 的 Docker 设置界面，或改 /etc/docker/daemon.json）
sudo systemctl restart docker

# ② 或者：完全不用 Docker —— 在别处交叉编译好二进制传过去
./scripts/build-linux.sh amd64    # 生成 fmh-linux-amd64（11.6 MB，纯静态）
scp fmh-linux-amd64 user@<NAS-IP>:~/
```

### 容器起不来，日志说 `打开数据库失败: permission denied`

⚠️ **这是权限问题**，不是配置问题。

原因：NAS 上 `./data` 目录如果是**别的方式建的**（比如手动 `mkdir`，
或者你在 NAS 文件管理器里建的），属主可能不是容器里的 nobody。

修法 —— **让容器自己管这个目录**：

```bash
sudo rm -rf ./data          # ⚠️ 会删掉所有 Key！只在首次部署时做
docker compose up -d
```

程序启动时会自动把目录属主改成 nobody 再降权。

### 防火墙 / 端口不通

```bash
# NAS 上先自测
curl -s http://127.0.0.1:8678/healthz && echo OK
```

本机通、局域网不通 → 是**防火墙**：
- 群晖：控制面板 → 安全 → 防火墙 → 新增规则 → TCP 8678
- 威联：控制面板 → 安全性 → 防火墙 → 新增规则
- 路由器：确认没开 AP 隔离

### 免密车道全 403

看日志里有没有：

```
⚠️ 上游闸门拒绝（xxx）：…
```

这说明**上游改了指纹规格**或者**出口 IP 被风控**。

打开管理台「上游源」页 → 点**开始诊断** → 它会**逐项试出是哪一条坏了**。

### OpenRouter 额度

「上游源」页能看到**剩余额度**和**已用**。

⚠️ 免费模型（`:free` 后缀）**不扣钱**，但仍消耗你的每日免费额度。

### 更新

```bash
cd ~/fmh
git pull
docker compose up -d --build
```

⚠️ **别删 `./data`** —— 那是所有 Key 和凭据。

---

## 7. 备份

要备份的只有两样：

```bash
cd ~/fmh
tar czf fmh-backup-$(date +%F).tar.gz data/
```

⚠️ `data/` 里包含：
- `fmh.db` —— 设置、Key、用量
- `.secret-key` —— **凭据加密密钥**（丢了就解不开 OpenRouter Key）
- `fmh.db-wal` / `fmh.db-shm` —— SQLite 的 WAL

⚠️ **备份时先停容器** —— 否则 WAL 里的数据可能不完整：
```bash
docker compose stop
tar czf fmh-backup-$(date +%F).tar.gz data/
docker compose start
```
