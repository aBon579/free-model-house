package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aBon579/free-model-house/internal/lane"
	"github.com/aBon579/free-model-house/internal/loghub"
	"github.com/aBon579/free-model-house/internal/store"
)

// customKeyMeta 是自定义端点的**元数据**（不含 Key）。
type customKeyMeta struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	BaseURL string   `json:"baseURL"`
	Models  []string `json:"models"`
	// ContextLength 是用户填写的上下文长度（0 = 没填）。
	//
	// ⚠️ 它会被带到模型清单里（`/v1/models` 的 `context_window`），
	//   这样 agent 端能看到用户声明的容量。
	ContextLength int `json:"contextLength,omitempty"`
}

// customSecretName 是自定义端点 Key 在 secrets 表里的名字。
func customSecretName(id string) string { return "custom_key:" + id }

// maxContextLength 是上下文长度填写的硬上限。
//
// ⚠️ 只用来挡住明显离谱的输入（例如多打一个零）——
//
//	真实上限由上游清单探测，探测不到时允许用户填写。
const maxContextLength = 10_000_000

// handleCustomKeys 返回自定义 Key 列表。
func (a *Admin) handleCustomKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的方法"})
		return
	}
	metas := a.customKeyMetas()
	out := make([]map[string]any, 0, len(metas))
	for _, m := range metas {
		_, hint, found, _ := a.db.GetSecret(customSecretName(m.ID))
		out = append(out, map[string]any{
			"id":            m.ID,
			"name":          m.Name,
			"baseURL":       m.BaseURL,
			"models":        m.Models,
			"contextLength": m.ContextLength,
			"hint":          hint,
			"ready":         found,
			// ⚠️ 这批 Key 是**用户自己的额度** —— 只手动探测。
			"manualProbeOnly": true,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// customKeyMetas 从设置里读端点元数据。
func (a *Admin) customKeyMetas() []customKeyMeta {
	out := []customKeyMeta{}
	if _, err := a.db.GetJSON(store.SettingCustomProviders, &out); err != nil {
		return []customKeyMeta{}
	}
	return out
}

// ReloadCustomProviders 重新装载自定义端点到车道与模型池。
//
// ⚠️ 启动时和增删改后都要调用 —— 端点变了，模型池必须跟着变。
func (a *Admin) ReloadCustomProviders() {
	if a.lanes == nil || a.catalog == nil {
		return
	}
	l, ok := a.lanes.Get(lane.CustomLaneID)
	if !ok {
		return
	}
	cl, ok := l.(*lane.CustomLane)
	if !ok {
		return
	}
	metas := a.customKeyMetas()
	providers := make([]lane.CustomProvider, 0, len(metas))
	for _, m := range metas {
		sealed, _, found, err := a.db.GetSecret(customSecretName(m.ID))
		if err != nil || !found {
			continue
		}
		plain, err := a.vault.Open(sealed)
		if err != nil {
			continue
		}
		providers = append(providers, lane.CustomProvider{
			ID:            m.ID,
			Name:          m.Name,
			BaseURL:       m.BaseURL,
			APIKey:        plain,
			Models:        m.Models,
			ContextLength: m.ContextLength,
		})
	}
	cl.SetProviders(providers)

	// ⚠️ **先摘掉旧模型，再合并新的** —— 只 Merge 不 Remove，
	//   用户删掉的端点模型会永远留在池子里。
	a.catalog.RemoveLane(lane.CustomLaneID)
	if models, uerr := cl.ListModels(context.Background()); uerr == nil {
		a.catalog.Merge(models)
	}
}

// handleCustomKeyCreate 添加一个自定义 OpenAI 兼容端点。
func (a *Admin) handleCustomKeyCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的方法"})
		return
	}
	var body struct {
		Name          string   `json:"name"`
		BaseURL       string   `json:"baseURL"`
		APIKey        string   `json:"apiKey"`
		Models        []string `json:"models"`
		ContextLength int      `json:"contextLength"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是 JSON"})
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	body.BaseURL = strings.TrimSpace(body.BaseURL)
	body.APIKey = strings.TrimSpace(body.APIKey)
	if body.Name == "" || len([]rune(body.Name)) > 40 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "名称必填且不超过 40 字"})
		return
	}
	if err := validateBaseURL(body.BaseURL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if body.APIKey == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "API Key 必填"})
		return
	}
	models := normalizeModels(body.Models)
	if len(models) == 0 {
		// ⚠️ **没填模型就拉一次 /models** —— 用户不用手抄几十个模型名。
		details, err := discoverModelDetails(body.BaseURL, body.APIKey)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"error": "模型清单为空，且自动拉取失败：" + err.Error(),
				"hint":  discoveryHint(err),
			})
			return
		}
		for _, d := range details {
			models = append(models, d.ID)
		}
		models = normalizeModels(models)
	}
	if len(models) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "至少需要一个模型"})
		return
	}
	if _, _, err := validateContextLength(body.ContextLength, body.BaseURL, body.APIKey, models); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	id := store.GenerateKey("ck-")
	sealed, err := a.vault.Seal(body.APIKey)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "加密 Key 失败: " + err.Error()})
		return
	}
	if err := a.db.PutSecret(customSecretName(id), sealed, store.CredentialHint(body.APIKey)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	metas := append(a.customKeyMetas(), customKeyMeta{
		ID: id, Name: body.Name, BaseURL: body.BaseURL, Models: models,
		ContextLength: body.ContextLength,
	})
	if err := a.db.PutJSON(store.SettingCustomProviders, metas); err != nil {
		_ = a.db.DeleteSecret(customSecretName(id))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	a.ReloadCustomProviders()
	if a.loghub != nil {
		a.loghub.Log(loghub.CategoryKey, "添加自定义 API Key「%s」（%s，%d 个模型，上下文 %s，仅手动探测）",
			body.Name, body.BaseURL, len(models), contextNote(body.ContextLength))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": id, "models": models, "contextLength": body.ContextLength,
	})
}

// handleCustomKeyUpdate 编辑一个自定义端点（名称 / Base URL / Key / 模型 / 上下文长度）。
//
// ⚠️ 编辑后**立即**重建模型池 —— 用户在表单里删掉的模型会立刻从
// `/v1/models` 与车道上消失，不需要删掉重建端点。
func (a *Admin) handleCustomKeyUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的方法"})
		return
	}
	var body struct {
		ID            string   `json:"id"`
		Name          string   `json:"name"`
		BaseURL       string   `json:"baseURL"`
		APIKey        string   `json:"apiKey"`
		Models        []string `json:"models"`
		ContextLength *int     `json:"contextLength"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 id"})
		return
	}
	metas := a.customKeyMetas()
	idx := -1
	for i := range metas {
		if metas[i].ID == body.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "找不到这条自定义 API Key"})
		return
	}
	old := metas[idx]

	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = old.Name
	}
	if len([]rune(name)) > 40 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "名称不超过 40 字"})
		return
	}
	baseURL := strings.TrimSpace(body.BaseURL)
	if baseURL == "" {
		baseURL = old.BaseURL
	}
	if err := validateBaseURL(baseURL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	models := old.Models
	if body.Models != nil {
		models = normalizeModels(body.Models)
		if len(models) == 0 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "至少需要一个模型"})
			return
		}
	}
	contextLength := old.ContextLength
	if body.ContextLength != nil {
		contextLength = *body.ContextLength
	}
	if contextLength < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "上下文长度不能为负数"})
		return
	}

	// ⚠️ Key 为空 = **保持原值**（编辑时用户通常不想重新输入一遍）。
	plainKey := strings.TrimSpace(body.APIKey)
	if plainKey == "" {
		sealed, _, found, err := a.db.GetSecret(customSecretName(old.ID))
		if err != nil || !found {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "这条端点还没有可用的 Key，请填写 API Key"})
			return
		}
		plainKey, err = a.vault.Open(sealed)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "读取已保存的 Key 失败: " + err.Error()})
			return
		}
	}
	if _, _, err := validateContextLength(contextLength, baseURL, plainKey, models); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	if body.APIKey != "" {
		sealed, err := a.vault.Seal(plainKey)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "加密 Key 失败: " + err.Error()})
			return
		}
		if err := a.db.PutSecret(customSecretName(old.ID), sealed, store.CredentialHint(plainKey)); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
	}

	metas[idx] = customKeyMeta{
		ID: old.ID, Name: name, BaseURL: baseURL, Models: models,
		ContextLength: contextLength,
	}
	if err := a.db.PutJSON(store.SettingCustomProviders, metas); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	a.ReloadCustomProviders()
	if a.loghub != nil {
		a.loghub.Log(loghub.CategoryKey, "更新自定义 API Key「%s」（%s，%d 个模型，上下文 %s）",
			name, baseURL, len(models), contextNote(contextLength))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "id": old.ID, "models": models, "contextLength": contextLength,
	})
}

