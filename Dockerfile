# Free Model House —— 多阶段构建（自包含）
#
#   docker build -t fmh .        # 不需要任何预编译产物
#   docker compose up -d --build # 同上
#
# 阶段：node 构建前端 → golang 静态编译 → scratch 运行
#
# ⚠️ CGO_ENABLED=0 是硬约束 —— SQLite 用纯 Go 的 ncruces/go-sqlite3。

FROM node:22-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json* ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-mod=vendor
# vendor/ 已在仓库里，构建时不需要联网取依赖。
COPY go.mod go.sum ./
COPY vendor/ ./vendor/
COPY . .
COPY --from=web /internal/server/web/dist ./internal/server/web/dist
RUN go build -trimpath -ldflags="-s -w" -o /out/fmh ./cmd/fmh

# 时区数据 —— Go 的 time.Local 只查固定路径，scratch 里没有 unzip。
RUN mkdir -p /rootfs/zoneinfo \
    && cd /rootfs/zoneinfo \
    && unzip -q /usr/local/go/lib/time/zoneinfo.zip \
    && test -f Asia/Shanghai \
    && mkdir -p /rootfs/etc \
    && printf 'root:x:0:0:root:/root:/sbin/nologin\nnobody:x:65534:65534:nobody:/:/sbin/nologin\n' > /rootfs/etc/passwd \
    && printf 'root:x:0:\nnobody:x:65534:\n' > /rootfs/etc/group

FROM scratch

# CA 证书 —— 没有它，所有 HTTPS 请求都会握手失败。
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build --chmod=0755 /rootfs/zoneinfo/ /usr/share/zoneinfo/
COPY --from=build --chmod=0755 /rootfs/etc/passwd /etc/passwd
COPY --from=build --chmod=0755 /rootfs/etc/group /etc/group
COPY --from=build --chmod=0755 /out/fmh /fmh

VOLUME ["/data"]

# 容器以 root 启动 → 程序把 /data 交给 nobody → setuid 降权。
USER 0

ENV FMH_ADDR=:8678 \
    FMH_DATA=/data

EXPOSE 8678

ENTRYPOINT ["/fmh"]
