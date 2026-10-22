package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aBon579/free-model-house/internal/store"
)

// 本文件实现 **Key 分层**。
// 三把钥匙，永不相交：
//	管理会话   HttpOnly Cookie（8h）  管理面     只有 NAS 本机浏览器
//	LAN Key    fmh-…                  推理面     局域网 Agent 用户
// 用户要求"项目自己生成一个固定前缀的 API key"，所以**两把推理 Key 用同一个前缀**
// `fmh-`（形状为 `fmh-<40 hex>`）。
// 为什么本机 Key 和 LAN Key 用同一个前缀？因为它们是**同一类东西** ——
// 都是"调用 FMH 推理接口的凭据"。区别不在**形状**上，而在**这把钥匙能开哪扇门**。
// ⚠️ 但这带来一个设计要求：**两者必须仍然是两把不同的钥匙**。
// 爆炸半径的分离靠的是"独立轮换"，不是"前缀不同"：
// 轮换 LAN Key → 旧 LAN Key 失效，本机 Key 与所有工具照常工作。
// 如果两把钥匙**字面相同**，那 LAN Key 泄露 = 本机 Key 泄露，爆炸半径就完全没分离了。所以：
// ① 首次生成时，**强制**保证两把钥匙不同
// ② 启动时校验，若相同则重新生成 LAN Key
// ## 为什么要分层？爆炸半径。
// 一把要在网络上跑的 Key 迟早会泄露。
// 泄露的代价应该是"轮换一把 Key"，而不是"本机所有工具的钥匙"。
// 详见 docs/design/02-key-model.md。

const (
	// KeyPrefix 是所有推理 Key 的固定前缀。
	// 用户要求："项目自己生成一个固定前缀的 API key"。
	KeyPrefix = "fmh-"

	// LocalKeyName 是本机推理 Key 的名字（管理台显示用）。
	LocalKeyName = "本机 Key"
	// LanKeyName 是局域网推理 Key 的名字。
	LanKeyName = "局域网 Key"
)

// Keys 管理钥匙与会话。
type Keys struct {
	mu sync.RWMutex
	db *store.DB

	// lanKey 是**唯一**的推理 Key。
	//
	// ⚠️ **本机 Key 已砍掉** ——
	//   管理台改用**密码**登录（见 internal/auth）。
	//   之前两把钥匙的分工是"本机 Key 换管理会话，LAN Key 调推理"。
	//   现在管理会话靠密码，所以只剩一把推理 Key。
	lanKey string

	// sessions 是管理会话（内存态）。
	sessions map[string]sessionEntry
	// sessionTTL 是会话有效期。
	sessionTTL time.Duration
	// bootstrap 是一次性登录令牌。
	bootstrap string
	// bootstrapUntil 是它的有效期。
	bootstrapUntil time.Time
}

// sessionEntry 是一个管理会话。
type sessionEntry struct {
	id      string
	expires time.Time
	// label 是会话的标签（给日志用）。
	label string
	// mustChange 标记"这个会话必须先改密码"。
	//
	// ⚠️ 用**初始密码** `admin` 登录时置位 ——
	//   改完密码就清掉（实际上改完密码所有会话都作废了）。
	mustChange bool
}

// NewKeys 建一个钥匙管理器。
func NewKeys(db *store.DB) *Keys {
	k := &Keys{
		db:         db,
		sessions:   map[string]sessionEntry{},
		sessionTTL: 8 * time.Hour,
	}
	k.load()
	return k
}

// load 从数据库读出（或生成）推理 Key。
// ⚠️ **懒生成**：第一次访问时铸造并持久化。
// 这样密钥文件被删也不会导致服务起不来（只是换了一把新钥匙）。
func (k *Keys) load() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.lanKey = k.readOrMint(store.SettingLanKey)
}

// readOrMint 读一把钥匙；没有就铸造一把并存起来。
func (k *Keys) readOrMint(setting string) string {
	if v, ok, err := k.db.GetSetting(setting); err == nil && ok && v != "" {
		return v
	}
	key := store.GenerateKey(KeyPrefix)
	_ = k.db.SetSetting(setting, key)
	return key
}

// LanKey 返回局域网推理 Key。
func (k *Keys) LanKey() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.lanKey
}

// RotateLanKey 轮换 LAN Key。
// ⚠️ 旧 Key 立即失效；管理会话**不受影响**。
func (k *Keys) RotateLanKey() string {
	key := store.GenerateKey(KeyPrefix)
	k.mu.Lock()
	k.lanKey = key
	k.mu.Unlock()
	_ = k.db.SetSetting(store.SettingLanKey, key)
	return key
}