// handleCustomKeyDelete 删除一个自定义端点。
func (a *Admin) handleCustomKeyDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的方法"})
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "缺少 id"})
		return
	}
	metas := a.customKeyMetas()
	kept := make([]customKeyMeta, 0, len(metas))
	var name string
	for _, m := range metas {
		if m.ID == body.ID {
			name = m.Name
			continue
		}
		kept = append(kept, m)
	}
	if err := a.db.PutJSON(store.SettingCustomProviders, kept); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	_ = a.db.DeleteSecret(customSecretName(body.ID))
	a.ReloadCustomProviders()
	if a.loghub != nil {
		a.loghub.Log(loghub.CategoryKey, "删除自定义 API Key「%s」，其模型已从模型池移除", name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleCustomKeyDiscover 拉取某个端点的模型清单（**不落库**）。
//
// ⚠️ 这是表单里的"拉取模型"按钮 —— 用户填 Base URL + Key 后先看清单，
// 勾选要接入的子集，再走 create。
//
// ⚠️ 失败时把**具体原因**（尝试了哪些地址、状态码、响应片段）带回去，
// 前端不能只说"拉取失败" —— 用户没法据此判断是地址错、Key 错还是代理不通。
func (a *Admin) handleCustomKeyDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "不支持的方法"})
		return
	}
	var body struct {
		BaseURL string `json:"baseURL"`
		APIKey  string `json:"apiKey"`
		// ID 非空 = 编辑已有端点：Key 留空时用已保存的那把去拉清单，
		// 用户不用为了"看看有哪些模型"重新粘贴一次 Key。
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求体不是 JSON"})
		return
	}
	body.BaseURL = strings.TrimSpace(body.BaseURL)
	body.APIKey = strings.TrimSpace(body.APIKey)
	if err := validateBaseURL(body.BaseURL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if body.APIKey == "" && body.ID != "" {
		if sealed, _, found, err := a.db.GetSecret(customSecretName(body.ID)); err == nil && found {
			if plain, oerr := a.vault.Open(sealed); oerr == nil {
				body.APIKey = plain
			}
		}
	}
	details, err := discoverModelDetails(body.BaseURL, body.APIKey)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": "拉取模型清单失败：" + err.Error(),
			"hint":  discoveryHint(err),
		})
		return
	}
	models := make([]string, 0, len(details))
	contexts := map[string]int{}
	maxContext := 0
	for _, d := range details {
		models = append(models, d.ID)
		if d.ContextLength > 0 {
			contexts[d.ID] = d.ContextLength
			if d.ContextLength > maxContext {
				maxContext = d.ContextLength
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":     models,
		"count":      len(models),
		"contexts":   contexts,
		"maxContext": maxContext,
		"hint":       "勾选要接入的模型；也可以手动补充",
	})
}

