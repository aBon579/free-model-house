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
//
// ## 多把调用 Key（调用管理页）
//
// 需求是"不一样的设备/人用不一样的 Key"：
//   - 每把 Key 一个命名标签（≤6 字），认得出是谁的
//   - 每把 Key 独立档位（light / standard / deep）—— 网关级为 off 时生效
//   - 每把 Key 独立启停（收走某台设备不影响别人）
//
// ## 两层限流的执行顺序
//
//	请求进来 →
//	  ① 网关级档位不是 off → 用网关的限流器（所有 Key 共用）
//	  ② 网关级是 off       → 用**这把 Key 自己**的限流器（按 Key 记账）
//
// 网关级限流器只有一个；单 Key 限流器每把一个，互不干扰 ——
// 笔记本 60 次/分钟打满了，手机不受影响。
//
// ## 为什么要分层？爆炸半径。
//
// 一把要在网络上跑的 Key 迟早会泄露。
// 泄露的代价应该是"轮换那一把 Key"，而不是"所有设备都要换钥匙"。
// 多 Key 模式下这更直接：每台设备一把，泄了哪把换哪把。
//
// 鉴权是**按 Key** 的，不是按端口：
//	管理会话   HttpOnly Cookie（8h）  管理面     只有 NAS 本机浏览器
//	调用 Key   fmh-…                  推理面     局域网里的每台设备各一把
// 详见 docs/design/02-key-model.md。

const (
	// KeyPrefix 是所有推理 Key 的固定前缀。
	KeyPrefix = "fmh-"

	// MaxKeyLabelChars 是命名标签的最大长度（按字符数，不是字节数）。
	//
	// ⚠️ 中文一个字算一个字符 —— 用 []rune 计数，
	//   否则 "笔记本" 三个汉字会被 UTF-8 字节数误判成超长。
	MaxKeyLabelChars = 6
)

// ApiKey 是一把调用 Key 的完整形态。
//
// ⚠️ `Key` 用 json:"-" —— **永不序列化**。
// 它只在两个时刻离开后端：创建/轮换的响应里（只显示一次）。
type ApiKey struct {
	ID        string `json:"id"`
	Key       string `json:"-"`
	Label     string `json:"label"`
	Tier      string `json:"tier"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"createdAt"`
}

