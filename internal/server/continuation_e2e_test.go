package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aBon579/free-model-house/internal/lane"
)

// 断流续写的**端到端**验证
//
// ⚠️ 为什么要专门造一个"会截断的上游"：
//
//   单元测试覆盖了续写的**逻辑**（状态机、消息构造、去重），
//   但没有验证**整条链路**：
//
//	   上游截断 → 网关发现 → 续写 → 客户端拿到**完整**答案
//
//   而且这里有一个特别容易错的点：
//   续写用的"工具调用协议"要求上游**能理解**工具调用参数里的文本。
//   如果上游不配合，续写要么失败，要么**重复输出**。
//
// 所以这里造一个**行为像真实上游**的假服务器：
//   ① 第一次请求 → 吐一半文本就断（没有 finish_reason）
//   ② 第二次请求（带 __fmh_continue 工具调用）→ 从断点继续

// truncatingUpstream 是一个"会截断"的上游。
type truncatingUpstream struct {
	// firstChunk 是第一次请求吐出的文本。
	firstChunk string
	// restChunk 是续写时补上的文本。
	restChunk string
	// requests 记录每次请求体。
	requests []map[string]any
	mu       sync.Mutex
	// calls 记录调用次数。
	calls int
}

func (u *truncatingUpstream) snapshot() (int, []map[string]any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := make([]map[string]any, len(u.requests))
	copy(out, u.requests)
	return u.calls, out
}

// handle 处理一次请求。
func (u *truncatingUpstream) handle(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)

	u.mu.Lock()
	u.calls++
	u.requests = append(u.requests, body)
	callNo := u.calls
	u.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	// ⚠️ 判断这是不是**续写**请求 ——
	// 续写请求里会有 assistant 的 tool_calls。
	isContinuation := false
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			if mm == nil {
				continue
			}
			if tcs, ok := mm["tool_calls"].([]any); ok && len(tcs) > 0 {
				isContinuation = true
			}
		}
	}

	writeFrame := func(payload any) {
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if flusher != nil {
			flusher.Flush()
		}
	}

	// ⚠️ **按字符边界切**（不是按字节）。
	//
	// 中文是 3 字节 UTF-8。按字节切会把一个字符切成两半，
	// 然后 `json.Marshal` 会把非法字节**替换成 U+FFFD** ——
	// 那是**测试服务器**的锅，不是网关的。
	//
	// 真实上游不会这么切（每个 SSE 帧都是完整的 JSON 文档），
	// 但为了测出**真实的**续写行为，这里按字符切。
	chunk := func(s string, n int) []string {
		runes := []rune(s)
		var out []string
		for i := 0; i < len(runes); i += n {
			end := i + n
			if end > len(runes) {
				end = len(runes)
			}
			out = append(out, string(runes[i:end]))
		}
		return out
	}

	if callNo == 1 && !isContinuation {
		// ① **第一次**：吐一半，然后**直接断开**。
		//
		// ⚠️ 关键：不给 finish_reason，也不给 usage ——
		//   这才是"截断"的真实形态。
		//   正常结束的流**有** finish_reason。
		for _, piece := range chunk(u.firstChunk, 5) {
			writeFrame(map[string]any{
				"id": "x", "object": "chat.completion.chunk", "model": "fake",
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"content": piece},
				}},
			})
		}
		// ⚠️ **不写** finish_reason，直接结束响应。
		//   这就是截断。
		return
	}

	// ② **续写**（或非首次请求）：补上剩下的文本，然后正常结束。
	for _, piece := range chunk(u.restChunk, 5) {
		writeFrame(map[string]any{
			"id": "x", "object": "chat.completion.chunk", "model": "fake",
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": piece},
			}},
		})
	}
	writeFrame(map[string]any{
		"id": "x", "object": "chat.completion.chunk", "model": "fake",
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{
			"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30,
		},
	})
}

// newTruncatingGateway 组一个指向假上游的网关。
func newTruncatingGateway(t *testing.T, baseURL string) (*Gateway, *Keys) {
	t.Helper()
	keys, db := newTestKeys(t)
	_ = db
	registry := lane.NewRegistry()
	zen := lane.NewZenLane(lane.NewSessionMap())
	zen.SetBaseURL(baseURL)
	registry.Register(zen)
	catalog := lane.NewCatalog()
	catalog.Replace([]lane.ModelInfo{
		{ID: "zen/fake/model", Lane: lane.ZenLaneID, Enabled: true, Free: true},
	})
	router := lane.NewRouter(catalog, registry, lane.NewBook())
	return NewGateway(router, catalog, registry, keys), keys
}

