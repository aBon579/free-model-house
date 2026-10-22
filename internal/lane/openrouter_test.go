package lane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// OpenRouter 车道的测试。
// ⚠️ 重点验证两件事：
//  1. 它的请求**不带**任何指纹（它不需要）
//  2. 它的思考档位是**真档位**（与免密车道完全不同）

// orStandIn 是一个模拟 OpenRouter 的测试服务器。
type orStandIn struct {
	gotRequest *http.Request
	gotBody    map[string]any
	status     int
	errorBody  string
	frames     []string
	quotaBody  string
}

func (s *orStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.gotRequest = r
	if r.Body != nil {
		raw, _ := readAll(r)
		if len(raw) > 0 {
			_ = jsonUnmarshal(raw, &s.gotBody)
		}
	}
	if strings.HasSuffix(r.URL.Path, "/key") {
		w.Header().Set("Content-Type", "application/json")
		if s.quotaBody == "" {
			s.quotaBody = `{"data":{"label":"free","usage":0.5,"limit":10,"limit_remaining":9.5,"free_tier_usage":0.25,"is_free_tier":true}}`
		}
		_, _ = w.Write([]byte(s.quotaBody))
		return
	}
	if s.status != 0 && (s.status < 200 || s.status >= 300) {
		body := s.errorBody
		if body == "" {
			body = `{"error":{"message":"User not found.","code":401}}`
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		_, _ = w.Write([]byte(body))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	for _, f := range s.frames {
		_, _ = w.Write([]byte("data: " + f + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// newORServer 起一个 OpenRouter 替身。
func newORServer(t *testing.T, s *orStandIn) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return srv
}

// TestOpenRouterSendsNoFingerprint 验证 OpenRouter 请求**不带**指纹。
// 它是标准 OpenAI 兼容 —— 加指纹头反而可能让严格的网关困惑。
func TestOpenRouterSendsNoFingerprint(t *testing.T) {
	s := &orStandIn{frames: []string{
		`{"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`,
	}}
	srv := newORServer(t, s)

	lane := NewOpenRouterLane("sk-or-v1-test-key")
	lane.client = Transport()
	// 把 base 指到替身。
	lane.baseURL = srv.URL

	turn := &Turn{
		Model:    "deepseek/deepseek-r1:free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hello")}}},
		Seed:     "s",
	}
	_, finish, uerr := lane.Do(context.Background(), turn, func(Chunk) {})
	if uerr != nil {
		t.Fatalf("不该有错误: %v", uerr)
	}
	if finish != FinishStop {
		t.Fatalf("结束原因不对: %v", finish)
	}

	// ⚠️ 绝不能有指纹头。
	h := s.gotRequest.Header
	for _, banned := range []string{
		"x-opencode-session", "x-opencode-session-id",
		"x-session-affinity", "x-session-id", "x-opencode-client",
	} {
		if h.Get(banned) != "" {
			t.Errorf("OpenRouter 不该带 %s", banned)
		}
	}
	if h.Get("User-Agent") != "free-model-house/0.1" {
		t.Errorf("UA 不该是 opencode 客户端: %q", h.Get("User-Agent"))
	}
	// 但要带自己的 Key。
	if h.Get("Authorization") != "Bearer sk-or-v1-test-key" {
		t.Errorf("授权头不对: %q", h.Get("Authorization"))
	}
	// 普通 OpenAI 形状。
	if s.gotBody["model"] != "deepseek/deepseek-r1:free" {
		t.Errorf("模型不对: %v", s.gotBody["model"])
	}
	if s.gotBody["stream"] != true {
		t.Error("应该是流式")
	}
}

// TestOpenRouterNeedsKey 验证没配 Key 时**不 ready**，且不浪费请求。
func TestOpenRouterNeedsKey(t *testing.T) {
	lane := NewOpenRouterLane("")
	if lane.Ready() {
		t.Fatal("没配 Key 时不该 ready")
	}
	status := lane.Status()
	if status.Ready {
		t.Fatal("状态不该是 ready")
	}
	if status.Reason == "" {
		t.Fatal("应该说明为什么没 ready")
	}
	if status.NeedsCredential != true {
		t.Fatal("应该标记需要凭据")
	}

	// 而且不该发出任何请求。
	_, _, uerr := lane.Do(context.Background(), &Turn{
		Model:    "x:free",
		Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}},
	}, func(Chunk) {})
	if uerr == nil {
		t.Fatal("应该报错")
	}
	if uerr.Code != CodeCredential {
		t.Fatalf("错误码不对: %v", uerr.Code)
	}
}

