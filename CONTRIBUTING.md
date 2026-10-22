# 贡献指南

感谢你愿意花时间看这个项目。

## 先读这个

免密车道的上游对**客户端身份**有硬性限制。本项目通过复现官方客户端的
**线上行为**来满足它。

这意味着：

- **任何"帮我绕过上游限制"的 PR 都不会被合并。**
- 免密车道一旦收紧，本项目对那部分功能**会失效** —— 那是预期内的。
- 你提交的任何贡献，都表示你同意本项目继续以这个方式工作。

如果你不能接受这一点，OpenRouter 车道是干净的路 —— 它不需要任何指纹。

## 环境

| 需要 | 版本 |
|---|---|
| Go | 1.26+ |
| Node.js | 22+ |
| Docker | 可选 |

**不需要** CGO —— SQLite 走的是纯 Go 的 WASM 实现。

```bash
git clone <repo> && cd "Free Mdel House"
cd web && npm ci && npm run build
cd .. && go build ./cmd/fmh
```

## 开发

```bash
# 后端
go test ./...
go vet ./...
gofmt -l .        # 应该没有输出

# 前端
cd web
npm run dev        # 开发服务器
npm run build      # 构建到 internal/server/web/dist
npx tsc --noEmit   # 类型检查
```

### ⚠️ 构建时必须加 `-trimpath`

```bash
go build -trimpath -ldflags="-s -w" -o fmh ./cmd/fmh
```

**为什么**：不加 `-trimpath`，Go 会把编译机的绝对路径写进二进制。
Windows Defender 的机器学习启发式会把它判成木马
（`Trojan:Win32/Bearfoos.A!ml`）并**直接删掉你的产物**。

这不是你的代码有问题 —— 是路径字符串命中了它的可疑模式。
`-trimpath` 把路径换成模块路径，就干净了。

Linux 发行版（`build-linux.sh` / `Dockerfile`）**本来就带这个参数**。

## 改免密车道的时候

`internal/lane/zen_fingerprint.go` 是整个项目**最易碎**的文件。

两条纪律：

1. **所有闸门相关的字面量集中在这一处** —— 上游改规格时只改这里。
2. **`zen_fingerprint_test.go` 里的回归测试必须一起改** ——
   那些测试的作用就是锁住字面量，防止被"顺手优化"掉。

如果你要动它，请先读 [`docs/design/01-zen-fingerprint.md`](docs/design/01-zen-fingerprint.md)。

**不要**为了让测试通过而放宽断言 —— 那些断言是上游规格的**唯一记录**。

## 提交信息

用祈使句，说明**为什么**而不只是**做了什么**：

```
✅ 探测失败时区分「限流」与「改规格」
❌ fix bug
❌ update code
```

## PR 前自查

- [ ] `go test ./...` 全绿
- [ ] `go vet ./...` 无输出
- [ ] `gofmt -l .` 无输出
- [ ] `cd web && npx tsc --noEmit` 无输出
- [ ] `cd web && npm run build` 成功
- [ ] 新增的代码有注释说明**为什么**，不是**做了什么**
- [ ] 没有引入新的外部依赖（依赖要谨慎加，`vendor/` 要同步）
- [ ] 如果动了前端，**手机宽度下也看一眼**

## 依赖

`vendor/` **必须提交** —— Docker 镜像在 NAS 上离线构建，不联网拉依赖。

加了依赖后：

```bash
go mod tidy && go mod vendor
git add go.mod go.sum vendor/
```

**注意**：`vendor/` 里的代码是**第三方的**，对它跑 `gofmt` 会产生噪音。
提交前确认 `gofmt -l .` 只报你自己的文件。

## 不要做的事

- 不要在代码里硬编码任何凭据、IP、域名
- 不要在 UI 上写"开发说明"式的自述（比如"我们特意这么设计是因为…"）
- 不要在 UI 上暴露实现细节（算法名、加密方式、内部格式）
- 不要把 `data/`、`.env`、`.secret-key` 提交进仓库

## 报告安全问题

**不要开公开 issue** —— 见 [`SECURITY.md`](SECURITY.md)。