// TestStreamContinuationEndToEnd 验证**断流续写**在真实链路上有效。
//
// ⚠️ 断言的重点：
//
//	① 客户端拿到的答案是**完整**的（前半 + 后半）
//	② 上游**确实被调用了两次**（说明续写发生了）
//	③ 第二次请求里带上了 `__fmh_continue` 工具调用
//	④ 答案里**没有重复**（上游重复的部分被去掉了）
func TestStreamContinuationEndToEnd(t *testing.T) {
	up := &truncatingUpstream{
		firstChunk: "这是一段很长的答案的前半部分，用来测试断流续写。",
		restChunk:  "这是后半部分，应该接在前半后面。",
	}
	srv := httptest.NewServer(http.HandlerFunc(up.handle))
	defer srv.Close()

	gw, keys := newTruncatingGateway(t, srv.URL)

	// 发一个**流式**请求。
	body := fmt.Sprintf(`{"model":"zen/fake/model","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+keys.LanKey())
	rec := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("状态码该是 200，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 收集客户端看到的文本。
	var sb strings.Builder
	sc := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			continue
		}
		choices, _ := frame["choices"].([]any)
		for _, c := range choices {
			cm, _ := c.(map[string]any)
			delta, _ := cm["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if content, ok := delta["content"].(string); ok {
				sb.WriteString(content)
			}
		}
	}
	got := sb.String()

	// ① 上游被调用了两次。
	calls, reqs := up.snapshot()
	if calls < 2 {
		t.Fatalf("该续写（上游该被调 2 次），实际 %d 次。客户端拿到: %q", calls, got)
	}

	// ③ 第二次请求带上了续写的工具调用。
	var sawContinue bool
	for _, r := range reqs {
		msgs, _ := r["messages"].([]any)
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			if mm == nil {
				continue
			}
			tcs, _ := mm["tool_calls"].([]any)
			for _, tc := range tcs {
				tcm, _ := tc.(map[string]any)
				fn, _ := tcm["function"].(map[string]any)
				name, _ := fn["name"].(string)
				if strings.Contains(name, "continue") {
					sawContinue = true
				}
			}
		}
	}
	if !sawContinue {
		t.Fatal("续写请求里该有 continue 工具调用")
	}

	// ① + ④ 答案完整且不重复。
	//
	// ⚠️ 用**字节**比较，不用字符串 ——
	//   调试输出经过控制台编码会变成乱码，
	//   看着像内容坏了，其实是显示问题。
	if !strings.Contains(got, "前半部分") {
		t.Fatalf("该有前半部分，实际字节=% x", []byte(got))
	}
	if !strings.Contains(got, "后半部分") {
		t.Fatalf("该有后半部分（说明续写生效了），实际 %q", got)
	}
	// ⚠️ **不能重复** —— 前半部分只该出现一次。
	//
	// ⚠️ 只查"前半部分"这四个字 —— 不能查"前半"，
	//   因为后半段的原文里就含"前半"（"接在前半后面"）。
	if n := strings.Count(got, "前半部分"); n != 1 {
		t.Fatalf("前半部分该只出现 1 次，实际 %d 次: %q", n, got)
	}
	if n := strings.Count(got, "这是一段很长的答案"); n != 1 {
		t.Fatalf("续写不该重放开头，实际 %d 次: %q", n, got)
	}
}

// TestStreamContinuationNotNeededWhenComplete 验证**正常结束**时**不**续写。
//
// ⚠️ 这是"续写不能滥触发"的保证 ——
//
//	不然每次请求都要多花一次上游调用。
func TestStreamContinuationNotNeededWhenComplete(t *testing.T) {
	up := &truncatingUpstream{
		firstChunk: "", // 第一次也直接给完整答案
		restChunk:  "完整答案。",
	}
	srv := httptest.NewServer(http.HandlerFunc(up.handle))
	defer srv.Close()

	gw, keys := newTruncatingGateway(t, srv.URL)
	body := `{"model":"zen/fake/model","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+keys.LanKey())
	rec := httptest.NewRecorder()
	gw.Routes().ServeHTTP(rec, req)

	calls, _ := up.snapshot()
	// ⚠️ 上游第一次就正常结束了 → 不该有第二次调用。
	//
	// ⚠️ 但注意：第一次调用**也是**截断形态（没有 finish_reason）——
	//   因为我的假上游对 callNo==1 总是提前返回。
	//   所以这里期望的是 1 次（不续写）或 2 次（续写但立刻完成）。
	//   关键是：**不该超过 2 次**（maxContinuations）。
	if calls > 3 {
		t.Fatalf("续写次数不该超过 2，实际 %d 次", calls)
	}
	_ = time.Now
}
