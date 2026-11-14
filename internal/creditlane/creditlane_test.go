package creditlane

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aBon579/free-model-house/internal/credit"
	"github.com/aBon579/free-model-house/internal/lane"
)

// 假后端：把 credit.Manager 收窄成的那六个方法换成内存版。
//
// 帧形翻译与账号轮换这两件事**跟凭据无关** —— 真 Manager 要 vault、要 sqlite、
// 要真登录，而这两条逻辑必须在没有凭据的环境里也能跑起来。
type fakeBackend struct {
	mu       sync.Mutex
	attempts []string
	records  []string // "ok <id>" / "fail <id> <code> <delay>"

	accounts   []credit.Account
	providers  []credit.Provider
	urls       map[string]string
	wires      map[string]string
	prepareErr map[string]error // 让 Prepare 阶段就失败（根本不碰网络）
}

func (f *fakeBackend) Snapshot() credit.Snapshot {
	return credit.Snapshot{
		Accounts:  append([]credit.Account(nil), f.accounts...),
		Providers: append([]credit.Provider(nil), f.providers...),
	}
}

func (f *fakeBackend) Candidates(provider, model, accountID string) []credit.Account {
	out := []credit.Account{}
	for _, a := range f.accounts {
		if a.Provider != provider || !a.Enabled {
			continue
		}
		if accountID != "" && a.ID != accountID {
			continue
		}
		if a.CooldownUntil > time.Now().UnixMilli() {
			continue
		}
		if contains(a.DisabledModels, model) {
			continue
		}
		for _, m := range a.Models {
			if m.ID == model {
				out = append(out, a)
				break
			}
		}
	}
	return out
}

func (f *fakeBackend) Prepare(_ context.Context, id string, in credit.Inference) (credit.Prepared, error) {
	f.mu.Lock()
	f.attempts = append(f.attempts, id)
	f.mu.Unlock()
	if err := f.prepareErr[id]; err != nil {
		return credit.Prepared{}, err
	}
	return credit.Prepared{
		URL:     f.urls[id],
		Headers: map[string]string{"content-type": "application/json"},
		Body:    []byte(`{"model":"` + in.Model.ID + `"}`),
		Wire:    f.wires[id],
	}, nil
}

func (f *fakeBackend) RecordSuccess(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, "ok "+id)
}

func (f *fakeBackend) RecordFailure(id string, err error, delay time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	code := "nil"
	var e *lane.Error
	if errors.As(err, &e) {
		code = string(e.Code)
	}
	f.records = append(f.records, fmt.Sprintf("fail %s %s %s", id, code, delay))
}

func (f *fakeBackend) HTTPClient() *http.Client { return &http.Client{} }

func (f *fakeBackend) tried() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.attempts, ",")
}

func (f *fakeBackend) logged() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.records, "|")
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// ── 夹具 ────────────────────────────────────────────────────────────

// upstream 起一个假上游，按**请求次序**依次用这几个 handler。
//
// 用完后重复最后一个 —— 轮换测试只关心"哪个账号被试过、第几次成功"，
// 而账号次序就是请求次序（候选按池子顺序排队）。
func upstream(t *testing.T, handlers ...http.HandlerFunc) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	n := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := n
		n++
		mu.Unlock()
		if i >= len(handlers) {
			i = len(handlers) - 1
		}
		handlers[i](w, r)
	}))
	t.Cleanup(s.Close)
	return s
}

// sseHandler 回一段固定的 SSE 文本。
func sseHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, body)
	}
}

func sseURL(t *testing.T, body string) string {
	t.Helper()
	return upstream(t, sseHandler(body)).URL
}

func quotaHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"quota exceeded"}}`)
	}
}

func account(id, provider, model string) credit.Account {
	return credit.Account{
		ID:       id,
		Provider: provider,
		Enabled:  true,
		Models:   []credit.Model{{ID: model, Name: model, ContextWindow: 1000}},
	}
}

// laneFor 造一条车道：每个账号打同一个 url、用同一种帧形。
func laneFor(url, wire string, accounts ...credit.Account) (*Lane, *fakeBackend) {
	f := &fakeBackend{
		accounts:   accounts,
		providers:  []credit.Provider{{ID: "qoder", Name: "Qoder"}, {ID: "trae", Name: "TRAE"}},
		urls:       map[string]string{},
		wires:      map[string]string{},
		prepareErr: map[string]error{},
	}
	for _, a := range accounts {
		f.urls[a.ID] = url
		f.wires[a.ID] = wire
	}
	return New(f), f
}

func turnFor(model string) *lane.Turn {
	return &lane.Turn{
		Model:     model,
		Messages:  []lane.Message{{Role: lane.RoleUser, Parts: []lane.Part{lane.TextPart("hi")}}},
		MaxTokens: 128,
		Seed:      "seed",
	}
}

// seen 是一次调用里真正收到的东西。
type seen struct {
	text     string
	reason   string
	args     string
	toolName string
	toolID   string
	usage    *lane.Usage
}

func (s *seen) emit(ch lane.Chunk) {
	switch ch.Kind {
	case lane.ChunkTextDelta:
		s.text += ch.Delta
	case lane.ChunkReasonDelta:
		s.reason += ch.Delta
	case lane.ChunkToolCallDelta:
		s.args += ch.Arguments
	case lane.ChunkBlockStart:
		if ch.BlockType == lane.BlockToolCall {
			s.toolName, s.toolID = ch.Name, ch.ID
		}
	case lane.ChunkUsage:
		s.usage = ch.Usage
	}
}

// qoderFrame 把一帧内容包成 Qoder 的响应信封。
//
// ⚠️ 内层是**一个 JSON 字符串**，不是嵌套对象 —— 真实上游就是这么发的，
//
//	解析要先反转义再解一次。测试里写成嵌套对象会放过一个真 bug。
func qoderFrame(body string) string {
	inner := "null"
	if body != "" {
		inner = strconv.Quote(body)
	}
	return fmt.Sprintf("data:{\"headers\":{},\"body\":%s,\"statusCodeValue\":200,\"statusCode\":\"OK\"}\n\n", inner)
}

// qoderError 拼一条"顶层是 HTTP 样子、业务码埋在内层"的错误帧。
func qoderError(inner string) string {
	return qoderFrame(`{"code":403,"message":` + strconv.Quote(inner) + `,"type":"model_error"}`)
}

func chunkBody(delta, finish string) string {
	parts := []string{}
	if delta != "" {
		parts = append(parts, `"delta":{`+delta+`}`)
	}
	if finish != "" {
		parts = append(parts, `"finish_reason":"`+finish+`"`)
	}
	return `{"choices":[{"index":0,` + strings.Join(parts, ",") + `}]}`
}

func stopFrame() string { return qoderFrame(chunkBody("", "stop")) }

func textFrame(s string) string { return qoderFrame(chunkBody(`"content":"`+s+`"`, "")) }

func reasoningFrame(s string) string {
	return qoderFrame(chunkBody(`"reasoning_content":"`+s+`"`, ""))
}

func usageFrame(prompt, completion, reasoning int) string {
	return qoderFrame(fmt.Sprintf(`{"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"completion_tokens_details":{"reasoning_tokens":%d}}}`,
		prompt, completion, reasoning))
}

func billingFrame() string {
	return qoderFrame(`{"code":110,"message":"Billing daily count exceeded","type":"model_error"}`)
}

// ── Qoder 帧形 ──────────────────────────────────────────────────────

func TestQoderStreamTextReasoningToolAndUsage(t *testing.T) {
	body := qoderFrame(chunkBody(`"role":"assistant"`, "")) +
		reasoningFrame("think") +
		textFrame("he") + textFrame("llo") +
		// 心跳帧：body 是 null —— 绝不能被当成业务错误。
		qoderFrame("") +
		qoderFrame(chunkBody(`"tool_calls":[{"index":0,"id":"call_1","function":{"name":"lookup","arguments":"{\"q\""}}]`, "")) +
		qoderFrame(chunkBody(`"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]`, "")) +
		qoderFrame(chunkBody("", "tool_calls")) +
		usageFrame(11, 7, 4) +
		"data: [DONE]\n\n"

	l, f := laneFor(sseURL(t, body), "qoder", account("a1", "qoder", "chat_model"))
	var got seen
	usage, finish, uerr := l.Do(context.Background(), turnFor("qoder/chat_model"), got.emit)
	if uerr != nil {
		t.Fatalf("Do: %v", uerr)
	}
	if got.text != "hello" {
		t.Errorf("text = %q, want hello", got.text)
	}
	if got.reason != "think" {
		t.Errorf("reasoning = %q, want think", got.reason)
	}
	if got.args != `{"q":1}` {
		t.Errorf("tool args = %q（两段增量要拼成一个）", got.args)
	}
	if got.toolName != "lookup" || got.toolID != "call_1" {
		t.Errorf("tool start = %q/%q", got.toolName, got.toolID)
	}
	if finish != lane.FinishToolCalls {
		t.Errorf("finish = %q, want tool-calls", finish)
	}
	if usage == nil || usage.Input != 11 || usage.Output != 7 || usage.Reasoning != 4 {
		t.Errorf("usage = %+v", usage)
	}
	if got.usage == nil || got.usage.Input != 11 {
		t.Errorf("usage chunk not emitted: %+v", got.usage)
	}
	if logged := f.logged(); logged != "ok a1" {
		t.Errorf("records = %q", logged)
	}
}