// validateBaseURL 校验 Base URL。
func validateBaseURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("Base URL 必须是 http(s) 地址")
	}
	return nil
}

// contextNote 把上下文长度格式化成日志里的短说明。
func contextNote(n int) string {
	if n <= 0 {
		return "未填写"
	}
	return strconv.Itoa(n)
}

// normalizeModels 去空、去重、保序。
func normalizeModels(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		m = strings.TrimSpace(m)
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// discoveredModel 是探测到的一条模型（含上游声明的上下文上限）。
type discoveredModel struct {
	ID            string
	ContextLength int
}

// discoverModels 用用户的 Key 拉一次 OpenAI 兼容的 /models（只要模型名）。
func discoverModels(baseURL, apiKey string) ([]string, error) {
	details, err := discoverModelDetails(baseURL, apiKey)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(details))
	for _, d := range details {
		out = append(out, d.ID)
	}
	return out, nil
}

// discoverModelDetails 拉 `/models`，并尽量带上上游声明的上下文长度。
func discoverModelDetails(baseURL, apiKey string) ([]discoveredModel, error) {
	endpoints := modelsEndpoints(baseURL)
	client := lane.Transport()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var failures []string
	var lastErr error
	for _, endpoint := range endpoints {
		for _, h := range authHeaders(apiKey) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if err != nil {
				lastErr = err
				failures = append(failures, fmt.Sprintf("%s → %v", endpoint, err))
				break
			}
			req.Header.Set("Accept", "application/json")
			if h.name != "" {
				req.Header.Set(h.name, h.value)
			}
			res, err := client.Do(req)
			if err != nil {
				lastErr = err
				failures = append(failures, fmt.Sprintf("%s → %v", endpoint, err))
				continue
			}
			raw, readErr := io.ReadAll(io.LimitReader(res.Body, 8<<20))
			_ = res.Body.Close()
			if readErr != nil {
				lastErr = readErr
				failures = append(failures, fmt.Sprintf("%s → 读取响应失败: %v", endpoint, readErr))
				continue
			}
			if res.StatusCode < 200 || res.StatusCode >= 300 {
				lastErr = &discoverError{
					msg:  fmt.Sprintf("%s → HTTP %d: %s", endpoint, res.StatusCode, snippet(raw)),
					hint: statusHint(res.StatusCode, endpoint),
				}
				failures = append(failures, fmt.Sprintf("%s → HTTP %d", endpoint, res.StatusCode))
				// ⚠️ 401/403 才换认证头重试；404/500 换认证头没用，直接换下一个地址。
				if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
					continue
				}
				break
			}
			details, perr := parseModelList(raw)
			if perr != nil {
				lastErr = &discoverError{
					msg:  fmt.Sprintf("%s → 无法解析响应: %v（响应片段: %s）", endpoint, perr, snippet(raw)),
					hint: "上游返回的不是 OpenAI 兼容的模型清单；确认 Base URL 是否指向兼容接口",
				}
				failures = append(failures, fmt.Sprintf("%s → 响应无法解析", endpoint))
				break
			}
			if len(details) == 0 {
				lastErr = &discoverError{
					msg:  fmt.Sprintf("%s → 返回 200 但模型清单为空", endpoint),
					hint: "该端点返回了空清单；确认这个 Base URL 是否真的提供 /models",
				}
				failures = append(failures, fmt.Sprintf("%s → 空清单", endpoint))
				break
			}
			return details, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("没有可尝试的模型地址")
	}
	if de, ok := lastErr.(*discoverError); ok {
		return nil, &discoverError{
			msg:  fmt.Sprintf("%s（已尝试: %s）", de.msg, strings.Join(failures, "；")),
			hint: de.hint,
		}
	}
	return nil, &discoverError{
		msg:  fmt.Sprintf("%v（已尝试: %s）", lastErr, strings.Join(failures, "；")),
		hint: transportHint(lastErr),
	}
}

