package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aBon579/free-model-house/internal/lane"
	"github.com/aBon579/free-model-house/internal/store"
)

// TestProxyTestDoesNotReenableProxy 复现用户报的 bug：
//
//	手动关掉代理 → 点"检测出口" → 代理自己变回 enabled=true。
//
// 排查思路：把整个链路在内存里跑一遍，
// 检测前后各读一次数据库里的 proxy_config —— 被动了就是后端的锅。
func TestProxyTestDoesNotReenableProxy(t *testing.T) {
	db, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("开库失败: %v", err)
	}
	defer db.Close()

	admin := NewAdmin(NewKeys(db), lane.NewRegistry(), lane.NewCatalog(), db, nil)
	proxy := lane.NewProxyManager()
	admin.SetProxy(proxy)
	admin.SetEgress(lane.NewEgressDetector())

	// ① 开着代理保存（和用户之前的操作一样）。
	body := map[string]any{"enabled": true, "scheme": "http", "host": "192.168.5.1", "port": 7890}
	b1, _ := json.Marshal(body)
	rec1 := httptest.NewRecorder()
	req1 := httptest.NewRequest(http.MethodPost, "/api/proxy", bytes.NewReader(b1))
	admin.handleProxy(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("保存代理失败: %d %s", rec1.Code, rec1.Body.String())
	}

	// ② 关掉代理。
	b2, _ := json.Marshal(map[string]any{"enabled": false})
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest(http.MethodPost, "/api/proxy", bytes.NewReader(b2))
	admin.handleProxy(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("关闭代理失败: %d", rec2.Code)
	}
	if got := readProxyEnabled(t, db); got {
		t.Fatal("关闭后 DB 里 enabled 该是 false")
	}

	// ③ 点检测出口（POST 触发 + GET 轮询到完成）。
	rec3 := httptest.NewRecorder()
	req3 := httptest.NewRequest(http.MethodPost, "/api/proxy/test", nil)
	admin.handleProxyTest(rec3, req3)
	if rec3.Code != http.StatusAccepted {
		t.Fatalf("触发检测失败: %d", rec3.Code)
	}
	// 轮询直到 running=false。
	for i := 0; i < 150; i++ {
		rec4 := httptest.NewRecorder()
		req4 := httptest.NewRequest(http.MethodGet, "/api/proxy/test", nil)
		admin.handleProxyTest(rec4, req4)
		var res map[string]any
		_ = json.Unmarshal(rec4.Body.Bytes(), &res)
		if res["running"] == false {
			break
		}
	}

	// ④ 核验：代理必须**仍然关着**。
	if got := readProxyEnabled(t, db); got {
		t.Fatal("bug 复现：检测出口后代理自己变回 enabled=true")
	}
	if got := proxy.Config().Enabled; got {
		t.Fatal("bug 复现：内存里的代理也被拉起了")
	}
}

// readProxyEnabled 从 DB 读 enabled。
func readProxyEnabled(t *testing.T, db *store.DB) bool {
	t.Helper()
	raw, ok, err := db.GetSetting(store.SettingProxyConfig)
	if err != nil || !ok {
		t.Fatalf("读代理配置失败: %v ok=%v", err, ok)
	}
	cfg := lane.ParseProxyConfig(raw)
	return cfg.Enabled
}
