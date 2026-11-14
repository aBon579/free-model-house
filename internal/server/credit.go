package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/aBon579/free-model-house/internal/credit"
	"github.com/aBon579/free-model-house/internal/creditlane"
	"github.com/aBon579/free-model-house/internal/lane"
	"github.com/aBon579/free-model-house/internal/loghub"
)

// 本文件是管理台 **Credit 页**（`/api/credit*`）的后端。
//
// 鉴权：跟其他管理 API 一样吃管理会话 Cookie —— 拦截点在 handleAdmin 里，
// 这里**不放行任何**子路径。页面能看到上游账号的 uid、凭据指纹和余额，
// 所以它只该在 NAS 本机浏览器里可用（理由见 admin.go 的文件头）。
//
// ⚠️ 这一层只做**校验 + 超时 + 错误翻译**，业务全在 internal/credit 里。
//   上游请求的凭据、协议细节、风控边界都不在这里重复一遍。

// Credit 各类操作的超时。
//
// ⚠️ 为什么每张卡都单独限：`credit.JSONRequest` 只限**单次**上游请求（30 秒），
//
//	而一次领取要「拉活动清单 → 逐个领取 → 复查余额」，一次导入要「每个账号
//	都验凭据 + 拉清单」。没有整体上限的话，管理台点一下可能挂住几分钟。
const (
	creditReadTimeout   = 15 * time.Second
	creditCallTimeout   = time.Minute
	creditActionTimeout = 3 * time.Minute
	creditImportTimeout = 5 * time.Minute
	creditBodyLimit     = 4 << 20
)

// SetCredit 装上 Credit 账号管理器。
func (a *Admin) SetCredit(m *credit.Manager) { a.credit = m }

// handleCredit 分发 `/api/credit` 下的所有路径。
func (a *Admin) handleCredit(w http.ResponseWriter, r *http.Request) {
	m := a.credit
	if m == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "Credit 服务未启用 —— 凭据存储不可用，其余功能不受影响",
		})
		return
	}
	switch strings.TrimPrefix(r.URL.Path, "/api/credit") {
	case "":
		if !creditGet(w, r) {
			return
		}
		// 快照只读内存里的账号状态；Qoder 的模型表是本地的，不发上游请求。
		writeJSON(w, http.StatusOK, m.Snapshot())
	case "/accounts":
		var body struct {
			Provider   string            `json:"provider"`
			Name       string            `json:"name"`
			Credential credit.Credential `json:"credential"`
		}
		if !creditPost(w, r, &body) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), creditCallTimeout)
		defer cancel()
		account, err := m.Add(ctx, body.Provider, body.Name, body.Credential)
		if err != nil {
			writeCreditError(w, err)
			return
		}
		a.logCredit("添加 Credit 账户：%s / %s（%s）", account.Provider, account.Name, account.Hint)
		writeJSON(w, http.StatusOK, map[string]any{"account": account})
	case "/accounts/update":
		var body credit.AccountPatch
		if !creditPost(w, r, &body) {
			return
		}
		if err := m.Patch(body); err != nil {
			writeCreditError(w, err)
			return
		}
		// 领取时刻是这项设置里唯一会"自己按时打上游"的字段，改动要留一行日志；
		// 改名、禁用模型都是被动的，不值得占日志。
		if body.AutoClaimAt != nil {
			scheduled := *body.AutoClaimAt
			if scheduled == "" {
				scheduled = "已关闭"
			}
			a.logCredit("自动领取时间：%s", scheduled)
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/accounts/delete":
		var body struct {
			ID string `json:"id"`
		}
		if !creditPost(w, r, &body) {
			return
		}
		if err := m.Delete(body.ID); err != nil {
			writeCreditError(w, err)
			return
		}
		a.logCredit("删除 Credit 账户：%s", body.ID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/accounts/action":
		a.handleCreditAction(w, r, m)
	case "/login":
		a.handleCreditLogin(w, r, m)
	case "/login/complete":
		var body struct {
			ID       string `json:"id"`
			Callback string `json:"callback"`
		}
		if !creditPost(w, r, &body) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), creditCallTimeout)
		defer cancel()
		login, err := m.CompleteLogin(ctx, body.ID, body.Callback)
		if err != nil {
			writeCreditError(w, err)
			return
		}
		a.logCredit("回调授权完成：%s（%s）", login.Provider, login.State)
		writeJSON(w, http.StatusOK, login)
	case "/login/cancel":
		var body struct {
			ID string `json:"id"`
		}
		if !creditPost(w, r, &body) {
			return
		}
		if err := m.CancelLogin(body.ID); err != nil {
			writeCreditError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case "/import":
		var body struct {
			Accounts []credit.BackupAccount `json:"accounts"`
		}
		if !creditPost(w, r, &body) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), creditImportTimeout)
		defer cancel()
		count, err := m.Import(ctx, body.Accounts)
		if err != nil {
			// ⚠️ 导入是**逐个**落盘的，中途失败要说清已经进了几个。
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		a.logCredit("导入 Credit 账户：%d 个", count)
		writeJSON(w, http.StatusOK, map[string]any{"imported": count})
	case "/export":
		var body struct {
			Confirm bool `json:"confirm"`
		}
		if !creditPost(w, r, &body) {
			return
		}
		// ⚠️ 导出的是**完整凭据明文** —— 必须显式确认，且在日志里留痕。
		if !body.Confirm {
			writeJSON(w, http.StatusBadRequest, map[string]any{
				"error": "导出会包含完整凭据，需要 confirm=true",
			})
			return
		}
		accounts := m.Export()
		a.logCredit("导出 Credit 账户凭据：%d 个（文件由用户保存，请自行妥善保管）", len(accounts))
		writeJSON(w, http.StatusOK, map[string]any{
			"format":   "fmh-credit",
			"version":  1,
			"accounts": accounts,
		})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

// handleCreditAction 跑一次显式的账户操作。
func (a *Admin) handleCreditAction(w http.ResponseWriter, r *http.Request, m *credit.Manager) {
	var body struct {
		ID         string `json:"id"`
		Action     string `json:"action"`
		CampaignID string `json:"campaignId"`
		Model      string `json:"model"`
	}
	if !creditPost(w, r, &body) {
		return
	}
	if body.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 id"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), creditActionTimeout)
	defer cancel()

	switch body.Action {
	case "refresh":
		if err := m.Refresh(ctx, body.ID); err != nil {
			writeCreditError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "凭据已刷新"})
	case "models":
		if err := m.RefreshModels(ctx, body.ID); err != nil {
			writeCreditError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "模型清单已刷新"})
	case "credits":
		credits, err := m.Credits(ctx, body.ID)
		if err != nil {
			writeCreditError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "credits": credits})
	case "claim":
		results, err := m.Claim(ctx, body.ID, body.CampaignID)
		kind, until := m.ClaimHold(body.ID)
		hold := map[string]any{"kind": kind, "untilMs": until}
		// ⚠️ 领取失败一律 200 + ok:false，不走 writeCreditError。
		//   上游回的 401 说的是"**你那个账号的凭据**没了"，与管理台会话无关；
		//   原样透传 HTTP 401 会让前端 dispatch('fmh:logout')，把管理员踢下线。
		//   claimHold 同时回给页面：批量签到靠它做"同类错误连续两个号就整批停"。
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error(), "results": results, "claimHold": hold})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": results, "claimHold": hold})
	case "reset":
		if err := m.Reset(body.ID); err != nil {
			writeCreditError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已清除错误与冷却"})
	case "test":
		a.handleCreditTest(w, ctx, m, body.ID, body.Model)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知的操作"})
	}
}

