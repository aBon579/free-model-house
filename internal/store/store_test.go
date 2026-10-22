package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 密封存储

func TestVaultSealAndOpen(t *testing.T) {
	dir := t.TempDir()
	v, err := NewVault(dir)
	if err != nil {
		t.Fatalf("建 vault 失败: %v", err)
	}
	secret := "sk-or-v1-EXAMPLEKEYFORTESTSONLY0000"
	sealed, err := v.Seal(secret)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if sealed == "" {
		t.Fatal("密文不该为空")
	}
	// ⚠️ 密文**不得**包含明文。
	if strings.Contains(sealed, secret) {
		t.Fatal("密文里出现了明文")
	}
	open, err := v.Open(sealed)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if open != secret {
		t.Fatalf("解密结果不对: %q", open)
	}
}

func TestVaultNonceIsFreshEveryTime(t *testing.T) {
	// ⚠️ 同一个明文两次加密必须得到**不同**的密文 ——
	// 否则相同凭据在数据库里一眼就能看出"这是同一个"。
	v, _ := NewVault(t.TempDir())
	secret := "same-secret"
	a, _ := v.Seal(secret)
	b, _ := v.Seal(secret)
	if a == b {
		t.Fatal("两次加密的结果不该相同")
	}
	// 但都该解回原文。
	oa, _ := v.Open(a)
	ob, _ := v.Open(b)
	if oa != secret || ob != secret {
		t.Fatalf("解密结果不对: %q %q", oa, ob)
	}
}

func TestVaultRejectsWrongKey(t *testing.T) {
	// ⚠️ 认证失败**绝不能**返回"部分正确"的结果。
	dirA := t.TempDir()
	dirB := t.TempDir()
	vA, _ := NewVault(dirA)
	vB, _ := NewVault(dirB)

	sealed, _ := vA.Seal("secret")
	out, err := vB.Open(sealed)
	if err == nil {
		t.Fatal("错的密钥必须解密失败")
	}
	if out != "" {
		t.Fatalf("失败时不该返回内容: %q", out)
	}
}

func TestVaultKeyFileIsCreated(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewVault(dir); err != nil {
		t.Fatalf("建 vault 失败: %v", err)
	}
	path := filepath.Join(dir, ".secret-key")
	if _, err := readFile(path); err != nil {
		t.Fatalf("密钥文件没生成: %v", err)
	}
}

func TestVaultReusesExistingKey(t *testing.T) {
	// 重启后必须用**同一个**密钥 —— 否则已存的凭据解不开。
	dir := t.TempDir()
	v1, _ := NewVault(dir)
	sealed, _ := v1.Seal("secret")

	v2, _ := NewVault(dir)
	out, err := v2.Open(sealed)
	if err != nil {
		t.Fatalf("重启后该能解密: %v", err)
	}
	if out != "secret" {
		t.Fatalf("解密结果不对: %q", out)
	}
}

func TestRedactRemovesBothSpellings(t *testing.T) {
	secret := "sk-or-v1-EXAMPLEKEYFORTESTSONLY0000"
	// 原文。
	text := "the key is " + secret + " here"
	out := Redact(text, secret)
	if strings.Contains(out, secret) {
		t.Fatalf("没脱敏: %q", out)
	}
	// ⚠️ JSON 转义形式也要脱敏 ——
	// 文本可能已经被 JSON 编码过了。
	escaped, _ := jsonMarshalString(secret)
	jsonText := `{"key":"` + escaped + `"}`
	out2 := Redact(jsonText, secret)
	if strings.Contains(out2, escaped) {
		t.Fatalf("JSON 转义形式没脱敏: %q", out2)
	}
}

func TestCredentialHint(t *testing.T) {
	if CredentialHint("") != "" {
		t.Fatal("空 key 的指纹应为空")
	}
	h := CredentialHint("sk-or-v1-EXAMPLEKEYFORTESTSONLY0000")
	if strings.Contains(h, "EXAMPLEKEYFORTESTSONLY") {
		t.Fatalf("指纹不该含完整 key: %q", h)
	}
	if !strings.HasSuffix(h, "0000") {
		t.Fatalf("指纹应保留后四位: %q", h)
	}
}

func TestGenerateKeyFormat(t *testing.T) {
	// ⚠️ 用户要求："项目自己生成一个固定前缀的 API key"，
	// 前缀是 `fmh-`。
	key := GenerateKey("fmh-")
	if !strings.HasPrefix(key, "fmh-") {
		t.Fatalf("前缀不对: %q", key)
	}
	// 20 字节 = 40 hex。
	body := strings.TrimPrefix(key, "fmh-")
	if len(body) != 40 {
		t.Fatalf("key 主体长度不对: %q", key)
	}
	// 必须是合法的 hex。
	if _, err := hexDecode(body); err != nil {
		t.Fatalf("key 主体不是合法 hex: %q", body)
	}
	// 两次生成必须不同。
	if GenerateKey("fmh-") == GenerateKey("fmh-") {
		t.Fatal("两次生成不该相同")
	}
}

// 数据库

func newTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSettingsRoundTrip(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetSetting("k", "v"); err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	v, ok, err := db.GetSetting("k")
	if err != nil || !ok || v != "v" {
		t.Fatalf("读设置不对: %q %v %v", v, ok, err)
	}
	_, ok, _ = db.GetSetting("nope")
	if ok {
		t.Fatal("不存在的键不该返回存在")
	}
	// 覆盖。
	db.SetSetting("k", "v2")
	v, _, _ = db.GetSetting("k")
	if v != "v2" {
		t.Fatalf("覆盖失败: %q", v)
	}
}