func TestQoderQueuedErrorReadsNestedCodeNotTopLevel(t *testing.T) {
	// 实测形状：顶层 code 是 403，业务码 10605 在 message 里**又套一层 JSON 串**。
	// 参考实现第一版写成 `if (data.code === '10605')` —— 拿 403 比 10605 必然不命中，
	// 排队于是被归成"可重试的 5xx"，客户端按 500…8000ms 连打五次（约 15.5 秒），
	// 而服务端要等 30 秒：**永远等不到**，只白烧了整轮预算。
	queued := qoderError(`{"code":"10605","message":` +
		strconv.Quote(`{"isQueued":true,"queueCount":3,"retryAfterSeconds":30}`) + `}`)
	l, _ := laneFor(sseURL(t, queued), "qoder", account("a1", "qoder", "m"))
	_, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), nil)
	if uerr == nil {
		t.Fatal("want error")
	}
	if uerr.Code != lane.CodeQuota {
		t.Errorf("code = %s, want RATE_LIMIT（排队要算限流，不能算 5xx）", uerr.Code)
	}
	if uerr.RetryAfterSec < queueWaitSec {
		t.Errorf("retryAfter = %d, want >= %d", uerr.RetryAfterSec, queueWaitSec)
	}
	// ⚠️ message 原样保留、不拼后缀：拼上 "(10605)" 会破坏里面那段内层 JSON，
	//   二次解析直接失败，退避时间只能退回兜底值。
	if !strings.HasPrefix(uerr.Message, "模型正在排队") {
		t.Errorf("message = %q", uerr.Message)
	}
}

func TestQueueDelayPrefersServerValue(t *testing.T) {
	// 上游确实会下发 30 以外的值（实测有 retryAfterSeconds=2 的瞬时排队，
	// 用户报障原话就是"一次重试就能成功"）。写死兜底值会让该等的没等够。
	quick := qoderError(`{"code":"10605","message":` + strconv.Quote(`{"isQueued":true,"retryAfterSeconds":2}`) + `}`)
	l, _ := laneFor(sseURL(t, quick), "qoder", account("a1", "qoder", "m"))
	_, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), nil)
	if uerr == nil {
		t.Fatal("want error")
	}
	if uerr.Code != lane.CodeQuota || uerr.RetryAfterSec != 2 {
		t.Errorf("got %s/%d, want RATE_LIMIT/2", uerr.Code, uerr.RetryAfterSec)
	}
}

func TestTransientQueueWithServiceAvailable(t *testing.T) {
	// ⚠️ 实测的**瞬时排队**形状：isQueued 是 **false**，只有 serviceAvailable:true
	//	和 retryAfterSeconds:2 —— 用户报障原话是"一次重试就能成功"。
	//	判据若要求 isQueued===true 它就落到兜底退避；外层那个 403 若挡住业务码，
	//	它整个被当成 SERVER 白烧一轮。两层都得穿过。
	transient := qoderError(`{"code":"10605","message":` +
		strconv.Quote(`{"isQueued":false,"serviceAvailable":true,"queueCount":1,"waitTime":0,"retryAfterSeconds":2}`) + `}`)
	l, _ := laneFor(sseURL(t, transient), "qoder", account("a1", "qoder", "m"))
	_, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), nil)
	if uerr == nil {
		t.Fatal("want error")
	}
	if uerr.Code != lane.CodeQuota || uerr.RetryAfterSec != 2 {
		t.Errorf("got %s/%d, want RATE_LIMIT/2", uerr.Code, uerr.RetryAfterSec)
	}
}

func TestQueuedFlagWithoutAnyBusinessCode(t *testing.T) {
	// 信封剥掉之后顶层可能压根没有 code —— isQueued 是唯一的线索。
	noCode := qoderError(`{"message":` + strconv.Quote(`{"isQueued":true,"retryAfterMs":8000}`) + `}`)
	l, _ := laneFor(sseURL(t, noCode), "qoder", account("a1", "qoder", "m"))
	_, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), nil)
	if uerr == nil {
		t.Fatal("want error")
	}
	if uerr.Code != lane.CodeQuota || uerr.RetryAfterSec != 8 {
		t.Errorf("got %s/%d, want RATE_LIMIT/8（毫秒要换算成秒）", uerr.Code, uerr.RetryAfterSec)
	}
}

func TestQueueDelayCappedNotInfinity(t *testing.T) {
	// 上游偶尔给一个分钟级的等待量。这里罚的是**这个账号**，
	// 照单全收会让用户看到"这个模型突然全池不可用"，而实际只要等几秒。
	long := qoderError(`{"code":"10605","message":` + strconv.Quote(`{"isQueued":true,"retryAfterSeconds":86400}`) + `}`)
	l, _ := laneFor(sseURL(t, long), "qoder", account("a1", "qoder", "m"))
	_, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), nil)
	if uerr == nil {
		t.Fatal("want error")
	}
	if uerr.RetryAfterSec != queueWaitCapSec {
		t.Errorf("retryAfter = %d, want 封顶 %d", uerr.RetryAfterSec, queueWaitCapSec)
	}
}

