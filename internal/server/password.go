package server

import (
	"sync"

	"github.com/aBon579/free-model-house/internal/auth"
	"github.com/aBon579/free-model-house/internal/store"
)

// 本文件实现**管理台的密码**。
//
// # 状态机
//
//	还没设密码 → 密码是 `admin`（初始）
//	用户改过     → 密码是他自己设的
//
// # 强制改密码
//
// 用初始密码**登录是成功的** —— 但会话被标记成
// `MustChangePassword`。
//
// ⚠️ **为什么不直接拒绝登录** ——
//
//	那会让用户"登录不进去，也不知道为什么"。
//	登录成功 → 看到"请设置新密码" → 改完就能用了。
//	这个路径更清楚。
//
// # 密码存哪
//
// 数据库里存的是 **bcrypt 哈希**。
// 明文密码**从不落盘**、**从不进日志**、**从不回显**。
//
// 详见 internal/auth/password.go。

// SettingAdminPassword 是管理台密码哈希的存储键。
const SettingAdminPassword = "admin_password_hash"

// DefaultAdminPassword 是管理台的**初始密码**。
//
// ⚠️ 用户明确要求："初始密码是 admin"。
const DefaultAdminPassword = "admin"

// SettingPasswordChanged 标记"用户已经改过初始密码"。
//
// ⚠️ **单独一个标记**，而不是靠比对哈希 ——
//
//	比对哈希要求每次启动都跑一次 bcrypt（200ms+，而且要读文件）。
//	一个布尔标记是 O(1) 的。
const SettingPasswordChanged = "admin_password_changed"

// Password 管理管理台的密码。
type Password struct {
	mu sync.RWMutex
	db *store.DB

	// hash 是 bcrypt 哈希。
	hash string
	// changed 标记用户是否改过初始密码。
	changed bool
}

// NewPassword 读出密码状态；没有就用初始密码。
func NewPassword(db *store.DB) *Password {
	p := &Password{db: db}
	p.load()
	return p
}

// load 读数据库里的密码状态。
func (p *Password) load() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.changed = p.db.GetBool(SettingPasswordChanged, false)
	h, ok, err := p.db.GetSetting(SettingAdminPassword)
	if err != nil || !ok || h == "" {
		// ⚠️ **还没有密码** —— 用初始密码 `admin` 铸一个哈希。
		//   这样登录逻辑跟"用户改过密码"是同一条路径。
		hashed, herr := auth.HashDefault(auth.DefaultPassword)
		if herr == nil {
			_ = p.db.SetSetting(SettingAdminPassword, hashed)
			p.hash = hashed
			return
		}
	}
	p.hash = h
}

// IsDefault 报告当前是不是**还在用初始密码**。
func (p *Password) IsDefault() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return !p.changed
}

// Verify 校验密码。
//
// ⚠️ 返回的 `mustChange` 表示"这次登录用的是初始密码" ——
//
//	调用方要据此把会话标记成"必须改密码"。
func (p *Password) Verify(password string) (ok bool, mustChange bool) {
	p.mu.RLock()
	h := p.hash
	changed := p.changed
	p.mu.RUnlock()

	if h == "" {
		return false, false
	}
	if !auth.Verify(h, password) {
		return false, false
	}
	// ⚠️ 用初始密码登录 → 必须改。
	return true, !changed
}

// Set 改密码。
//
// ⚠️ **长度不够直接拒绝** —— 不接受弱密码。
// ⚠️ 改完 `changed = true` —— 之后就不再强制改了。
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
