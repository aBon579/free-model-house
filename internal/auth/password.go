// Package auth 实现管理台的密码认证。
//
// 存 bcrypt 哈希，不存明文。初始密码 admin，首次登录后强制修改。
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// MinPasswordLen 是密码的最短长度。
const MinPasswordLen = 8

// DefaultPassword 是初始密码。
//
// ⚠️ 只在这一刻存在 —— 第一次登录后必须改。
const DefaultPassword = "admin"

// bcryptCost 是哈希的计算成本。
//
// ⚠️ 12 是平衡点：太快离线爆破便宜，太慢登录要等。
// 实测约 200~300ms。
const bcryptCost = 12

// ErrPasswordTooShort 表示密码太短。
var ErrPasswordTooShort = fmt.Errorf("密码不能少于 %d 位", MinPasswordLen)

// ErrPasswordMismatch 表示两次输入不一致。
var ErrPasswordMismatch = errors.New("两次输入的密码不一致")

// ErrPasswordIsDefault 表示还在用初始密码。
//
// ⚠️ 这个错误**专门**用来驱动"强制改密码"的流程 ——
//
//	登录**成功**，但会话被标记为"必须改密码"。
var ErrPasswordIsDefault = errors.New("还在使用初始密码，必须先修改")

// Validate 校验密码的**格式**（不检查是否重复）。
//
// ⚠️ 只查长度 —— 不查"强弱"是**故意的**：
//
//	复杂的强度规则（要特殊字符、要大小写混合……）会逼用户
//	把密码写在便签上，或者反复改到自己都记不住。
//	长度 + 不可爆破的存储，才是真的保护。
func Validate(password string) error {
	// ⚠️ 按**字符数**算，不是字节数 ——
	//   中文密码 4 个字是 12 字节，但用户只输了 4 个字。
	if utf8.RuneCountInString(password) < MinPasswordLen {
		return ErrPasswordTooShort
	}
	return nil
}

// Hash 给密码做 bcrypt 哈希。
//
// ⚠️ **会校验长度** —— 弱密码直接拒绝。
func Hash(password string) (string, error) {
	if err := Validate(password); err != nil {
		return "", err
	}
	return hashUnchecked(password)
}

// HashDefault 给**初始密码**做哈希。
//
// ⚠️ **不校验长度** ——
//
//	初始密码是 `admin`（5 位），比 MinPasswordLen 短。
//	它是**系统给定的**，不是用户选的，
//	所以不该走"用户密码"的强度检查。
//
//	⚠️ 但它**必须能改成合规密码** —— 用户改完就没事了。
func HashDefault(password string) (string, error) {
	return hashUnchecked(password)
}

// hashUnchecked 不做长度校验地哈希。
func hashUnchecked(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("密码哈希失败: %w", err)
	}
	return string(h), nil
}

// Verify 校验密码对不对。
//
// ⚠️ **恒定时间比较** —— bcrypt 自己做的，
//
//	不比较哈希字符串（那会泄漏前缀信息）。
func Verify(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

// IsDefault 判断一个哈希是不是**初始密码**的哈希。
//
// ⚠️ 只在刚初始化时用一次 —— 判断"用户改没改过"。
func IsDefault(hash string) bool {
	return Verify(hash, DefaultPassword)
}

// RandomToken 生成一个随机的 URL 安全令牌。
//
// ⚠️ 用它做**会话 id** 和**密码重置码**。
func RandomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