func TestBoolAndIntSettings(t *testing.T) {
	db := newTestDB(t)
	if !db.GetBool("missing", true) {
		t.Fatal("缺失时该用默认值")
	}
	db.SetSetting("flag", "1")
	if !db.GetBool("flag", false) {
		t.Fatal("布尔设置不对")
	}
	db.SetSetting("num", "42")
	if db.GetInt("num", 0) != 42 {
		t.Fatal("整数设置不对")
	}
	if db.GetInt("missing", 7) != 7 {
		t.Fatal("缺失时该用默认值")
	}
}

func TestSecretRoundTrip(t *testing.T) {
	db := newTestDB(t)
	if err := db.PutSecret("openrouter", "sealed-value", "sk-or-…abcd"); err != nil {
		t.Fatalf("存密文失败: %v", err)
	}
	sealed, hint, found, err := db.GetSecret("openrouter")
	if err != nil || !found {
		t.Fatalf("读密文失败: %v %v", found, err)
	}
	if sealed != "sealed-value" || hint != "sk-or-…abcd" {
		t.Fatalf("内容不对: %q %q", sealed, hint)
	}
	// 删除。
	db.DeleteSecret("openrouter")
	_, _, found, _ = db.GetSecret("openrouter")
	if found {
		t.Fatal("删除后不该还存在")
	}
}

func TestSessionPersistence(t *testing.T) {
	// ⚠️ 会话映射**必须持久化** ——
	// 重启后复用同一个 session，prompt cache 才不会每次冷启动。
	dir := t.TempDir()
	db, err := Open(dir)
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	db.PutSession("seed-1", "ses_abc")
	db.PutSession("seed-2", "ses_def")
	db.Close()

	db2, err := Open(dir)
	if err != nil {
		t.Fatalf("重开库失败: %v", err)
	}
	defer db2.Close()
	sessions, err := db2.LoadSessions()
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	if sessions["seed-1"] != "ses_abc" || sessions["seed-2"] != "ses_def" {
		t.Fatalf("会话没恢复: %v", sessions)
	}
}

func TestThrottlePersistence(t *testing.T) {
	db := newTestDB(t)
	row := ThrottleRow{
		Model:         "m1",
		ThrottledAt:   1000,
		CooldownUntil: 2000,
		LastOK:        500,
		Episodes:      `[{"start":1000,"end":0}]`,
	}
	if err := db.PutThrottle(row); err != nil {
		t.Fatalf("写限流失败: %v", err)
	}
	rows, err := db.LoadThrottle()
	if err != nil {
		t.Fatalf("读限流失败: %v", err)
	}
	got, ok := rows["m1"]
	if !ok {
		t.Fatal("限流记录没读到")
	}
	if got.CooldownUntil != 2000 || got.Episodes != row.Episodes {
		t.Fatalf("内容不对: %+v", got)
	}
}

func TestProbePersistence(t *testing.T) {
	db := newTestDB(t)
	db.PutProbe(ProbeRow{
		Model: "m1", State: "available", Detail: "ok",
		Latency: 1200, TTFT: 800, ProbedAt: time.Now().UnixMilli(),
	})
	rows, err := db.LoadProbes()
	if err != nil {
		t.Fatalf("读探测失败: %v", err)
	}
	if rows["m1"].State != "available" || rows["m1"].TTFT != 800 {
		t.Fatalf("内容不对: %+v", rows["m1"])
	}
}

func TestUsageAggregation(t *testing.T) {
	db := newTestDB(t)
	today := "2026-10-19"
	for i := 0; i < 3; i++ {
		db.PutUsage(UsageRecord{
			At: time.Now().UnixMilli(), Day: today,
			Lane: "zen", Model: "m1", ActualModel: "m1", Client: "c1",
			Requests: 1, Turns: 1, Attempts: 1,
			InputTokens: 100, OutputTokens: 50,
		})
	}
	total, err := db.Total("2000-01-01")
	if err != nil {
		t.Fatalf("读总量失败: %v", err)
	}
	if total.Requests != 3 || total.InputTokens != 300 || total.OutputTokens != 150 {
		t.Fatalf("总量不对: %+v", total)
	}
	daily, err := db.DailyUsageRange("2000-01-01", "2099-01-01")
	if err != nil {
		t.Fatalf("读按天失败: %v", err)
	}
	if len(daily) != 1 || daily[0].Requests != 3 {
		t.Fatalf("按天不对: %+v", daily)
	}
	byModel, err := db.ModelUsageAll("2000-01-01")
	if err != nil {
		t.Fatalf("读按模型失败: %v", err)
	}
	if len(byModel) != 1 || byModel[0].Model != "m1" || byModel[0].Requests != 3 {
		t.Fatalf("按模型不对: %+v", byModel)
	}
}

func TestModelToggles(t *testing.T) {
	db := newTestDB(t)
	db.SetModelEnabled("m1", false)
	db.SetModelEnabled("m2", true)
	toggles, err := db.LoadModelToggles()
	if err != nil {
		t.Fatalf("读开关失败: %v", err)
	}
	if toggles["m1"] {
		t.Error("m1 应是关的")
	}
	if !toggles["m2"] {
		t.Error("m2 应是开的")
	}
}

func TestJSONSettings(t *testing.T) {
	db := newTestDB(t)
	type payload struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	if err := db.PutJSON("cfg", payload{Name: "x", Count: 3}); err != nil {
		t.Fatalf("写 JSON 失败: %v", err)
	}
	var got payload
	ok, err := db.GetJSON("cfg", &got)
	if err != nil || !ok {
		t.Fatalf("读 JSON 失败: %v %v", ok, err)
	}
	if got.Name != "x" || got.Count != 3 {
		t.Fatalf("内容不对: %+v", got)
	}
}