// discoverError 带"下一步该怎么办"的探测错误。
type discoverError struct {
	msg  string
	hint string
}

func (e *discoverError) Error() string { return e.msg }

// discoveryHint 取探测错误的提示；没有就返回通用提示。
func discoveryHint(err error) string {
	var de *discoverError
	if errors.As(err, &de) && de.hint != "" {
		return de.hint
	}
	return "确认 Base URL 指向 OpenAI 兼容接口（通常以 /v1 结尾），且 Key 有权限；也可以手动填写模型名"
}

// modelsEndpoints 按可能性列出 `/models` 地址。
//
// ⚠️ 最常见的两个坑：
//
//	① Base URL 已经带 /v1 → 再拼 `/v1/models` 就变成 `/v1/v1/models`；
//	② Base URL 带更长的路径（如 `/openai/v1`）→ 同样不能再套一层 /v1。
//
// 所以先按"最后一段像不像版本号"判断，再退回 /v1、/models 两种常见写法。
func modelsEndpoints(baseURL string) []string {
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	var out []string
	add := func(s string) {
		for _, x := range out {
			if x == s {
				return
			}
		}
		out = append(out, s)
	}
	if strings.HasSuffix(base, "/models") {
		return []string{base}
	}
	last := base
	if i := strings.LastIndex(base, "/"); i >= 0 {
		last = base[i+1:]
	}
	if looksVersioned(last) {
		add(base + "/models")
	}
	add(base + "/v1/models")
	add(base + "/models")
	return out
}

