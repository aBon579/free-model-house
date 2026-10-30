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
// ⚠️ **每个源的 country 都必须有值**（拿不到就返回空串）——
//
//	probeEcho 里 `echo.country(doc)` 对 nil 函数直接调会 SIGSEGV，
//	实测崩过一轮：ipify 那条没写 country，探通后进程崩了。
//
// ⚠️ 顺序是"最可靠的在前"：ipify 简单稳定；
//
//	ipinfo 和 ipapi 额外报国家（面板要显示"这个出口在哪个地区"）。
var egressEchoes = []egressEcho{
	{
		url:     "https://api.ipify.org?format=json",
		ip:      func(m map[string]any) string { return strOf(m["ip"]) },
		country: func(map[string]any) string { return "" },
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
	// onChange 在出口变化时触发（用于触发重探）。
	onChange func(old, new Egress)
	mu       sync.Mutex
	last     Egress
	hasLast  bool
}

// NewEgressDetector 造一个检测器。
func NewEgressDetector() *EgressDetector {
	return &EgressDetector{}
}

// client 现取一个走**当前生效** transport 的 client。
//
// ⚠️ **不能在构造时固化 client** ——
//
//	代理开关/切换会**替换全局 transport 实例**
//	（见 proxy.go 的 applyProxyToTransport），
//	钉死旧实例的话，连接池里留着的直连空闲连接会一直被复用，
//	出口检测就永远走**老路** —— 页面上"出口 IP"就永远不更新。
//	每次探测现取，用哪个 transport 由**此刻**的全局指针决定。
func (d *EgressDetector) client() *http.Client {
	return &http.Client{
		Transport: currentTransport(),
		Timeout:   15 * time.Second,
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
		// ⚠️ **探通了但没国家码 → 补查一次**。
		//
		//	ipify（排第一、最稳）只回 IP 不回国家；
		//	面板上"出口 IP（国家）"是用户判断"有没有换成海外"的关键信息，
		//	缺了它，一个不认识的 IP 数字毫无意义。
		//	用 ip-api.com 的免费接口补 —— 无 Key、回 {"country": "…"}。
		//
		//	⚠️ 失败不报错 —— 国家是"最好有"，不是"必须有"；
		//	  补查挂了照样返回探通的 IP。
		if got.Country == "" {
			if cc, err := lookupCountry(ctx, got.IP); err == nil {
				got.Country = cc
			}
		}
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
	resp, err := d.client().Do(req)
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
	// ⚠️ country 可能为 nil（不需要国家码的源）——
	//   实测崩过：直接调 nil 函数 = SIGSEGV，进程整个挂。
	cc := ""
	if echo.country != nil {
		cc = echo.country(doc)
	}
	return Egress{IP: ip, Country: cc}, nil
}

// lookupCountry 补查一个 IP 的国家码（两字母，大写）。
//
// ⚠️ 用 ip-api.com 的免费端点 —— 无 Key、无频控烦恼
//
//	（我们的调用频率是"每次出口检测一次"，远低于它的限制）。
//
// ⚠️ **必须走共享 transport** —— 补查的是"经代理出去的 IP"的国家，
//
//	探测路径一致才有意义；走直连的话查询本身可能被封，结果也不必一致。
//
// ⚠️ 失败返回错误，调用方忽略即可 —— 国家是锦上添花。
func lookupCountry(ctx context.Context, ip string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://ip-api.com/line/"+ip+"?fields=countryCode", nil)
	if err != nil {
		return "", err
	}
	resp, err := currentTransportClient().Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", &httpStatusError{code: resp.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	cc := strings.ToUpper(strings.TrimSpace(string(data)))
	// 竍返回格式是两字母国家码；别的都当失败。
	if len(cc) != 2 {
		return "", &httpStatusError{code: resp.StatusCode}
	}
	return cc, nil
}

// currentTransportClient 是一个走当前生效 transport 的 client
// （egress 包内复用 —— 与 EgressDetector.client 同一来源）。
func currentTransportClient() *http.Client {
	return &http.Client{Transport: currentTransport(), Timeout: 10 * time.Second}
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
