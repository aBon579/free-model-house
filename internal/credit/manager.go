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
	// OfficialIdentity 只对有意义的提供方生效（目前只有 ZCode）：
	// 打开后，推理请求带上官方客户端的身份块。
	//
	// ⚠️ **默认关闭**。它的作用是让上游把我们当成官方客户端，
	//
	//	不是修请求格式；上游对拒绝有阶梯处罚（见 zcode_identity.go）。
	OfficialIdentity bool     `json:"officialIdentity,omitempty"`
	Credits          *Credits `json:"credits,omitempty"`
	Error            string   `json:"error,omitempty"`
	CooldownUntil    int64    `json:"cooldownUntil,omitempty"`
}

type storedAccount struct {
	Account
	Credential Credential `json:"credential"`
	Revision   int64      `json:"revision"`
}

type state struct {
	Accounts      []storedAccount   `json:"accounts"`
	AutoClaim     bool              `json:"autoClaim"`
	LastAutoClaim map[string]string `json:"lastAutoClaim"`
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
	AutoClaim bool       `json:"autoClaim"`
}

type AccountPatch struct {
	ID             string    `json:"id"`
	Name           *string   `json:"name"`
	Enabled        *bool     `json:"enabled"`
	DisabledModels *[]string `json:"disabledModels"`
	Effort         *string   `json:"effort"`
	MaxMode        *bool     `json:"maxMode"`
	// OfficialIdentity 是"按账号开启官方身份块"的开关（目前只有 ZCode 有意义）。
	OfficialIdentity *bool  `json:"officialIdentity"`
	Move             string `json:"move"`
}

type BackupAccount struct {
	Provider       string     `json:"provider"`
	Name           string     `json:"name"`
	Credential     Credential `json:"credential"`
	Enabled        *bool      `json:"enabled,omitempty"`
	DisabledModels []string   `json:"disabledModels,omitempty"`
	Effort         string     `json:"effort,omitempty"`
	MaxMode        bool       `json:"maxMode,omitempty"`
}

type loginJob struct {
	Login
	cancel   context.CancelFunc
	targetID string
}

type CaptchaRequest struct {
	ID        string `json:"id"`
	AccountID string `json:"accountId"`
	Provider  string `json:"provider"`
	ExpiresAt int64  `json:"expiresAt"`
	result    chan string
}

type Manager struct {
	mu         sync.RWMutex
	state      state
	db         *store.DB
	vault      *store.Vault
	client     *http.Client
	adapters   map[string]Adapter
	jobs       map[string]*loginJob
	challenges map[string]*CaptchaRequest
	locks      sync.Map
	onChange   func()
}

const stateSecret = "credit:accounts:v1"

func NewManager(db *store.DB, vault *store.Vault, client *http.Client) (*Manager, error) {
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }
	m := &Manager{db: db, vault: vault, client: &c, jobs: map[string]*loginJob{}, challenges: map[string]*CaptchaRequest{}, state: state{Accounts: []storedAccount{}, LastAutoClaim: map[string]string{}}}
	m.adapters = map[string]Adapter{"qoder": NewQoder(&c, false), "qodercn": NewQoder(&c, true), "trae": NewTrae(&c), "zcode": NewZCode(&c)}
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
	return m, nil
}

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
	out := Snapshot{Accounts: []Account{}, Providers: []Provider{}, AutoClaim: s.AutoClaim}
	for _, p := range []Provider{{ID: "qoder", Name: "Qoder", LoginModes: []string{"default"}}, {ID: "qodercn", Name: "Qoder 中国版", LoginModes: []string{"default"}}, {ID: "trae", Name: "TRAE", LoginModes: []string{"default"}}, {ID: "zcode", Name: "ZCode", LoginModes: []string{"bigmodel", "zai"}}} {
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
	if p.OfficialIdentity != nil {
		// 只对 ZCode 有意义。存到别的提供方账号上，界面就会显示一个
		// "打开着但什么都不做"的开关 —— 那比拒绝这次修改更糟。
		if a.Provider != "zcode" {
			m.mu.Unlock()
			return errors.New("只有 ZCode 账号有「官方客户端身份」开关")
		}
		a.OfficialIdentity = *p.OfficialIdentity
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
	if a.ExpiresAt > 0 && a.ExpiresAt < time.Now().Add(2*time.Minute).UnixMilli() && a.Provider != "zcode" {
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

func (m *Manager) Claim(ctx context.Context, id, campaignID, captcha string) ([]ClaimResult, error) {
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
		if a.Provider == "zcode" && captcha == "" {
			return results, &APIError{Status: 409, Code: "captcha_required", Message: "请在浏览器中完成 ZCode 人机验证后领取"}
		}
		res, claimErr := adapter.Claim(ctx, a.Credential, campaign.ID, captcha)
		captcha = ""
		if claimErr != nil {
			var ae *APIError
			if errors.As(claimErr, &ae) && ae.Code == "captcha_required" {
				return results, claimErr
			}
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

func (m *Manager) Prepare(ctx context.Context, id string, in Inference, captcha string) (Prepared, error) {
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
	in.OfficialIdentity = a.OfficialIdentity
	c := a.Credential.Clone()
	if captcha != "" {
		c["captchaVerifyParam"] = captcha
	}
	return m.adapters[a.Provider].Prepare(ctx, c, in)
}

func (m *Manager) HTTPClient() *http.Client { return m.client }

func (m *Manager) SetAutoClaim(enabled bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.copyLocked()
	s.AutoClaim = enabled
	return m.saveLocked(s)
}

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
	if !s.AutoClaim {
		return
	}
	now := time.Now().In(time.FixedZone("UTC+8", 8*3600))
	day := now.Format("2006-01-02")
	for _, a := range s.Accounts {
		if ctx.Err() != nil {
			return
		}
		if s.LastAutoClaim[a.ID] == day {
			continue
		}
		if (a.Provider == "qoder" || a.Provider == "qodercn") && now.Hour() < 10 {
			continue
		}
		if a.Provider == "zcode" {
			continue
		}
		// 一天一次自动尝试；失败留给手动重试，避免反复触发领取风控。
		m.mu.Lock()
		next := m.copyLocked()
		next.LastAutoClaim[a.ID] = day
		err := m.saveLocked(next)
		m.mu.Unlock()
		if err != nil {
			return
		}
		jobCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		_, _ = m.Claim(jobCtx, a.ID, "", "")
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
		out = append(out, BackupAccount{Provider: a.Provider, Name: a.Name, Credential: a.Credential, Enabled: &enabled, DisabledModels: a.DisabledModels, Effort: a.Effort, MaxMode: a.MaxMode})
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
	}
	count := 0
	for _, item := range items {
		a, err := m.Add(ctx, item.Provider, item.Name, item.Credential)
		if err != nil {
			return count, fmt.Errorf("已导入 %d 个账号，后续导入失败：%w", count, err)
		}
		count++
		patch := AccountPatch{ID: a.ID, Enabled: item.Enabled, MaxMode: &item.MaxMode}
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