// creditTestMaxTokens 是手动测试的输出上限。
//
// ⚠️ 刻意给得**很小**：这一次请求的存在理由只是"证明这条路能通"，
//
//	让模型写一段长答案只是多烧额度，对判断没有任何帮助。
const creditTestMaxTokens = 64

// handleCreditTest 真的向上游发一次最小推理 —— Credit 页那个「手动测试（耗额度）」。
//
// ⚠️ 这条路径**会花用户自己的额度**，所以它只由显式点击触发：
//
//	周期探测在车道自己的 `AutoProbeable() = false` 那里就被挡在门外了。
//	这个区分不能靠"记得别探测它" —— 靠的是接口上的一个断言。
func (a *Admin) handleCreditTest(w http.ResponseWriter, ctx context.Context, m *credit.Manager, id, model string) {
	l, ok := a.lanes.Get(creditlane.LaneID)
	if !ok {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "message": "Credit 车道没有注册（服务未启用），本次没有发出任何上游请求",
		})
		return
	}
	provider, target, reason := creditTestTarget(m, id, model)
	if reason != "" {
		// ⚠️ 还没打上游就把话说清楚 —— 否则用户以为"测试失败了"，
		//   其实是选错了卡或模型被关掉了。
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "message": reason})
		return
	}

	start := time.Now()
	var first time.Time
	chars := 0
	usage, finish, uerr := l.Do(ctx, &lane.Turn{
		// 车道拿到的是**已剥掉 `credit/` 前缀**的 id（路由统一这么约定）。
		Model:     provider + "/" + target,
		Messages:  []lane.Message{{Role: lane.RoleUser, Parts: []lane.Part{lane.TextPart("ping")}}},
		MaxTokens: creditTestMaxTokens,
		Seed:      "credit-test:" + id,
	}, func(ch lane.Chunk) {
		switch ch.Kind {
		case lane.ChunkTextDelta, lane.ChunkReasonDelta:
			if first.IsZero() {
				first = time.Now()
			}
			chars += len(ch.Delta)
		}
	})

	result := map[string]any{
		"ok":        uerr == nil,
		"model":     creditlane.FormatModelID(provider, target),
		"latencyMs": time.Since(start).Milliseconds(),
		"chars":     chars,
		"finish":    string(finish),
	}
	if !first.IsZero() {
		result["ttftMs"] = first.Sub(start).Milliseconds()
	}
	if usage != nil {
		result["usage"] = usage
	}
	if uerr != nil {
		// ⚠️ 只透出 Code 与 Message —— `lane.Error` 的构造规矩本来就不许带凭据，
		//   这里也不额外把 Status/Scope 那些内部字段塞给浏览器。
		result["code"] = string(uerr.Code)
		result["message"] = uerr.Message
		a.logCredit("手动测试 %s 失败：%s", result["model"].(string), uerr.Message)
	} else {
		result["message"] = "通了"
		a.logCredit("手动测试 %s 成功（%d 字，%d 毫秒）", result["model"].(string), chars, result["latencyMs"])
	}
	writeJSON(w, http.StatusOK, result)
}