// looksVersioned 判断路径最后一段是不是版本号（v1 / v1beta / v4 / v1.1）。
func looksVersioned(seg string) bool {
	if len(seg) < 2 || (seg[0] != 'v' && seg[0] != 'V') {
		return false
	}
	for _, c := range seg[1:] {
		if c >= '0' && c <= '9' {
			return true
		}
	}
	return false
}

// authHeader 是一种认证头写法。
type authHeader struct {
	name  string
	value string
}

// authHeaders 按常见程度列出认证头写法。
//
// ⚠️ 不同"OpenAI 兼容"实现的头并不一样：
//
//	OpenAI/大多数网关 → Authorization: Bearer
//	Anthropic 兼容     → x-api-key
//	Azure OpenAI       → api-key
//
// 先 Bearer，遇到 401/403 再换 —— 用户不用自己猜该选哪个。
func authHeaders(apiKey string) []authHeader {
	if strings.TrimSpace(apiKey) == "" {
		// ⚠️ 本地无认证端点（如 Ollama / LM Studio）—— 别塞一个空的 Bearer。
		return []authHeader{{}}
	}
	return []authHeader{
		{name: "Authorization", value: "Bearer " + apiKey},
		{name: "x-api-key", value: apiKey},
		{name: "api-key", value: apiKey},
	}
}

// parseModelList 解析模型清单，兼容几种常见形状：
//
//	{"data":[{"id":…}]}                 OpenAI
//
//	{"models":[{"id":…}]} / {"models":["…"]}
//	{"data":{"models":[…]}}             部分自建网关
//	[…直接就是数组…]
func parseModelList(raw []byte) ([]discoveredModel, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, errors.New("响应为空")
	}
	var entries []json.RawMessage
	if trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, err
		}
	} else {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &obj); err != nil {
			return nil, err
		}
		for _, key := range []string{"data", "models", "result", "items"} {
			v, ok := obj[key]
			if !ok {
				continue
			}
			var arr []json.RawMessage
			if err := json.Unmarshal(v, &arr); err == nil {
				entries = append(entries, arr...)
				continue
			}
			// `{"data":{"models":[…]}}`
			var nested map[string]json.RawMessage
			if err := json.Unmarshal(v, &nested); err == nil {
				if inner, ok := nested["models"]; ok {
					var arr2 []json.RawMessage
					if err := json.Unmarshal(inner, &arr2); err == nil {
						entries = append(entries, arr2...)
					}
				}
			}
		}
	}
	out := make([]discoveredModel, 0, len(entries))
	for _, rawEntry := range entries {
		var s string
		if err := json.Unmarshal(rawEntry, &s); err == nil {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, discoveredModel{ID: s})
			}
			continue
		}
		var e modelEntry
		if err := json.Unmarshal(rawEntry, &e); err != nil {
			continue
		}
		id := strings.TrimSpace(e.ID)
		if id == "" {
			id = strings.TrimSpace(e.Name)
		}
		if id == "" {
			id = strings.TrimSpace(e.Model)
		}
		if id != "" {
			out = append(out, discoveredModel{ID: id, ContextLength: e.context()})
		}
	}
	// 去重（同名模型可能出现在多个键里）。
	seen := map[string]bool{}
	uniq := out[:0]
	for _, d := range out {
		if seen[d.ID] {
			continue
		}
		seen[d.ID] = true
		uniq = append(uniq, d)
	}
	return uniq, nil
}

