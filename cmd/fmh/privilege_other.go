//go:build !linux

package main

// 非 Linux 平台的空实现 —— 降权是 Linux 特有的
// （setuid/setgid/setgroups 在 Windows 上不存在，macOS 上语义不同）。
// 这里只提供同名空函数，让 main.go 不需要 build tag。
func dropPrivilegesIfRoot(dataDir string) error {
	return nil
}
