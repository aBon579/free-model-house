package lane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProbeEchoNilCountryNoCrash 验证**回显源没配 country 函数时不会崩**。
//
// ⚠️ 这是实测踩过的崩溃：ipify 那条只写了 ip 函数，
//
//	探通后 `echo.country(doc)` 调到 nil → SIGSEGV → 进程整个挂掉。
//	当时的表现是：一开代理，几秒后容器反复重启。
func TestProbeEchoNilCountryNoCrash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ip":"1.2.3.4"}`))
	}))
	defer srv.Close()

	d := NewEgressDetector()
	// 故意构造 country=nil 的源 —— 探通也不许崩。
	echo := egressEcho{url: srv.URL, ip: func(m map[string]any) string { return strOf(m["ip"]) }}
	got, err := d.probeEcho(context.Background(), echo)
	if err != nil {
		t.Fatalf("该探通: %v", err)
	}
	if got.IP != "1.2.3.4" {
		t.Fatalf("IP 该是 1.2.3.4，实际 %q", got.IP)
	}
	if got.Country != "" {
		t.Fatalf("没配 country 的源该回空串，实际 %q", got.Country)
	}
}

// TestEgressEchoesAllHaveCountry 验证**内置的每个回显源都带 country 函数**。
//
// ⚠️ 与上面的 nil 保护双保险 —— 新增源时忘了配 country，
//
//	这里直接报出来，不用等线上崩。
func TestEgressEchoesAllHaveCountry(t *testing.T) {
	for i, e := range egressEchoes {
		if e.ip == nil {
			t.Errorf("回显源[%d] (%s) 缺 ip 函数", i, e.url)
		}
		if e.country == nil {
			t.Errorf("回显源[%d] (%s) 缺 country 函数 —— 会 SIGSEGV", i, e.url)
		}
	}
}
