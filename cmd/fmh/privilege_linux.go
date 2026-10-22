//go:build linux

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// 放弃 root 权限。
//
// 容器以 nobody（65534）运行时有个死结：Docker 创建 bind mount 的宿主目录
// 属主是 root:root 0755，nobody 对它**没有写权限**，首次启动就写不了
// /data/fmh.db 与 /data/.secret-key。而以 root 启动的话 nobody 又写不了。
//
// 解法跟 PostgreSQL / Redis 官方镜像一样：root 启动 → chown 给 nobody →
// 立刻 setuid 降权 → 之后全程非特权。
//
// ⚠️ 降权后进程**再也不能** chown —— 所以 chown 必须在**开库之前**做完。
// ⚠️ 不用 gosu/su-exec —— 它们要往镜像里塞二进制，而我们是 scratch。

// 65534 是 Linux 惯例里的 nobody；scratch 里没有 /etc/passwd，只能用数字 UID。
const (
	unprivilegedUID = 65534
	unprivilegedGID = 65534
)

// dropPrivilegesIfRoot 在**以 root 运行时**把数据目录交给非特权用户后降权。
//
// ⚠️ 已经不是 root 时什么都不做 —— 让同一个二进制在 Docker 外
//
//	以普通用户运行时也完全正常。
func dropPrivilegesIfRoot(dataDir string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	if dataDir != "" {
		if err := chownDataDir(dataDir); err != nil {
			return err
		}
	}
	// 放弃附加组 —— 否则还能靠组权限读到数据。
	if err := syscall.Setgroups([]int{unprivilegedGID}); err != nil {
		return fmt.Errorf("放弃附加组失败: %w", err)
	}
	// 先组后用户 —— 顺序反了就降不下来。
	if err := syscall.Setgid(unprivilegedGID); err != nil {
		return fmt.Errorf("切换组失败: %w", err)
	}
	if err := syscall.Setuid(unprivilegedUID); err != nil {
		return fmt.Errorf("切换用户失败: %w", err)
	}
	return nil
}

// chownDataDir 把数据目录和它**直接包含的普通文件**交给 nobody。
//
// ⚠️ **只处理一层** —— SQLite 的 -wal/-shm 是直接子文件，够用；
//
//	递归下去万一用户挂错目录，会把整个文件系统都改一遍。
//
// ⚠️ **不跟随符号链接** —— 跟过去可能改到挂载点外面去。
// ⚠️ 目录不存在不算错 —— os.MkdirAll 会在后面创建它，属主天然正确。
func chownDataDir(dataDir string) error {
	info, err := os.Stat(dataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("访问数据目录 %s 失败: %w", dataDir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("数据目录 %s 不是目录", dataDir)
	}
	if err := os.Chown(dataDir, unprivilegedUID, unprivilegedGID); err != nil {
		return fmt.Errorf("把 %s 的属主改成 %d:%d 失败: %w",
			dataDir, unprivilegedUID, unprivilegedGID, err)
	}

	entries, err := os.ReadDir(dataDir)
	if err != nil {
		// 目录读不了就只改目录本身 —— 后面开库会报更具体的错。
		return nil
	}
	for _, e := range entries {
		// 只改**普通文件**，符号链接和目录一律跳过。
		if !e.Type().IsRegular() {
			continue
		}
		p := filepath.Join(dataDir, e.Name())
		if err := os.Chown(p, unprivilegedUID, unprivilegedGID); err != nil {
			// 单个文件改不动不该让启动失败 —— 可能属主本来就不是我们。
			continue
		}
	}
	return nil
}
