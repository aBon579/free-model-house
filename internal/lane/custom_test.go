package lane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCustomLaneBasics(t *testing.T) {
	var gotPath, gotAuth, gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("authorization")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotModel, _ = body["model"].(string)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"pong"},"finish_reason":null}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}

data: [DONE]

`))
	}))
	defer srv.Close()

	l := NewCustomLane()
	l.SetProviders([]CustomProvider{{
		ID: "p1", Name: "测试端点", BaseURL: srv.URL + "/v1", APIKey: "sk-test", Models: []string{"m1", "m2"},
	}})

	if l.AutoProbeable() {
		t.Fatal("自定义 Key 不应参与自动探测")
	}
	if st := l.Status(); st.AutoProbe || !st.NeedsCredential {
		t.Fatalf("Status 标记不对: %+v", st)
	}

	models, uerr := l.ListModels(context.Background())
	if uerr != nil {
		t.Fatalf("ListModels: %v", uerr)
	}
	if len(models) != 2 || models[0].ID != "custom/p1/m1" {
		t.Fatalf("模型清单不对: %+v", models)
	}

	usage, finish, uerr := l.Do(context.Background(),
		&Turn{Model: "p1/m1", Messages: []Message{{Role: RoleUser, Parts: []Part{TextPart("hi")}}}},
		func(Chunk) {})
	if uerr != nil {
		t.Fatalf("Do: %v (state=%v)", uerr, finish)
	}
	if usage.Input != 3 || usage.Output != 2 {
		t.Fatalf("用量不对: %+v", usage)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("请求路径不对: %s", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("认证头不对: %s", gotAuth)
	}
	if gotModel != "m1" {
		t.Fatalf("上游模型名不对: %s", gotModel)
	}

	res := l.Probe(context.Background(), "p1/m1")
	if res.State != StateAvailable {
		t.Fatalf("手动探测应可用: %+v", res)
	}
}
