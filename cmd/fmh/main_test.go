package main

import (
	"os"
	"strings"
	"testing"

	"github.com/aBon579/free-model-house/internal/store"
)

// TestShowKeyDefaultsToFull 验证**默认**打完整 Key。
//
// ⚠️ 为什么这么重要：启动日志是唯一可靠的取 Key 途径 ——
//
//	管理台要登录才能看 Key，登录要 Key（鸡生蛋）；
//	`docker exec free-model-house /fmh key` 要求容器正在运行。
//	日志只给指纹的话，首次部署就卡死了。
func TestShowKeyDefaultsToFull(t *testing.T) {
	os.Unsetenv("FMH_HIDE_KEYS")
	key := "fmh-" + strings.Repeat("a", 40)

	got := showKey(key)
	if got != key {
		t.Fatalf("默认该打完整 Key，实际 %q", got)
	}
}

// TestShowKeyHidesWhenRequested 验证 FMH_HIDE_KEYS=1 时只打指纹。
func TestShowKeyHidesWhenRequested(t *testing.T) {
	t.Setenv("FMH_HIDE_KEYS", "1")
	key := "fmh-" + strings.Repeat("a", 40)

	got := showKey(key)
	if got == key {
		t.Fatal("FMH_HIDE_KEYS=1 时不该打完整 Key")
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("该打指纹，实际 %q", got)
	}
	if got != store.CredentialHint(key) {
		t.Fatalf("指纹格式不对：%q", got)
	}
}

// TestShowKeyOnlyExactOneHides 验证只有 "1" 才隐藏。
func TestShowKeyOnlyExactOneHides(t *testing.T) {
	key := "fmh-" + strings.Repeat("a", 40)
	for _, v := range []string{"0", "", "true", "yes"} {
		t.Setenv("FMH_HIDE_KEYS", v)
		if got := showKey(key); got != key {
			t.Errorf("FMH_HIDE_KEYS=%q 不该隐藏，实际 %q", v, got)
		}
	}
}
