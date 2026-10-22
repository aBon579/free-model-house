# fnOS（飞牛 NAS）拉镜像 401 —— 根因与解法

> ⚠️ **本文是历史记录，当前版本已不受影响。**
>
> Dockerfile 现在用 `FROM scratch`，只拷贝预编译好的 `dist/`，
> **构建时不拉任何镜像** —— 所以下面的问题不会再遇到。
> 保留本文是为了解释当初为什么踩这个坑。

> 报错长这样：
>
> ```
> failed to solve: node:22-alpine: failed to resolve source metadata for
> docker.io/library/node:22-alpine: unexpected status from HEAD request to
> https://docker.fnnas.com/v2/library/node/manifests/22-alpine?ns=docker.io:
> 401 Unauthorized
> ```

---

## 根因

fnOS 默认把 `https://docker.fnnas.com` 配成 registry mirror。
那个 Harbor registry **需要认证**，靠两个自定义头：

```
X-Meta-Sign
X-Meta-Token
```

⚠️ **Docker 客户端不会把这两个头传给 registry 的 token 请求** ——
`HttpHeaders` 只用于 CLI ↔ daemon 之间的 API 通信。

所以：手动带头发 `curl` **能成功**，但 Docker 自己拉 **401**。

> 官方已确认并修复过（fnOS 1.1.9 / dockermgr 更新）。
> 如果你的 NAS 版本较旧，多半还没修。

⚠️ **影响的不止 FMH** —— 应用商店里所有基于 Docker 的应用（迅雷、qBittorrent…）
   都会一样挂。

---

## ⚠️ 为什么改 `daemon.json` 没用

两条路都试过了：

| 做法 | 结果 |
|---|---|
| 改 `/etc/docker/daemon.json` 的 `registry-mirrors` | ❌ **无效** —— 报错里还是 `docker.fnnas.com` |
| 在 fnOS 界面「镜像仓库 → 设置」里改 | ⚠️ 界面改的是 fnOS 自己的配置存储，跟 `daemon.json` 不是一回事 |

**fnOS 的 `dockermgr` 会在系统更新或重启后把 `docker.fnnas.com` 加回去**，
而且它自己还有一份独立的镜像源配置。**你跟它较劲是赢不了的。**

---

## ✅ 真正的解法：在 Dockerfile 里写死带前缀的镜像地址

**完全不碰 `daemon.json`，所以 fnOS 改什么都没影响。**

```dockerfile
ARG REGISTRY=docker.1ms.run
FROM ${REGISTRY}/library/node:22-alpine AS web
FROM ${REGISTRY}/library/golang:1.24-alpine AS build
```

Docker 解析 `FROM docker.1ms.run/library/node:22-alpine` 时，
会把它当成**普通 registry**（不是 Docker Hub），直接去 `docker.1ms.run` 拉 ——
**根本不经过 `registry-mirrors` 这条链路**。

---

## 实测数据（2026-10-20）

完整走一遍 Docker 的拉取链路：**取 token → 取 manifest → 拉 layer blob**。

| 镜像源 | node:22-alpine | golang:1.24-alpine |
|---|---|---|
| `docker.fnnas.com` | ❌ HTTP 401 | ❌ HTTP 401 |
| `registry.hub.docker.com` | ❌ 超时（30s） | ❌ 超时 |
| `docker.xuanyuan.me` | ❌ HTTP 403 | ❌ HTTP 403 |
| `docker.nju.edu.cn` | ❌ HTTP 403 | ❌ HTTP 403 |
| `dockerproxy.net` | ❌ 超时（15s） | ❌ 超时 |
| **`docker.1ms.run`** | ✅ 4 层，blob 可拉（2.5s） | ✅ 5 层，blob 可拉（2.8s） |
| **`docker.m.daocloud.io`** | ✅ 4 层，blob 可拉（1.9s） | ✅ 5 层，blob 可拉（2.3s） |
| **`hub.rat.dev`** | ✅ 4 层，blob 可拉（5.7s） | ✅ 5 层，blob 可拉（11.8s） |

架构支持（三个可用源都一样）：

```
node:22-alpine      linux/amd64, linux/arm, linux/arm64, linux/s390x
golang:1.24-alpine  linux/amd64, linux/arm, linux/arm64, linux/386, linux/ppc64le, linux/riscv64, linux/s390x
```

⚠️ **`linux/amd64` 在列** —— 你的 x86 NAS 完全没问题。

⚠️ **`library/` 前缀两种形式都支持** ——
   `docker.1ms.run/library/node:22-alpine` 和 `docker.1ms.run/node:22-alpine` 都能拉到。
   Dockerfile 用 `library/` 形式（更标准）。

---

## 换源

```bash
# ① 命令行
docker compose build --build-arg REGISTRY=docker.m.daocloud.io

# ② 写进 .env（compose 里读的是 ${FMH_REGISTRY:-docker.1ms.run}）
echo 'FMH_REGISTRY=docker.m.daocloud.io' >> .env
```

> ⚠️ **本文是历史记录**，记录的是早期用「镜像地址加前缀」绕开 fnOS mirror 的做法。
>
> 现在 Dockerfile 用标准镜像名（`node:22-alpine` / `golang:1.26-alpine`），
> 没有 `ARG REGISTRY` 这个开关了。fnOS 上如果仍然 401，
> 用 `docker save` / `docker load` 把镜像带过去 ——
> 见 [`nas-deploy.md`](./nas-deploy.md) §1.5。

---

## 备选方案

### 传镜像 tar（**完全不拉基础镜像**）

在有 Docker 的机器上：

```bash
cd "Free Mdel House"
docker compose build
docker save -o fmh-image.tar free-model-house:0.1.0
scp fmh-image.tar user@<NAS-IP>:~/
```

NAS 上：

```bash
docker load -i ~/fmh-image.tar
docker compose up -d
```

⚠️ **架构必须一致** —— 本地 x86_64，NAS 也要 x86_64。

### 直接跑二进制（**完全不用 Docker**）

```bash
# 在有 Go 的机器上
cd web && npm ci && npm run build && cd ..
./scripts/build-linux.sh amd64    # → fmh-linux-amd64，11.6 MB，纯静态
scp fmh-linux-amd64 user@<NAS-IP>:~/
```

NAS 上：

```bash
mkdir -p ~/fmh-data
FMH_ADDR=":8678" FMH_DATA="$HOME/fmh-data" ~/fmh-linux-amd64
```

⚠️ 前端**必须先构建** —— 它被 `go:embed` 打进二进制。

---

## 怎么选

| 情况 | 用哪个 |
|---|---|
| NAS 能连外网（**你现在的情况**） | ✅ **Dockerfile 带前缀** —— 已经配好了 |
| 某个源挂了 | `docker compose build --build-arg REGISTRY=另一个` |
| NAS 完全不能上网 | 传二进制（`build-linux.sh`） |
| 懒得折腾 | 传镜像 tar |

---

## 验证

```bash
docker compose build 2>&1 | tail -20
# 不该再出现 "failed to resolve source metadata"
```

拉下来的镜像应该是这样的（**注意仓库名带源前缀**）：

```bash
docker images
#  docker.1ms.run/library/node        22-alpine   ...
#  docker.1ms.run/library/golang      1.24-alpine ...
```