// MatchAny 校验一个 Key 是不是**推理 Key**。
//
// ⚠️ 推理面用这个 —— 单端口下，LAN 里的 agent 拿的就是 LAN Key，
// 而它们访问的是同一个端口。
//
// ⚠️ **管理面不用这个** —— 管理面用**密码**。
// 那个区分保证了：LAN Key 泄露 = 白嫖额度，但进不了管理面。
func (k *Keys) MatchAny(presented string) bool {
	if presented == "" {
		return false
	}
	lan := k.LanKey()
	return subtle.ConstantTimeCompare([]byte(lan), []byte(presented)) == 1
}

// MatchLan 校验一个 Key 是不是 LAN Key。
func (k *Keys) MatchLan(presented string) bool {
	return k.matches(k.LanKey(), presented)
}

// matches 常数时间比较。
// ⚠️ 必须用常数时间 —— 否则可以靠响应时间**逐字节猜出** Key。
func (k *Keys) matches(expected, presented string) bool {
	if expected == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(expected), []byte(presented)) == 1
}

// NewBootstrapToken 生成一个一次性登录令牌。
// ⚠️ 它是**一次性**的，且有效期很短（10 分钟）。
// 它通过 URL 的 **fragment** 传递（`#token=...`）——
// fragment 不会发到服务器，所以不会进访问日志。
func (k *Keys) NewBootstrapToken() string {
	token := store.GenerateKey("fmh-boot")
	k.mu.Lock()
	defer k.mu.Unlock()
	k.bootstrap = token
	k.bootstrapUntil = time.Now().Add(10 * time.Minute)
	return token
}

// RedeemBootstrap 兑换一次性令牌，返回一个会话 id。
// ⚠️ **只能用一次** —— 兑换后立刻作废。
func (k *Keys) RedeemBootstrap(token string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.bootstrap == "" || token == "" {
		return "", false
	}
	if time.Now().After(k.bootstrapUntil) {
		k.bootstrap = ""
		return "", false
	}
	// ⚠️ 常数时间比较 —— token 是一次性的，但泄露了就能直接登录。
	if subtle.ConstantTimeCompare([]byte(k.bootstrap), []byte(token)) != 1 {
		return "", false
	}
	// 立刻作废。
	k.bootstrap = ""
	id := store.GenerateKey("fmh-sess")
	k.sessions[id] = sessionEntry{
		id:      id,
		expires: time.Now().Add(k.sessionTTL),
	}
	return id, true
}

// NewSession 直接建一个会话（用密码登录时用）。
func (k *Keys) NewSession(label string) string {
	id := store.GenerateKey("fmh-sess")
	k.mu.Lock()
	defer k.mu.Unlock()
	k.sessions[id] = sessionEntry{
		id:      id,
		expires: time.Now().Add(k.sessionTTL),
		label:   label,
	}
	return id
}

// SetSessionMustChange 标记一个会话"必须先改密码"。
func (k *Keys) SetSessionMustChange(id string, must bool) {
	if id == "" {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if entry, ok := k.sessions[id]; ok {
		entry.mustChange = must
		k.sessions[id] = entry
	}
}

// SessionMustChange 报告一个会话是不是"必须先改密码"。
func (k *Keys) SessionMustChange(id string) bool {
	if id == "" {
		return false
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	entry, ok := k.sessions[id]
	return ok && entry.mustChange
}

// ValidSession 校验一个会话 id。
func (k *Keys) ValidSession(id string) bool {
	if id == "" {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	entry, ok := k.sessions[id]
	if !ok {
		return false
	}
	if time.Now().After(entry.expires) {
		delete(k.sessions, id)
		return false
	}
	return true
}

// InvalidateSession 作废一个会话。
func (k *Keys) InvalidateSession(id string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.sessions, id)
}

// InvalidateAllSessionsExcept 作废除某个之外的所有会话。
// ⚠️ 轮换本机 Key 时用 —— 旧 Key 的持有者的会话必须一起失效。
func (k *Keys) InvalidateAllSessionsExcept(keep string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for id := range k.sessions {
		if id != keep {
			delete(k.sessions, id)
		}
	}
}

// SessionCount 返回当前会话数（给看板用）。
func (k *Keys) SessionCount() int {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return len(k.sessions)
}

// BearerOf 从请求里取出 Bearer Key。
// 同时接受 `Authorization: Bearer <key>` 与 `x-api-key: <key>`
// —— 后者是 Anthropic SDK 的习惯。
func BearerOf(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if strings.HasPrefix(strings.ToLower(h), "bearer ") {
			return strings.TrimSpace(h[7:])
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}