func TestQoderBillingErrorIsNotRetryableServer(t *testing.T) {
	l, _ := laneFor(sseURL(t, billingFrame()), "qoder", account("a1", "qoder", "m"))
	_, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), nil)
	if uerr == nil {
		t.Fatal("want error")
	}
	if uerr.Code != lane.CodeQuota {
		t.Errorf("code = %s, want RATE_LIMIT", uerr.Code)
	}
	// ⚠️ 额度用尽是**当天**的确定性结论：归成可重试的 5xx 会让客户端白烧五轮整段对话。
	if uerr.RetrySameModel() {
		t.Errorf("billing 错误不该原地重试")
	}
	if !uerr.Switchable() {
		t.Errorf("额度类必须可切换 —— 换**账号**是唯一有效的杠杆")
	}
	// 状态必须是合法 HTTP 码：把业务码 110 写进响应行，客户端只会报"连不上"。
	if uerr.Status < 0 || uerr.Status > 599 {
		t.Errorf("status = %d, want 合法 HTTP 码或 0", uerr.Status)
	}
}

func TestQoderGatewayShapedErrorNotSwallowed(t *testing.T) {
	// 既没有 code 也没有 error，只有 statusCodeValue + message。
	// 早期解析器三条判据全不命中 → 整帧丢弃 → 客户端收到一个空回答且毫无报错。
	body := qoderFrame(`{"statusCodeValue":503,"stackTrace":"...","message":"upstream busy"}`)
	l, _ := laneFor(sseURL(t, body), "qoder", account("a1", "qoder", "m"))
	_, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), nil)
	if uerr == nil {
		t.Fatal("want error —— 这帧不能被当成心跳丢掉")
	}
	if !strings.Contains(uerr.Message, "upstream busy") {
		t.Errorf("message = %q", uerr.Message)
	}
}

func TestQoderPlainEnvelopePassesThrough(t *testing.T) {
	// 万一哪天上游直接回标准帧（不带信封），得照常工作。
	body := "data:" + chunkBody(`"content":"ok"`, "") + "\n\n" +
		"data:" + chunkBody("", "stop") + "\n\n" +
		"data: [DONE]\n\n"
	l, _ := laneFor(sseURL(t, body), "qoder", account("a1", "qoder", "m"))
	var got seen
	if _, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), got.emit); uerr != nil {
		t.Fatalf("Do: %v", uerr)
	}
	if got.text != "ok" {
		t.Errorf("text = %q", got.text)
	}
}

// ── 风控码：上游在 HTTP 层回的 JSON 错误体 ─────────────────────────────

func TestEnvelopeFailureKeepsNonJSONText(t *testing.T) {
	// 上游也可能直接回一句纯文本（网关自己吐的 "Internal Server Error"）。
	// 这种没有码可拆，原样退回，别把它改写成"认不出来的帧"。
	plain := &lane.Error{Code: lane.CodeServer, Status: http.StatusInternalServerError, Message: "Internal Server Error"}
	if got := envelopeFailure(plain); got != plain {
		t.Errorf("非 JSON 错误体该原样退回，实际 %+v", got)
	}
}

// ── TRAE 帧形 ───────────────────────────────────────────────────────

func TestTraeNamedEvents(t *testing.T) {
	body := "event: metadata\n" + "data:{\"trace\":\"x\"}\n\n" +
		"event: output\n" + "data:{\"reasoning_content\":\"想\"}\n\n" +
		"event: output\n" + "data:{\"response\":\"你\"}\n\n" +
		"event: output\n" + "data:{\"response\":\"好\"}\n\n" +
		"event: output\n" + `data:{"tool_calls":[{"index":0,"id":"c1","function_call":{"name":"lookup","arguments":"{\"a\":1}","namespace":"ns","partial_arguments":"{"}}]}` + "\n\n" +
		"event: token_usage\n" + `data:{"prompt_tokens":100,"completion_tokens":9,"reasoning_tokens":3,"prompt_cache_hit_tokens":80}` + "\n\n" +
		"event: timing_cost\n" + "data:{\"ms\":12}\n\n" +
		"event: done\n" + `data:{"finish_reason":"tool_calls"}` + "\n\n"

	l, f := laneFor(sseURL(t, body), "trae", account("a1", "trae", "solo"))
	var got seen
	usage, finish, uerr := l.Do(context.Background(), turnFor("trae/solo"), got.emit)
	if uerr != nil {
		t.Fatalf("Do: %v", uerr)
	}
	if got.text != "你好" {
		t.Errorf("text = %q", got.text)
	}
	if got.reason != "想" {
		t.Errorf("reasoning = %q", got.reason)
	}
	if got.args != `{"a":1}` || got.toolName != "lookup" {
		t.Errorf("tool = %q/%q（线上字段叫 function_call，必须改名成 function）", got.toolName, got.args)
	}
	if finish != lane.FinishToolCalls {
		t.Errorf("finish = %s", finish)
	}
	// ⚠️ 缓存命中写在 prompt_cache_hit_tokens 上 —— 只读 Anthropic 那个名字
	//   会让命中率恒为 0，账本把每一次都按未命中计价。
	if usage == nil || usage.Input != 100 || usage.Output != 9 || usage.Reasoning != 3 || usage.CacheRead != 80 {
		t.Errorf("usage = %+v", usage)
	}
	if got.usage == nil {
		t.Error("usage chunk not emitted")
	}
	if f.logged() != "ok a1" {
		t.Errorf("records = %q", f.logged())
	}
}

