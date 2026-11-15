package credit

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

// 这一组锁的是"领取的开关只能管领取"。推理车道读 CooldownUntil，
// 领取分档读 ClaimCooldownUntil —— 两者一旦被写成同一个字段，
// 签到撞一次限流就会让这个号在几十秒内不接对话请求。

func TestClassifyClaimError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		kind string
		hold time.Duration
	}{
		{"凭据过期 401", &APIError{Status: http.StatusUnauthorized, Message: "上游拒绝"}, claimKindCredential, claimHoldForever},
		{"适配器自报 token_expired", &APIError{Status: 401, Code: "token_expired", Message: "TRAE 需要先刷新访问凭据"}, claimKindCredential, claimHoldForever},
		{"套餐上限 1005", &APIError{Status: 200, Code: "1005", Message: "已达套餐上限"}, claimKindPlan, 12 * time.Hour},
		{"限流 429", &APIError{Status: http.StatusTooManyRequests, Message: "上游返回 HTTP 429"}, claimKindRate, time.Minute},
		{"签到人数过多 9074", &APIError{Status: 200, Code: "9074", Message: "签到人数过多"}, claimKindRate, time.Minute},
		{"上游 5xx", &APIError{Status: http.StatusBadGateway, Message: "上游返回 HTTP 502"}, claimKindServer, 10 * time.Minute},
		{"连不上上游", ErrUpstreamTransport, claimKindServer, 10 * time.Minute},
		{"答非所问", ErrUpstreamBadBody, claimKindServer, 10 * time.Minute},
		{"90 秒领取超时", context.DeadlineExceeded, claimKindServer, 10 * time.Minute},
		// 下面这些都必须"不罚号"：换一个号拿到的判决一样，停账号只是把整池领哑。
		{"请求本身不合法 400", &APIError{Status: http.StatusBadRequest, Message: "参数不合法"}, "", 0},
		{"渠道没有该领取项", errors.New("TRAE 不支持该领取项目"), "", 0},
		{"凭据缺字段", errors.New("Qoder 凭据缺少 machineId"), "", 0},
		{"成功", nil, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := classifyClaimError(c.err)
			if got.kind != c.kind || (c.kind != "" && got.hold != c.hold) {
				t.Fatalf("分类不对：%+v，期望 kind=%q hold=%s", got, c.kind, c.hold)
			}
		})
	}
}

func TestClaimCoolingWindow(t *testing.T) {
	now := time.Now().UnixMilli()
	for _, c := range []struct {
		name  string
		until int64
		want  bool
	}{
		{"没设过", 0, false},
		{"已过点", now - 1000, false},
		{"还在冷却", now + 60_000, true},
		{"停到人工解除", -1, true},
	} {
		if got := claimCooling(c.until, now); got != c.want {
			t.Fatalf("%s：claimCooling(%d)=%v，期望 %v", c.name, c.until, got, c.want)
		}
	}
}

// holdAdapter 只实现到领取路径用得上的部分；上游一律不发请求。
type holdAdapter struct {
	creditsErr error
	claimErr   error
	claims     int
	// lastCred 记下适配器**实际收到**的凭据，用来验证全局设备身份确实补进了调用。
	lastCred Credential
}

func (h *holdAdapter) StartLogin(context.Context, string) (*Login, error) {
	return nil, errors.New("测试不走登录")
}
func (h *holdAdapter) PollLogin(context.Context, *Login, string) (Credential, bool, error) {
	return nil, false, errors.New("测试不走登录")
}
func (h *holdAdapter) Validate(Credential) error { return nil }
func (h *holdAdapter) Refresh(context.Context, Credential) (Credential, error) {
	return nil, errors.New("测试不刷新凭据")
}
func (h *holdAdapter) Models(context.Context, Credential) ([]Model, error) { return []Model{}, nil }
func (h *holdAdapter) Credits(_ context.Context, c Credential) (Credits, error) {
	h.lastCred = c
	if h.creditsErr != nil {
		return Credits{}, h.creditsErr
	}
	return Credits{Campaigns: []Campaign{{ID: "daily", Name: "每日签到", Claimable: true}}}, nil
}
func (h *holdAdapter) Claim(_ context.Context, c Credential, _ string) (ClaimResult, error) {
	h.lastCred = c
	h.claims++
	if h.claimErr != nil {
		return ClaimResult{ID: "daily", Name: "每日签到"}, h.claimErr
	}
	return ClaimResult{ID: "daily", Name: "每日签到", Claimed: true}, nil
}
func (h *holdAdapter) Prepare(context.Context, Credential, Inference) (Prepared, error) {
	return Prepared{}, errors.New("测试不推理")
}

func holdFixture(t *testing.T) (*Manager, *holdAdapter) {
	t.Helper()
	db, vault := newTestStore(t)
	// q1 的时刻写 00:00：任何钟点都已到点，好让"跳没跳过"只由分档决定。
	// models 里放一个模型，是为了能用 Candidates 断言"车道还认这个号"。
	sealState(t, db, vault, `{"accounts":[`+
		`{"id":"q1","provider":"qoder","name":"号一","enabled":true,"models":[{"id":"m1","name":"M1"}],`+qoderCredJSON+`,"autoClaimAt":"00:00"}],`+
		`"lastAutoClaim":{}}`)
	m := openManager(t, db, vault)
	stub := &holdAdapter{}
	m.adapters = map[string]Adapter{"qoder": stub, "qodercn": stub, "trae": stub}
	return m, stub
}

func holdOf(t *testing.T, m *Manager, id string) (string, int64, int64) {
	t.Helper()
	kind, until := m.ClaimHold(id)
	for _, a := range m.Snapshot().Accounts {
		if a.ID == id {
			return kind, until, a.CooldownUntil
		}
	}
	t.Fatalf("快照里没有 %s", id)
	return "", 0, 0
}