// creditTestTarget 挑一个**这张卡上的**模型来测。
//
// ⚠️ 点名要测的模型必须属于**这张卡** —— 顺着账号池去找第一个能用的，
//
//	测的就不是用户点的那张卡了（多卡时尤其会误导：坏卡显示"通了"）。
//	reason 非空表示**没发请求**的理由。
func creditTestTarget(m *credit.Manager, id, model string) (string, string, string) {
	for _, a := range m.Snapshot().Accounts {
		if a.ID != id {
			continue
		}
		off := map[string]bool{}
		for _, disabled := range a.DisabledModels {
			off[disabled] = true
		}
		if model != "" {
			for _, item := range a.Models {
				if item.ID != model {
					continue
				}
				if off[model] {
					return "", "", "这个模型的开关是关着的"
				}
				return a.Provider, model, ""
			}
			return "", "", "这个账号的清单里没有这个模型（可以点「刷新模型」再试）"
		}
		for _, item := range a.Models {
			if off[item.ID] || strings.TrimSpace(item.ID) == "" {
				continue
			}
			return a.Provider, item.ID, ""
		}
		return "", "", "这个账号里没有可测试的模型（全被关掉了？）"
	}
	return "", "", "找不到这个账号（可能已经删除）"
}

// handleCreditLogin 发起或轮询一次官方登录。
func (a *Admin) handleCreditLogin(w http.ResponseWriter, r *http.Request, m *credit.Manager) {
	if r.Method == http.MethodGet {
		id := r.URL.Query().Get("id")
		if id == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 id"})
			return
		}
		login, err := m.Login(id)
		if err != nil {
			writeCreditError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, login)
		return
	}
	var body struct {
		Provider  string `json:"provider"`
		Mode      string `json:"mode"`
		AccountID string `json:"accountId"`
	}
	if !creditPost(w, r, &body) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), creditCallTimeout)
	defer cancel()
	login, err := m.StartLogin(ctx, body.Provider, body.Mode, body.AccountID)
	if err != nil {
		writeCreditError(w, err)
		return
	}
	a.logCredit("发起官方登录：%s（%s）", login.Provider, login.State)
	writeJSON(w, http.StatusOK, login)
}

// creditGet 校验 GET 方法。
func creditGet(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的方法"})
		return false
	}
	return true
}

// creditPost 校验 POST 方法并解出 JSON 请求体（带体积上限）。
func creditPost(w http.ResponseWriter, r *http.Request, target any) bool {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的方法"})
		return false
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, creditBodyLimit)).Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是 JSON"})
		return false
	}
	return true
}

// writeCreditError 把 credit 层的错误翻译成页面能用的形态。
//
// ⚠️ `code` 是上游自己的**机器可读判决**，原样透出而不是只留一句中文：
//
//	TRAE 用它标 `token_expired`（刷一次凭据能救）区别于普通 401（得重新登录）。
//	页面目前按文案走，但这个字段是给"下一步该做什么"用的 —— 吞掉就再也分不出来。
func writeCreditError(w http.ResponseWriter, err error) {
	var ae *credit.APIError
	if errors.As(err, &ae) {
		payload := map[string]any{"error": ae.Message}
		if ae.Code != "" {
			payload["code"] = ae.Code
		}
		status := ae.Status
		if status == 0 {
			status = http.StatusBadGateway
		}
		writeJSON(w, status, payload)
		return
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": "上游响应超时，请稍后重试"})
	case errors.Is(err, context.Canceled):
		writeJSON(w, http.StatusRequestTimeout, map[string]any{"error": "请求已取消"})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
}

// logCredit 把 Credit 页的管理动作记进运行日志。
//
// ⚠️ 只记**渠道、名称、指纹**这类可定位但不敏感的信息，
//
//	凭据与验证码参数一律不进日志。
func (a *Admin) logCredit(format string, args ...any) {
	if a.loghub != nil {
		a.loghub.Log(loghub.CategorySystem, format, args...)
		return
	}
	a.logf(format, args...)
}
