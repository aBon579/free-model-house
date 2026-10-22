package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/aBon579/free-model-house/internal/lane"
	"github.com/aBon579/free-model-house/internal/store"
)

// 本文件是 **管理面**。
//
// 鉴权：管理会话 Cookie。
//
// ⚠️ **从 LAN 访问管理面会失败** —— 因为没有管理会话。
// 这是有意的：管理面能改配置、能看到上游凭据指纹、能轮换 Key，
// 所以它只该在 NAS 本机可用。
//
// 单端口下不需要靠"只监听 127.0.0.1"来隔离 ——
// 靠"这个浏览器没有管理会话"来隔离。

// Admin 是管理面。
type Admin struct {
	keys    *Keys
	lanes   *lane.Registry
	catalog *lane.Catalog
	db      *store.DB
	vault   *store.Vault
	// onConfigChanged 配置变更后的回调（重新拉清单等）。
	onConfigChanged func()
	// prober 是探测调度器（手动探测用）。
	prober *lane.ProbeScheduler
	// throttle 是限流账本（用来显示冷却倒计时）。
	throttle *lane.Book
	// egress 是出口探测器（显示当前出口 IP + 国家）。
	//
	// ⚠️ 免密车道的可用性是**网络路径的属性** ——
	//   上游的原话是"detected abusive traffic from **this IP address**"。
	//   所以面板必须能回答"我现在从哪个 IP 出去"，
	//   否则用户看到"被风控"却不知道该换什么。
	egress *lane.EgressDetector
	// switches 是车道开关（哪条车道开不开）。
	switches *lane.LaneSwitches
	// password 是管理台密码。
	password *Password
	// logger 记后台任务的日志（可选）。
	logger *log.Logger
}

// SetEgress 装上出口探测器。
func (a *Admin) SetEgress(e *lane.EgressDetector) { a.egress = e }

// SetLaneSwitches 装上车道开关。
func (a *Admin) SetLaneSwitches(s *lane.LaneSwitches) { a.switches = s }

// SetPassword 装上管理台密码。
func (a *Admin) SetPassword(p *Password) { a.password = p }

// mustChangePassword 报告当前会话是不是"必须先改密码"。
func (a *Admin) mustChangePassword(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	return a.keys.SessionMustChange(cookie.Value)
}

