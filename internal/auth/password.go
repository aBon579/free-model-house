// Package auth 实现管理台的密码认证：bcrypt 哈希存储，初始密码 admin，首次登录强制修改。
package auth

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"golang.org/x/crypto/bcrypt"
)

// MinPasswordLen 是密码的最短长度（按字符数算）。
const MinPasswordLen = 8

// DefaultPassword 是初始密码，只在第一次登录前有效。
const DefaultPassword = "admin"

// bcryptCost 是哈希成本：12 约需 200~300ms，再慢会影响登录体验。
const bcryptCost = 12

// ErrPasswordTooShort 表示密码太短。
var ErrPasswordTooShort = fmt.Errorf("密码不能少于 %d 位", MinPasswordLen)

// ErrPasswordMismatch 表示两次输入不一致。
var ErrPasswordMismatch = errors.New("两次输入的密码不一致")

// Validate 只查长度，不查强度规则 —— 复杂规则只会逼用户把密码写在便签上。
func Validate(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLen {
		return ErrPasswordTooShort
	}
	return nil
}

// Hash 哈希一个用户密码（先过长度校验）。
func Hash(password string) (string, error) {
	if err := Validate(password); err != nil {
		return "", err
	}
	return hashUnchecked(password)
}

// HashDefault 哈希初始密码 —— admin 只有 5 位，绕过用户密码的长度检查。
func HashDefault(password string) (string, error) {
	return hashUnchecked(password)
}

func hashUnchecked(password string) (string, error) {
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("密码哈希失败: %w", err)
	}
	return string(h), nil
}

// Verify 校验密码（bcrypt 内部是恒定时间比较）。
func Verify(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
