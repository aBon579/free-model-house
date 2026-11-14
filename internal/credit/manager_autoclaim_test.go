package credit

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/aBon579/free-model-house/internal/store"
)

// 自动领取的调度只依赖**状态**（时刻、上次尝试日、渠道下限），不依赖凭据，
// 所以这三件事都能离线测。反而一旦让它碰上游就是拿真账号烧额度。

func TestAutoClaimDue(t *testing.T) {
	for _, tc := range []struct {
		name      string
		value     string
		last      string
		today     string
		minuteNow int
		want      bool
	}{
		{"留空即关闭", "", "2026-11-13", "2026-11-14", 1439, false},
		{"到点", "00:05", "", "2026-11-14", 5, true},
		{"差一分钟", "00:05", "", "2026-11-14", 4, false},
		{"当天已经试过就不再碰", "10:05", "2026-11-14", "2026-11-14", 605, false},
		{"昨天试过今天照领", "10:05", "2026-11-13", "2026-11-14", 605, true},
		// 时刻写坏时**当作关闭**：领一次失败当天就不再重试，宁可什么都不做。
		{"写坏的时刻", "9:5", "", "2026-11-14", 1439, false},
		{"越界的时刻", "24:00", "", "2026-11-14", 1439, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := autoClaimDue(tc.value, tc.last, tc.today, tc.minuteNow)
			if got != tc.want {
				t.Fatalf("autoClaimDue(%q, last=%q, now=%d) = %v，期望 %v", tc.value, tc.last, tc.minuteNow, got, tc.want)
			}
		})
	}
}

func TestMigrateLegacyAutoClaim(t *testing.T) {
	db, vault := newTestStore(t)
	sealState(t, db, vault, `{"accounts":[`+
		`{"id":"q1","provider":"qoder","name":"Q 号","enabled":true,"credential":{}},`+
		`{"id":"q2","provider":"qoder","name":"已自定义","enabled":true,"autoClaimAt":"23:00","credential":{}},`+
		`{"id":"t1","provider":"trae","name":"T 号","enabled":true,"credential":{}}],`+
		`"lastAutoClaim":{},"autoClaim":true}`)
	// 旧开关开着 = 每个账号都要有时刻；但用户已经填过的不能被迁移覆盖。
	want := map[string]string{"q1": "10:05", "q2": "23:00", "t1": "00:05"}
	m := openManager(t, db, vault)
	// 先确认种子真读进来了，否则下面的循环会空转着通过。
	if got := len(m.Snapshot().Accounts); got != len(want) {
		t.Fatalf("读到 %d 个账号，期望 %d 个", got, len(want))
	}
	for _, a := range m.Snapshot().Accounts {
		if got := a.AutoClaimAt; got != want[a.ID] {
			t.Errorf("迁移后 %s 的时刻 = %q，期望 %q", a.ID, got, want[a.ID])
		}
	}

	// ⚠️ 旧键必须从**落盘的状态**里消失，否则每次启动都会重跑一遍迁移，
	//   把用户后来关掉的账号又打开。
	if _, ok := rawState(t, db, vault)["autoClaim"]; ok {
		t.Fatal("迁移后状态里仍留着 autoClaim 旧键")
	}

	// 重启一次：迁移不该再有东西可改。
	reopened := openManager(t, db, vault)
	for _, a := range reopened.Snapshot().Accounts {
		if got := a.AutoClaimAt; got != want[a.ID] {
			t.Errorf("重开之后 %s 的时刻变成 %q，期望保持 %q", a.ID, got, want[a.ID])
		}
	}
}

func TestNoLegacySwitchMeansNoAutoClaim(t *testing.T) {
	db, vault := newTestStore(t)
	sealState(t, db, vault, `{"accounts":[`+
		`{"id":"q1","provider":"qoder","name":"Q 号","enabled":true,"credential":{}}],`+
		`"lastAutoClaim":{}}`)

	// 旧版**没开**总开关的用户，升级后不能莫名多出每日领取 —— 那是把他的额度花掉。
	accounts := openManager(t, db, vault).Snapshot().Accounts
	if len(accounts) != 1 {
		t.Fatalf("读到 %d 个账号，期望 1 个", len(accounts))
	}
	for _, a := range accounts {
		if a.AutoClaimAt != "" {
			t.Fatalf("%s 的自动领取被打开了（%q），但旧状态里开关是关的", a.ID, a.AutoClaimAt)
		}
	}
}