// Keys 管理调用 Key 与管理会话。
type Keys struct {
	mu sync.RWMutex
	db *store.DB

	// keys 是全部调用 Key（内存缓存，按 Key 本体索引 —— 鉴权热路径）。
	keys map[string]ApiKey
	// limiters 是**每把 Key** 的限流器（按 Key 本体索引）。
	//
	// ⚠️ 网关级为 off 时才被问到 ——
	//   网关级限着时网关限流器直接裁决，根本走不到这里。
	//   但限流器**常驻**：Key 的档位改了就重建，
	//   "网关级锁了一阵再放开"后各 Key 的限流器还在、
	//   各自的令牌桶状态（剩多少额度）也还在。
	limiters map[string]*limiter
	// lanKey 是**兼容层**：旧版那把唯一的 Key。
	//
	// ⚠️ 它现在只是"多 Key 里的第一把" ——
	//   老实例升级后，原来那把 Key 变成 `api_keys` 表里的一行
	//   （标签"主 Key"），所有现有 Agent 不用改配置。
	//   保留这个字段是为了 `Banner()` 与 `fmh key` 的启动提示
	//   还能显示"默认那把"。
	lanKey string

	// gatewayTier 是网关级档位（off / light / standard / deep）。
	//
	// ⚠️ 非 off 时**所有 Key 强制跟随** —— 单 Key 的 tier 被锁定。
	//   回 off 后各 Key 按自己存的 tier 恢复生效。
	gatewayTier LanTier
	// gatewayLimiter 是网关级的限流器（所有 Key 共用）。
	// nil = 网关级是 off（或 deep —— 不限），不拦。
	gatewayLimiter *limiter

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

// load 读出全部 Key；一把都没有时铸造第一把。
//
// ⚠️ **老实例迁移** ——
//
//	旧版只有 settings 表里的一把 `lan_key`。
//	升级后 `api_keys` 表是空的：把那把 Key 迁进去（标签"主 Key"，deep 档），
//	所有现有 Agent 的配置**一个字都不用改**。
//
//	旧版还有一个**网关级**档位（lan_tier）—— 直接搬到 gateway_tier。
//	但旧的 light/standard/deep 在新模型里含义变了
//	（旧：唯一的限流；新：锁死所有 Key）——
//	为升级后行为不突变，一律映射到 off，让单 Key 接管。
func (k *Keys) load() {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.keys = map[string]ApiKey{}
	k.limiters = map[string]*limiter{}

	rows, err := k.db.ListApiKeys()
	if err == nil && len(rows) > 0 {
		for _, r := range rows {
			k1 := ApiKey{
				ID: r.ID, Key: r.Key, Label: r.Label,
				Tier: string(ParseKeyTier(r.Tier)), Enabled: r.Enabled, CreatedAt: r.CreatedAt,
			}
			k.keys[r.Key] = k1
			k.rebuildLimiterLocked(k1)
		}
		k.lanKey = k.oldestLocked()
	} else {
		// 一把都没有 —— 迁移旧 Key，或铸造新的。
		var key string
		if old, ok, err := k.db.GetSetting(store.SettingLanKey); err == nil && ok && old != "" {
			key = old
		} else {
			key = store.GenerateKey(KeyPrefix)
			_ = k.db.SetSetting(store.SettingLanKey, key)
		}
		row := store.ApiKeyRow{
			ID: store.GenerateKey("fmh-kid"), Key: key,
			Label: "主 Key", Tier: string(LanTierDeep),
			Enabled: true, CreatedAt: time.Now().UnixMilli(),
		}
		if err := k.db.PutApiKey(row); err == nil {
			k1 := ApiKey{
				ID: row.ID, Key: key, Label: row.Label,
				Tier: row.Tier, Enabled: true, CreatedAt: row.CreatedAt,
			}
			k.keys[key] = k1
			k.rebuildLimiterLocked(k1)
		}
		k.lanKey = key
	}

	// 网关级档位：新键优先，旧键迁移到 off。
	if raw, ok, err := k.db.GetSetting(store.SettingGatewayTier); err == nil && ok && raw != "" {
		k.gatewayTier = ParseLanTier(raw)
	} else {
		k.gatewayTier = LanTierOff
		_ = k.db.SetSetting(store.SettingGatewayTier, string(LanTierOff))
	}
	k.rebuildGatewayLimiterLocked()
}

// rebuildLimiterLocked 按 Key 的档位（重）建它的限流器（调用方持锁）。
//
// ⚠️ 换新限流器 = 令牌桶清零重来 ——
//
//	改档位本来就是用户明确想改限制，旧桶里的余额没有保留价值。
func (k *Keys) rebuildLimiterLocked(k1 ApiKey) {
	t := ParseKeyTier(k1.Tier)
	if t == LanTierDeep {
		k.limiters[k1.Key] = nil
		return
	}
	perMin, conc := TierLimits(t)
	k.limiters[k1.Key] = newLimiter(perMin, conc)
}

// rebuildGatewayLimiterLocked 按网关档位（重）建网关限流器（调用方持锁）。
func (k *Keys) rebuildGatewayLimiterLocked() {
	switch k.gatewayTier {
	case LanTierLight, LanTierStandard:
		perMin, conc := TierLimits(k.gatewayTier)
		k.gatewayLimiter = newLimiter(perMin, conc)
	default:
		// off / deep —— 不拦。
		k.gatewayLimiter = nil
	}
}

// oldestLocked 取最早创建的 Key（调用方持锁）。
func (k *Keys) oldestLocked() string {
	var best string
	var bestAt int64
	first := true
	for _, k1 := range k.keys {
		if first || k1.CreatedAt < bestAt {
			best, bestAt, first = k1.Key, k1.CreatedAt, false
		}
	}
	return best
}

// LanKey 返回默认（最早创建）的调用 Key。
//
// ⚠️ 只用于启动横幅与 `fmh key` ——
//
//	管理台上应该用"调用管理"页列出全部 Key。
func (k *Keys) LanKey() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.lanKey
}

// GatewayTier 返回网关级档位。
func (k *Keys) GatewayTier() LanTier {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.gatewayTier
}

// SetGatewayTier 设置网关级档位。
//
// ⚠️ 非 off 时各 Key 的 tier **保留不动**（只是被盖住）——
//
//	回 off 后自然恢复，不需要"存回来"这一步。
//	这就是"记住各个 Key 的限制设置"的实现方式：
//	设置从来没被改过，只是生效的优先级变了。
func (k *Keys) SetGatewayTier(t LanTier) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.gatewayTier = t
	k.rebuildGatewayLimiterLocked()
}

