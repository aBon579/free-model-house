package credit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// 这一组锁的是 Qoder 每日 100 Credits 的**根因**：上游只在请求带上
// Cosy-MachineToken + Cosy-MachineType（成对）时才下发 CLAIM_BENEFIT 活动。
// 网关自己（NAS / 容器）没有客户端可以现场生成这两个值，所以它们存在
// 全局状态里，由装了客户端的机器送一份进来。

func TestSetMachineIdentityRequiresPair(t *testing.T) {
	m, _ := holdFixture(t)
	if err := m.SetMachineIdentity("tok", ""); err == nil {
		t.Fatal("只给 machineToken 不给 machineType，必须报错")
	}
	if err := m.SetMachineIdentity("", "typ"); err == nil {
		t.Fatal("只给 machineType 不给 machineToken，必须报错")
	}
	if m.Snapshot().Machine.Configured {
		t.Fatal("半个身份不该算「已配置」——上游会退回备用身份，症状和没活动一模一样")
	}
	if err := m.SetMachineIdentity("  tok  ", " typ "); err != nil {
		t.Fatalf("成对提供不该报错：%v", err)
	}
	status := m.Snapshot().Machine
	if !status.Configured || status.Hint == "" {
		t.Fatalf("成对提供后应已配置并带指纹：%+v", status)
	}
	// 两个都空 = 清除，这是界面「清掉」唯一的走法。
	if err := m.SetMachineIdentity("", ""); err != nil {
		t.Fatalf("清除不该报错：%v", err)
	}
	if m.Snapshot().Machine.Configured {
		t.Fatal("清除后仍显示已配置")
	}
}

func TestMachineIdentitySurvivesReload(t *testing.T) {
	db, vault := newTestStore(t)
	sealState(t, db, vault, `{"accounts":[],"lastAutoClaim":{}}`)
	m := openManager(t, db, vault)
	if err := m.SetMachineIdentity("tok-1", "typ-1"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	again := openManager(t, db, vault)
	if got := again.MachineIdentity().Text("machineToken"); got != "tok-1" {
		t.Fatalf("重开后身份丢了：machineToken=%q", got)
	}
}

func TestSnapshotMachineNeverLeaksToken(t *testing.T) {
	m, _ := holdFixture(t)
	const secret = "P1gASECRETdoNotLeakThisTokenValue"
	if err := m.SetMachineIdentity(secret, "SYNTHETIC-machineType-never-a-real-credential"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	b, err := json.Marshal(m.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), secret) {
		t.Fatal("快照每 30 秒被轮询一次，绝不能带 machineToken 原值")
	}
	if !strings.Contains(string(b), `"machine"`) {
		t.Fatalf("快照里没有 machine 状态，界面无法显示是否已配置：%s", b)
	}
}

func TestCredentialForPriority(t *testing.T) {
	db, vault := newTestStore(t)
	sealState(t, db, vault, `{"accounts":[],"lastAutoClaim":{}}`)
	m := openManager(t, db, vault)
	if err := m.SetMachineIdentity("global-tok", "global-typ"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	bare := m.credentialFor(storedAccount{Account: Account{Provider: "qoder"}, Credential: Credential{"accessToken": "at"}})
	if bare.Text("machineToken") != "global-tok" || bare.Text("machineType") != "global-typ" {
		t.Fatalf("缺设备身份的 Qoder 凭据没拿到全局值：%+v", bare)
	}
	if bare.Text("accessToken") != "at" {
		t.Fatal("补身份不能动账号原有凭据")
	}

	own := m.credentialFor(storedAccount{Account: Account{Provider: "qodercn"}, Credential: Credential{"machineToken": "own-tok", "machineType": "own-typ"}})
	if own.Text("machineToken") != "own-tok" || own.Text("machineType") != "own-typ" {
		t.Fatalf("账号自带的设备身份应优先于全局：%+v", own)
	}

	trae := m.credentialFor(storedAccount{Account: Account{Provider: "trae"}, Credential: Credential{"accessToken": "at"}})
	if _, ok := trae["machineToken"]; ok {
		t.Fatal("TRAE 用另一套领取接口，不该被塞进 Qoder 的设备身份")
	}

	// 没配全局时，Qoder 凭据保持原样（今天的线上行为）。
	if err := m.SetMachineIdentity("", ""); err != nil {
		t.Fatalf("清除失败：%v", err)
	}
	plain := m.credentialFor(storedAccount{Account: Account{Provider: "qoder"}, Credential: Credential{"accessToken": "at"}})
	if _, ok := plain["machineToken"]; ok {
		t.Fatal("没配全局身份却凭空多出 machineToken")
	}
}

// 界面靠快照里那个 machineReady 分清"这个号没有活动"和"网关缺设备身份"。
// 它是**算出来的**：账号自带的成对值或全局那一份，任一满足即就绪；TRAE 不吃这两个头，恒就绪。
func TestSnapshotMachineReady(t *testing.T) {
	db, vault := newTestStore(t)
	sealState(t, db, vault, `{"accounts":[`+
		`{"id":"q1","provider":"qoder","name":"号一","enabled":true,`+qoderCredJSON+`},`+
		`{"id":"q2","provider":"qoder","name":"号二","enabled":true,"credential":{"accessToken":"at-2","uid":"uid-2","machineId":"mid-2","machineToken":"own-tok","machineType":"own-typ"}},`+
		`{"id":"t1","provider":"trae","name":"T 号","enabled":true,"credential":{}}],`+
		`"lastAutoClaim":{}}`)
	m := openManager(t, db, vault)
	ready := map[string]bool{}
	for _, a := range m.Snapshot().Accounts {
		ready[a.ID] = a.MachineReady
	}
	if ready["q1"] {
		t.Fatal("什么设备身份都没有的 Qoder 号不该算就绪")
	}
	if !ready["q2"] || !ready["t1"] {
		t.Fatalf("自带成对值的号与 TRAE 都该就绪：%+v", ready)
	}

	if err := m.SetMachineIdentity("global-tok", "global-typ"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	for _, a := range m.Snapshot().Accounts {
		if !a.MachineReady {
			t.Fatalf("全局身份到位后 %s 仍显示不就绪", a.ID)
		}
	}
	if err := m.SetMachineIdentity("", ""); err != nil {
		t.Fatalf("清除失败：%v", err)
	}
	for _, a := range m.Snapshot().Accounts {
		if a.ID == "q1" && a.MachineReady {
			t.Fatal("清除全局身份后 q1 必须回到不就绪 —— 界面那句提醒要跟真值同步")
		}
	}
}

// 真正的根因验证：签到打到适配器的那份凭据里，必须带着全局身份。
// 少了这一步，"存了但没接上请求头" 一样领不到。
func TestClaimSendsGlobalIdentityToAdapter(t *testing.T) {
	m, stub := holdFixture(t)
	if err := m.SetMachineIdentity("global-tok", "global-typ"); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	if _, err := m.Claim(context.Background(), "q1", ""); err != nil {
		t.Fatalf("领取失败：%v", err)
	}
	if stub.lastCred.Text("machineToken") != "global-tok" || stub.lastCred.Text("machineType") != "global-typ" {
		t.Fatalf("适配器收到的凭据没带全局设备身份：%+v", stub.lastCred)
	}
}