func TestTraeInStreamErrorCodes(t *testing.T) {
	// 这些码**只有 TRAE 自己认得**：措辞是中文的，ClassifyFailure 的英文正则
	// 认不出来；HTTP 又是 200，走状态码分支也认不出来。
	cases := []struct {
		data string
		want lane.Code
	}{
		{`{"code":4008,"message":"额度用尽"}`, lane.CodeQuota},
		{`{"code":"4011","message":"slow down"}`, lane.CodeQuota},
		{`{"code":401,"message":"session dead"}`, lane.CodeCredential},
		{`{"code":1005,"message":"plan"}`, lane.CodePermission},
		{`{"code":4001,"message":"param is invalid"}`, lane.CodeClient},
	}
	for _, tc := range cases {
		body := "event: error\n" + "data:" + tc.data + "\n\n"
		l, _ := laneFor(sseURL(t, body), "trae", account("a1", "trae", "solo"))
		_, _, uerr := l.Do(context.Background(), turnFor("trae/solo"), nil)
		if uerr == nil {
			t.Fatalf("%s: want error", tc.data)
		}
		if uerr.Code != tc.want {
			t.Errorf("%s → %s, want %s", tc.data, uerr.Code, tc.want)
		}
	}
}

func TestTraeDoneWithoutFinishReasonStillCloses(t *testing.T) {
	body := "event: output\n" + "data:{\"response\":\"嗨\"}\n\n" + "event: done\n" + "data:{}\n\n"
	l, _ := laneFor(sseURL(t, body), "trae", account("a1", "trae", "solo"))
	var got seen
	_, finish, uerr := l.Do(context.Background(), turnFor("trae/solo"), got.emit)
	if uerr != nil {
		t.Fatalf("Do: %v —— TRAE 没有 [DONE]，收尾全靠 done 事件", uerr)
	}
	if finish != lane.FinishStop {
		t.Errorf("finish = %s, want stop", finish)
	}
	if got.text != "嗨" {
		t.Errorf("text = %q", got.text)
	}
}

// ── 帧形翻译：认不出的帧形必须报错 ─────────────────────────────────

func TestUnknownWireIsRefusedNotGuessed(t *testing.T) {
	// ⚠️ 认不出帧形就退回 chat 的话，最可能的结果是"内容全空但流正常结束"——
	//   客户端收到一个没字的回答，日志里什么都没有。
	l, _ := laneFor(sseURL(t, "data:{}\n\n"), "brand_new_protocol", account("a1", "qoder", "m"))
	_, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), nil)
	if uerr == nil || uerr.Code != lane.CodeServer {
		t.Fatalf("uerr = %+v, want SERVER", uerr)
	}
	if !strings.Contains(uerr.Message, "brand_new_protocol") {
		t.Errorf("message = %q —— 要说清是哪个帧形不认识", uerr.Message)
	}
}

// ── 账号轮换 ────────────────────────────────────────────────────────

func TestRotatesToNextAccountWhenQuota(t *testing.T) {
	// 两个账号打**同一个**假上游：第 1 次请求 429，第 2 次正常。
	// 候选按池子顺序排队，所以"第几次请求"就是"第几个账号"。
	srv := upstream(t, quotaHandler(), sseHandler(textFrame("通了")+stopFrame()))
	l, f := laneFor(srv.URL, "qoder", account("a1", "qoder", "m"), account("a2", "qoder", "m"))

	var got seen
	_, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), got.emit)
	if uerr != nil {
		t.Fatalf("Do: %v", uerr)
	}
	if got.text != "通了" {
		t.Errorf("text = %q", got.text)
	}
	if f.tried() != "a1,a2" {
		t.Errorf("tried = %q, want a1 再 a2", f.tried())
	}
	logged := f.logged()
	if !strings.Contains(logged, "fail a1 RATE_LIMIT") || !strings.Contains(logged, "ok a2") {
		t.Errorf("records = %q（被拒的账号要被冷却，成功的要清状态）", logged)
	}
}

func TestStopsRotatingAfterContentWasEmitted(t *testing.T) {
	// 换账号重放会从头再生成一遍 —— 已经发出去的那些字会变成两遍。
	// 这与路由器"committed 之后不再换候选"是同一条规矩，只是粒度更细。
	half := textFrame("半句") + billingFrame()
	srv := upstream(t, sseHandler(half), sseHandler(textFrame("重来")+stopFrame()))
	l, f := laneFor(srv.URL, "qoder", account("a1", "qoder", "m"), account("a2", "qoder", "m"))
	var got seen
	_, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), got.emit)
	if uerr == nil {
		t.Fatal("want error")
	}
	if f.tried() != "a1" {
		t.Errorf("tried = %q —— 发过内容之后不许换账号", f.tried())
	}
	if got.text != "半句" {
		t.Errorf("text = %q", got.text)
	}
}

