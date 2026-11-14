package credit

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
)

func (m *Manager) StartLogin(ctx context.Context, provider, mode, targetID string) (Login, error) {
	adapter, ok := m.adapters[provider]
	if !ok {
		return Login{}, errors.New("未知渠道")
	}
	if targetID != "" {
		a, err := m.account(targetID)
		if err != nil {
			return Login{}, err
		}
		if a.Provider != provider {
			return Login{}, errors.New("账号与登录渠道不匹配")
		}
	}
	m.mu.Lock()
	active := 0
	for id, job := range m.jobs {
		if time.Now().UnixMilli() > job.ExpiresAt {
			job.cancel()
			delete(m.jobs, id)
		} else if job.State == "pending" {
			active++
		}
	}
	m.mu.Unlock()
	if active >= 8 {
		return Login{}, errors.New("登录会话过多，请先取消未完成的登录")
	}
	login, err := adapter.StartLogin(ctx, mode)
	if err != nil {
		return Login{}, err
	}
	u, err := url.Parse(login.URL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return Login{}, errors.New("上游返回了无效的授权地址")
	}
	login.ID = randomID()
	login.Provider = provider
	login.State = "pending"
	if login.Interval < 1 {
		login.Interval = 2
	}
	if login.ExpiresAt <= time.Now().UnixMilli() {
		login.ExpiresAt = time.Now().Add(5 * time.Minute).UnixMilli()
	}
	jobCtx, cancel := context.WithDeadline(context.Background(), time.UnixMilli(login.ExpiresAt))
	job := &loginJob{Login: *login, cancel: cancel, targetID: targetID}
	m.mu.Lock()
	m.jobs[login.ID] = job
	m.mu.Unlock()
	if provider != "trae" {
		go m.pollLogin(jobCtx, login.ID)
	} else {
		go func() { <-jobCtx.Done(); m.finishLoginError(login.ID, "登录已过期，请重新发起") }()
	}
	return m.Login(login.ID)
}

func (m *Manager) Login(id string) (Login, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job := m.jobs[id]
	if job == nil {
		return Login{}, &APIError{Status: 404, Message: "登录会话不存在或已过期"}
	}
	out := job.Login
	out.Data = nil
	return out, nil
}

func (m *Manager) loginPrivate(id string) (Login, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	job := m.jobs[id]
	if job == nil || job.State != "pending" {
		return Login{}, errors.New("登录会话已结束")
	}
	out := job.Login
	out.Data = job.Data.Clone()
	return out, nil
}

func (m *Manager) finishLoginError(id, message string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if job := m.jobs[id]; job != nil && job.State == "pending" {
		job.State = "error"
		job.Message = message
		job.Data = nil
		job.cancel()
	}
}

func (m *Manager) pollLogin(ctx context.Context, id string) {
	failures := 0
	for {
		login, err := m.loginPrivate(id)
		if err != nil {
			return
		}
		credential, done, err := m.adapters[login.Provider].PollLogin(ctx, &login, "")
		if ctx.Err() != nil {
			m.finishLoginError(id, "登录已过期，请重新发起")
			return
		}
		if err != nil {
			failures++
			var ae *APIError
			if failures >= 5 || (errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403 || ae.Status == 400)) {
				m.finishLoginError(id, err.Error())
				return
			}
		} else {
			failures = 0
		}
		if done {
			_, err = m.add(ctx, login.Provider, "", credential, id)
			if err != nil {
				m.finishLoginError(id, err.Error())
			}
			m.mu.Lock()
			if job := m.jobs[id]; job != nil {
				job.cancel()
			}
			m.mu.Unlock()
			return
		}
		timer := time.NewTimer(time.Duration(login.Interval) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			m.finishLoginError(id, "登录已过期，请重新发起")
			return
		case <-timer.C:
		}
	}
}

func (m *Manager) CompleteLogin(ctx context.Context, id, callback string) (Login, error) {
	login, err := m.loginPrivate(id)
	if err != nil {
		return Login{}, err
	}
	if login.Provider != "trae" {
		return Login{}, errors.New("此渠道无需粘贴回调地址")
	}
	if time.Now().UnixMilli() > login.ExpiresAt {
		return Login{}, errors.New("登录会话已过期")
	}
	credential, done, err := m.adapters[login.Provider].PollLogin(ctx, &login, callback)
	if err != nil {
		return Login{}, err
	}
	if !done {
		return Login{}, errors.New("回调数据中尚未包含完整授权结果")
	}
	if _, err = m.add(ctx, login.Provider, "", credential, id); err != nil {
		return Login{}, err
	}
	m.mu.Lock()
	if job := m.jobs[id]; job != nil {
		job.cancel()
	}
	m.mu.Unlock()
	return m.Login(id)
}

func (m *Manager) CancelLogin(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	job := m.jobs[id]
	if job == nil {
		return errors.New("登录会话不存在")
	}
	if job.State == "pending" {
		job.State = "cancelled"
		job.Data = nil
		job.cancel()
	}
	return nil
}

func (m *Manager) CaptchaConfig(ctx context.Context, id string) (map[string]any, error) {
	a, err := m.account(id)
	if err != nil {
		return nil, err
	}
	provider, ok := m.adapters[a.Provider].(interface {
		CaptchaConfig(context.Context, Credential) (map[string]any, error)
	})
	if !ok {
		return nil, errors.New("此渠道不使用浏览器验证码")
	}
	return provider.CaptchaConfig(ctx, a.Credential)
}

func (m *Manager) CaptchaRequests() []CaptchaRequest {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []CaptchaRequest{}
	for _, request := range m.challenges {
		if request.ExpiresAt > time.Now().UnixMilli() {
			out = append(out, *request)
		}
	}
	return out
}

func (m *Manager) WaitCaptcha(ctx context.Context, accountID string) (string, error) {
	a, err := m.account(accountID)
	if err != nil {
		return "", err
	}
	request := &CaptchaRequest{ID: randomID(), AccountID: accountID, Provider: a.Provider, ExpiresAt: time.Now().Add(3 * time.Minute).UnixMilli(), result: make(chan string, 1)}
	m.mu.Lock()
	if len(m.challenges) >= 20 {
		m.mu.Unlock()
		return "", errors.New("待验证请求过多，请稍后重试")
	}
	m.challenges[request.ID] = request
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.challenges, request.ID); m.mu.Unlock() }()
	timer := time.NewTimer(3 * time.Minute)
	defer timer.Stop()
	select {
	case token := <-request.result:
		return token, nil
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
		return "", &APIError{Status: 409, Code: "captcha_required", Message: "等待人机验证超时，请在 Credit 页面完成验证后重试"}
	}
}

func (m *Manager) SubmitCaptcha(accountID, requestID, token string) error {
	if strings.TrimSpace(token) == "" || len(token) > 65536 {
		return errors.New("验证码参数为空或过长")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	request := m.challenges[requestID]
	if request == nil || request.AccountID != accountID || request.ExpiresAt <= time.Now().UnixMilli() {
		return errors.New("验证请求已过期或不属于该账号")
	}
	select {
	case request.result <- token:
		return nil
	default:
		return errors.New("此验证请求已提交")
	}
}