// modelEntry 是上游清单里的一条模型记录。
type modelEntry struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	Model            string `json:"model"`
	ContextLength    int    `json:"context_length"`
	ContextWindow    int    `json:"context_window"`
	MaxContextLength int    `json:"max_context_length"`
	MaxModelLen      int    `json:"max_model_len"`
	MaxInputTokens   int    `json:"max_input_tokens"`
}

// context 取上游声明的上下文上限（0 = 没声明）。
func (e modelEntry) context() int {
	for _, n := range []int{e.ContextLength, e.ContextWindow, e.MaxContextLength, e.MaxModelLen, e.MaxInputTokens} {
		if n > 0 {
			return n
		}
	}
	return 0
}

// validateContextLength 校验用户填的上下文长度。
//
// 返回运行结果：detected（探测到的上限，0 = 没探测到）+ note（给前端的说明）。
// ⚠️ 规则：
//
//	填 0            → 不校验（不展示）
//	探测到上限且超过 → 报错（不能超过模型自身能力）
//	探测不到         → 允许填写，但返回说明
func validateContextLength(n int, baseURL, apiKey string, models []string) (int, string, error) {
	if n <= 0 {
		return 0, "", nil
	}
	if n > maxContextLength {
		return 0, "", fmt.Errorf("上下文长度 %d 过大（上限 %d）", n, maxContextLength)
	}
	details, err := discoverModelDetails(baseURL, apiKey)
	if err != nil {
		return 0, fmt.Sprintf("未探测到模型上限（%v），已按填写的 %d 展示", err, n), nil
	}
	byID := map[string]int{}
	for _, d := range details {
		if d.ContextLength > 0 {
			byID[d.ID] = d.ContextLength
		}
	}
	detected := 0
	for _, m := range models {
		lim, ok := byID[m]
		if !ok {
			continue
		}
		if lim > detected {
			detected = lim
		}
		if n > lim {
			return 0, "", fmt.Errorf("上下文长度 %d 超过模型 %q 的上限 %d", n, m, lim)
		}
	}
	if detected == 0 {
		return 0, fmt.Sprintf("未探测到模型上限，已按填写的 %d 展示", n), nil
	}
	return detected, fmt.Sprintf("已探测到模型上限 %d，填写值可用", detected), nil
}

// snippet 把响应体压成一行短文本（用于错误信息）。
func snippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		runes := []rune(s)
		if len(runes) > 200 {
			s = string(runes[:200]) + "…"
		}
	}
	if s == "" {
		return "（空响应）"
	}
	return s
}

// statusHint 按状态码给出下一步建议。
func statusHint(status int, endpoint string) string {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return "上游拒绝了认证：检查 Key 是否有效/过期，以及该服务商要求的认证头（Bearer / x-api-key / api-key）"
	case http.StatusNotFound:
		return fmt.Sprintf("该地址没有 /models 接口（%s）；确认 Base URL 是否已经包含 /v1，或服务商是否真的提供模型清单", endpoint)
	default:
		return fmt.Sprintf("上游返回 HTTP %d；可稍后重试，或手动填写模型名", status)
	}
}

// transportHint 把网络错误翻译成人能看懂的下一步。
func transportHint(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
		return "请求超时：上游不可达或很慢；若在容器里运行，确认容器能出网或已配置代理"
	case strings.Contains(msg, "x509"), strings.Contains(msg, "tls"), strings.Contains(msg, "certificate"):
		return "TLS 证书校验失败：上游使用了自签/中间代理证书；请检查代理或换用 http 端点（仅限内网）"
	case strings.Contains(msg, "proxyconnect"), strings.Contains(msg, "socks"), strings.Contains(msg, "proxy"):
		return "代理连接失败：检查「设置 → 网络代理」的地址与端口"
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "dial tcp"), strings.Contains(msg, "network is unreachable"):
		return "无法连接上游：域名/端口是否正确；容器内可能需要配置代理才能访问外网"
	default:
		return "网络请求失败：" + err.Error()
	}
}