func TestRequestLevelFailureDoesNotWalkTheWholePool(t *testing.T) {
	// 参数不合法（TRAE 的 4001 这类）是**请求自己的错**：
	// 每个账号都会给同样的拒绝，轮换只是把一次失败放大成"账号数 × 一次完整上游请求"，
	// 既慢又白烧额度。
	badParam := qoderFrame(`{"code":4001,"message":"param is invalid","type":"model_error"}`)
	srv := upstream(t, sseHandler(badParam), sseHandler(textFrame("不该打到")+stopFrame()))
	l, f := laneFor(srv.URL, "qoder", account("a1", "qoder", "m"), account("a2", "qoder", "m"))
	_, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), nil)
	if uerr == nil || uerr.Code != lane.CodeClient {
		t.Fatalf("uerr = %v (code %v), want CLIENT", uerr, codeOf(uerr))
	}
	if f.tried() != "a1" {
		t.Errorf("tried = %q, want 只试 a1", f.tried())
	}
	// ⚠️ 原因要记（Credit 页得显示为什么失败），但**不能冷却**：
	//   因为客户端写错参数而把一张好卡关进小黑屋，是最难查的那种自伤。
	if logged := f.logged(); logged != "fail a1 CLIENT_ERROR 0s" {
		t.Errorf("records = %q, want 记原因、零冷却", logged)
	}
}

func codeOf(e *lane.Error) lane.Code {
	if e == nil {
		return ""
	}
	return e.Code
}

func TestPrepareFailureStillRotates(t *testing.T) {
	// Prepare 不碰网络（Qoder 那边要跑 wasm 签名），它的失败与账号状态无关，
	// 但**必须**换下一个：不然一次本地抖动就把整个池子判死。
	srv := upstream(t, sseHandler(textFrame("好")+stopFrame()))
	l, f := laneFor(srv.URL, "qoder", account("a1", "qoder", "m"), account("a2", "qoder", "m"))
	f.prepareErr["a1"] = errors.New("wasm 没能构建请求")
	var got seen
	if _, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), got.emit); uerr != nil {
		t.Fatalf("Do: %v", uerr)
	}
	if f.tried() != "a1,a2" {
		t.Errorf("tried = %q", f.tried())
	}
	if got.text != "好" {
		t.Errorf("text = %q", got.text)
	}
	if !strings.Contains(f.logged(), "fail a1") || !strings.Contains(f.logged(), "ok a2") {
		t.Errorf("records = %q", f.logged())
	}
}

func TestEveryAccountFailingReturnsLastError(t *testing.T) {
	srv := upstream(t, quotaHandler())
	l, f := laneFor(srv.URL, "qoder", account("a1", "qoder", "m"), account("a2", "qoder", "m"))
	_, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), nil)
	if uerr == nil || uerr.Code != lane.CodeQuota {
		t.Fatalf("uerr = %+v", uerr)
	}
	if f.tried() != "a1,a2" {
		t.Errorf("tried = %q", f.tried())
	}
}

func TestCancelledContextStopsImmediately(t *testing.T) {
	srv := upstream(t, sseHandler(textFrame("不该发出")+stopFrame()))
	l, f := laneFor(srv.URL, "qoder", account("a1", "qoder", "m"), account("a2", "qoder", "m"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, uerr := l.Do(ctx, turnFor("qoder/m"), nil)
	if uerr == nil || uerr.Code != lane.CodeAborted {
		t.Fatalf("uerr = %+v", uerr)
	}
	if f.tried() != "" {
		t.Errorf("tried = %q —— 客户端都走了，还打上游做什么", f.tried())
	}
}

func TestNoCandidateReasonsAreDistinct(t *testing.T) {
	off := account("a1", "qoder", "m")
	off.DisabledModels = []string{"m"}
	cooling := account("a2", "qoder", "m")
	cooling.CooldownUntil = time.Now().Add(time.Minute).UnixMilli()
	deactivated := account("a3", "qoder", "m")
	deactivated.Enabled = false

	cases := []struct {
		name     string
		accounts []credit.Account
		want     lane.Code
	}{
		{"模型被关掉", []credit.Account{off}, lane.CodeClient},
		{"账号在冷却", []credit.Account{cooling}, lane.CodeQuota},
		{"账号被停用", []credit.Account{deactivated}, lane.CodeCredential},
		{"没有账号带它", []credit.Account{account("a4", "qoder", "别的")}, lane.CodeCredential},
	}
	// ⚠️ 四种情形**必须给不同结论**：用户要看懂"等一会"和"没戏了"的区别，
	//   路由器更要靠错码决定还试不试别的候选。
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, f := laneFor(sseURL(t, textFrame("不该打到")+stopFrame()), "qoder", tc.accounts...)
			_, _, uerr := l.Do(context.Background(), turnFor("qoder/m"), nil)
			if uerr == nil {
				t.Fatal("want error")
			}
			if uerr.Code != tc.want {
				t.Errorf("code = %s, want %s", uerr.Code, tc.want)
			}
			if strings.TrimSpace(uerr.Message) == "" {
				t.Error("message 不能空 —— 界面要拿它告诉用户为什么")
			}
			if f.tried() != "" {
				t.Errorf("tried = %q —— 没有候选就不该打上游", f.tried())
			}
		})
	}
}

