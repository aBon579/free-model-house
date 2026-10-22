package lane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Egress 是本机在免密车道那边**呈现出来的出口**。
//
// ⚠️ **为什么必须知道它** ——
//
// 免密车道的可用性**是网络路径的属性，不是账号的属性**。
// 上游的原话就是"detected abusive traffic from **this IP address**"。
//
// 所以：
//
//   - "被风控"说的是**这个出口**，不是"这个模型"；
//   - 用户开了 VPN / 换了代理，**能用的模型集合就变了**；
//   - 面板必须能回答"我现在从哪个 IP 出去"。
//
// ⚠️ **这不是给模型加一个字段** —— 它是**车道级**的状态：
//
//	整条车道被锁时，逐个模型标"被风控"是**误导**。
//	正确的说法是"出口被风控，这一整条车道暂时都不能用"。
//
// ⚠️ 检测**失败不报错** —— 拿不到出口信息就当没有，
//
//	不能因为一个 IP 回显服务挂了就不让用户用网关。
type Egress struct {
	// IP 是本机的公网出口地址。
	IP string `json:"ip,omitempty"`
	// Country 是国家代码（大写），只有部分回显服务会报。
	Country string `json:"country,omitempty"`
	// At 是检测到的时间（epoch ms）。
	At int64 `json:"at,omitempty"`
	// Err 是检测失败的原因（成功时为空）。
	Err string `json:"err,omitempty"`
}

// egressEcho 是一个 IP 回显源。
type egressEcho struct {
	url string
	// ip 从响应里取 IP。
	ip func(map[string]any) string
	// country 从响应里取国家码（可选）。
	country func(map[string]any) string
}

// egressEchoes 是回显源列表（按顺序试，第一个答就算）。
//
// ⚠️ 三个源**互为备份** —— 单个源挂掉（限流、改接口、被墙）不能影响功能。
//
// ⚠️ 顺序是"最可靠的在前"：ipify 简单稳定；
//
//	ipinfo 和 ipapi 额外报国家（面板要显示"这个出口在哪个地区"）。
var egressEchoes = []egressEcho{
	{
		url: "https://api.ipify.org?format=json",
		ip:  func(m map[string]any) string { return strOf(m["ip"]) },
	},
	{
		url: "https://ipinfo.io/json",
		ip:  func(m map[string]any) string { return strOf(m["ip"]) },
		country: func(m map[string]any) string {
			return strings.ToUpper(strOf(m["country"]))
		},
	},
	{
		url: "https://ipapi.co/json/",
		ip:  func(m map[string]any) string { return strOf(m["ip"]) },
		country: func(m map[string]any) string {
			return strings.ToUpper(strOf(m["country_code"]))
		},
	},
}

func strOf(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

// EgressDetector 检测本机的出口地址。
//
// ⚠️ 它**可失败** —— 拿不到就是拿不到，不影响别的功能。
type EgressDetector struct {
	client *http.Client
	// onChange 在出口变化时触发（用于触发重探）。
	onChange func(old, new Egress)
	mu       sync.Mutex
	last     Egress
	hasLast  bool
}

// NewEgressDetector 造一个检测器。
func NewEgressDetector() *EgressDetector {
	return &EgressDetector{
		// ⚠️ **必须用共享的 transport** ——
		//
		//   裸 `http.Client` 走的是 Go 的默认 transport，
		//   它在**连接时**才读系统证书池 ——
		//   而那时候进程可能已经降权成 nobody，
		//   `/etc/ssl/certs/ca-certificates.crt` 读不了 →
		//   出口检测永远失败：
		//
		//     tls: failed to verify certificate: x509: failed to load
		//     system roots and no roots provided; open
		//     /etc/ssl/certs/ca-certificates.crt: permission denied
		//
		//   `Transport()` 里显式挂了**降权前加载的内存证书池**，
		//   所以不依赖文件权限。
		client: Transport(),
	}
}

// SetOnChange 注册"出口变了"的回调。
func (d *EgressDetector) SetOnChange(fn func(old, new Egress)) {
	d.mu.Lock()
	d.onChange = fn
	d.mu.Unlock()
}

// Last 返回上次检测到的出口。
func (d *EgressDetector) Last() Egress {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last
}

// Detect 检测当前出口。
//
// ⚠️ **失败不报错** —— 返回一个带 Err 的 Egress，调用方自己决定要不要显示。
//
//	拿不到出口信息**绝不能**影响网关可用性。
func (d *EgressDetector) Detect(ctx context.Context) Egress {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()

	var lastErr string
	for _, echo := range egressEchoes {
		got, err := d.probeEcho(ctx, echo)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		got.At = time.Now().UnixMilli()
		d.remember(got)
		return got
	}
	// 一个都没成。
	out := Egress{At: time.Now().UnixMilli(), Err: lastErr}
	if lastErr == "" {
		out.Err = "所有出口检测服务都没有响应"
	}
	d.remember(out)
	return out
}

// probeEcho 试一个回显源。
func (d *EgressDetector) probeEcho(ctx context.Context, echo egressEcho) (Egress, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, echo.url, nil)
	if err != nil {
		return Egress{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return Egress{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Egress{}, &httpStatusError{code: resp.StatusCode}
	}
	// ⚠️ 只读一小段 —— 回显服务不该返回大 body，
	//   读多了是给攻击面（也可能是上游被劫持返回垃圾）。
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return Egress{}, err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return Egress{}, err
	}
	ip := echo.ip(doc)
	if ip == "" {
		return Egress{}, &httpStatusError{code: resp.StatusCode}
	}
	return Egress{IP: ip, Country: echo.country(doc)}, nil
}

// remember 记下这次的出口，变了就通知。
func (d *EgressDetector) remember(got Egress) {
	d.mu.Lock()
	prev := d.last
	had := d.hasLast
	changed := !had || prev.IP != got.IP
	d.last = got
	d.hasLast = true
	fn := d.onChange
	d.mu.Unlock()

	if changed && fn != nil && got.IP != "" {
		go fn(prev, got)
	}
}

// httpStatusError 是一个极简的状态码错误。
type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string {
	return "出口检测服务返回 HTTP " + itoa(e.code)
}
