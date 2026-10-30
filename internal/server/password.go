package server

import (
	"sync"

	"github.com/aBon579/free-model-house/internal/auth"
	"github.com/aBon579/free-model-house/internal/store"
)

// 管理台密码：bcrypt 哈希存数据库，明文从不落盘/进日志/回显。
// 用初始密码登录**成功**但会话被标记 MustChange —— 直接拒绝的话
// 用户进不去也不知道为什么；"登录 → 提示改 → 改完能用"更清楚。

// SettingAdminPassword 是密码哈希的存储键。
const SettingAdminPassword = "admin_password_hash"

// DefaultAdminPassword 是初始密码。
const DefaultAdminPassword = "admin"

// SettingPasswordChanged 标记用户改过初始密码 ——
// 单独的布尔标记，不用每次启动都跑一次 bcrypt 比对。
const SettingPasswordChanged = "admin_password_changed"

// Password 管理管理台的密码。
type Password struct {
	mu      sync.RWMutex
	db      *store.DB
	hash    string
	changed bool
}

// NewPassword 读出密码状态；没有就铸一个初始密码的哈希。
func NewPassword(db *store.DB) *Password {
	p := &Password{db: db}
	p.load()
	return p
}

func (p *Password) load() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.changed = p.db.GetBool(SettingPasswordChanged, false)
	h, ok, err := p.db.GetSetting(SettingAdminPassword)
	if err == nil && ok && h != "" {
		p.hash = h
		return
	}
	// 没有密码 —— 用初始密码铸一个哈希，登录逻辑走同一条路径。
	if hashed, herr := auth.HashDefault(auth.DefaultPassword); herr == nil {
		_ = p.db.SetSetting(SettingAdminPassword, hashed)
		p.hash = hashed
	}
}

// IsDefault 报告是否还在用初始密码。
func (p *Password) IsDefault() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return !p.changed
}

// Verify 校验密码。mustChange=true 表示这次登录用的是初始密码。
func (p *Password) Verify(password string) (ok bool, mustChange bool) {
	p.mu.RLock()
	h := p.hash
	changed := p.changed
	p.mu.RUnlock()

	if h == "" || !auth.Verify(h, password) {
		return false, false
	}
	return true, !changed
}

// Set 改密码：两次一致、长度达标，改完置 changed。
func (p *Password) Set(newPassword, confirm string) error {
	if newPassword != confirm {
		return auth.ErrPasswordMismatch
	}
	if err := auth.Validate(newPassword); err != nil {
		return err
	}
	hashed, err := auth.Hash(newPassword)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.db.SetSetting(SettingAdminPassword, hashed); err != nil {
		return err
	}
	if err := p.db.SetSetting(SettingPasswordChanged, "1"); err != nil {
		return err
	}
	p.hash = hashed
	p.changed = true
	return nil
}
