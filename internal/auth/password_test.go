package auth

import (
	"strings"
	"testing"
)

// TestHashAndVerify 验证**哈希 + 校验**能配对。
func TestHashAndVerify(t *testing.T) {
	h, err := Hash("correct horse")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if !Verify(h, "correct horse") {
		t.Fatal("正确密码该通过")
	}
	if Verify(h, "wrong horse") {
		t.Fatal("错误密码不该通过")
	}
}

// TestHashIsSalted 验证**每次哈希都不一样**。
//
// ⚠️ 这很重要 —— 一样的哈希意味着：
//
//	① 两个用户用同一个密码时，数据库里能看出"他们用了一样的密码"；
//	② 彩虹表直接命中。
func TestHashIsSalted(t *testing.T) {
	h1, _ := Hash("same password")
	h2, _ := Hash("same password")
	if h1 == h2 {
		t.Fatal("两次哈希必须不同（要有随机 salt）")
	}
	if !Verify(h1, "same password") || !Verify(h2, "same password") {
		t.Fatal("两个哈希都该能校验通过")
	}
}

// TestValidateRejectsShort 验证**太短的密码被拒**。
func TestValidateRejectsShort(t *testing.T) {
	if err := Validate("1234567"); err == nil {
		t.Fatal("7 位该被拒")
	}
	if err := Validate("12345678"); err != nil {
		t.Fatalf("8 位该通过: %v", err)
	}
}

// TestValidateCountsRunes 验证**按字符数算长度**，不是字节数。
//
// ⚠️ 中文密码 4 个字 = 12 字节，但用户只输了 4 个字。
func TestValidateCountsRunes(t *testing.T) {
	// ⚠️ **3 个汉字 = 9 字节** ——
	//   按字节算是 9 位（过），按字符算是 3 位（不过）。
	//   正好能区分两种算法。
	if err := Validate("密码密码密码"); err == nil {
		t.Fatal("3 个汉字（9 字节）该被拒 —— 长度按字符算不是按字节")
	}
	// 4 个汉字 = 8 字节 = 8 字符 —— 该通过。
	if err := Validate("密码密码密码密码"); err != nil {
		t.Fatalf("4 个汉字该通过: %v", err)
	}
}

// TestHashDefaultAllowsShort 验证**初始密码能哈希**。
//
// ⚠️ 初始密码是 `admin`（5 位），比 MinPasswordLen 短。
// 它是**系统给定的**，不该走用户密码的强度检查。
func TestHashDefaultAllowsShort(t *testing.T) {
	h, err := HashDefault(DefaultPassword)
	if err != nil {
		t.Fatalf("初始密码该能哈希: %v", err)
	}
	if !Verify(h, DefaultPassword) {
		t.Fatal("初始密码该能校验通过")
	}
	// ⚠️ 但普通 Hash **不行** —— 用户密码必须够长。
	if _, err := Hash(DefaultPassword); err == nil {
		t.Fatal("用户密码走 Hash 时该被长度检查拦下")
	}
}

// TestIsDefault 验证**能认出初始密码**。
func TestIsDefault(t *testing.T) {
	h, _ := HashDefault(DefaultPassword)
	if !IsDefault(h) {
		t.Fatal("该认出这是初始密码")
	}
	h2, _ := Hash("a-long-password")
	if IsDefault(h2) {
		t.Fatal("不该把普通密码当成初始密码")
	}
}

// TestPlaintextNeverInHash 验证**哈希里没有明文**。
func TestPlaintextNeverInHash(t *testing.T) {
	h, _ := Hash("super secret password")
	if strings.Contains(h, "super") {
		t.Fatal("哈希里不该出现明文")
	}
	if !strings.HasPrefix(h, "$2") {
		t.Fatalf("该是 bcrypt 格式，实际 %q", h[:min(10, len(h))])
	}
}
