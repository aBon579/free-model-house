import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'

// Vite 配置。
//
// ⚠️ 用 `.mjs` 而不是 `.ts` ——
// Vite 加载 TS 配置时要**起一个子进程**跑 esbuild，
// 在受限环境里那会撞上 `spawn EPERM`。
// 纯 JS 配置不需要子进程。
//
// ⚠️ `outDir` 是 `dist` —— 它会被 `go:embed` 打进 Go 二进制
// （见 internal/server/ui.go）。
export default defineConfig({
  plugins: [react(), tailwindcss()],
  build: {
    // ⚠️ 输出目录必须与 `go:embed` 的路径一致 ——
    // internal/server/ui.go 里写的是 `all:web/dist`，
    // 相对于 internal/server/ 目录。
    outDir: '../internal/server/web/dist',
    emptyOutDir: true,
    // ⚠️ sourcemap 关掉 —— 打进二进制会让体积翻倍，
    // 而且生产环境的 sourcemap 会暴露源码路径。
    sourcemap: false,
    chunkSizeWarningLimit: 900,
  },
  server: {
    port: 5173,
    // ⚠️ 开发时把 API 代理到本地 Go 服务 ——
    // 这样前端热更新时后端不用重启。
    proxy: {
      '/api': 'http://127.0.0.1:8678',
      '/v1': 'http://127.0.0.1:8678',
      '/healthz': 'http://127.0.0.1:8678',
    },
  },
})