// 领取失败要记下自己的档位，并且一步都不碰车道那个闸门。
func TestClaimFailureHoldsClaimOnly(t *testing.T) {
	m, stub := holdFixture(t)
	stub.claimErr = &APIError{Status: http.StatusTooManyRequests, Message: "上游返回 HTTP 429"}

	if _, err := m.Claim(context.Background(), "q1", ""); err != nil {
		t.Fatalf("逐活动失败不该让整个请求报错: %v", err)
	}
	kind, until, lane := holdOf(t, m, "q1")
	if kind != claimKindRate || until <= time.Now().UnixMilli() {
		t.Fatalf("429 应当记成 rate 档并留到 60 秒后：%q %d", kind, until)
	}
	if lane != 0 {
		t.Fatalf("领取失败不许写车道闸门 CooldownUntil，实得 %d", lane)
	}
	// 车道照旧挑得中这个号 —— 领额度挂了与它能不能转发对话无关。
	if got := m.Candidates("qoder", "m1", "q1"); len(got) != 1 {
		t.Fatalf("冷却中的账号仍应是推理候选，实得 %d 个", len(got))
	}

	// 自动那条要跳过；跳过时不盖当天的章，冷却过后当天还能补领。
	stub.claims = 0
	m.autoClaim(context.Background())
	if stub.claims != 0 {
		t.Fatalf("冷却期间自动领取不许碰上游，实发 %d 次", stub.claims)
	}
	m.mu.RLock()
	_, stamped := m.state.LastAutoClaim["q1"]
	m.mu.RUnlock()
	if stamped {
		t.Fatal("被冷却挡住时不该盖当天的章")
	}

	// 重置状态 = 人工解除，之后自动那条恢复。
	if err := m.Reset("q1"); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	if kind, _, _ := holdOf(t, m, "q1"); kind != "" {
		t.Fatalf("重置状态必须清掉领取档位，实得 %q", kind)
	}
	m.autoClaim(context.Background())
	if stub.claims != 1 {
		t.Fatalf("解除后自动领取应当试一次，实发 %d 次", stub.claims)
	}
}

// 手动签到是用户按下的，冷却不许挡它；而一次正常应答的领取要把旧档位清掉。
func TestManualClaimBypassesHoldAndSuccessClears(t *testing.T) {
	m, stub := holdFixture(t)
	stub.claimErr = &APIError{Status: 200, Code: "1005", Message: "已达套餐上限"}

	if _, err := m.Claim(context.Background(), "q1", ""); err != nil {
		t.Fatalf("领取应当返回逐项结果: %v", err)
	}
	kind, until, _ := holdOf(t, m, "q1")
	if kind != claimKindPlan || until <= time.Now().Add(11*time.Hour).UnixMilli() {
		t.Fatalf("1005 应当记成 plan 档、约 12 小时：%q %d", kind, until)
	}

	stub.claimErr = nil
	stub.claims = 0
	if _, err := m.Claim(context.Background(), "q1", ""); err != nil {
		t.Fatalf("冷却中的手动领取不该被挡: %v", err)
	}
	if stub.claims != 1 {
		t.Fatal("手动签到必须照打上游（冷却只挡自动）")
	}
	if kind, until, _ := holdOf(t, m, "q1"); kind != "" || until != 0 {
		t.Fatalf("这一轮上游正常应答，旧档位必须清除：%q %d", kind, until)
	}
}

// 凭据失效那一档不带到期时间：只有重新登录、刷新凭据或重置状态能解除。
func TestCredentialHoldWaitsForHuman(t *testing.T) {
	m, stub := holdFixture(t)
	stub.claimErr = &APIError{Status: 401, Code: "token_expired", Message: "TRAE 需要先刷新访问凭据"}

	if _, err := m.Claim(context.Background(), "q1", ""); err != nil {
		t.Fatalf("领取应当返回逐项结果: %v", err)
	}
	kind, until, _ := holdOf(t, m, "q1")
	if kind != claimKindCredential || until >= 0 {
		t.Fatalf("401 应当记成不带到期时间的暂停：%q %d", kind, until)
	}
	// 时间往前走一天，自动那条依然不许碰它。
	if !claimCooling(until, time.Now().Add(24*time.Hour).UnixMilli()) {
		t.Fatal("凭据档不能随时间自己失效")
	}
	stub.claims = 0
	m.autoClaim(context.Background())
	if stub.claims != 0 {
		t.Fatalf("凭据已死时自动领取不许再撞，实发 %d 次", stub.claims)
	}
}

// 预读失败够不上档位时要**保留**旧档位：一次本地报错不该把 12 小时上限抹掉，
// 否则第二天又会照撞一堵已经明说"到上限了"的墙。
func TestUnclassifiedPreReadFailureKeepsHold(t *testing.T) {
	m, stub := holdFixture(t)
	stub.claimErr = &APIError{Status: http.StatusTooManyRequests, Message: "上游返回 HTTP 429"}
	if _, err := m.Claim(context.Background(), "q1", ""); err != nil {
		t.Fatalf("先制造一个 rate 档: %v", err)
	}
	_, before, _ := holdOf(t, m, "q1")

	stub.creditsErr = errors.New("Qoder 凭据缺少 machineId")
	if _, err := m.Claim(context.Background(), "q1", ""); err == nil {
		t.Fatal("预读失败要把错误抛给调用方")
	}
	kind, after, _ := holdOf(t, m, "q1")
	if kind != claimKindRate || after != before {
		t.Fatalf("够不上档位的失败不许动旧档位：%q %d（原 %q %d）", kind, after, claimKindRate, before)
	}
}