// ── 模型池 ──────────────────────────────────────────────────────────

func TestListModelsDedupesAndSkipsDisabled(t *testing.T) {
	a1 := account("a1", "qoder", "m1")
	// 0× —— 上游明说这次推理不扣额度，`Free` 才会是 true。
	zero := 0.0
	a1.Models = append(a1.Models, credit.Model{ID: "m2", Name: "第二个", Reasoning: true, Efforts: []string{"low", "high"}, Multiplier: &zero})
	// 第二个账号带着同样的 m1 —— 清单里只能有一行（两行同名用户会以为是两个模型）。
	a2 := account("a2", "qoder", "m1")
	a2.DisabledModels = []string{"m2"}
	// 停用的账号**一个模型都不贡献**。
	a3 := account("a3", "trae", "solo")
	a3.Enabled = false

	l, _ := laneFor("", "qoder", a1, a2, a3)
	models, uerr := l.ListModels(context.Background())
	if uerr != nil {
		t.Fatalf("ListModels: %v", uerr)
	}
	ids := []string{}
	for _, m := range models {
		ids = append(ids, m.ID)
	}
	want := "credit/qoder/m1,credit/qoder/m2"
	if got := strings.Join(ids, ","); got != want {
		t.Errorf("ids = %q, want %s", got, want)
	}

	byID := map[string]lane.ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	if byID["credit/qoder/m2"].Wire != "chat" {
		t.Errorf("qoder wire = %s", byID["credit/qoder/m2"].Wire)
	}
	info := byID["credit/qoder/m2"]
	if !info.Reasoning || !info.CanDisableThinking || len(info.Efforts) != 2 {
		t.Errorf("info = %+v", info)
	}
	if info.Lane != LaneID || !info.Enabled || !info.Free {
		t.Errorf("info = %+v", info)
	}
	if info.Name != "Qoder · 第二个" {
		t.Errorf("name = %q —— 多来源池子里不带渠道名的清单没法看", info.Name)
	}
	// 能力结论来自上游自己的声明，比名称启发式硬。
	if info.Capability != lane.CapabilitySourceListing {
		t.Errorf("capability = %s, want listing", info.Capability)
	}
	if !l.Ready() {
		t.Error("Ready = false, want true")
	}
	status := l.Status()
	if status.ModelCount != 2 || status.AutoProbe || !status.NeedsCredential || !status.Ready {
		t.Errorf("status = %+v", status)
	}
	if status.Reason != "" {
		t.Errorf("reason = %q, want 空（可用时不该给理由）", status.Reason)
	}
}

func TestFreeMeansUpstreamZeroMultiplier(t *testing.T) {
	// `Free` 的语义是**这次推理不扣额度**，不是"它来自 credit 车道"。
	// 0× 的 Qwen3.8-Flash 与 1.4× 的 Kimi-K3 必须在清单里分得开 ——
	// 用户就是按这个挑模型的，标错等于让他拿付费模型当免费的烧。
	free, paid := 0.0, 1.4
	cases := []struct {
		name string
		m    credit.Model
		free bool
		want *float64
	}{
		{"0× 算免费", credit.Model{ID: "qfmodel", Multiplier: &free}, true, &free},
		{"1.4× 不算免费", credit.Model{ID: "kmodel_latest", Multiplier: &paid}, false, &paid},
		{"上游没说 → 不猜免费", credit.Model{ID: "coding-plan", Multiplier: nil}, false, nil},
	}
	for _, c := range cases {
		info := modelInfo("Qoder", "qoder", c.m)
		if info.Free != c.free {
			t.Errorf("%s: Free = %v, want %v", c.name, info.Free, c.free)
		}
		switch {
		case c.want == nil && info.Multiplier != nil:
			t.Errorf("%s: Multiplier = %v, want nil", c.name, *info.Multiplier)
		case c.want != nil && (info.Multiplier == nil || *info.Multiplier != *c.want):
			t.Errorf("%s: Multiplier = %v, want %v", c.name, info.Multiplier, *c.want)
		}
	}
}

func TestForcedThinkingModelCannotDisableThinking(t *testing.T) {
	// reasoning=true 且没有档位 = 强制思考的模型，路由要按这个加倍输出预算。
	a := account("a1", "trae", "m")
	a.Models = []credit.Model{{ID: "m", Reasoning: true}}
	l, _ := laneFor("", "trae", a)
	info := l.ListModels0()[0]
	if info.CanDisableThinking {
		t.Error("CanDisableThinking = true, want false（上游没给档位就是关不掉）")
	}
}

