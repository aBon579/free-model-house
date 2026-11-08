package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDiscoverModelsOpenAI 验证 OpenAI 兼容端点（`{"data":[{"id":…}]}`）。
func TestDiscoverModelsOpenAI(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o"},{"id":"gpt-4o-mini"},{"id":"gpt-4o"}]}`))
	}))
	defer ts.Close()

	models, err := discoverModels(ts.URL+"/v1", "sk-test")
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("认证头不对: %q", gotAuth)
	}
	if len(models) != 2 || models[0] != "gpt-4o" || models[1] != "gpt-4o-mini" {
		t.Fatalf("应去重保序返回 2 个模型，实际 %v", models)
	}
}

// TestDiscoverModelsAltShapes 验证 `{"models":["…"]}` / `{"models":[{"id":…}]}` 两种变体。
func TestDiscoverModelsAltShapes(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ⚠️ 没有 Key 时不该塞空的 Bearer。
		if r.Header.Get("authorization") != "" {
			t.Errorf("空 Key 不应带 authorization，实际 %q", r.Header.Get("authorization"))
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"models":["llama3","qwen2.5"]}`))
	}))
	defer ts.Close()

	models, err := discoverModels(ts.URL, "")
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(models) != 2 || models[0] != "llama3" {
		t.Fatalf("解析 models 数组失败: %v", models)
	}

	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[{"id":"a"},{"name":"b"}]}`))
	}))
	defer ts2.Close()
	models2, err := discoverModels(ts2.URL, "")
	if err != nil {
		t.Fatalf("拉取失败: %v", err)
	}
	if len(models2) != 2 || models2[0] != "a" || models2[1] != "b" {
		t.Fatalf("解析 models 对象失败: %v", models2)
	}
}

// TestDiscoverModelsHTTPError 验证上游错误会带出状态码与响应体片段。
func TestDiscoverModelsHTTPError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer ts.Close()

	if _, err := discoverModels(ts.URL, "bad"); err == nil {
		t.Fatal("401 应当报错")
	}
}

// TestKeyLabelFromContext 验证鉴权阶段挂到 context 的标签能被读出来。
func TestKeyLabelFromContext(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r = r.WithContext(context.WithValue(r.Context(), keyLabelCtxKey{}, "主key"))
	if got := keyLabelOf(r); got != "主key" {
		t.Fatalf("应读到「主key」，实际 %q", got)
	}
}

// TestModelsEndpointsNoDoubleV1 验证不会拼出 `/v1/v1/models`。
func TestModelsEndpointsNoDoubleV1(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com/v1":        "https://api.example.com/v1/models",
		"https://api.example.com/v1/":       "https://api.example.com/v1/models",
		"https://api.example.com/openai/v1": "https://api.example.com/openai/v1/models",
		"https://api.example.com/v1beta":    "https://api.example.com/v1beta/models",
		"https://api.example.com/v1/models": "https://api.example.com/v1/models",
		"https://api.example.com":           "https://api.example.com/v1/models",
	}
	for in, wantFirst := range cases {
		got := modelsEndpoints(in)
		if len(got) == 0 || got[0] != wantFirst {
			t.Fatalf("%s → 首选应为 %s，实际 %v", in, wantFirst, got)
		}
	}
	// 已经带 /models 的地址只尝试它自己，不再叠加。
	if got := modelsEndpoints("https://api.example.com/v1/models"); len(got) != 1 {
		t.Fatalf("已带 /models 的地址应只有 1 个候选，实际 %v", got)
	}
}

// TestDiscoverFallsBackToPlainModels 验证 `/v1/models` 404 时回退到 `/models`。
func TestDiscoverFallsBackToPlainModels(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer ts.Close()

	models, err := discoverModels(ts.URL, "")
	if err != nil {
		t.Fatalf("应回退成功: %v", err)
	}
	if len(models) != 1 || models[0] != "m1" {
		t.Fatalf("回退结果不对: %v", models)
	}
}

// TestDiscoverTriesXAPIKey 验证 Bearer 401 时改用 x-api-key / api-key。
func TestDiscoverTriesXAPIKey(t *testing.T) {
	var used string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Header.Get("x-api-key") == "sk-test":
			used = "x-api-key"
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
		case r.Header.Get("api-key") == "sk-test":
			used = "api-key"
			_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer ts.Close()

	models, err := discoverModels(ts.URL+"/v1", "sk-test")
	if err != nil {
		t.Fatalf("应换认证头成功: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("模型数不对: %v", models)
	}
	if used != "x-api-key" {
		t.Fatalf("应优先 x-api-key，实际用了 %q", used)
	}
}

// TestDiscoverContextLength 验证上游声明的上下文上限会被解析出来。
func TestDiscoverContextLength(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"id":"a","context_length":8192},
			{"id":"b","context_window":128000},
			{"id":"c"}
		]}`))
	}))
	defer ts.Close()

	details, err := discoverModelDetails(ts.URL+"/v1", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(details) != 3 {
		t.Fatalf("应解析 3 个模型，实际 %v", details)
	}
	if details[0].ContextLength != 8192 || details[1].ContextLength != 128000 || details[2].ContextLength != 0 {
		t.Fatalf("上下文解析不对: %+v", details)
	}
}

// TestValidateContextLength 验证"不能超过模型自身能力上限"。
func TestValidateContextLength(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m1","context_length":8192}]}`))
	}))
	defer ts.Close()

	if _, _, err := validateContextLength(16000, ts.URL+"/v1", "k", []string{"m1"}); err == nil {
		t.Fatal("超过探测到的上限应当报错")
	}
	detected, note, err := validateContextLength(4096, ts.URL+"/v1", "k", []string{"m1"})
	if err != nil {
		t.Fatalf("未超限不该报错: %v", err)
	}
	if detected != 8192 || note == "" {
		t.Fatalf("应探测到 8192 并给出说明，实际 detected=%d note=%q", detected, note)
	}

	// 探测不到上限 → 允许填写，但要有说明。
	ts2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m2"}]}`))
	}))
	defer ts2.Close()
	if _, note, err := validateContextLength(64000, ts2.URL+"/v1", "k", []string{"m2"}); err != nil || note == "" {
		t.Fatalf("探测不到时应允许并说明，实际 err=%v note=%q", err, note)
	}
}
