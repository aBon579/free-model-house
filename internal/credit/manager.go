package credit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/aBon579/free-model-house/internal/store"
)

type Account struct {
	ID             string   `json:"id"`
	Provider       string   `json:"provider"`
	Name           string   `json:"name"`
	Enabled        bool     `json:"enabled"`
	Hint           string   `json:"hint"`
	UID            string   `json:"uid,omitempty"`
	ExpiresAt      int64    `json:"expiresAt,omitempty"`
	Models         []Model  `json:"models"`
	DisabledModels []string `json:"disabledModels"`
	Effort         string   `json:"effort"`
	MaxMode        bool     `json:"maxMode"`
	// AutoClaimAt 是 UTC+8 的每日领取时刻（"HH:MM"），空 = 该账号不自动领取。
	// 旧版是一个"全部渠道"总开关，没有"什么时候领"的概念；每个上游的刷新点不同，
	// 所以时刻必须落在账号上。
	AutoClaimAt   string   `json:"autoClaimAt,omitempty"`
	Credits       *Credits `json:"credits,omitempty"`
	Error         string   `json:"error,omitempty"`
	CooldownUntil int64    `json:"cooldownUntil,omitempty"`
}

type storedAccount struct {
	Account
	Credential Credential `json:"credential"`
	Revision   int64      `json:"revision"`
}

type state struct {
	Accounts      []storedAccount   `json:"accounts"`
	LastAutoClaim map[string]string `json:"lastAutoClaim"`
	// LegacyAutoClaim 只为装载旧状态而存在：migrateAutoClaim 把它落成每账号的时刻
	// 后立刻写回 false，omitempty 让这个键从此消失。调度路径不读它。
	LegacyAutoClaim bool `json:"autoClaim,omitempty"`
}

type Provider struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Models     []Model  `json:"models"`
	LoginModes []string `json:"loginModes"`
}

type Snapshot struct {
	Providers []Provider `json:"providers"`
	Accounts  []Account  `json:"accounts"`
}

type AccountPatch struct {
	ID             string    `json:"id"`
	Name           *string   `json:"name"`
	Enabled        *bool     `json:"enabled"`
	DisabledModels *[]string `json:"disabledModels"`
	Effort         *string   `json:"effort"`
	MaxMode        *bool     `json:"maxMode"`
	AutoClaimAt    *string   `json:"autoClaimAt"`
	Move           string    `json:"move"`
}

type BackupAccount struct {
	Provider       string     `json:"provider"`
	Name           string     `json:"name"`
	Credential     Credential `json:"credential"`
	Enabled        *bool      `json:"enabled,omitempty"`
	DisabledModels []string   `json:"disabledModels,omitempty"`
	Effort         string     `json:"effort,omitempty"`
	MaxMode        bool       `json:"maxMode,omitempty"`
	AutoClaimAt    string     `json:"autoClaimAt,omitempty"`
}

type loginJob struct {
	Login
	cancel   context.CancelFunc
	targetID string
}

type Manager struct {
	mu       sync.RWMutex
	state    state
	db       *store.DB
	vault    *store.Vault
	client   *http.Client
	adapters map[string]Adapter
	jobs     map[string]*loginJob
	locks    sync.Map
	obsolete []string
	onChange func()
}

const stateSecret = "credit:accounts:v1"

func NewManager(db *store.DB, vault *store.Vault, client *http.Client) (*Manager, error) {
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	m := &Manager{db: db, vault: vault, client: &c, jobs: map[string]*loginJob{}, state: state{Accounts: []storedAccount{}, LastAutoClaim: map[string]string{}}}
	m.adapters = map[string]Adapter{"qoder": NewQoder(&c, false), "qodercn": NewQoder(&c, true), "trae": NewTrae(&c)}
	sealed, _, found, err := db.GetSecret(stateSecret)
	if err != nil {
		return nil, err
	}
	if found {
		plain, err := vault.Open(sealed)
		if err != nil {
			return nil, errors.New("无法解密 Credit 账号，请检查原有主密钥")
		}
		if err = json.Unmarshal([]byte(plain), &m.state); err != nil {
			return nil, errors.New("Credit 账号数据损坏")
		}
		if m.state.LastAutoClaim == nil {
			m.state.LastAutoClaim = map[string]string{}
		}
	}
	dropped, err := m.dropUnknownProviders()
	if err != nil {
		return nil, err
	}
	m.obsolete = dropped
	if err := m.migrateAutoClaim(); err != nil {
		return nil, err
	}
	return m, nil
}

