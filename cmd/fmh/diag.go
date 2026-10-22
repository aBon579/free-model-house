package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aBon579/free-model-house/internal/lane"
)

// runDiag 打印运行环境的诊断信息。
//
// ⚠️ **为什么需要它** ——
//
//	容器镜像是 `FROM scratch` —— 里面**什么都没有**，
//	只有我们显式 COPY 进去的文件。
//	少一个文件（比如 CA 证书），症状就是
//	"模型清单是空的"，但日志里看不出原因。
//
//	这个子命令把"缺什么"一次性说清楚。
func runDiag() int {
	fmt.Println("=== FMH 运行环境诊断 ===")
	fmt.Println()

	// ── 1. 时区 ──
	fmt.Println("[1] 时区")
	now := time.Now()
	fmt.Printf("    本地时区      : %s\n", now.Location())
	fmt.Printf("    当前时间      : %s\n", now.Format("2006-01-02 15:04:05"))
	if _, err := time.LoadLocation("Asia/Shanghai"); err != nil {
		fmt.Printf("    ⚠️  加载 Asia/Shanghai 失败: %v\n", err)
	} else {
		fmt.Printf("    ✅ Asia/Shanghai 可加载\n")
	}
	fmt.Println()

	// ── 2. CA 证书 ──
	fmt.Println("[2] CA 证书")
	const caPath = "/etc/ssl/certs/ca-certificates.crt"
	if st, err := os.Stat(caPath); err != nil {
		fmt.Printf("    ❌ %s 不存在: %v\n", caPath, err)
		fmt.Printf("       → scratch 镜像必须显式 COPY 进去，否则 HTTPS 全挂\n")
	} else {
		fmt.Printf("    %s 大小 %d 字节\n", caPath, st.Size())
		if st.Size() == 0 {
			fmt.Printf("    ❌ **文件是空的** → 所有 HTTPS 握手都会失败\n")
			fmt.Printf("       → 镜像里的证书包没打进真实内容，检查构建流程\n")
		}
		// ⚠️ **能不能读** —— 降权后是 nobody，读不了就是致命伤。
		if _, err := os.ReadFile(caPath); err != nil {
			fmt.Printf("    ❌ **读不了**: %v\n", err)
			fmt.Printf("       → 降权后（nobody）读不到证书 → 所有 HTTPS 都挂\n")
			fmt.Printf("       → 检查 Dockerfile 里有没有 --chmod=0755\n")
		} else {
			fmt.Printf("    ✅ 当前进程可读\n")
		}
	}
	// ⚠️ **内存里那份** —— 降权前加载的，不依赖文件权限。
	if _, ok, err := lane.LoadCACerts(); ok {
		fmt.Printf("    ✅ 内存证书池可用（%d 张）—— HTTPS 一定能用\n", lane.CACertCount())
	} else if err != nil {
		fmt.Printf("    ⚠️  内存证书池不可用: %v\n", err)
		fmt.Printf("       → 将退回系统证书库（容器里通常没有 → HTTPS 会挂）\n")
	}
	fmt.Println()

	// ── 3. HTTPS 连通性 ──
	fmt.Println("[3] HTTPS 连通性")
	for _, target := range []string{
		"https://opencode.ai/inference/v1/models",
		"https://api.kilo.ai/api/gateway/models",
		"https://openrouter.ai/api/v1/models",
	} {
		fmt.Printf("    %-52s ", target)
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			fmt.Printf("❌ 构造请求失败: %v\n", err)
			continue
		}
		client := &http.Client{
			Timeout: 12 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			},
		}
		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("❌ %v\n", shorten(err.Error(), 60))
			continue
		}
		resp.Body.Close()
		fmt.Printf("✅ HTTP %d\n", resp.StatusCode)
	}
	fmt.Println()

	// ── 4. 关键文件 ──
	fmt.Println("[4] 关键文件")
	for _, p := range []string{"/fmh", "/usr/share/zoneinfo/Asia/Shanghai", "/etc/passwd", "/etc/group"} {
		if st, err := os.Stat(p); err == nil {
			fmt.Printf("    ✅ %-42s %d 字节\n", p, st.Size())
		} else {
			fmt.Printf("    ❌ %-42s 缺失\n", p)
		}
	}
	fmt.Println()

	// ── 5. 数据目录 ──
	fmt.Println("[5] 数据目录")
	dataDir := os.Getenv("FMH_DATA")
	if dataDir == "" {
		dataDir = "/data"
	}
	fmt.Printf("    FMH_DATA = %s\n", dataDir)
	for _, p := range []string{dataDir, filepath.Join(dataDir, "fmh.db")} {
		if st, err := os.Stat(p); err == nil {
			fmt.Printf("    ✅ %-42s %d 字节\n", p, st.Size())
		} else {
			fmt.Printf("    ❌ %-42s %v\n", p, err)
		}
	}

	fmt.Println()
	fmt.Println("=== 诊断结束 ===")
	return 0
}

// shorten 截短字符串。
func shorten(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