// TestOpenRouterEffortIsReal 验证思考档位是**真档位**。
// 免密车道是"接受但忽略"；OpenRouter 是真生效的。
// 所以它用 `reasoning: {effort: "..."}` 对象。
// ⚠️ 这里用的是**统一档位名**（light/balanced/deep/EffortOff）——
//
//	那是网关层给下来的形状。
//	收敛到模型声明的档位、以及 mandatory 的特殊处理，
//	见 openrouter_effort_test.go（那里用的是真实上游数据）。
func TestOpenRouterEffortIsReal(t *testing.T) {
	cases := []struct {
		effort string
		want   map[string]any
	}{
		{"light", map[string]any{"effort": "low"}},
		{"balanced", map[string]any{"effort": "medium"}},
		{"deep", map[string]any{"effort": "high"}},
		// ⚠️ "关掉思考"在**不强制思考**的模型上是 enabled:false。
		{EffortOff, map[string]any{"enabled": false}},
	}
	for _, c := range cases {
		lane := NewOpenRouterLane("sk-or-v1-test")
		turn := &Turn{Model: "x:free", Effort: c.effort}
		got := lane.effortPatch(turn)
		if got == nil {
			t.Errorf("%s 应该产出补丁", c.effort)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%s → %v，期望 %s=%v", c.effort, got, k, v)
			}
		}
	}
}

// TestOpenRouterEffortSnapsToDeclaredLevels 验证档位会**就近收敛**。
// 一个"点了没反应"的档位比没有档位更糟。
func TestOpenRouterEffortSnapsToDeclaredLevels(t *testing.T) {
	lane := NewOpenRouterLane("sk-or-v1-test")
	lane.setEfforts(map[string][]string{
		"strict-model:free": {"low", "high"},
	})
	// medium 不在声明里 → 应收敛到最接近的。
	turn := &Turn{Model: "strict-model:free", Effort: "balanced"}
	got := lane.effortPatch(turn)
	if got["effort"] != "low" {
		t.Fatalf("应收敛到最接近的档位，实际 %v", got["effort"])
	}
}

// TestOpenRouterQuota 验证额度查询。
func TestOpenRouterQuota(t *testing.T) {
	s := &orStandIn{}
	srv := newORServer(t, s)
	lane := NewOpenRouterLane("sk-or-v1-test")
	lane.baseURL = srv.URL

	info, uerr := lane.FetchQuota(context.Background())
	if uerr != nil {
		t.Fatalf("查额度失败: %v", uerr)
	}
	if info.Limit != 10 || info.Usage != 0.5 {
		t.Fatalf("额度解析不对: %+v", info)
	}
	if info.Remaining != 9.5 {
		t.Fatalf("剩余不对: %+v", info)
	}
	if info.FreeTierUsageToday != 0.25 {
		t.Fatalf("免费额度用量不对: %+v", info)
	}
}

func TestCredentialHintNeverShowsFullKey(t *testing.T) {
	hint := credentialHint("sk-or-v1-EXAMPLEKEYFORTESTSONLY0000")
	if strings.Contains(hint, "EXAMPLEKEYFORTESTSONLY") {
		t.Fatalf("指纹不得包含完整 key: %q", hint)
	}
	if !strings.HasSuffix(hint, "0000") {
		t.Fatalf("指纹应保留后四位: %q", hint)
	}
	if hint == "" {
		t.Fatal("指纹不该为空")
	}
	if credentialHint("") != "" {
		t.Fatal("空 key 的指纹应为空")
	}
}

// TestOpenRouterCatalogParsesRealShape 用真实的 OpenRouter 目录形状做测试。
func TestOpenRouterCatalogParsesRealShape(t *testing.T) {
	payload := `{"data":[
	  {"id":"deepseek/deepseek-r1:free","name":"DeepSeek R1 (free)",
	   "context_length":163840,"created":1,
	   "top_provider":{"max_completion_tokens":163840},
	   "architecture":{"input_modalities":["text"],"output_modalities":["text"]},
	   "supported_parameters":["include_reasoning","max_tokens","reasoning","reasoning_effort","tools"],
	   "pricing":{"prompt":"0","completion":"0"},
	   "reasoning":{"mandatory":false,"supported_efforts":["low","medium","high"],"default_effort":"medium"}},
	  {"id":"openai/gpt-4.1","context_length":1000000,
	   "pricing":{"prompt":"0.0000025","completion":"0.00001"}}
	]}`
	var doc struct {
		Data []openRouterCatalogRow `json:"data"`
	}
	if err := jsonUnmarshal([]byte(payload), &doc); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(doc.Data) != 2 {
		t.Fatalf("应有 2 行，实际 %d", len(doc.Data))
	}
	row := doc.Data[0]
	if row.ContextLength != 163840 {
		t.Errorf("上下文不对: %d", row.ContextLength)
	}
	if row.TopProvider.MaxCompletionTokens != 163840 {
		t.Errorf("输出上限不对: %d", row.TopProvider.MaxCompletionTokens)
	}
	supports, efforts, canDisable := parseOpenRouterReasoning(row.Reasoning)
	if !supports {
		t.Error("应该支持思考")
	}
	if len(efforts) != 3 {
		t.Errorf("档位不对: %v", efforts)
	}
	if !canDisable {
		t.Error("mandatory:false 时应该能关掉思考")
	}
}