// migrateAutoClaim 把旧版"全部渠道"总开关落成每账号的领取时刻。
//
// 旧语义是"开着就每天进程醒来后的第一分钟全池一起领"——既没得选时间，也让
// Qoder（10:00 才刷新）和 TRAE 共用同一个时刻。迁移按渠道填保守默认值，
// 行为等价但不再挤在同一分钟，而且从此可以逐个账号改或关。
// 只在旧 bool 为 true 时执行一次，之后这个键就消失了。
func (m *Manager) migrateAutoClaim() error {
	if !m.state.LegacyAutoClaim {
		return nil
	}
	next := m.copyLocked()
	for i := range next.Accounts {
		if next.Accounts[i].AutoClaimAt == "" {
			next.Accounts[i].AutoClaimAt = defaultAutoClaimAt(next.Accounts[i].Provider)
		}
	}
	next.LegacyAutoClaim = false
	return m.saveLocked(next)
}

// defaultAutoClaimAt 是旧开关迁移用的时刻：错开刷新点几分钟，避开整点那一分钟。
func defaultAutoClaimAt(provider string) string {
	if provider == "qoder" || provider == "qodercn" {
		return "10:05"
	}
	return "00:05"
}

// autoClaimFloor 是该渠道每天最早的可领取时刻（UTC+8），空串表示没有下限。
// Qoder 的每日领取活动原文写着「每日 10:00（UTC+8）刷新」，早于它去领只会换来
// 一次失败，而当天又不会重试——所以这个下限在保存时就拒绝，而不是运行时无声跳过。
func autoClaimFloor(provider string) string {
	if provider == "qoder" || provider == "qodercn" {
		return "10:00"
	}
	return ""
}

// validateAutoClaimAt 是时刻的**唯一**判入口：格式 + 该渠道的刷新下限。
//
// 设置和导入都要过它 —— 备份文件是用户手边的文本，改一个数字就能把
// "10 点才刷新却 9 点去领"带进来，而那天只会领一次、失败也不再重试。
func validateAutoClaimAt(provider, value string) error {
	minute, off, err := parseAutoClaimAt(value)
	if err != nil || off {
		return err
	}
	floor := autoClaimFloor(provider)
	if floor == "" {
		return nil
	}
	limit, _, _ := parseAutoClaimAt(floor)
	if minute < limit {
		return fmt.Errorf("该渠道每日 %s 才刷新领取，时间不能设得更早", floor)
	}
	return nil
}

// autoClaimDue 判断这个账号今天是否该自动领取：时刻写得有效、已经到点、今天还没试过。
//
// 时间写坏只在旧数据里可能出现（Patch 会拒绝），当作"没开"处理而不是报错——
// 调度循环里抛错会让整池账号当天都领不了。
func autoClaimDue(value, last, today string, minuteNow int) bool {
	if last == today {
		return false
	}
	at, off, err := parseAutoClaimAt(value)
	return err == nil && !off && minuteNow >= at
}

// parseAutoClaimAt 把 "HH:MM" 换成当天分钟数；空串走 off=true。
// 用 time.Parse 而不是自己切字符串：它顺带把 "9:5"、"24:00" 这类写法挡掉。
func parseAutoClaimAt(value string) (minute int, off bool, err error) {
	if value == "" {
		return 0, true, nil
	}
	t, err := time.Parse("15:04", value)
	if err != nil {
		return 0, false, errors.New("自动领取时间要写成 HH:MM（24 小时制），留空表示关闭")
	}
	return t.Hour()*60 + t.Minute(), false, nil
}

