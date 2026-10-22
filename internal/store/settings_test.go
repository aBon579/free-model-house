package store

import (
	"path/filepath"
	"testing"
)

// TestGetAutoModelSwitchMigratesLegacyKey 验证**旧键会被迁移**。
//
// ⚠️ 老实例的设置存在 `cross_lane_fallback` 里。
//
//	升级之后如果读不到，用户会发现"开关自己变回关闭了" ——
//	那是最糟的静默行为。
func TestGetAutoModelSwitchMigratesLegacyKey(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer db.Close()

	// ⚠️ **只有旧键** —— 模拟老实例。
	if err := db.SetSetting(SettingCrossLane, "1"); err != nil {
		t.Fatalf("写旧键失败: %v", err)
	}

	if !db.GetAutoModelSwitch(false) {
		t.Fatal("该继承旧键的设置")
	}
	// ⚠️ 迁移后新键也要有 —— 下次读就不用再看旧键了。
	v, ok, err := db.GetSetting(SettingAutoModelSwitch)
	if err != nil || !ok {
		t.Fatalf("该把设置迁到新键: ok=%v err=%v", ok, err)
	}
	if v != "1" {
		t.Fatalf("新键该是 1，实际 %q", v)
	}
}

// TestGetAutoModelSwitchNewKeyWins 验证**新键优先于旧键**。
func TestGetAutoModelSwitchNewKeyWins(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer db.Close()

	// 旧键是开的，新键是关的 —— 新键赢。
	if err := db.SetSetting(SettingCrossLane, "1"); err != nil {
		t.Fatalf("写旧键失败: %v", err)
	}
	if err := db.SetSetting(SettingAutoModelSwitch, "0"); err != nil {
		t.Fatalf("写新键失败: %v", err)
	}
	if db.GetAutoModelSwitch(true) {
		t.Fatal("新键该优先（用户在新版本改过）")
	}
}

// TestGetAutoModelSwitchDefault 验证**两个键都没有时用默认值**。
func TestGetAutoModelSwitchDefault(t *testing.T) {
	dir := t.TempDir()
	db, err := Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer db.Close()

	if db.GetAutoModelSwitch(false) {
		t.Fatal("默认该是关的")
	}
	if !db.GetAutoModelSwitch(true) {
		t.Fatal("该尊重传入的默认值")
	}
}