// List 返回全部 Key（不含 Key 本体）。
func (k *Keys) List() []ApiKey {
	k.mu.RLock()
	defer k.mu.RUnlock()
	out := make([]ApiKey, 0, len(k.keys))
	for _, k1 := range k.keys {
		out = append(out, k1)
	}
	// 按创建时间排 —— 稳定的展示顺序。
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].CreatedAt < out[i].CreatedAt {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// Create 铸一把新 Key。
//
// ⚠️ label ≤ 6 字符（按字符数）—— 超长直接报错，
//
//	不悄悄截断（截断后的名字可能认不出是谁）。
func (k *Keys) Create(label, tier string) (ApiKey, error) {
	label = strings.TrimSpace(label)
	if n := len([]rune(label)); n > MaxKeyLabelChars {
		return ApiKey{}, errLabelTooLong(n)
	}
	t := ParseKeyTier(tier)
	key := store.GenerateKey(KeyPrefix)
	row := store.ApiKeyRow{
		ID: store.GenerateKey("fmh-kid"), Key: key,
		Label: label, Tier: string(t), Enabled: true,
		CreatedAt: time.Now().UnixMilli(),
	}
	if err := k.db.PutApiKey(row); err != nil {
		return ApiKey{}, err
	}
	out := ApiKey{
		ID: row.ID, Key: key, Label: label, Tier: string(t),
		Enabled: true, CreatedAt: row.CreatedAt,
	}
	k.mu.Lock()
	k.keys[key] = out
	k.rebuildLimiterLocked(out)
	k.mu.Unlock()
	return out, nil
}

// Update 改一把 Key 的标签 / 档位 / 开关。
//
// ⚠️ 不允许把**最后一把启用的 Key** 停掉 ——
//
//	那等于把自己锁在门外，所有 Agent 一起失效。
//
// ⚠️ 网关级不是 off 时**拒绝改档位** ——
//
//	此时档位被网关盖住，改了也不生效，用户会以为界面坏了；
//	明确报错反而让他知道该先去关网关级限制。
func (k *Keys) Update(id, label, tier string, enabled *bool) (ApiKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	var target ApiKey
	for _, k1 := range k.keys {
		if k1.ID == id {
			target = k1
			break
		}
	}
	if target.ID == "" {
		return ApiKey{}, errKeyNotFound
	}

	if label != "" {
		label = strings.TrimSpace(label)
		if n := len([]rune(label)); n > MaxKeyLabelChars {
			return ApiKey{}, errLabelTooLong(n)
		}
		target.Label = label
	}
	if tier != "" {
		if k.gatewayTier != LanTierOff {
			return ApiKey{}, errTierLocked
		}
		target.Tier = string(ParseKeyTier(tier))
	}
	if enabled != nil {
		if !*enabled {
			on := 0
			for _, k1 := range k.keys {
				if k1.Enabled && k1.ID != id {
					on++
				}
			}
			if on == 0 {
				return ApiKey{}, errDisableLast
			}
		}
		target.Enabled = *enabled
	}
	if err := k.db.PutApiKey(store.ApiKeyRow{
		ID: target.ID, Key: target.Key, Label: target.Label,
		Tier: target.Tier, Enabled: target.Enabled, CreatedAt: target.CreatedAt,
	}); err != nil {
		return ApiKey{}, err
	}
	k.keys[target.Key] = target
	k.rebuildLimiterLocked(target)
	return target, nil
}

// Rotate 轮换一把 Key —— 旧 Key 立即失效。
func (k *Keys) Rotate(id string) (ApiKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	var target ApiKey
	for _, k1 := range k.keys {
		if k1.ID == id {
			target = k1
			break
		}
	}
	if target.ID == "" {
		return ApiKey{}, errKeyNotFound
	}

	newKey := store.GenerateKey(KeyPrefix)
	// 旧 Key 的限流器跟着删 —— 它已经不是 Key 了。
	delete(k.limiters, target.Key)
	delete(k.keys, target.Key)
	target.Key = newKey
	if err := k.db.PutApiKey(store.ApiKeyRow{
		ID: target.ID, Key: newKey, Label: target.Label,
		Tier: target.Tier, Enabled: target.Enabled, CreatedAt: target.CreatedAt,
	}); err != nil {
		return ApiKey{}, err
	}
	k.keys[newKey] = target
	k.rebuildLimiterLocked(target)
	if k.lanKey != "" && !k.keyExistsLocked(k.lanKey) {
		k.lanKey = k.oldestLocked()
	}
	return target, nil
}

// keyExistsLocked 报告一把 Key 还在不在（调用方持锁）。
func (k *Keys) keyExistsLocked(key string) bool {
	_, ok := k.keys[key]
	return ok
}

// Delete 删一把 Key。
//
// ⚠️ 不允许删**最后一把** —— 同 Update 的理由。
func (k *Keys) Delete(id string) error {
	k.mu.Lock()
	defer k.mu.Unlock()

	var target ApiKey
	for _, k1 := range k.keys {
		if k1.ID == id {
			target = k1
			break
		}
	}
	if target.ID == "" {
		return errKeyNotFound
	}
	if len(k.keys) <= 1 {
		return errDisableLast
	}
	if err := k.db.DeleteApiKey(id); err != nil {
		return err
	}
	delete(k.keys, target.Key)
	delete(k.limiters, target.Key)
	if k.lanKey == target.Key {
		k.lanKey = k.oldestLocked()
	}
	return nil
}

// RotateLanKey 轮换默认 Key（兼容旧接口 —— 管理台的旧按钮）。
//
// ⚠️ 旧接口语义是"只有一把 Key"—— 多 Key 下它轮换**默认那把**。
func (k *Keys) RotateLanKey() string {
	k.mu.RLock()
	id := ""
	for _, k1 := range k.keys {
		if k1.Key == k.lanKey {
			id = k1.ID
			break
		}
	}
	k.mu.RUnlock()
	if id == "" {
		return ""
	}
	out, err := k.Rotate(id)
	if err != nil {
		return ""
	}
	return out.Key
}

// MatchAny 校验一个 Key 是不是**启用的调用 Key**。
//
// ⚠️ 推理面用这个 —— 单端口下，LAN 里的 agent 拿的就是自己的那把，
//
//	而它们访问的是同一个端口。
//
// ⚠️ **停用的 Key 在这里被拒** —— 那正是"停用"的含义：
//
//	那台设备立刻失去调用权，不用等任何刷新。
//
// ⚠️ **管理面不用这个** —— 管理面用**密码**。
func (k *Keys) MatchAny(presented string) bool {
	if presented == "" {
		return false
	}
	k.mu.RLock()
	defer k.mu.RUnlock()
	for key, info := range k.keys {
		if subtle.ConstantTimeCompare([]byte(key), []byte(presented)) == 1 {
			return info.Enabled
		}
	}
	return false
}

// Allow 报告这把 Key 的这次请求能不能过限流。
//
// ⚠️ **两层限流的裁决点** ——
//
//	网关级 != off → 只看网关限流器（所有 Key 共用一个桶）。
//	网关级 == off → 看这把 Key 自己的限流器（deep = nil，直接过）。
//
// ⚠️ **名额要在请求结束时归还**（`Release`）——
//
//	并发上限靠"占着名额才算并发"来算。
func (k *Keys) Allow(presented string) bool {
	k.mu.RLock()
	defer k.mu.RUnlock()
	// ① 网关级优先 —— 非 off 时所有 Key 一视同仁。
	if k.gatewayTier != LanTierOff {
		if k.gatewayLimiter == nil {
			return true // deep
		}
		return k.gatewayLimiter.allow()
	}
	// ② 单 Key 级。
	l := k.limiters[presented]
	if l == nil {
		return true // deep / 未知 Key（鉴权那关已经拦过）
	}
	return l.allow()
}

// Release 归还并发名额。
//
// ⚠️ 与 Allow 用的**同一个限流器**才需要归还 ——
//
//	所以这里要重复一遍"网关 or 这把 Key"的判定。
//	两处判定不一致的话，名额会被还到别的桶里，
//	并发计数只涨不降，几轮之后全部请求被拒。
func (k *Keys) Release(presented string) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if k.gatewayTier != LanTierOff {
		if k.gatewayLimiter != nil {
			k.gatewayLimiter.release()
		}
		return
	}
	if l := k.limiters[presented]; l != nil {
		l.release()
	}
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

// —— 管理会话 ——

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
// ⚠️ 改密码时用 —— 旧密码持有者的会话必须一起失效。
func (k *Keys) InvalidateAllSessionsExcept(keep string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for id := range k.sessions {
		if id != keep {
			delete(k.sessions, id)
		}
	}
}
