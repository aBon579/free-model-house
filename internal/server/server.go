package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aBon579/free-model-house/internal/store"
)

// 本文件是 **HTTP 服务器** —— 单端口，同时提供推理面与管理面。
//
// ## 为什么是单端口？
//
// 用户要求 `BASEURL: http://NAS-IP:8678/v1` + `API Key: fmh-…`，
// 所以**一个端口**同时承载：
//
//	/v1/*        推理面（给 LAN 里的 agent）
//	/*           管理面 + Web UI（给 NAS 本机的浏览器）
//
// 这比"两个端口"简单得多，而且没有额外风险 ——
// 因为**鉴权是按 Key 做的，不是按端口做的**。
//
// 之前设计里"管理面只监听 127.0.0.1"的做法在单端口下不成立，
// 改为**靠 Key 区分**：管理面需要管理会话 Cookie（只在 NAS 本机浏览器里才有），
// 推理面需要 fmh- Key（局域网里每个 agent 都能拿到）。
//
// ⚠️ 所以：**从 LAN 访问管理面，Cookie 鉴权会失败**（没有会话）。
// 这是对的 —— 管理面就该只有本机能进。
//
// 详见 docs/design/02-key-model.md。

// Options 是服务器选项。
type Options struct {
	// Addr 是监听地址（默认 ":8678"）。
	Addr    string
	DataDir string
	Log     *log.Logger
}

// Server 是 FMH 的 HTTP 服务器。
type Server struct {
	opts Options
	log  *log.Logger

	gateway *Gateway
	admin   *Admin
	keys    *Keys
	mux     *http.ServeMux
	// httpServer 是标准库的服务器。
	httpServer *http.Server

	mu      sync.Mutex
	started bool
}

// NewServer 建一个服务器。
func NewServer(opts Options, gateway *Gateway, keys *Keys) *Server {
	if opts.Addr == "" {
		opts.Addr = ":8678"
	}
	if opts.Log == nil {
		opts.Log = log.New(os.Stdout, "", log.LstdFlags)
	}
	s := &Server{
		opts:    opts,
		log:     opts.Log,
		gateway: gateway,
		keys:    keys,
	}
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

// SetAdmin 装上管理面。
func (s *Server) SetAdmin(a *Admin) { s.admin = a }

// Handler 返回总路由（测试用）。
func (s *Server) Handler() http.Handler { return s.mux }

// routes 注册所有路由。
func (s *Server) routes() {
	// 推理面。
	s.mux.Handle("/v1/", s.gateway.Routes())
	// 兼容不带 /v1 前缀的老客户端。
	s.mux.Handle("/chat/completions", s.gateway.Routes())
	s.mux.Handle("/responses", s.gateway.Routes())
	s.mux.Handle("/messages", s.gateway.Routes())
	s.mux.Handle("/models", s.gateway.Routes())

	// 健康检查 —— ⚠️ **不鉴权**。
	//
	// 它只暴露"服务活着吗"与版本，不暴露任何模型或凭据信息。
	// 容器健康检查需要它能工作。
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/fmh/healthcheck", s.handleHealthz)

	// 管理 API。
	s.mux.HandleFunc("/api/", s.handleAdmin)

	// Web UI（静态资源）。
	s.mux.Handle("/", s.handleUI())
}

// handleHealthz 健康检查。
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "free-model-house",
		"version": Version,
	})
}

// Version 是版本号。
const Version = "0.1.0"

// BuildStamp 是构建时间戳 —— 由 Dockerfile 用 -ldflags -X 注入。
//
// ⚠️ 用来**确认镜像到底重新构建没有**：
// `docker compose up` 在镜像已存在时**不会**重新构建，
// 于是改了 Dockerfile 却还在跑旧镜像 —— 报错一模一样，很难分辨。
// 启动横幅里会打这个时间戳，一眼就能看出是不是新镜像。
//
// ⚠️ 没注入时是 "dev"（本地 go build）。
var BuildStamp = "dev"

// Listen 监听端口（但不开始服务）。
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("监听 %s 失败: %w", s.opts.Addr, err)
	}
	return ln, nil
}

// Serve 开始服务。
func (s *Server) Serve(ln net.Listener) error {
	s.mu.Lock()
	s.started = true
	s.mu.Unlock()

	srv := &http.Server{
		Handler: s.mux,
		// ⚠️ **不能设过短的 WriteTimeout** —— 一次推理可能沉默 70 秒以上
		//   （思考模型），写超时一断，客户端拿到的是"连接被重置"而不是一个答案。
		//   所以用 ReadHeaderTimeout（防慢速头部攻击）+ IdleTimeout（防空闲连接堆积），
		//   **不设** WriteTimeout。
		ReadHeaderTimeout: 20 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	s.httpServer = srv
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown 优雅关闭。
func (s *Server) Shutdown(ctx context.Context) error {
	if s.httpServer == nil {
		return nil
	}
	return s.httpServer.Shutdown(ctx)
}

// LocalAddr 返回实际监听地址。
func (s *Server) LocalAddr() string {
	return s.opts.Addr
}

// Banner 返回启动横幅。
//
// ⚠️ 它**打印完整局域网 Key** —— 不是指纹。
//
//	理由跟 cmd/fmh 里的一样：启动日志是唯一可靠的取 Key 途径
//	（管理台要登录才能看，登录要 Key；`docker exec` 要容器在跑）。
//	不想明文就设 `FMH_HIDE_KEYS=1`。
func (s *Server) Banner() string {
	var sb strings.Builder
	sb.WriteString("\n")
	sb.WriteString("  Free Model House " + Version + "  (构建 " + BuildStamp + ")\n")
	sb.WriteString("  ─────────────────────────────────────────\n")
	sb.WriteString("  管理台   http://localhost" + s.portSuffix() + "/\n")
	sb.WriteString("  推理面   http://<NAS-IP>" + s.portSuffix() + "/v1\n")
	sb.WriteString("  局域网 Key " + s.showKey(s.keys.LanKey()) + "\n")
	sb.WriteString("\n")
	return sb.String()
}

// showKey 按 FMH_HIDE_KEYS 决定打完整 Key 还是指纹。
func (s *Server) showKey(key string) string {
	if os.Getenv("FMH_HIDE_KEYS") == "1" {
		return store.CredentialHint(key)
	}
	return key
}

// portSuffix 返回端口后缀。
func (s *Server) portSuffix() string {
	_, port, err := net.SplitHostPort(s.opts.Addr)
	if err != nil {
		return ":8678"
	}
	return ":" + port
}