func TestPatchAutoClaimAtValidation(t *testing.T) {
	db, vault := newTestStore(t)
	sealState(t, db, vault, `{"accounts":[`+
		`{"id":"q1","provider":"qoder","name":"Q 号","enabled":true,"credential":{}},`+
		`{"id":"t1","provider":"trae","name":"T 号","enabled":true,"credential":{}}],`+
		`"lastAutoClaim":{}}`)
	m := openManager(t, db, vault)

	// Qoder 每日 10:00（UTC+8）刷新，早于它只会领失败，而当天不再重试。
	early := "09:00"
	if err := m.Patch(AccountPatch{ID: "q1", AutoClaimAt: &early}); err == nil {
		t.Fatal("Qoder 早于 10:00 的时刻必须拒绝")
	}
	ok := "10:30"
	if err := m.Patch(AccountPatch{ID: "q1", AutoClaimAt: &ok}); err != nil {
		t.Fatalf("Qoder 10:30 应当接受: %v", err)
	}
	// TRAE 没有固定刷新点，不设下限。
	if err := m.Patch(AccountPatch{ID: "t1", AutoClaimAt: &early}); err != nil {
		t.Fatalf("TRAE 09:00 应当接受（它没有刷新下限）: %v", err)
	}
	bad := "9:5"
	if err := m.Patch(AccountPatch{ID: "t1", AutoClaimAt: &bad}); err == nil {
		t.Fatal("非 HH:MM 的时刻必须拒绝")
	}
	off := ""
	if err := m.Patch(AccountPatch{ID: "t1", AutoClaimAt: &off}); err != nil {
		t.Fatalf("留空表示关闭，应当接受: %v", err)
	}
	if err := m.Patch(AccountPatch{ID: "missing", AutoClaimAt: &off}); err == nil {
		t.Fatal("账号不存在时要报错，不能静默通过")
	}

	got := map[string]string{}
	for _, a := range m.Snapshot().Accounts {
		got[a.ID] = a.AutoClaimAt
	}
	if got["q1"] != "10:30" || got["t1"] != "" {
		t.Fatalf("保存后的时刻不对：q1=%q t1=%q", got["q1"], got["t1"])
	}
}

// TestBackupRoundTripKeepsAutoClaimAt 验证备份能把领取时刻一起带走、一起回来。
//
// 导入重建的是**新账号**（id 由 Add 重新生成），所以这里按名称对齐。
func TestBackupRoundTripKeepsAutoClaimAt(t *testing.T) {
	srcDB, srcVault := newTestStore(t)
	sealState(t, srcDB, srcVault, `{"accounts":[`+
		`{"id":"q1","provider":"qoder","name":"有时刻","enabled":true,`+qoderCredJSON+`},`+
		`{"id":"q2","provider":"qoder","name":"没时刻","enabled":true,`+qoderCredJSON2+`}],`+
		`"lastAutoClaim":{}}`)
	src := openManager(t, srcDB, srcVault)
	at := "10:30"
	if err := src.Patch(AccountPatch{ID: "q1", AutoClaimAt: &at}); err != nil {
		t.Fatalf("设置时刻失败: %v", err)
	}

	dstDB, dstVault := newTestStore(t)
	dst := openManager(t, dstDB, dstVault)
	if _, err := dst.Import(context.Background(), src.Export()); err != nil {
		t.Fatalf("导入自己的备份失败: %v", err)
	}
	got := map[string]string{}
	for _, a := range dst.Snapshot().Accounts {
		got[a.Name] = a.AutoClaimAt
	}
	// ⚠️ 丢了时刻就等于"换机器之后不再自动领"，而界面上看着一切正常。
	if got["有时刻"] != "10:30" || got["没时刻"] != "" {
		t.Fatalf("还原后的时刻不对：有时刻=%q 没时刻=%q", got["有时刻"], got["没时刻"])
	}
}

