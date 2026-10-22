//go:build linux

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestDropPrivilegesNonRootIsNoop 验证**非 root 时什么都不做**。
//
// ⚠️ 这条最重要 ——
//
//	开发机上以普通用户跑 `go run ./cmd/fmh` **不能**被降权逻辑影响。
func TestDropPrivilegesNonRootIsNoop(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("当前是 root，跳过")
	}
	dir := t.TempDir()
	before := os.Geteuid()

	if err := dropPrivilegesIfRoot(dir); err != nil {
		t.Fatalf("非 root 时不该报错: %v", err)
	}
	if os.Geteuid() != before {
		t.Fatalf("UID 不该变: %d → %d", before, os.Geteuid())
	}
}

// TestChownDataDirSkipsSymlinks 验证**不跟随符号链接**。
//
// ⚠️ 跟过去可能改到挂载点**外面**的属主 ——
//
//	那就可能把宿主机上别的目录给改了。
func TestChownDataDirSkipsSymlinks(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("需要 root 才能测 chown")
	}
	dir := t.TempDir()

	// 指向一个"外部"文件。
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("建不了符号链接: %v", err)
	}

	// 一个普通文件。
	regular := filepath.Join(dir, "regular.db")
	if err := os.WriteFile(regular, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := chownDataDir(dir); err != nil {
		t.Fatalf("chown 失败: %v", err)
	}

	// 目录本身该被改了。
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sys, ok := di.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != unprivilegedUID {
		t.Fatalf("目录属主该是 %d，实际 %d", unprivilegedUID, sys.Uid)
	}

	// 符号链接**不能**被跟随 —— 外部文件的属主必须不变。
	oi, err := os.Stat(outside)
	if err != nil {
		t.Fatal(err)
	}
	if sys, ok := oi.Sys().(*syscall.Stat_t); ok && int(sys.Uid) == unprivilegedUID {
		t.Fatal("符号链接被跟随了 —— 改到了挂载点外面")
	}
}

// TestChownDataDirMissingIsOK 验证**目录不存在不算错**。
//
// ⚠️ `os.MkdirAll` 会在后面创建它 ——
//
//	那时已经是降权后的用户，属主天然正确。
func TestChownDataDirMissingIsOK(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-there")
	if err := chownDataDir(missing); err != nil {
		t.Fatalf("目录不存在不该报错: %v", err)
	}
}

// TestChownDataDirRejectsFile 验证**传文件进去会报错**。
func TestChownDataDirRejectsFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := chownDataDir(f); err == nil {
		t.Fatal("传文件进去该报错")
	}
}