// dropUnknownProviders 清掉提供方已经没有适配器的账号，返回被清掉的账号（供启动日志用）。
//
// ⚠️ 这一步是必须的：refresh / Models / Credits / Claim / Prepare 里的适配器查找
//
//	**不带存在性检查**，库里留一个孤儿账号就是第一次碰到它时 nil panic。
//
// 删的是密封保存的凭据，所以要留个响 —— 静默消失没人能发现。
func (m *Manager) dropUnknownProviders() ([]string, error) {
	next := m.copyLocked()
	kept := make([]storedAccount, 0, len(next.Accounts))
	dropped := []string{}
	for _, a := range next.Accounts {
		if _, ok := m.adapters[a.Provider]; ok {
			kept = append(kept, a)
			continue
		}
		dropped = append(dropped, a.Provider+" / "+a.Name)
	}
	if len(dropped) == 0 {
		return nil, nil
	}
	next.Accounts = kept
	for id := range next.LastAutoClaim {
		live := false
		for _, a := range kept {
			if a.ID == id {
				live = true
				break
			}
		}
		if !live {
			delete(next.LastAutoClaim, id)
		}
	}
	return dropped, m.saveLocked(next)
}

// ObsoleteAccounts 是启动时因为提供方已下线而被清掉的账号，用于启动日志。
func (m *Manager) ObsoleteAccounts() []string { return m.obsolete }

