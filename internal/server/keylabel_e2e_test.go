package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aBon579/free-model-house/internal/store"
)

// 这一组测试覆盖「用量页的调用 Key 到底记的是哪一把」。
//
// ⚠️ 全程用**内存库** —— 本机文件型 SQLite 不落盘（环境限制），
//   但内存库跑的是同一套 migrate / PutUsage / ModelUsageAll。

// newKeysForTest 建一个带默认「主 Key」的钥匙管理器。
func newKeysForTest(t *testing.T) (*Keys, *store.DB) {
	t.Helper()
	db, err := store.OpenMemory()
	if err != nil {
		t.Fatalf("打开内存库失败: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewKeys(db), db
}

// TestKeyMatchIsExactNotPrefix 验证前缀相近的 Key 不会互相命中。
func TestKeyMatchIsExactNotPrefix(t *testing.T) {
	d, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for i, row := range []store.ApiKeyRow{
		{ID: "a", Key: "fmh-abc", Label: "A", Tier: "deep", Enabled: true, CreatedAt: 1},
		{ID: "b", Key: "fmh-abcdef", Label: "B", Tier: "deep", Enabled: true, CreatedAt: 2},
		{ID: "c", Key: "fmh-abcdef-extra", Label: "C", Tier: "deep", Enabled: true, CreatedAt: 3},
	} {
		row.CreatedAt = int64(i + 1)
		if err := d.PutApiKey(row); err != nil {
			t.Fatal(err)
		}
	}
	k := NewKeys(d)
	for key, want := range map[string]string{
		"fmh-abc":          "A",
		"fmh-abcdef":       "B",
		"fmh-abcdef-extra": "C",
		"fmh-ab":           "",
	} {
		got, ok := k.Match(key)
		if want == "" {
			if ok {
				t.Fatalf("%q 不该命中任何 Key", key)
			}
			continue
		}
		if !ok || got.Label != want {
			t.Fatalf("%q 应命中标签 %q，实际 ok=%v label=%q", key, want, ok, got.Label)
		}
	}
}

// TestGatewayRecordsActualKeyLabel 验证：
// 两把不同标签的 Key 分别调用 → context 里是各自的标签。
func TestGatewayRecordsActualKeyLabel(t *testing.T) {
	keys, _ := newKeysForTest(t)
	mainKey := keys.LanKey()
	created, err := keys.Create("test", "deep")
	if err != nil {
		t.Fatal(err)
	}

	g := &Gateway{keys: keys}
	capture := func(w http.ResponseWriter, r *http.Request) {
		info := keyInfoOf(r)
		w.Header().Set("X-Test-Label", info.Label)
		w.Header().Set("X-Test-ID", info.ID)
		w.Header().Set("X-Test-Hint", info.Hint)
		w.WriteHeader(http.StatusOK)
	}
	h := g.handle(capture)

	for _, tc := range []struct {
		name   string
		auth   string
		xkey   string
		want   string
		wantID string
	}{
		{name: "主 Key", auth: "Bearer " + mainKey, want: keys.LabelOfMust(t, mainKey), wantID: ""},
		{name: "test Key", xkey: created.Key, want: "test", wantID: created.ID},
		// ⚠️ 同时带两个头：真正有效的那把（X-Api-Key）优先，
		//   否则会出现"用了 test，账记在主 Key 上"。
		{name: "两个头都有", auth: "Bearer " + mainKey, xkey: created.Key, want: "test", wantID: created.ID},
	} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if tc.auth != "" {
			r.Header.Set("Authorization", tc.auth)
		}
		if tc.xkey != "" {
			r.Header.Set("X-Api-Key", tc.xkey)
		}
		rec := httptest.NewRecorder()
		h(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: 应放行，实际 %d（%s）", tc.name, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("X-Test-Label"); got != tc.want {
			t.Fatalf("%s: 标签应为 %q，实际 %q", tc.name, tc.want, got)
		}
		if tc.wantID != "" && rec.Header().Get("X-Test-ID") != tc.wantID {
			t.Fatalf("%s: KeyID 应为 %q，实际 %q", tc.name, tc.wantID, rec.Header().Get("X-Test-ID"))
		}
		if rec.Header().Get("X-Test-Hint") == "" {
			t.Fatalf("%s: 应带 Key 指纹", tc.name)
		}
	}
}

// LabelOfMust 是测试辅助：读标签并在失败时直接 fail。
func (k *Keys) LabelOfMust(t *testing.T, key string) string {
	t.Helper()
	label, ok := k.LabelOf(key)
	if !ok {
		t.Fatalf("Key %q 未命中", key)
	}
	return label
}

// TestCreateRejectsDuplicateLabel 验证同名标签不能在创建时重复。
func TestCreateRejectsDuplicateLabel(t *testing.T) {
	keys, _ := newKeysForTest(t)
	if _, err := keys.Create("家里", "standard"); err != nil {
		t.Fatalf("第一次创建应成功: %v", err)
	}
	// ⚠️ 前后空格要被归一化 —— " 家里 " 与 "家里" 是同一个名字。
	for _, label := range []string{"家里", " 家里 "} {
		if _, err := keys.Create(label, "standard"); err == nil {
			t.Fatalf("重复标签 %q 应当被拒绝", label)
		} else if !strings.Contains(err.Error(), "已被其他 Key 使用") {
			t.Fatalf("错误信息应说明重名，实际: %v", err)
		}
	}
	// 默认「主 Key」也不能被抢注。
	if _, err := keys.Create("主 Key", "standard"); err == nil {
		t.Fatal("默认「主 Key」的标签不应被新建 Key 占用")
	}
	// ⚠️ 大小写不敏感 —— "Phone" 与 "phone" 在界面上看起来是同一个名字。
	if _, err := keys.Create("Phone", "standard"); err != nil {
		t.Fatalf("第一次创建 Phone 应成功: %v", err)
	}
	if _, err := keys.Create("phone", "standard"); err == nil {
		t.Fatal("大小写不同但同名应当被拒绝")
	}
	// 标签必填 —— 空标签（含全空白）直接拒绝。
	for _, label := range []string{"", "   "} {
		if _, err := keys.Create(label, "standard"); err == nil {
			t.Fatalf("空标签 %q 应当被拒绝", label)
		} else if !strings.Contains(err.Error(), "标签必填") {
			t.Fatalf("错误信息应说明必填，实际: %v", err)
		}
	}
}

// TestUpdateRequiresLabelAndBackfillsLegacyEmpty 验证：
// 历史上的空标签**不自动改**，但允许用户补填，且补填同样受必填/唯一约束。
func TestUpdateRequiresLabelAndBackfillsLegacyEmpty(t *testing.T) {
	_, db := newKeysForTest(t)
	legacy := store.ApiKeyRow{
		ID: "legacy-id", Key: "fmh-legacy", Label: "",
		Tier: "standard", Enabled: true, CreatedAt: 1,
	}
	if err := db.PutApiKey(legacy); err != nil {
		t.Fatal(err)
	}
	keys := NewKeys(db)

	// 空白 = 试图改成空标签 → 拒绝。
	if _, err := keys.Update(legacy.ID, "   ", "", nil); err == nil {
		t.Fatal("改名成全空白应被拒绝")
	} else if !strings.Contains(err.Error(), "标签必填") {
		t.Fatalf("错误信息应说明必填，实际: %v", err)
	}

	// 正常补填 → 成功（这是历史空标签的过渡方式）。
	got, err := keys.Update(legacy.ID, "补名", "", nil)
	if err != nil {
		t.Fatalf("补填应成功: %v", err)
	}
	if got.Label != "补名" {
		t.Fatalf("补填后标签应为「补名」，实际 %q", got.Label)
	}

	// 补填也要唯一。
	other, err := keys.Create("已占用", "standard")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Update(other.ID, "补名", "", nil); err == nil {
		t.Fatal("改成已被占用的标签应被拒绝")
	}
}

// TestUpdateRejectsDuplicateLabel 验证改名时同样要唯一。
func TestUpdateRejectsDuplicateLabel(t *testing.T) {
	keys, _ := newKeysForTest(t)
	if _, err := keys.Create("甲", "standard"); err != nil {
		t.Fatal(err)
	}
	b, err := keys.Create("乙", "standard")
	if err != nil {
		t.Fatal(err)
	}
	// 乙 → 甲：撞名要拒绝。
	if _, err := keys.Update(b.ID, "甲", "", nil); err == nil {
		t.Fatal("改成别人的标签应当被拒绝")
	}
	// 乙 → 乙：保持自己原来的名字可以（`exceptID` 生效）。
	if got, err := keys.Update(b.ID, "乙", "", nil); err != nil {
		t.Fatalf("保持自己的标签应成功: %v", err)
	} else if got.Label != "乙" {
		t.Fatalf("标签不应变化，实际 %q", got.Label)
	}
	// 乙 → 丙：没被占用，允许。
	if got, err := keys.Update(b.ID, "丙", "", nil); err != nil {
		t.Fatalf("改成未占用标签应成功: %v", err)
	} else if got.Label != "丙" {
		t.Fatalf("标签应为「丙」，实际 %q", got.Label)
	}
	if _, err := keys.Update(b.ID, "甲", "", nil); err == nil {
		t.Fatal("再次撞名应当被拒绝")
	}
}