// TestImportRejectsBadScheduleBeforeWriting 验证坏时刻**一个账号都不进**。
func TestImportRejectsBadScheduleBeforeWriting(t *testing.T) {
	db, vault := newTestStore(t)
	m := openManager(t, db, vault)

	// 旧备份没有 autoClaimAt 这个键 → 解成空串 = 不自动领，不能算错误。
	var legacy []BackupAccount
	if err := json.Unmarshal([]byte(`[{"provider":"qoder","name":"旧备份",`+qoderCredJSON+`}]`), &legacy); err != nil {
		t.Fatalf("测试自身前置失败: %v", err)
	}
	if legacy[0].AutoClaimAt != "" {
		t.Fatalf("缺键的备份该解成空串，实际 %q", legacy[0].AutoClaimAt)
	}
	if n, err := m.Import(context.Background(), legacy); err != nil || n != 1 {
		t.Fatalf("旧备份该照常导入，实际 n=%d err=%v", n, err)
	}

	early := BackupAccount{Provider: "qoder", Name: "填早了", Credential: qoderCred(), AutoClaimAt: "09:00"}
	before := len(m.Snapshot().Accounts)
	if n, err := m.Import(context.Background(), []BackupAccount{early}); err == nil {
		t.Fatal("早于渠道刷新点的备份时刻必须拒绝：领一次就烧掉当天唯一一次机会")
	} else if n != 0 {
		t.Fatalf("拒绝时该一个都没写，实际已导入 %d 个", n)
	}
	if after := len(m.Snapshot().Accounts); after != before {
		t.Fatalf("导入被拒之后账号数从 %d 变成 %d", before, after)
	}
}

// noUpstream 让"测试里碰上游"变成一次崩溃，而不是一次真额度消耗。
type noUpstream struct{}

func (noUpstream) RoundTrip(*http.Request) (*http.Response, error) {
	panic("credit 状态测试不许连上游")
}

// 导入要过 adapter.Validate，所以凭据得给全（这三项都是本地校验，不发上游）。
// 两个账号要**不同 uid**：同 uid 会被 Add 当同一个账号合并，测不出往返。
const (
	qoderCredJSON  = `"credential":{"accessToken":"at-1","uid":"uid-1","machineId":"mid-1"}`
	qoderCredJSON2 = `"credential":{"accessToken":"at-2","uid":"uid-2","machineId":"mid-2"}`
)

func qoderCred() Credential {
	return Credential{"accessToken": "at-secret", "uid": "uid-1", "machineId": "mid-1"}
}

func newTestStore(t *testing.T) (*store.DB, *store.Vault) {
	t.Helper()
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatalf("开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	vault, err := store.NewVault(t.TempDir())
	if err != nil {
		t.Fatalf("初始化密钥环失败: %v", err)
	}
	return db, vault
}

// sealState 直接塞一份状态明文，绕开 Add（它要校验凭据形状并打上游）。
func sealState(t *testing.T, db *store.DB, vault *store.Vault, plain string) {
	t.Helper()
	sealed, err := vault.Seal(plain)
	if err != nil {
		t.Fatalf("密封失败: %v", err)
	}
	if err := db.PutSecret(stateSecret, sealed, ""); err != nil {
		t.Fatalf("写入状态失败: %v", err)
	}
}

func openManager(t *testing.T, db *store.DB, vault *store.Vault) *Manager {
	t.Helper()
	m, err := NewManager(db, vault, &http.Client{Transport: noUpstream{}})
	if err != nil {
		t.Fatalf("建管理器失败: %v", err)
	}
	return m
}

// rawState 读出**已落盘**的状态，用来断言字段真的写回去了（而不是只在内存里）。
func rawState(t *testing.T, db *store.DB, vault *store.Vault) map[string]any {
	t.Helper()
	sealed, _, found, err := db.GetSecret(stateSecret)
	if err != nil || !found {
		t.Fatalf("读状态失败: found=%v err=%v", found, err)
	}
	plain, err := vault.Open(sealed)
	if err != nil {
		t.Fatalf("解密状态失败: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(plain), &out); err != nil {
		t.Fatalf("状态不是 JSON: %v", err)
	}
	return out
}
