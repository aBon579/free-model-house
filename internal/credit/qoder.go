package credit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aBon579/free-model-house/internal/credit/qoderwasm"
)

type qoderAdapter struct {
	client   *http.Client
	china    bool
	api      string
	auth     string
	host     string
	clientID string
}

func NewQoder(client *http.Client, china bool) Adapter {
	q := &qoderAdapter{client: client, china: china, api: "https://openapi.qoder.sh", auth: "https://qoder.com", host: "https://api2.qoder.sh", clientID: "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"}
	if china {
		q.api = "https://openapi.qoder.com.cn"
		q.auth = "https://qoder.cn"
		q.host = "https://gateway.qoder.com.cn"
		q.clientID = "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa"
	}
	return q
}

func randomID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	s := hex.EncodeToString(b)
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

func (q *qoderAdapter) StartLogin(ctx context.Context, mode string) (*Login, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	hash := sha256.Sum256([]byte(verifier))
	nonce, machine := randomID(), randomID()
	params := url.Values{"challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "challenge_method": {"S256"}, "nonce": {nonce}, "machine_id": {machine}, "client_id": {q.clientID}}
	return &Login{URL: q.auth + "/device/selectAccounts?" + params.Encode(), State: "pending", ExpiresAt: time.Now().Add(5 * time.Minute).UnixMilli(), Interval: 2, Data: Credential{"verifier": verifier, "nonce": nonce, "machineId": machine}}, nil
}

func (q *qoderAdapter) PollLogin(ctx context.Context, login *Login, callback string) (Credential, bool, error) {
	params := url.Values{"nonce": {login.Data.Text("nonce")}, "verifier": {login.Data.Text("verifier")}, "challenge_method": {"S256"}}
	var data map[string]any
	err := JSONRequest(ctx, q.client, http.MethodGet, q.api+"/api/v1/deviceToken/poll?"+params.Encode(), nil, nil, &data)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == 404 {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	token := Text(data["token"])
	if token == "" {
		return nil, false, nil
	}
	c := Credential{"accessToken": token, "refreshToken": Text(data["refresh_token"]), "uid": Text(data["user_id"]), "name": Text(data["user_name"]), "machineId": login.Data.Text("machineId"), "expiresAt": Timestamp(data["expires_at"]), "refreshExpiresAt": Timestamp(data["refresh_token_expires_at"])}
	var user map[string]any
	if JSONRequest(ctx, q.client, http.MethodGet, q.api+"/api/v1/userinfo", q.headers(c), nil, &user) == nil {
		if name := Text(user["name"]); name != "" {
			c["name"] = name
		}
		if c.Text("uid") == "" {
			c["uid"] = Text(user["id"])
		}
	}
	return c, true, q.Validate(c)
}

func (q *qoderAdapter) Validate(c Credential) error {
	if c.Text("accessToken", "access_token", "security_oauth_token", "token") == "" {
		return errors.New("Qoder 凭据缺少 accessToken")
	}
	if c.Text("uid", "user_id", "userId") == "" {
		return errors.New("Qoder 凭据缺少 uid")
	}
	if c.Text("machineId", "machine_id") == "" {
		return errors.New("Qoder 凭据缺少授权时的 machineId")
	}
	return nil
}

func (q *qoderAdapter) headers(c Credential) map[string]string {
	h := map[string]string{"Authorization": "Bearer " + c.Text("accessToken", "access_token", "security_oauth_token", "token"), "User-Agent": "Qoder", "Cosy-ClientType": "10"}
	if c.Text("machineToken") != "" && c.Text("machineType") != "" {
		h["Cosy-MachineToken"] = c.Text("machineToken")
		h["Cosy-MachineType"] = c.Text("machineType")
	}
	return h
}

func (q *qoderAdapter) Refresh(ctx context.Context, c Credential) (Credential, error) {
	refresh := c.Text("refreshToken", "refresh_token")
	if refresh == "" {
		return nil, errors.New("该凭据没有 refreshToken，请重新授权")
	}
	var data map[string]any
	err := JSONRequest(ctx, q.client, http.MethodPost, q.api+"/api/v1/deviceToken/refresh", map[string]string{"User-Agent": "qoder/1.0.0"}, map[string]any{"refresh_token": refresh, "machine_id": c.Text("machineId", "machine_id")}, &data)
	if err != nil {
		return nil, err
	}
	token := Credential(data).Text("token", "device_token", "access_token")
	if token == "" {
		return nil, errors.New("续期响应没有返回新凭据，请重新授权")
	}
	out := c.Clone()
	out["accessToken"] = token
	if s := Text(data["refresh_token"]); s != "" {
		out["refreshToken"] = s
	}
	if expiry := Timestamp(data["expires_at"]); expiry > 0 {
		out["expiresAt"] = expiry
	}
	return out, nil
}

func (q *qoderAdapter) Models(ctx context.Context, c Credential) ([]Model, error) {
	type entry struct {
		id, name string
		rate     float64
		efforts  string
	}
	entries := []entry{
		{"auto", "Auto", .5, ""}, {"qmodel_38max", "Qwen3.8-Max", .5, "none,low,medium,xhigh"}, {"qfmodel", "Qwen3.8-Flash", 0, "none,low,medium,xhigh"},
		{"qmodel_latest", "Qwen3.7-Max", .5, "none"}, {"qmodel", "Qwen3.7-Plus", .1, "none"}, {"kmodel_latest", "Kimi-K3", 1.4, "low,high,max"}, {"kmodel", "Kimi-K2.8-Preview", .8, "low,high,max"},
		{"gmodel", "GLM-5.3", .8, "low,high,max"}, {"gfmodel", "GLM-5.3-Flash", .1, "high,max"}, {"dmodel", "DeepSeek-V4-Pro", .5, "none,high,max"}, {"dfmodel", "DeepSeek-Flash", .1, "none,low,high,max"}, {"mmodel", "MiniMax-M3", .2, ""},
	}
	if q.china {
		entries = append(entries, entry{"q37fmodel", "Qwen3.7-Flash", .1, ""}, entry{"gm51model", "GLM-5.2", .6, "none,high,max"})
	} else {
		entries = append(entries, entry{"ultimate", "Ultimate", 2, "none,low,medium,high,xhigh,max"}, entry{"performance", "Performance", 1.1, "none,low,medium,high,xhigh,max"}, entry{"efficient", "Efficient", .3, ""}, entry{"smodel", "Sonus", 8, "low,medium,high,xhigh,max"}, entry{"cmodel", "Cantus", 4, "low,medium,high,xhigh,max"})
	}
	hour := time.Now().In(time.FixedZone("UTC+8", 8*3600)).Hour()
	models := make([]Model, 0, len(entries))
	for _, e := range entries {
		m := Model{ID: e.id, Name: e.name, ContextWindow: 1000000, Vision: true, Reasoning: true}
		if e.efforts != "" {
			m.Efforts = strings.Split(e.efforts, ",")
		}
		if e.id == "auto" {
			m.ContextWindow = 200000
			m.Reasoning = q.china
		}
		if e.id == "mmodel" {
			m.Reasoning = false
			if q.china {
				m.Name = "MiniMax-M2.7"
				m.Vision = false
				m.ContextWindow = 200000
			}
		}
		if e.id == "performance" || e.id == "efficient" || e.id == "kmodel_latest" {
			m.Reasoning = false
		}
		if (!q.china && (e.id == "qmodel" || e.id == "qmodel_latest" || e.id == "kmodel")) || (q.china && e.id == "dfmodel") {
			m.Reasoning = false
		}
		price := e.rate
		if hour >= 22 || hour < 8 {
			switch e.id {
			case "qmodel_38max", "qmodel":
				price *= .4
			case "qmodel_latest":
				price *= .2
			}
		}
		m.Multiplier = &price
		models = append(models, m)
	}
	return models, nil
}

func (q *qoderAdapter) Credits(ctx context.Context, c Credential) (Credits, error) {
	out := Credits{Balances: []Balance{}, Campaigns: []Campaign{}, UpdatedAt: time.Now().UnixMilli()}
	var usage, campaigns map[string]any
	uerr := JSONRequest(ctx, q.client, http.MethodGet, q.api+"/sash/api/v2/me/usage", q.headers(c), nil, &usage)
	if uerr == nil {
		u := Object(usage["qoderUsage"])
		add := func(name string, v any) {
			p := Object(v)
			b := Balance{Name: name, Total: Number(p["total"]), Used: Number(p["used"]), Remaining: Number(p["remaining"]), Unit: Text(p["unit"]), ExpiresAt: Timestamp(p["expiresAt"])}
			if b.Unit == "" {
				b.Unit = "credits"
			}
			if b.Remaining == nil && b.Total != nil && b.Used != nil {
				x := *b.Total - *b.Used
				b.Remaining = &x
			}
			if b.Total != nil || b.Remaining != nil {
				out.Balances = append(out.Balances, b)
			}
		}
		add("套餐额度", u["userQuota"])
		add("资源包", u["addOnQuota"])
		for _, v := range Array(u["dedicatedResourcePackages"]) {
			p := Object(v)
			name := Text(p["name"])
			if name == "" {
				name = "专用资源包"
			}
			add(name, p)
		}
		if len(out.Balances) == 0 {
			out.Warning = "上游未提供可用的额度数字（企业账号或尚未开通），不代表余额为零"
		}
	} else {
		out.Warning = "额度查询失败：" + uerr.Error()
	}
	cerr := JSONRequest(ctx, q.client, http.MethodGet, q.api+"/sash/api/v1/me/campaigns", q.headers(c), nil, &campaigns)
	if cerr == nil {
		for _, v := range Array(campaigns["campaigns"]) {
			p := Object(v)
			if Text(p["actionType"]) != "CLAIM_BENEFIT" {
				continue
			}
			id := Text(p["campaignId"])
			if id == "" {
				continue
			}
			name := Credential(p).Text("name", "campaignKey")
			if name == "" {
				name = "积分活动"
			}
			out.Campaigns = append(out.Campaigns, Campaign{ID: id, Name: name, Amount: Number(Object(p["benefit"])["amount"]), Claimable: Text(p["claimStatus"]) == "CLAIMABLE", Claimed: Text(p["claimStatus"]) == "CLAIMED", ExpiresAt: Timestamp(p["expiresAt"])})
		}
	} else {
		out.Warning = strings.TrimSpace(out.Warning + " 活动查询失败：" + cerr.Error())
	}
	if c.Text("machineToken") == "" || c.Text("machineType") == "" {
		out.Warning = strings.TrimSpace(out.Warning + " 缺少官方设备身份 machineToken / machineType：上游因此不会下发「每日签到」，活动清单看着是空的。请在 Credit 页「设备身份」里导入（由装有 Qoder 客户端的电脑生成，网关自己读不到桌面账号）。")
	}
	if uerr != nil && cerr != nil {
		return out, uerr
	}
	return out, nil
}

func (q *qoderAdapter) Claim(ctx context.Context, c Credential, id string) (ClaimResult, error) {
	out := ClaimResult{ID: id, Name: "积分活动"}
	if id == "" {
		return out, errors.New("请选择具体活动")
	}
	if c.Text("machineToken") == "" || c.Text("machineType") == "" {
		return out, errors.New("这台网关还缺官方设备身份，请先在 Credit 页「设备身份」导入 machineToken 和 machineType —— 缺它时上游连「每日签到」都不会下发")
	}
	h := q.headers(c)
	h["Content-Type"] = "application/json"
	var data map[string]any
	if err := JSONRequest(ctx, q.client, http.MethodPost, q.api+"/sash/api/v1/me/campaigns/"+url.PathEscape(id)+"/claim", h, nil, &data); err != nil {
		return out, err
	}
	if data["replayed"] == true {
		out.AlreadyClaimed = true
		out.Message = "该活动已领取"
		return out, nil
	}
	if Text(data["status"]) != "CLAIMED" {
		return out, errors.New("上游未确认领取成功，请刷新活动状态")
	}
	out.Claimed = true
	out.Amount = Number(Object(data["benefit"])["amount"])
	out.Message = "领取成功"
	return out, nil
}

func (q *qoderAdapter) Prepare(ctx context.Context, c Credential, in Inference) (Prepared, error) {
	if err := q.Validate(c); err != nil {
		return Prepared{}, err
	}
	messages := make([]map[string]any, 0, len(in.Messages))
	system := []map[string]string{}
	lastText := ""
	for _, msg := range in.Messages {
		role := Text(msg["role"])
		if role == "system" || role == "developer" {
			if s := messageText(msg["content"]); s != "" {
				system = append(system, map[string]string{"type": "text", "text": s})
			}
			continue
		}
		m := map[string]any{"role": role, "content": msg["content"]}
		for _, key := range []string{"tool_calls", "tool_call_id"} {
			if v, ok := msg[key]; ok {
				m[key] = v
			}
		}
		messages = append(messages, m)
		if role == "user" {
			lastText = messageText(msg["content"])
		}
	}
	params := map[string]any{"max_tokens": in.MaxTokens, "context_length": in.Model.ContextWindow}
	if in.Effort != "" {
		params["reasoning_effort"] = in.Effort
		params["enable_thinking"] = in.Effort != "none"
	}
	requestID := randomID()
	session := in.SessionID
	if session == "" {
		session = randomID()
	}
	tools := in.Tools
	if tools == nil {
		tools = []map[string]any{}
	}
	body := map[string]any{
		"request_id": requestID, "request_set_id": requestID, "chat_record_id": requestID, "session_id": session, "session_type": "qodercli",
		"stream": true, "chat_task": "FREE_INPUT", "agent_id": "agent_common", "task_id": "common", "version": "3", "source": 1, "is_reply": true, "is_retry": false, "aliyun_user_type": "", "custom_model": nil,
		"chat_context": map[string]any{"text": lastText, "features": []any{}, "chatPrompt": "", "imageUrls": nil, "extra": map[string]any{"originalContent": lastText, "context": []any{}, "modelConfig": map[string]any{"key": in.Model.ID, "is_reasoning": in.Model.Reasoning}}},
		"model_config": map[string]any{"key": in.Model.ID, "display_name": in.Model.Name, "model": "", "format": "openai", "is_vl": in.Model.Vision, "is_reasoning": in.Model.Reasoning, "api_key": "", "url": "", "source": "system", "max_input_tokens": in.Model.ContextWindow},
		"system":       system, "messages": messages, "tools": tools, "parameters": params, "business": map[string]string{"type": "agent"},
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Prepared{}, err
	}
	user, _ := json.Marshal(map[string]any{"uid": c.Text("uid", "user_id", "userId"), "securityOauthToken": c.Text("accessToken", "access_token", "security_oauth_token", "token"), "organizationId": c.Text("organizationId", "organization_id"), "dataPolicyAgreed": c["dataPolicyAgreed"] == true})
	req, err := qoderwasm.Prepare(ctx, user, c.Text("machineId", "machine_id"), "1.1.49", q.host, in.Model.ID, payload)
	if err != nil {
		return Prepared{}, errors.New("Qoder 官方加密组件未能构建请求：" + err.Error())
	}
	return Prepared{URL: req.URL, Headers: req.Headers, Body: []byte(req.Body), Wire: "qoder"}, nil
}

func messageText(content any) string {
	if s, ok := content.(string); ok {
		return s
	}
	var parts []string
	for _, v := range Array(content) {
		p := Object(v)
		if Text(p["type"]) == "text" {
			parts = append(parts, Text(p["text"]))
		}
	}
	return strings.Join(parts, "\n")
}
