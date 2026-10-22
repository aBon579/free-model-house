// 确保 embed 占位文件存在。
//
// ⚠️ 为什么需要它：
//
// `internal/server/ui.go` 用 `//go:embed all:web/dist` 把 UI 打进二进制。
// Go 的 embed 要求模式**至少匹配一个文件**，否则 `go build` 报
// "pattern web/dist: no matching files"。
//
// 而 Vite 的 `emptyOutDir: true` 会**清空**输出目录 ——
// 连占位文件一起清掉。
//
// 所以每次构建后把占位文件放回去。
// 这样即使没构建过 UI，`go build ./...` 也能过
// （只是 UI 会显示"资源未打包"的提示页）。

import { existsSync, mkdirSync, writeFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const distDir = join(here, '..', '..', 'internal', 'server', 'web', 'dist')
const keepFile = join(distDir, '.keep')

mkdirSync(distDir, { recursive: true })

if (!existsSync(keepFile)) {
  writeFileSync(
    keepFile,
    [
      '本文件是 go:embed 的占位符。',
      '',
      'internal/server/ui.go 用 `//go:embed all:web/dist` 把 UI 打进二进制，',
      'Go 要求该目录至少有一个文件。前端构建会清空此目录，所以构建后要把它放回来。',
      '',
    ].join('\n'),
    'utf8',
  )
  console.log('[fmh] 已补上 embed 占位文件 .keep')
}