func (m *Manager) SetChangeHook(fn func()) { m.mu.Lock(); m.onChange = fn; m.mu.Unlock() }
func (m *Manager) changed() {
	m.mu.RLock()
	fn := m.onChange
	m.mu.RUnlock()
	if fn != nil {
		fn()
	}
}
func (m *Manager) lock(id string) func() {
	v, _ := m.locks.LoadOrStore(id, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func (m *Manager) saveLocked(next state) error {
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	sealed, err := m.vault.Seal(string(b))
	if err != nil {
		return err
	}
	if err = m.db.PutSecret(stateSecret, sealed, "Credit accounts"); err != nil {
		return err
	}
	m.state = next
	return nil
}

func (m *Manager) copyLocked() state {
	b, _ := json.Marshal(m.state)
	var s state
	_ = json.Unmarshal(b, &s)
	return s
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.RLock()
	s := m.copyLocked()
	m.mu.RUnlock()
	out := Snapshot{Accounts: []Account{}, Providers: []Provider{}}
	for _, p := range []Provider{{ID: "qoder", Name: "Qoder", LoginModes: []string{"default"}}, {ID: "qodercn", Name: "Qoder 中国版", LoginModes: []string{"default"}}, {ID: "trae", Name: "TRAE", LoginModes: []string{"default"}}} {
		p.Models = []Model{}
		seen := map[string]bool{}
		for _, a := range s.Accounts {
			if a.Provider == p.ID {
				for _, model := range a.Models {
					if !seen[model.ID] {
						p.Models = append(p.Models, model)
						seen[model.ID] = true
					}
				}
			}
		}
		if p.ID == "qoder" || p.ID == "qodercn" {
			p.Models, _ = m.adapters[p.ID].Models(context.Background(), nil)
		}
		out.Providers = append(out.Providers, p)
	}
	for _, a := range s.Accounts {
		out.Accounts = append(out.Accounts, a.Account)
	}
	return out
}

func (m *Manager) account(id string) (storedAccount, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.state.Accounts {
		if a.ID == id {
			b, _ := json.Marshal(a)
			var cp storedAccount
			_ = json.Unmarshal(b, &cp)
			return cp, nil
		}
	}
	return storedAccount{}, &APIError{Status: 404, Message: "账号不存在或已删除"}
}

func normalizeCredential(c Credential) Credential {
	out := c.Clone()
	if out == nil {
		out = Credential{}
	}
	aliases := map[string][]string{"accessToken": {"accessToken", "access_token", "security_oauth_token", "token", "jwt"}, "refreshToken": {"refreshToken", "refresh_token"}, "uid": {"uid", "user_id", "userId"}, "machineId": {"machineId", "machine_id", "device_mid"}, "name": {"name", "nickname", "user_name"}}
	for key, keys := range aliases {
		if s := c.Text(keys...); s != "" {
			out[key] = s
		}
	}
	if Timestamp(out["expiresAt"]) == 0 {
		for _, key := range []string{"expires_at", "token_expire_at", "TokenExpireAt"} {
			if v := Timestamp(c[key]); v > 0 {
				out["expiresAt"] = v
				break
			}
		}
	}
	return out
}

func accountIdentity(a *storedAccount) {
	a.UID = a.Credential.Text("uid", "userId", "user_id")
	a.Hint = store.CredentialHint(a.Credential.Text("accessToken", "access_token", "token", "jwt"))
	a.ExpiresAt = Timestamp(a.Credential["expiresAt"])
}

func (m *Manager) Add(ctx context.Context, provider, name string, c Credential) (Account, error) {
	return m.add(ctx, provider, name, c, "")
}

func (m *Manager) add(ctx context.Context, provider, name string, c Credential, jobID string) (Account, error) {
	adapter, ok := m.adapters[provider]
	if !ok {
		return Account{}, errors.New("不支持的 Credit 渠道")
	}
	name = strings.TrimSpace(name)
	if len([]rune(name)) > 80 {
		return Account{}, errors.New("账号名称最多 80 字")
	}
	c = normalizeCredential(c)
	if err := adapter.Validate(c); err != nil {
		return Account{}, err
	}
	models, modelErr := adapter.Models(ctx, c)
	if models == nil {
		models = []Model{}
	}
	if name == "" {
		name = c.Text("name", "nickname")
		if name == "" {
			name = provider + " 账号"
		}
	}
	a := storedAccount{Account: Account{ID: randomID(), Provider: provider, Name: name, Enabled: true, Models: models, DisabledModels: []string{}}, Credential: c, Revision: 1}
	accountIdentity(&a)
	if modelErr != nil {
		a.Error = modelErr.Error()
	}
	m.mu.Lock()
	if jobID != "" {
		job := m.jobs[jobID]
		if job == nil || job.State != "pending" {
			m.mu.Unlock()
			return Account{}, errors.New("登录已取消或过期")
		}
		if job.targetID != "" {
			found := false
			for _, old := range m.state.Accounts {
				if old.ID == job.targetID {
					found = true
					if old.Provider != provider || (old.UID != "" && old.UID != a.UID) {
						m.mu.Unlock()
						return Account{}, errors.New("重新授权的身份与原账号不符")
					}
				}
			}
			if !found {
				m.mu.Unlock()
				return Account{}, errors.New("原账号已删除")
			}
		}
	}
	s := m.copyLocked()
	index := -1
	for i, old := range s.Accounts {
		if old.Provider == provider && a.UID != "" && old.UID == a.UID {
			index = i
			a.ID = old.ID
			a.Enabled = old.Enabled
			a.DisabledModels = old.DisabledModels
			a.Effort = old.Effort
			a.MaxMode = old.MaxMode
			a.Revision = old.Revision + 1
			if modelErr != nil {
				a.Models = old.Models
			}
			break
		}
	}
	if index < 0 {
		if len(s.Accounts) >= 100 {
			m.mu.Unlock()
			return Account{}, errors.New("最多保存 100 个授权账号")
		}
		s.Accounts = append(s.Accounts, a)
	} else {
		s.Accounts[index] = a
	}
	err := m.saveLocked(s)
	if err == nil && jobID != "" {
		m.jobs[jobID].State = "success"
		m.jobs[jobID].AccountID = a.ID
		m.jobs[jobID].Data = nil
	}
	m.mu.Unlock()
	if err == nil {
		m.changed()
	}
	return a.Account, err
}

func (m *Manager) Patch(p AccountPatch) error {
	m.mu.Lock()
	s := m.copyLocked()
	idx := -1
	for i := range s.Accounts {
		if s.Accounts[i].ID == p.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		m.mu.Unlock()
		return errors.New("账号不存在")
	}
	a := &s.Accounts[idx]
	if p.Name != nil {
		name := strings.TrimSpace(*p.Name)
		if name == "" || len([]rune(name)) > 80 {
			m.mu.Unlock()
			return errors.New("账号名称须为 1–80 字")
		}
		a.Name = name
	}
	if p.Enabled != nil {
		a.Enabled = *p.Enabled
	}
	if p.DisabledModels != nil {
		known := map[string]bool{}
		for _, model := range a.Models {
			known[model.ID] = true
		}
		for _, id := range *p.DisabledModels {
			if !known[id] {
				m.mu.Unlock()
				return errors.New("禁用列表包含未知模型")
			}
		}
		a.DisabledModels = append([]string{}, (*p.DisabledModels)...)
	}
	if p.Effort != nil {
		allowed := *p.Effort == ""
		for _, model := range a.Models {
			for _, effort := range model.Efforts {
				if effort == *p.Effort {
					allowed = true
				}
			}
		}
		if !allowed {
			m.mu.Unlock()
			return errors.New("该渠道不支持所选思考档位")
		}
		a.Effort = *p.Effort
	}
	if p.MaxMode != nil {
		a.MaxMode = *p.MaxMode
	}
	if p.AutoClaimAt != nil {
		value := strings.TrimSpace(*p.AutoClaimAt)
		if err := validateAutoClaimAt(a.Provider, value); err != nil {
			m.mu.Unlock()
			return err
		}
		a.AutoClaimAt = value
	}
	if p.Move != "" {
		switch p.Move {
		case "up":
			if idx > 0 {
				s.Accounts[idx], s.Accounts[idx-1] = s.Accounts[idx-1], s.Accounts[idx]
			}
		case "down":
			if idx+1 < len(s.Accounts) {
				s.Accounts[idx], s.Accounts[idx+1] = s.Accounts[idx+1], s.Accounts[idx]
			}
		default:
			m.mu.Unlock()
			return errors.New("无效的排序方向")
		}
	}
	err := m.saveLocked(s)
	m.mu.Unlock()
	if err == nil {
		m.changed()
	}
	return err
}

func (m *Manager) Delete(id string) error {
	m.mu.Lock()
	s := m.copyLocked()
	found := false
	for i, a := range s.Accounts {
		if a.ID == id {
			s.Accounts = append(s.Accounts[:i], s.Accounts[i+1:]...)
			found = true
			break
		}
	}
	if !found {
		m.mu.Unlock()
		return errors.New("账号不存在")
	}
	delete(s.LastAutoClaim, id)
	err := m.saveLocked(s)
	if err == nil {
		for _, job := range m.jobs {
			if job.targetID == id {
				job.cancel()
				job.State = "cancelled"
			}
		}
	}
	m.mu.Unlock()
	if err == nil {
		m.changed()
	}
	return err
}

func (m *Manager) updateAccount(old storedAccount, mutate func(*storedAccount)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.copyLocked()
	for i := range s.Accounts {
		if s.Accounts[i].ID == old.ID {
			if s.Accounts[i].Revision != old.Revision {
				return errors.New("账号已重新授权，本次旧请求结果已丢弃")
			}
			mutate(&s.Accounts[i])
			return m.saveLocked(s)
		}
	}
	return errors.New("账号已删除，本次结果已丢弃")
}

func (m *Manager) Refresh(ctx context.Context, id string) error {
	unlock := m.lock(id)
	defer unlock()
	a, err := m.account(id)
	if err != nil {
		return err
	}
	_, err = m.refresh(ctx, a)
	return err
}

func (m *Manager) refresh(ctx context.Context, a storedAccount) (storedAccount, error) {
	c, err := m.adapters[a.Provider].Refresh(ctx, a.Credential)
	if err != nil {
		_ = m.updateAccount(a, func(v *storedAccount) { v.Error = err.Error() })
		return a, err
	}
	c = normalizeCredential(c)
	if err = m.adapters[a.Provider].Validate(c); err != nil {
		return a, err
	}
	err = m.updateAccount(a, func(v *storedAccount) {
		v.Credential = c
		v.Revision++
		v.Error = ""
		v.CooldownUntil = 0
		accountIdentity(v)
	})
	if err != nil {
		return a, err
	}
	return m.account(a.ID)
}

func (m *Manager) activeCredential(ctx context.Context, a storedAccount) (storedAccount, error) {
	if a.ExpiresAt > 0 && a.ExpiresAt < time.Now().Add(2*time.Minute).UnixMilli() {
		return m.refresh(ctx, a)
	}
	return a, nil
}

func (m *Manager) RefreshModels(ctx context.Context, id string) error {
	unlock := m.lock(id)
	defer unlock()
	a, err := m.account(id)
	if err != nil {
		return err
	}
	a, err = m.activeCredential(ctx, a)
	if err != nil {
		return err
	}
	models, err := m.adapters[a.Provider].Models(ctx, a.Credential)
	if err != nil {
		_ = m.updateAccount(a, func(v *storedAccount) { v.Error = err.Error() })
		return err
	}
	if models == nil {
		models = []Model{}
	}
	err = m.updateAccount(a, func(v *storedAccount) { v.Models = models; v.Error = "" })
	if err == nil {
		m.changed()
	}
	return err
}

func (m *Manager) Credits(ctx context.Context, id string) (Credits, error) {
	unlock := m.lock(id)
	defer unlock()
	a, err := m.account(id)
	if err != nil {
		return Credits{}, err
	}
	a, err = m.activeCredential(ctx, a)
	if err != nil {
		return Credits{}, err
	}
	credits, err := m.adapters[a.Provider].Credits(ctx, a.Credential)
	if err != nil {
		_ = m.updateAccount(a, func(v *storedAccount) { v.Error = err.Error() })
		return credits, err
	}
	err = m.updateAccount(a, func(v *storedAccount) { v.Credits = &credits; v.Error = "" })
	return credits, err
}

func (m *Manager) Claim(ctx context.Context, id, campaignID string) ([]ClaimResult, error) {
	unlock := m.lock(id)
	defer unlock()
	a, err := m.account(id)
	if err != nil {
		return nil, err
	}
	a, err = m.activeCredential(ctx, a)
	if err != nil {
		return nil, err
	}
	adapter := m.adapters[a.Provider]
	credits, err := adapter.Credits(ctx, a.Credential)
	if err != nil {
		return nil, err
	}
	results := []ClaimResult{}
	for _, campaign := range credits.Campaigns {
		if campaignID != "" && campaign.ID != campaignID {
			continue
		}
		if campaign.Claimed {
			results = append(results, ClaimResult{ID: campaign.ID, Name: campaign.Name, AlreadyClaimed: true, Message: "该活动已领取"})
			continue
		}
		if !campaign.Claimable {
			continue
		}
		res, claimErr := adapter.Claim(ctx, a.Credential, campaign.ID)
		if claimErr != nil {
			res = ClaimResult{ID: campaign.ID, Name: campaign.Name, Message: claimErr.Error()}
		}
		results = append(results, res)
	}
	if len(results) == 0 {
		results = append(results, ClaimResult{Message: "当前没有可领取的活动，不等同于今天已领取"})
	}
	fresh, ferr := adapter.Credits(ctx, a.Credential)
	if ferr == nil {
		_ = m.updateAccount(a, func(v *storedAccount) { v.Credits = &fresh; v.Error = "" })
	}
	return results, nil
}

func (m *Manager) Reset(id string) error {
	a, err := m.account(id)
	if err != nil {
		return err
	}
	return m.updateAccount(a, func(v *storedAccount) { v.CooldownUntil = 0; v.Error = "" })
}
func (m *Manager) RecordFailure(id string, err error, delay time.Duration) {
	a, e := m.account(id)
	if e != nil {
		return
	}
	_ = m.updateAccount(a, func(v *storedAccount) {
		v.Error = err.Error()
		if delay > 0 {
			v.CooldownUntil = time.Now().Add(delay).UnixMilli()
		}
	})
}
func (m *Manager) RecordSuccess(id string) {
	a, e := m.account(id)
	if e != nil {
		return
	}
	_ = m.updateAccount(a, func(v *storedAccount) { v.Error = ""; v.CooldownUntil = 0 })
}

func (m *Manager) Candidates(provider, model, accountID string) []Account {
	s := m.Snapshot()
	out := []Account{}
	for _, a := range s.Accounts {
		if a.Provider != provider || !a.Enabled || (accountID != "" && a.ID != accountID) || a.CooldownUntil > time.Now().UnixMilli() {
			continue
		}
		disabled := false
		for _, id := range a.DisabledModels {
			if id == model {
				disabled = true
				break
			}
		}
		if disabled {
			continue
		}
		for _, item := range a.Models {
			if item.ID == model {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

func (m *Manager) Prepare(ctx context.Context, id string, in Inference) (Prepared, error) {
	unlock := m.lock(id)
	defer unlock()
	a, err := m.account(id)
	if err != nil {
		return Prepared{}, err
	}
	if !a.Enabled {
		return Prepared{}, errors.New("该账号已停用")
	}
	a, err = m.activeCredential(ctx, a)
	if err != nil {
		return Prepared{}, err
	}
	found := false
	for _, model := range a.Models {
		if model.ID == in.Model.ID {
			in.Model = model
			found = true
			break
		}
	}
	if !found {
		return Prepared{}, errors.New("该账号没有此模型")
	}
	for _, disabled := range a.DisabledModels {
		if disabled == in.Model.ID {
			return Prepared{}, errors.New("此模型已禁用")
		}
	}
	if in.Effort == "" {
		in.Effort = a.Effort
	}
	if in.Effort != "" {
		allowed := false
		for _, effort := range in.Model.Efforts {
			if in.Effort == effort {
				allowed = true
			}
		}
		if !allowed {
			in.Effort = ""
		}
	}
	in.MaxMode = in.MaxMode || a.MaxMode
	return m.adapters[a.Provider].Prepare(ctx, a.Credential, in)
}

func (m *Manager) HTTPClient() *http.Client { return m.client }

func (m *Manager) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		m.autoClaim(ctx)
		select {
		case <-ctx.Done():
			m.mu.Lock()
			for _, job := range m.jobs {
				job.cancel()
			}
			m.mu.Unlock()
			return
		case <-ticker.C:
		}
	}
}

func (m *Manager) autoClaim(ctx context.Context) {
	m.mu.RLock()
	s := m.copyLocked()
	m.mu.RUnlock()
	now := time.Now().In(time.FixedZone("UTC+8", 8*3600))
	day := now.Format("2006-01-02")
	minuteNow := now.Hour()*60 + now.Minute()
	for _, a := range s.Accounts {
		if ctx.Err() != nil {
			return
		}
		if !autoClaimDue(a.AutoClaimAt, s.LastAutoClaim[a.ID], day, minuteNow) {
			continue
		}
		// 一天一次自动尝试；失败留给手动重试，避免反复触发领取风控。
		m.mu.Lock()
		next := m.copyLocked()
		next.LastAutoClaim[a.ID] = day
		if err := m.saveLocked(next); err != nil {
			m.mu.Unlock()
			return
		}
		m.mu.Unlock()
		jobCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		_, _ = m.Claim(jobCtx, a.ID, "")
		cancel()
	}
}

func (m *Manager) Export() []BackupAccount {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s := m.copyLocked()
	out := []BackupAccount{}
	for _, a := range s.Accounts {
		enabled := a.Enabled
		out = append(out, BackupAccount{Provider: a.Provider, Name: a.Name, Credential: a.Credential, Enabled: &enabled, DisabledModels: a.DisabledModels, Effort: a.Effort, MaxMode: a.MaxMode, AutoClaimAt: a.AutoClaimAt})
	}
	return out
}

func (m *Manager) Import(ctx context.Context, items []BackupAccount) (int, error) {
	if len(items) == 0 || len(items) > 100 {
		return 0, errors.New("导入文件须包含 1–100 个账号")
	}
	for _, item := range items {
		a, ok := m.adapters[item.Provider]
		if !ok {
			return 0, errors.New("备份包含不支持的渠道")
		}
		if err := a.Validate(normalizeCredential(item.Credential)); err != nil {
			return 0, err
		}
		// ⚠️ 时刻要在**一个账号都还没落盘之前**判掉：导入是逐个写的，
		//   第 30 个才发现时间填早了，前 29 个已经进来了，而错误里只说第 30 个。
		if err := validateAutoClaimAt(item.Provider, strings.TrimSpace(item.AutoClaimAt)); err != nil {
			return 0, fmt.Errorf("账号「%s」的自动领取时间无效：%w", item.Name, err)
		}
	}
	count := 0
	for _, item := range items {
		a, err := m.Add(ctx, item.Provider, item.Name, item.Credential)
		if err != nil {
			return count, fmt.Errorf("已导入 %d 个账号，后续导入失败：%w", count, err)
		}
		count++
		patch := AccountPatch{ID: a.ID, Enabled: item.Enabled, MaxMode: &item.MaxMode}
		// 旧备份没有这个键 → 空串 → 该账号不自动领，和升级前的行为一致。
		autoClaimAt := strings.TrimSpace(item.AutoClaimAt)
		patch.AutoClaimAt = &autoClaimAt
		if item.Effort != "" {
			patch.Effort = &item.Effort
		}
		if len(item.DisabledModels) > 0 {
			known := []string{}
			for _, id := range item.DisabledModels {
				for _, model := range a.Models {
					if model.ID == id {
						known = append(known, id)
					}
				}
			}
			patch.DisabledModels = &known
		}
		if err = m.Patch(patch); err != nil {
			return count, err
		}
	}
	return count, nil
}
