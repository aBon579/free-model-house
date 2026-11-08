package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aBon579/free-model-house/internal/lane"
	"github.com/aBon579/free-model-house/internal/store"
)

// TestCustomKeyUpdateSyncsModelPool 验证编辑端点后模型池**立即**增删同步。
//
// ⚠️ 全程内存库 + httptest（不依赖文件型 SQLite）。
func TestCustomKeyUpdateSyncsModelPool(t *testing.T) {
	// 上游：只要能返回模型清单即可（不带 context 字段 → 走"探测不到"分支）。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"},{"id":"m3"}]}`))
	}))
	defer upstream.Close()

	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	vault := newVaultForTest(t)
	registry := lane.NewRegistry()
	registry.Register(lane.NewCustomLane())
	catalog := lane.NewCatalog()
	a := &Admin{db: db, vault: vault, lanes: registry, catalog: catalog}

	// 预置一条端点：只有 m1、m2。
	sealed, err := vault.Seal("sk-old")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.PutSecret(customSecretName("ck-1"), sealed, "…OLD"); err != nil {
		t.Fatal(err)
	}
	metas := []customKeyMeta{{
		ID: "ck-1", Name: "Old", BaseURL: upstream.URL + "/v1",
		Models: []string{"m1", "m2"}, ContextLength: 8192,
	}}
	if err := db.PutJSON(store.SettingCustomProviders, metas); err != nil {
		t.Fatal(err)
	}
	a.ReloadCustomProviders()
	if _, ok := catalog.Get("custom/ck-1/m2"); !ok {
		t.Fatalf("初始模型池应包含 m2，实际 %v", modelIDs(catalog))
	}

	// 编辑：改名、只留 m3、上下文改 4096、Key 留空（保持不变）。
	body := `{"id":"ck-1","name":"New","baseURL":"` + upstream.URL + `/v1","apiKey":"","models":["m3"],"contextLength":4096}`
	req := httptest.NewRequest(http.MethodPost, "/api/custom-keys/update", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.handleCustomKeyUpdate(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("更新失败: %d %s", rec.Code, rec.Body.String())
	}

	var got []customKeyMeta
	if _, err := db.GetJSON(store.SettingCustomProviders, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "New" || got[0].ContextLength != 4096 {
		t.Fatalf("元数据没更新: %+v", got)
	}
	if len(got[0].Models) != 1 || got[0].Models[0] != "m3" {
		t.Fatalf("模型没更新: %+v", got[0].Models)
	}

	// ⚠️ Key 留空时原 Key 必须保持不变。
	sealed2, hint, found, err := db.GetSecret(customSecretName("ck-1"))
	if err != nil || !found {
		t.Fatalf("Key 不应丢失: %v", err)
	}
	if sealed2 != sealed {
		t.Fatal("Key 留空不应改动已保存的密文")
	}
	if hint != "…OLD" {
		t.Fatalf("Key 指纹不应变，实际 %q", hint)
	}

	// 模型池即时同步：m3 在、m2 不在。
	if _, ok := catalog.Get("custom/ck-1/m3"); !ok {
		t.Fatalf("更新后模型池应包含 m3，实际 %v", modelIDs(catalog))
	}
	if _, ok := catalog.Get("custom/ck-1/m2"); ok {
		t.Fatalf("更新后模型池不应再有 m2，实际 %v", modelIDs(catalog))
	}
	// 上下文长度要传给模型信息（agent 的 /v1/models 读它）。
	m, ok := catalog.Get("custom/ck-1/m3")
	if !ok {
		t.Fatal("找不到 m3")
	}
	if m.ContextWindow != 4096 {
		t.Fatalf("模型信息应带上下文 4096，实际 %d", m.ContextWindow)
	}
}

// TestCustomKeyUpdateRejectsContextOverLimit 验证超过探测到的上限会被拒绝。
func TestCustomKeyUpdateRejectsContextOverLimit(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"m1","context_length":8192}]}`))
	}))
	defer upstream.Close()

	db, err := store.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	vault := newVaultForTest(t)
	registry := lane.NewRegistry()
	registry.Register(lane.NewCustomLane())
	a := &Admin{db: db, vault: vault, lanes: registry, catalog: lane.NewCatalog()}

	sealed, _ := vault.Seal("sk")
	_ = db.PutSecret(customSecretName("ck-1"), sealed, "…")
	_ = db.PutJSON(store.SettingCustomProviders, []customKeyMeta{{
		ID: "ck-1", Name: "X", BaseURL: upstream.URL + "/v1", Models: []string{"m1"},
	}})

	body := `{"id":"ck-1","baseURL":"` + upstream.URL + `/v1","models":["m1"],"contextLength":128000}`
	req := httptest.NewRequest(http.MethodPost, "/api/custom-keys/update", strings.NewReader(body))
	rec := httptest.NewRecorder()
	a.handleCustomKeyUpdate(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("超过上限应 400，实际 %d %s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &payload)
	if msg, _ := payload["error"].(string); !strings.Contains(msg, "8192") {
		t.Fatalf("错误信息应带上限 8192，实际 %q", msg)
	}
}

// newVaultForTest 建一个临时目录下的 vault。
//
// ⚠️ 不用 t.TempDir() —— 本机清理临时目录时会报 "directory is not empty"
//
//	而把测试判成失败（和文件型 SQLite 是同一个环境问题）。
func newVaultForTest(t *testing.T) *store.Vault {
	t.Helper()
	dir, err := os.MkdirTemp("", "fmh-vault-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	v, err := store.NewVault(dir)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// modelIDs 取当前模型池的 id（失败信息用）。
func modelIDs(c *lane.Catalog) []string {
	out := []string{}
	for _, m := range c.Models() {
		out = append(out, m.ID)
	}
	return out
}