// handleLaneSwitch 开/关一条车道。
func (a *Admin) handleLaneSwitch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Lane    string `json:"lane"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if body.Lane == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 lane"})
		return
	}
	if a.switches == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "车道开关不可用"})
		return
	}
	// ⚠️ **拒绝关掉最后一条车道** —— 那会让网关变成一块砖。
	if !a.switches.SetEnabled(body.Lane, body.Enabled) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": "不能关掉最后一条车道 —— 那样就没有任何模型可用了",
		})
		return
	}
	a.persistLaneSwitches()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":    true,
		"lanes": a.switches.Snapshot(),
	})
}

// persistLaneSwitches 把车道开关写进数据库。
func (a *Admin) persistLaneSwitches() {
	if a.switches == nil || a.db == nil {
		return
	}
	if err := a.db.SetSetting(store.SettingLaneEnabled, a.switches.Encode()); err != nil {
		a.logf("存车道开关失败: %v", err)
	}
}

// SetLogger 装上日志器。
func (a *Admin) SetLogger(l *log.Logger) { a.logger = l }

// logf 记一条日志。
func (a *Admin) logf(format string, args ...any) {
	if a.logger == nil {
		return
	}
	a.logger.Printf(format, args...)
}

// SetProber 装上探测调度器。
func (a *Admin) SetProber(p *lane.ProbeScheduler) { a.prober = p }

// SetThrottle 装上限流账本。
func (a *Admin) SetThrottle(b *lane.Book) { a.throttle = b }

// NewAdmin 建一个管理面。
func NewAdmin(keys *Keys, lanes *lane.Registry, catalog *lane.Catalog, db *store.DB, vault *store.Vault) *Admin {
	return &Admin{
		keys:    keys,
		lanes:   lanes,
		catalog: catalog,
		db:      db,
		vault:   vault,
	}
}

// SetConfigHook 设置配置变更回调。
func (a *Admin) SetConfigHook(fn func()) { a.onConfigChanged = fn }

// handleAdmin 分发管理 API。
func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	a := s.admin
	if a == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "管理面未启用"})
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api")

	// 登录相关不需要会话。
	switch path {
	case "/login":
		a.handleLogin(w, r)
		return
	case "/bootstrap":
		a.handleBootstrap(w, r)
		return
	case "/change-password":
		// ⚠️ 这个也要会话 —— 否则别人能改你的密码。
		//   但它**不能**在"必须改密码"的状态下被拦掉
		//   （那正是用户要改密码的时候）。
		if !a.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "未登录"})
			return
		}
		a.handleChangePassword(w, r)
		return
	}

	// 其余都要会话。
	if !a.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "未登录",
			"hint":  "管理面只能在 NAS 本机浏览器访问",
		})
		return
	}

	switch path {
	case "/status":
		a.handleStatus(w, r)
	case "/models":
		a.handleModels(w, r)
	case "/keys":
		a.handleKeys(w, r)
	case "/keys/rotate":
		a.handleRotate(w, r)
	case "/secrets":
		a.handleSecrets(w, r)
	case "/usage":
		a.handleUsage(w, r)
	case "/logout":
		a.handleLogout(w, r)
	case "/probes":
		a.handleProbes(w, r)
	case "/probes/run":
		a.handleRunProbes(w, r)
	case "/lanes":
		a.handleLanes(w, r)
	case "/lanes/switch":
		a.handleLaneSwitch(w, r)
	case "/settings":
		a.handleSettings(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

// authorized 校验管理会话。
func (a *Admin) authorized(r *http.Request) bool {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return false
	}
	return a.keys.ValidSession(cookie.Value)
}

// sessionCookieName 是会话 Cookie 的名字。
const sessionCookieName = "fmh_session"

// handleLogin 用密码登录。
//
// ⚠️ **用初始密码 `admin` 也能登录** ——
//
//	但会话会被标记成 `mustChangePassword`，
//	管理台会立刻弹「设置新密码」。
//	直接拒绝登录的话，用户会一头雾水。
func (a *Admin) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是 JSON"})
		return
	}
	ok, mustChange := a.password.Verify(body.Password)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "密码不对",
		})
		return
	}
	session := a.keys.NewSession("local")
	a.keys.SetSessionMustChange(session, mustChange)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    session,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		// ⚠️ 不设 Secure —— NAS 的 Web UI 常常是 http://192.168.x.x，
		// 设了 Secure 会导致 Cookie 被浏览器丢弃。
		MaxAge: int((8 * time.Hour).Seconds()),
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                 true,
		"mustChangePassword": mustChange,
	})
}

// handleChangePassword 修改管理台密码。
//
// ⚠️ 改完**作废所有会话** ——
//
//	密码变了，持有旧会话的人不该还能进管理面。
//	包括他自己 —— 所以前端要引导他重新登录。
func (a *Admin) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	var body struct {
		Current string `json:"current"`
		New     string `json:"new"`
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是 JSON"})
		return
	}
	// ⚠️ **先验当前密码** —— 别人借了你的浏览器不能改密码。
	ok, _ := a.password.Verify(body.Current)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "当前密码不对"})
		return
	}
	if err := a.password.Set(body.New, body.Confirm); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	// ⚠️ **作废所有会话** —— 密码变了，旧会话全部失效。
	a.keys.InvalidateAllSessionsExcept("")
	// 让当前浏览器也退出（Cookie 也要清）。
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"note": "密码已修改，所有管理会话已失效 —— 请用新密码重新登录",
	})
}

// handleBootstrap 兑换一次性令牌。
func (a *Admin) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	session, ok := a.keys.RedeemBootstrap(token)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "令牌无效或已过期",
		})
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    session,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((8 * time.Hour).Seconds()),
	})
	// 重定向到管理台首页（清掉地址栏里的 token）。
	http.Redirect(w, r, "/", http.StatusFound)
}

// handleLogout 注销。
func (a *Admin) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		a.keys.InvalidateSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/", HttpOnly: true, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleStatus 返回总体状态。
func (a *Admin) handleStatus(w http.ResponseWriter, r *http.Request) {
	lanes := []any{}
	for _, l := range a.lanes.All() {
		st := l.Status()
		// ⚠️ **车道开关** —— 前端要渲染开关，所以开关状态随车道一起发。
		enabled := true
		if a.switches != nil {
			enabled = a.switches.Enabled(st.ID)
		}
		lanes = append(lanes, map[string]any{
			"id":          st.ID,
			"name":        st.Name,
			"ready":       st.Ready,
			"enabled":     enabled,
			"modelCount":  st.ModelCount,
			"lastRefresh": st.LastRefresh,
			"lastError":   st.LastError,
			"reason":      st.Reason,
			// ⚠️ **这条车道参与自动探测吗** ——
			//   前端要靠它解释"为什么这批模型是「未探测」"。
			"autoProbe":       st.AutoProbe,
			"autoProbeReason": st.AutoProbeReason,
			"needsCredential": st.NeedsCredential,
			"credentialHint":  st.CredentialHint,
		})
	}
	models := a.catalog.Models()
	free := 0
	for _, m := range models {
		if m.Free {
			free++
		}
	}
	// ⚠️ **"可用模型"= 探测验证过可用的** ——
	//   不是"清单里标记免费的"。
	//   免密车道的"免费"是上游的承诺，"可用"是实测的结果 ——
	//   两者会差很远（被风控、被地区封锁的都不算可用）。
	available := 0
	for _, m := range models {
		if p := a.catalog.Probe(m.ID); p.State == lane.StateAvailable {
			available++
		}
	}
	total, _ := a.db.Total("2000-01-01")

	// ⚠️ **出口信息** —— 面板要能回答"我现在从哪个 IP 出去"。
	//   免密车道的"被风控"是**出口**的属性，
	//   不知道出口的话，用户看到那个红字也不知道该换什么。
	out := map[string]any{
		"version":         Version,
		"lanes":           lanes,
		"modelCount":      len(models),
		"freeModels":      free,
		"availableModels": available,
		"lanKeyHint":      store.CredentialHint(a.keys.LanKey()),
		"usage":           total,
		// ⚠️ **必须改密码** —— 前端要弹"设置新密码"。
		"mustChangePassword": a.mustChangePassword(r),
	}
	if a.egress != nil {
		e := a.egress.Last()
		out["egress"] = map[string]any{
			"ip":      e.IP,
			"country": e.Country,
			"at":      e.At,
			"err":     e.Err,
		}
	}
	// ⚠️ **车道级封锁** —— 全被拒 = 出口问题，不是模型问题。
	out["laneLocks"] = a.laneLocks()
	// ⚠️ **车道开关** —— 哪条车道开不开。
	if a.switches != nil {
		out["laneSwitches"] = a.switches.Snapshot()
	}
	writeJSON(w, http.StatusOK, out)
}

// laneLocks 报告每条车道的封锁状态。
func (a *Admin) laneLocks() map[string]any {
	out := map[string]any{}
	for _, l := range a.lanes.All() {
		lock := a.catalog.LaneLockOf(l.ID())
		out[l.ID()] = map[string]any{
			"locked": lock.Locked,
			"reason": lock.Reason,
			"state":  string(lock.State),
			"count":  lock.Count,
			"total":  lock.Total,
		}
	}
	return out
}

// handleModels 返回模型清单（管理视角，比 /v1/models 更详细）。
func (a *Admin) handleModels(w http.ResponseWriter, r *http.Request) {
	models := a.catalog.Models()
	lane.SortModels(models)
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		entry := map[string]any{
			// ⚠️ `m.ID` **已经带车道前缀**了 —— 别再拼一次。
			"id":         m.ID,
			"lane":       m.Lane,
			"name":       m.Name,
			"free":       m.Free,
			"enabled":    m.Enabled,
			"wire":       m.Wire,
			"reasoning":  m.Reasoning,
			"vision":     m.Vision,
			"audio":      m.AudioInput,
			"file":       m.FileInput,
			"context":    m.ContextWindow,
			"maxOutput":  m.MaxOutput,
			"capability": string(m.Capability),
			"efforts":    m.Efforts,
			"state":      m.State,
			"ttftMs":     m.TTFTMs,
		}
		if m.CanDisableThinking {
			entry["canDisableThinking"] = true
		}
		// ⚠️ **冷却倒计时** —— 让用户知道"这个模型还有几秒才能用"，
		// 而不是只看到一个干巴巴的"限流"。
		if a.throttle != nil {
			if remaining := a.throttle.CooldownRemaining(m.ID); remaining > 0 {
				entry["cooldownSec"] = remaining
				if eta, ok := a.throttle.RecoveryETA(m.ID); ok {
					entry["recoveryAt"] = eta
				}
			}
		}
		// 探测结论的细节（"为什么不可用"）。
		if p := a.catalog.Probe(m.ID); p.Detail != "" {
			entry["probeDetail"] = p.Detail
		}
		// ⚠️ **上次探测时间** —— 手动和自动都记。
		//   用户靠它判断"这个结论还新不新"（TTL 是 10 分钟）。
		if m.ProbedAt > 0 {
			entry["probedAt"] = m.ProbedAt
		}
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

// handleKeys 返回 Key 状态。
//
// ⚠️ **默认不回显完整 Key** —— 只给指纹。要看完整 Key 必须显式 `?reveal=1`。
//
// 这是有意的：管理台的截图、录屏、共享屏幕都会带上回显出来的 Key。
//
// ⚠️ **只有一把推理 Key 了** ——
//
//	本机 Key 已砍掉（管理台改用密码登录）。
//	现在**局域网 Key 就是唯一**的推理凭据。
func (a *Admin) handleKeys(w http.ResponseWriter, r *http.Request) {
	reveal := r.URL.Query().Get("reveal") == "1"
	lan := ""
	if reveal {
		lan = a.keys.LanKey()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"prefix": KeyPrefix,
		"lan": map[string]any{
			"name":  LanKeyName,
			"value": lan,
			"hint":  store.CredentialHint(a.keys.LanKey()),
		},
		"revealed": reveal,
	})
}

// handleRotate 轮换局域网 Key。
func (a *Admin) handleRotate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	var body struct {
		Which string `json:"which"` // 只剩 "lan"
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是 JSON"})
		return
	}
	switch body.Which {
	case "lan":
		key := a.keys.RotateLanKey()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "which": "lan", "value": key,
			"hint": store.CredentialHint(key),
			"note": "局域网 Key 已轮换，旧 Key 立即失效；管理会话不受影响",
		})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "which 必须是 lan"})
	}
}

// handleSecrets 管理上游凭据。
//
// ⚠️ **只接受写入，永不回显**。
// 上游 Key 一旦回显到浏览器，就等于抄进了剪贴板/截图。
func (a *Admin) handleSecrets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// 列出有哪些凭据（只有名字与指纹）。
		out := []any{}
		for _, name := range []string{store.SettingSecretOpenRouter} {
			_, hint, found, _ := a.db.GetSecret(name)
			if found {
				out = append(out, map[string]any{
					"name": name, "hint": hint, "set": true,
				})
			} else {
				out = append(out, map[string]any{
					"name": name, "hint": "", "set": false,
				})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"secrets": out})
	case http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是 JSON"})
			return
		}
		if body.Name != store.SettingSecretOpenRouter {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "未知的凭据名"})
			return
		}
		value := strings.TrimSpace(body.Value)
		if value == "" {
			// 空 = 删除。
			a.db.DeleteSecret(body.Name)
			a.applySecrets()
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": true})
			return
		}
		sealed, err := a.vault.Seal(value)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": "加密失败: " + err.Error(),
			})
			return
		}
		if err := a.db.PutSecret(body.Name, sealed, store.CredentialHint(value)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"error": "保存失败: " + err.Error(),
			})
			return
		}
		a.applySecrets()
		// ⚠️ **立刻查一次额度** —— 用户刚配好 Key，最想看的就是"我还剩多少"，
		//   等 30 分钟的定时刷新太慢了。
		//   而且这一步还能**验证 Key 对不对** —— Key 错了这里就会报错。
		if or, ok := a.lanes.Get(lane.OpenRouterLaneID); ok {
			if orl, ok := or.(*lane.OpenRouterLane); ok && orl.Ready() {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					if _, err := orl.FetchQuota(ctx); err != nil {
						a.logf("查 OpenRouter 额度失败: %s", err.Message)
					}
				}()
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "hint": store.CredentialHint(value),
		})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的方法"})
	}
}

// applySecrets 把存储里的凭据应用到车道上。
func (a *Admin) applySecrets() {
	for _, l := range a.lanes.All() {
		if or, ok := l.(*lane.OpenRouterLane); ok {
			sealed, _, found, _ := a.db.GetSecret(store.SettingSecretOpenRouter)
			if !found {
				or.SetAPIKey("")
				continue
			}
			value, err := a.vault.Open(sealed)
			if err != nil {
				// ⚠️ 解不开 —— 通常是密钥文件丢了。
				or.SetAPIKey("")
				continue
			}
			or.SetAPIKey(value)
		}
	}
	if a.onConfigChanged != nil {
		a.onConfigChanged()
	}
}

// handleUsage 返回用量统计。
func (a *Admin) handleUsage(w http.ResponseWriter, r *http.Request) {
	days := r.URL.Query().Get("days")
	if days == "" {
		days = "30"
	}
	from := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	daily, _ := a.db.DailyUsageRange(from, "2099-01-01")
	byModel, _ := a.db.ModelUsageAll(from)
	total, _ := a.db.Total(from)
	writeJSON(w, http.StatusOK, map[string]any{
		"daily":   daily,
		"byModel": byModel,
		"total":   total,
	})
}

// handleProbes 返回探测结论。
func (a *Admin) handleProbes(w http.ResponseWriter, r *http.Request) {
	rows, _ := a.db.LoadProbes()
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, map[string]any{
			"model":    row.Model,
			"state":    row.State,
			"detail":   row.Detail,
			"latency":  row.Latency,
			"ttft":     row.TTFT,
			"probedAt": row.ProbedAt,
		})
	}
	// 加上内存里的（比数据库新的）。
	summary := a.catalog.SummarizeProbes()
	out2 := map[string]any{
		"probes": out,
		"summary": map[string]any{
			"probed":    summary.Probed,
			"available": summary.Available,
			"failed":    summary.Failed,
			"unknown":   summary.Unknown,
		},
	}
	// ⚠️ **探测进度** ——
	//   整轮探测是**异步**的（可能几十秒）。
	//   前端要靠它显示"正在探测 12/27"，
	//   而不是点完就弹"探测完成"（那是**假的**）。
	if a.prober != nil {
		p := a.prober.Progress()
		out2["progress"] = map[string]any{
			"running":    p.Running,
			"finalizing": p.Finalizing,
			"total":      p.Total,
			"done":       p.Done,
			"round":      p.Round,
			"lastRunAt":  p.LastRunAt,
			// ⚠️ **上轮的共性失败原因** ——
			//   "探测完成但全是未探测" 要能说清为什么。
			//   少了这个，用户只能对着"未探测"猜。
			"lastFailure":      p.LastFailure,
			"lastFailureCount": p.LastFailureCount,
			// ⚠️ **探通了几个** ——
			//   只说"8 个失败"会让人以为全挂了。
			"lastAvailable": p.LastAvailable,
		}
	}
	writeJSON(w, http.StatusOK, out2)
}

// handleRunProbes 手动触发一轮探测。
//
// ⚠️ **这会消耗上游额度** —— 每探一个模型就是一次真实推理。
// 所以 UI 上的按钮要写明这一点。
func (a *Admin) handleRunProbes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "只支持 POST"})
		return
	}
	if a.prober == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "探测调度器未就绪",
		})
		return
	}

	// 可选：指定要探的模型。
	var body struct {
		Model string `json:"model"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	// ⚠️ **单模型探测是同步的** —— 用户点了按钮就该等结果（通常几秒）。
	if body.Model != "" {
		result := a.prober.ProbeOne(r.Context(), body.Model)
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     true,
			"result": probeToMap(result),
		})
		return
	}

	// 整轮探测是**异步**的 —— 全量探测可能要几十秒，不能让 HTTP 请求挂在那儿。
	//
	// ⚠️ **不能用 `r.Context()`** —— 那个 context 在**响应写完时就被取消了**，
	//   探测 goroutine 会立刻死掉，一个模型都探不到。
	//   `context.WithoutCancel` 保留它的值（超时等），但**去掉取消信号**。
	//
	// ⚠️ **手动探测不受自动探测开关限制** —— 用户点按钮是明确意图。
	go a.prober.RunOnceManual(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok":    true,
		"note":  "探测已在后台启动 —— 这会消耗上游额度",
		"async": true,
	})
}

