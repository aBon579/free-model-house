# 构建慢的根因与解法（2026-10-20）

> 症状：NAS 上 `docker compose up -d --build` **卡住不动**，十几分钟没动静。

## 根因：不是 Docker，是 Go 依赖树

`modernc.org/sqlite`（把 SQLite 的 C 代码转译成 Go）带来了**巨大的依赖树**：

| 模块 | 大小 | 文件数 |
|---|---|---|
| `modernc.org/sqlite` | 230.9 MB | 1324 |
| `modernc.org/libc` | 61.2 MB | 3018 |
| `modernc.org/gc/v3` | 3.7 MB | 38 |
| 其它 | ~0.1 MB | ~100 |
| **合计** | **~296 MB** | **~4500** |

实测 `go mod download`（干净缓存）：

| 代理 | 耗时 | 结果 |
|---|---|---|
| `proxy.golang.org`（默认） | **超时 20s** | ❌ **直接连不上** |
| `goproxy.cn` | 26.8s | ✅ 但下了 **359.9 MB / 5216 文件** |
| `mirrors.aliyun.com/goproxy` | — | 38MB 用了 15.1s（0.1MB/s） |

⚠️ **两个独立的坑**：
1. **Go 默认源 `proxy.golang.org` 在国内连不上** → `go mod download` 一直重试。
2. **依赖树本身有 360 MB** → 即使换了源，也要下载很久。

而 `go build` 本身只要 **3 秒** —— 瓶颈 100% 在下载。

---

## 解法 1：换 SQLite 驱动（**最大的一刀**）

改成 `github.com/ncruces/go-sqlite3`（SQLite 编译成 **WASM**，用 wazero 跑）。

| | modernc.org/sqlite | ncruces/go-sqlite3 |
|---|---|---|
| 依赖树 | **359.9 MB / 5216 文件** | **19.8 MB / 931 文件** |
| `go mod download` | 26.8s | **3.9s** |
| 依赖模块数 | 13 | 4 |
| 产物大小 | 11.6 MB | 13.2 MB |

⚠️ 换成 WASM 版需要改两处（`internal/store/db.go`）：

```go
// ① import
_ "github.com/ncruces/go-sqlite3/driver"
_ "github.com/ncruces/go-sqlite3/embed"

// ② DSN 必须带 file: 前缀（ncruces 用 URI 模式解析查询参数），
//    驱动名是 "sqlite3"（modernc 叫 "sqlite"）
dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
db, err := sql.Open("sqlite3", dsn)
```

### 性能实测（**没有明显劣化**）

| 场景 | modernc | ncruces (WASM) |
|---|---|---|
| 开库建表 | 13ms | 18ms |
| 批量写 2000 行 | 9ms | **6ms** |
| 聚合查询 | 0s | 1ms |
| 单行读 500 次 | 8ms | **6ms** |

WASM 版本甚至略快 —— 因为 modernc 要把 C 转译的代码跑一遍。

### 验证

- `go test -race ./...` 全绿
- **端到端 30/30 通过**（真实上游 + 真实数据库）

---

## 第二个坑：`go mod download` 在 NAS 上 exit 1

报错：

```
failed to solve: process "/bin/sh -c go mod download" did not complete
successfully: exit code: 1
```

### 根因 ①：Dockerfile 的 **ARG 作用域 bug**（我写的）

```dockerfile
ARG GOPROXY=https://goproxy.cn,direct     # ← 全局作用域
...
FROM golang:1.26-alpine AS build
ENV GOPROXY=${GOPROXY}                    # ← 展开成【空字符串】！
```

⚠️ **Docker 规则**：`FROM` 之前声明的 ARG 是**全局**的，
**只在 `FROM` 行里可见**。在 stage 内部引用 `${GOPROXY}` 会展开成空。

于是 Go 退回默认源 `proxy.golang.org` → 国内连不上 → **exit 1**。

**修法**：每个 stage 内部**重新声明**一次：