func TestNotReadyWithoutAccounts(t *testing.T) {
	l, _ := laneFor("", "qoder")
	if l.Ready() {
		t.Error("Ready = true，池子是空的")
	}
	status := l.Status()
	if status.Reason == "" {
		t.Error("界面上要能解释为什么不 Ready")
	}
	// ⚠️ 这两条断言是"手动测试才会花额度"的**唯一**结构性保证。
	//   靠"记得别探测它"迟早会漏。
	if l.AutoProbeable() {
		t.Error("credit 池绝不能参与周期探测 —— 那是每 10 分钟烧一次用户自己的额度")
	}
	if !l.NeedsCredential() {
		t.Error("NeedsCredential 必须为 true")
	}
	if l.ID() != LaneID {
		t.Errorf("id = %q", l.ID())
	}
}

func TestAllModelsOffIsNotReady(t *testing.T) {
	a := account("a1", "qoder", "m")
	a.DisabledModels = []string{"m"}
	l, _ := laneFor("", "qoder", a)
	if l.Ready() {
		t.Error("Ready = true，但模型全被关掉了")
	}
	if len(l.ListModels0()) != 0 {
		t.Error("关掉的模型必须从清单里消失，不然客户端还能点名为它、然后收到一个看不懂的失败")
	}
	if l.Status().Reason == "" {
		t.Error("要解释为什么不 Ready")
	}
}

func TestBadModelIDShape(t *testing.T) {
	l, f := laneFor(sseURL(t, textFrame("不该打到")+stopFrame()), "qoder", account("a1", "qoder", "m"))
	_, _, uerr := l.Do(context.Background(), turnFor("只有提供方"), nil)
	if uerr == nil || uerr.Code != lane.CodeClient {
		t.Fatalf("uerr = %+v", uerr)
	}
	if f.tried() != "" {
		t.Errorf("tried = %q —— id 都不对，不该打上游", f.tried())
	}
}

func TestSessionIDDropsRouterSuffix(t *testing.T) {
	// 路由器在 seed 后面拼 `\x00<车道>`，那是**本地**区分符，
	// 原样送进 JSON 会让上游收到带控制字符的会话 id。
	if got := sessionID("abc\x00credit"); got != "abc" {
		t.Errorf("sessionID = %q", got)
	}
	if got := sessionID(" abc "); got != "abc" {
		t.Errorf("sessionID = %q", got)
	}
}

func TestFormatModelIDSurvivesLanePrefixSplit(t *testing.T) {
	// 第一个斜杠是**车道**分隔（网关按它切），提供方前缀是车道内的第二层。
	id := FormatModelID("qoder", "model/with-slash")
	laneID, rest, ok := strings.Cut(id, "/")
	if !ok || laneID != "credit" {
		t.Fatalf("id = %q", id)
	}
	p, m, ok := splitModel(rest)
	if !ok || p != "qoder" || m != "model/with-slash" {
		t.Errorf("split = %q/%q (%v)", p, m, ok)
	}
}

// ── 手动探测 ────────────────────────────────────────────────────────

func TestProbeReportsAvailability(t *testing.T) {
	l, f := laneFor(sseURL(t, textFrame("pong")+stopFrame()), "qoder", account("a1", "qoder", "m"))
	res := l.Probe(context.Background(), "credit/qoder/m")
	if res.State != lane.StateAvailable {
		t.Fatalf("state = %s (%s)", res.State, res.Detail)
	}
	// ⚠️ 回填必须用**带车道前缀**的完整 id —— 不同车道有同名模型，
	//   不带前缀两边的结论会互相覆盖。
	if res.Model != "credit/qoder/m" {
		t.Errorf("model = %q", res.Model)
	}
	if res.LatencyMs < 0 || res.TTFTMs < 0 {
		t.Errorf("timing = %d/%d", res.LatencyMs, res.TTFTMs)
	}
	if f.tried() != "a1" {
		t.Errorf("tried = %q", f.tried())
	}
}

func TestProbeOnQuotaIsThrottledNotUnavailable(t *testing.T) {
	// ⚠️ 状态判断必须与内置车道**共用一份**逻辑：同一个错误两边各说一套，
	//   用户看到的是"这个模型一会儿被限流、一会儿不可用"。
	l, _ := laneFor(upstream(t, quotaHandler()).URL, "qoder", account("a1", "qoder", "m"))
	res := l.Probe(context.Background(), "credit/qoder/m")
	if res.State != lane.StateThrottled {
		t.Fatalf("state = %s (%s), want throttled", res.State, res.Detail)
	}
	// 但**措辞**是车道自己的事 —— "换出口"那种话对 credit 没意义。
	if strings.Contains(res.Detail, "出口") {
		t.Errorf("detail = %q", res.Detail)
	}
}

func TestProbeBadID(t *testing.T) {
	l, _ := laneFor("", "qoder")
	res := l.Probe(context.Background(), "credit")
	if res.State == lane.StateAvailable {
		t.Errorf("state = %s, want 非可用", res.State)
	}
	if res.Detail == "" {
		t.Error("detail 要说明 id 形状不对")
	}
}