// probeToMap 把探测结论转成 JSON。
func probeToMap(r lane.ProbeResult) map[string]any {
	return map[string]any{
		"model":    r.Model,
		"state":    string(r.State),
		"detail":   r.Detail,
		"latency":  r.LatencyMs,
		"ttft":     r.TTFTMs,
		"probedAt": r.At,
	}
}

// handleLanes 返回车道详情。
func (a *Admin) handleLanes(w http.ResponseWriter, r *http.Request) {
	lanes := []any{}
	for _, l := range a.lanes.All() {
		st := l.Status()
		entry := map[string]any{
			"id":         st.ID,
			"name":       st.Name,
			"ready":      st.Ready,
			"modelCount": st.ModelCount,
		}
		if or, ok := l.(*lane.OpenRouterLane); ok {
			if q := or.Quota(); q != nil {
				entry["quota"] = q
			}
		}
		lanes = append(lanes, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"lanes": lanes})
}

// handleSettings 读写设置。
func (a *Admin) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		// ⚠️ 旧数据里只有 `lan_enabled` 布尔值 —— 迁移成档位。
		tier := a.lanTier()
		// ⚠️ 限额用**档位的默认值**，除非用户自己改过。
		defPerMin, defConc := TierLimits(tier)
		writeJSON(w, http.StatusOK, map[string]any{
			// ⚠️ `lanEnabled` 保留是为了兼容旧前端 —— 真正的档位在 `lanTier`。
			"lanEnabled":       true,
			"lanTier":          string(tier),
			"lanTiers":         tierLabels(),
			"lanPerMinute":     a.db.GetInt(store.SettingLanPerMinute, defPerMin),
			"lanMaxConcurrent": a.db.GetInt(store.SettingLanMaxConcurrent, defConc),
			// ⚠️ 字段名从 `crossLaneFallback` 改成 `autoModelSwitch` ——
			//   语义已经是**模型级别**的（"自动换个模型试试"），
			//   旧名字说的是"换车道"，容易误解。
			// ⚠️ 旧字段仍然返回（标记为已废弃），让旧前端不至于报错。
			"autoModelSwitch":   a.db.GetAutoModelSwitch(false),
			"crossLaneFallback": a.db.GetAutoModelSwitch(false),
			"probeEnabled":      a.db.GetBool(store.SettingProbeEnabled, true),
			"probeIntervalMin":  a.db.GetInt(store.SettingProbeIntervalMin, 15),
			// ⚠️ **没有 probeBatchSize** —— 探测是**全量**的。
			//   每 15 分钟把每个免费模型都探一遍。
			"adaptiveProbe": a.db.GetBool(store.SettingAdaptiveProbe, true),
			// ⚠️ **没有 conformanceIntervalMin** —— 合规检查功能已砍掉。
		})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的方法"})
		return
	}
	var body struct {
		LanEnabled       *bool   `json:"lanEnabled"`
		LanTier          *string `json:"lanTier"`
		LanPerMinute     *int    `json:"lanPerMinute"`
		LanMaxConcurrent *int    `json:"lanMaxConcurrent"`
		AutoModelSwitch  *bool   `json:"autoModelSwitch"`
		// ⚠️ **旧字段，保留只为兼容旧前端** —— 新前端用 `autoModelSwitch`。
		CrossLaneFallback *bool `json:"crossLaneFallback"`
		ProbeEnabled      *bool `json:"probeEnabled"`
		ProbeIntervalMin  *int  `json:"probeIntervalMin"`
		// ⚠️ **没有 ProbeBatchSize** —— 探测是**全量**的。
		// ⚠️ **没有 ConformanceIntervalMin** —— 合规检查已砍掉。
		AdaptiveProbe *bool `json:"adaptiveProbe"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是 JSON"})
		return
	}
	// ⚠️ **档位优先** —— 前端还在用 `lanEnabled` 时也要能落到正确的档位。
	if body.LanTier != nil {
		tier := ParseLanTier(*body.LanTier)
		a.db.SetSetting(store.SettingLanTier, string(tier))
		a.db.SetSetting(store.SettingLanEnabled, "1")
		// ⚠️ **换档位就重置限额** —— 否则上一档的数值会带过来
		//   （比如从 deep 切到 light，却还留着"不限量"）。
		if body.LanPerMinute == nil {
			defPerMin, _ := TierLimits(tier)
			a.db.SetSetting(store.SettingLanPerMinute, itoa(defPerMin))
		}
		if body.LanMaxConcurrent == nil {
			_, defConc := TierLimits(tier)
			a.db.SetSetting(store.SettingLanMaxConcurrent, itoa(defConc))
		}
	} else if body.LanEnabled != nil {
		// ⚠️ 旧前端的布尔开关 —— true 是"全开"，false 是"最紧的一档"。
		tier := LanTierLight
		if *body.LanEnabled {
			tier = LanTierDeep
		}
		a.db.SetSetting(store.SettingLanTier, string(tier))
		a.db.SetSetting(store.SettingLanEnabled, boolToStr(*body.LanEnabled))
	}
	if body.LanPerMinute != nil {
		a.db.SetSetting(store.SettingLanPerMinute, itoa(*body.LanPerMinute))
	}
	if body.LanMaxConcurrent != nil {
		a.db.SetSetting(store.SettingLanMaxConcurrent, itoa(*body.LanMaxConcurrent))
	}
	// ⚠️ **新字段优先** —— 旧前端还在发 `crossLaneFallback`，
	//   但只要新字段也给了，就以新字段为准。
	if body.AutoModelSwitch != nil {
		a.db.SetSetting(store.SettingAutoModelSwitch, boolToStr(*body.AutoModelSwitch))
	} else if body.CrossLaneFallback != nil {
		a.db.SetSetting(store.SettingAutoModelSwitch, boolToStr(*body.CrossLaneFallback))
	}
	if body.ProbeEnabled != nil {
		a.db.SetSetting(store.SettingProbeEnabled, boolToStr(*body.ProbeEnabled))
	}
	if body.ProbeIntervalMin != nil {
		a.db.SetSetting(store.SettingProbeIntervalMin, itoa(*body.ProbeIntervalMin))
	}
	// ⚠️ **没有 probeBatchSize** —— 探测是**全量**的。
	if body.AdaptiveProbe != nil {
		a.db.SetSetting(store.SettingAdaptiveProbe, boolToStr(*body.AdaptiveProbe))
	}
	// ⚠️ **没有 conformanceIntervalMin** —— 合规检查已砍掉。
	if a.onConfigChanged != nil {
		a.onConfigChanged()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// boolToStr 把 bool 变成存储用的字符串。
func boolToStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// lanTier 读当前档位，并**兼容旧数据**。
//
// ⚠️ 旧版本只存了 `lan_enabled` 布尔值：
//
//	true → `deep`（全开），false → `light`（最紧的一档）。
//	没有 `lan_tier` 的老数据库要能平滑升级。
func (a *Admin) lanTier() LanTier {
	if raw, ok, err := a.db.GetSetting(store.SettingLanTier); err == nil && ok && raw != "" {
		return ParseLanTier(raw)
	}
	// 回退到旧开关。
	if a.db.GetBool(store.SettingLanEnabled, true) {
		return LanTierDeep
	}
	return LanTierLight
}

// tierLabels 返回档位的中文说明（给前端渲染）。
//
// ⚠️ **不含"关闭"档** —— 用户明确要求去掉"局域网内 Agent 完全用不了"。
func tierLabels() []map[string]string {
	return []map[string]string{
		{"value": string(LanTierLight), "label": "轻量（Light）", "hint": "少量调用 —— 10 次/分钟、2 并发。给偶尔查一下的 agent"},
		{"value": string(LanTierStandard), "label": "标准（Standard）", "hint": "正常使用 —— 60 次/分钟、6 并发。家里的主力档"},
		{"value": string(LanTierDeep), "label": "深度（Deep）", "hint": "不限量 —— 自己独占，或出口很干净时用"},
	}
}