```dockerfile
FROM golang:1.26-alpine AS build
ARG GOPROXY          # ← 必须有这行
ENV GOPROXY=${GOPROXY}
```

⚠️ `NPM_REGISTRY` 有同样的问题 —— `npm config set registry` 设了个空值。

### 根因 ②：go.sum 缺传递依赖的条目

`go mod download`（无参数）要**整个模块图**的校验和。
`go mod tidy` 只记录**实际 import** 的 —— 缺的 11 条会报
`missing go.sum entry`。

**修法**：`go mod download all`（比 tidy 全）。

### ✅ 真正的解法：**vendor 离线构建**

上面两个都是"网络会出问题"这类问题。根治办法是**让构建完全不需要网络**：

```dockerfile
COPY go.mod go.sum ./
COPY vendor/ ./vendor/
COPY . .
RUN GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/fmh ./cmd/fmh
```

⚠️ **不要**设 `GOFLAGS=-mod=mod` —— 那会**覆盖** vendor 模式。
   Go 1.14+ 在有 `vendor/` 且 `go >= 1.14` 时默认就用 vendor 模式。

vendor 体积：

| | 大小 |
|---|---|
| 原始 | 12.7 MB / 447 文件 |
| 压缩（上传包） | **2 MB** |

**验证**（把 GOPROXY 指向无效端口，仍能编译成功 = 真的没联网）：

```
GOPROXY=http://127.0.0.1:1  go build  →  ✅ exit=0  产物 13.2 MB
```

⚠️ **代价**：`vendor/` 必须跟 `go.mod` 同步。
   改了依赖要 `go mod tidy && go mod vendor`。

---

## 第三个坑：运行时阶段缺时区数据

```
failed to compute cache key: failed to calculate checksum of ref ...:
"/usr/share/zoneinfo": not found
```

### 根因

`alpine` 基础镜像**不含** `/usr/share/zoneinfo` —— 那是 `tzdata` 包的内容，
`apk add tzdata` 才会装。

⚠️ `golang:alpine` 也**不含** —— 我原来写的是：

```dockerfile
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo   # ← 源路径不存在
```

### ⚠️ 一个**网上常见但错误**的解法

很多地方说"拷 zoneinfo.zip + 设 `ZONEINFO` 环境变量"。**对 `time.Local` 无效。**

查 Go 源码 `src/time/zoneinfo_unix.go` 的 `initLocal()`：

```go
var platformZoneSources = []string{
	"/usr/share/zoneinfo/",
	"/usr/share/lib/zoneinfo/",
	"/usr/lib/locale/TZ/",
	"/etc/zoneinfo",
}

func initLocal() {
	tz, ok := syscall.Getenv("TZ")
	...
	case tz != "" && tz != "UTC":
		if z, err := loadLocation(tz, platformZoneSources); err == nil {
```

⚠️ 它只查这几条**固定路径**，**完全不读 `$ZONEINFO`**。
`$ZONEINFO` 只对 `time.LoadLocation()` 生效，而 `time.Local` 走的是 `initLocal()`。

所以只设 `ZONEINFO` 的话，`TZ=Asia/Shanghai` 会**静默退化成 UTC** ——
不报任何错，只是"今天用了多少"按 UTC 切，跟日历上的"今天"对不上。

### ✅ 正确解法：把 zip **解成目录**

```dockerfile
FROM golang:1.26-alpine AS tz
RUN mkdir -p /zoneinfo && cd /zoneinfo \
    && unzip -q /usr/local/go/lib/time/zoneinfo.zip \
    && test -f /zoneinfo/Asia/Shanghai

FROM scratch
COPY --from=tz /zoneinfo/ /usr/share/zoneinfo/
```

⚠️ `scratch` 里**没有 unzip** —— 所以在 build 阶段解好。
⚠️ `golang:alpine` 里**有** unzip（已验证：`/usr/bin/unzip` + busybox applet）。

为什么不用 `apk add tzdata`：要**联网**下载 apk 源，NAS 上可能很慢或失败。

### 验证

