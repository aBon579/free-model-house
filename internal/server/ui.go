package server

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

// 本文件是 **Web UI** 的静态资源服务。
//
// ⚠️ 资源用 `go:embed` 打进二进制 ——
// 这让 Docker 镜像可以做到 scratch（没有 shell、没有包管理器），
// 同时 UI 不需要单独分发。
//
// 详见 docs/design/04-ui.md。

// uiFS 是内嵌的 UI 资源。
//
// ⚠️ 用 `//go:embed all:web/dist` 而不是 `web/dist` —— `all:` 前缀会包含
// 以 `_` 或 `.` 开头的文件，而 Vite 生成的某些资源可能以这些字符开头。
//
// ⚠️ 目录里**必须**至少有一个文件，否则 `go build` 会报
// "pattern web/dist: no matching files"。所以这里放了一个 `.keep` 占位。
//
//go:embed all:web/dist
var uiFS embed.FS

// handleUI 返回 UI 的处理器。
//
// ⚠️ **SPA 回退**：任何没匹配到静态文件的路径都返回 index.html，
// 这样前端路由（/models、/usage 之类）刷新时不会 404。
func (s *Server) handleUI() http.Handler {
	sub, err := fs.Sub(uiFS, "web/dist")
	if err != nil {
		// ⚠️ 没打进 UI 资源 —— 回退到一条提示，
		// 而不是让整个服务 500。
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/v1/") {
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(
				"Free Model House " + Version + "\n\n" +
					"Web UI 资源未打包进本二进制。\n" +
					"开发时请在 web/ 目录执行 `npm run build` 后重新编译。\n"))
		})
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 静态资源。
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}
		if f, err := sub.Open(path); err == nil {
			info, statErr := f.Stat()
			_ = f.Close()
			if statErr == nil && !info.IsDir() {
				// ⚠️ 带指纹的静态资源可以长缓存。
				if strings.HasPrefix(path, "assets/") {
					w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
				}
				files.ServeHTTP(w, r)
				return
			}
		}
		// SPA 回退。
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		serveIndex(w, r2, sub)
	})
}

// serveIndex 返回 index.html。
func serveIndex(w http.ResponseWriter, r *http.Request, sub fs.FS) {
	data, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		http.Error(w, "UI 资源缺失", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// ⚠️ index.html 不长缓存 —— 否则前端更新了用户还在看旧的。
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