模拟 `initLocal()` 的查找逻辑：

| TZ | 结果 |
|---|---|
| `Asia/Shanghai` | ✅ offset=28800 (+08:00) CST |
| `UTC` | ✅ offset=0 |
| `America/New_York` | ✅ offset=-14400 (EDT 夏令时) |

解出来的内容：598 个时区文件，0.3 MB。

---

## 第四个坑：`scratch` 里的 `USER root`

```
Error response from daemon: unable to find user root:
no matching entries in passwd file
```

⚠️ 构建**成功**了，是**启动**时挂的。

### 根因

`scratch` 镜像里**没有** `/etc/passwd`。Docker 遇到 `USER root` 这个
**名字**时，会去 passwd 文件里查 `root` —— 查不到就报这个错。

### 修法：用**数字 UID**

```dockerfile
USER 0        # ✅ 数字，不需要查 passwd
# USER root   # ❌ 名字，在 scratch 里必挂
```

⚠️ 程序**不用** `os/user`（不查用户名），所以**不需要**拷 `/etc/passwd`。
   如果用了，就得从 build 阶段拷一个 `/etc/passwd` 过去。

⚠️ `USER 0` 只是**起点** —— 程序启动后会立刻把 `/data` 交给 nobody
   再 setuid 降权，之后全程非特权。

### ⚠️ 这个坑值得记一笔

我连着踩了四个坑，**每一个都是逐个暴露**的 —— Docker 一次只报一个。
教训：**scratch 镜像的运行时依赖要一次性查全**。

我为此写了个检查脚本（`tools/check-scratch.py`），一次性扫：

- `USER` 用的是名字还是数字
- 有没有 `os/user` 依赖
- healthcheck 的命令在不在镜像里
- 时区数据有没有
- `TZ` 有没有设

以后加新的 stage 或改 Dockerfile，跑一遍这个脚本。

---

## 解法 2：固定 Go 代理（**必须做**）

```dockerfile
ENV GOPROXY=https://goproxy.cn,direct \
    GOSUMDB=off
```

⚠️ 少了这一行，`go mod download` 会卡在 `proxy.golang.org` 上反复重试。

⚠️ `GOSUMDB=off` —— 不查校验和数据库，省一次外网请求。
   代价是少了供应链保护，⚠️ 但依赖是 `go.sum` 锁定的，风险可控。

## 解法 3：固定 npm 源

```dockerfile
RUN npm config set registry https://registry.npmmirror.com
```

| 源 | 响应 |
|---|---|
| `registry.npmjs.org` | 0.8s（时快时慢，会卡） |
| `registry.npmmirror.com` | **0.1s**（稳定） |
| `repo.huaweicloud.com` | 0.1s |

---

## 改完之后

完整构建链路：

```
npm ci          35s
npm run build    7s
go mod download  4s   ← 原来 26.8s（且换源前是超时）
go build         3s
─────────────────────
合计            ~50s
```

⚠️ **仍然没在真 Docker 上跑过**（本机没 Docker）——
   上面是逐阶段模拟 + 真实上游的端到端验证。

---

## 顺带：Go 版本

ncruces v0.35.6 要求 `go 1.26.0`，所以基础镜像要跟着升：

```dockerfile
FROM ${REGISTRY}/library/golang:1.26-alpine AS build
```

⚠️ 试过降回 1.24 —— `go mod tidy` 每次都会把 go 指令提到 1.26
（`wazero v1.9.0` 的要求）。与其跟工具链较劲，不如直接用 1.26。

已验证 `golang:1.26-alpine` 在三个候选源上都存在。

---

## 构建阶段换镜像源

构建阶段如果要用 `apk add`，需要把 alpine 的仓库地址换掉：

```dockerfile
ARG APK_MIRROR=mirrors.aliyun.com
RUN sed -i "s|dl-cdn.alpinelinux.org|${APK_MIRROR}|g" /etc/apk/repositories
```

⚠️ 运行时是 `scratch`，没有 `apk` —— 这条只对构建阶段有意义。
