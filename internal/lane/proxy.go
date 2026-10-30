package lane

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// **代理出口**。
//
// 免密车道的可用性是**网络路径的属性**：上游按出口 IP 记额度、判地区、
// 风控滥用。同一台 NAS，换一个出口，能用的模型集合就完全不同。
// 所以代理不是一个"高级选项"，而是和探测、限流同级的一等公民。
//
// ⚠️ **只做"把上游流量交给用户指定的代理"，不内置代理服务器** ——
//
//	用户已经在局域网里跑着代理工具（Clash / v2ray / SSR…），
//	FMH 替它们做一个重复实现没有意义。
//	FMH 的职责是：把所有**发往上游**的请求（清单、探测、推理、额度查询）
//	都走用户指定的那一个代理入口。
//
// ⚠️ **全局一个代理入口** —— 不按车道分、不支持池子。
//
//	上游的限流与风控都是**按出口 IP** 记账的，
//	"按车道分代理"意味着两条车道各占一个出口，
//	那会让额度分散而不是集中，与"换出口解禁"的初衷相反。
//	真要多出口，开多个 FMH 实例各配一个代理即可。
//
// ⚠️ **不内置代理服务器还有一个安全红利** ——
//
//	FMH 只需要能**连**代理，不需要能**接受**连接，
//	所以它不增加任何监听端口，不扩大攻击面。

// ProxyConfig 是代理的配置。
//
// ⚠️ **没有用户名/密码** —— 局域网内的代理工具普遍不开认证；
// 要认证的代理通常也不该暴露给整个局域网。
type ProxyConfig struct {
	// Enabled 是代理开关。
	Enabled bool `json:"enabled"`
	// Scheme 是协议：http / https / socks5。
	Scheme string `json:"scheme"`
	// Host 是代理地址（不含端口）。
	Host string `json:"host"`
	// Port 是代理端口。
	Port int `json:"port"`
}

// Address 返回 `scheme://host:port` 形式的地址。
//
// ⚠️ Host 是 IPv6 时要加方括号 —— `net.JoinHostPort` 处理。
func (c ProxyConfig) Address() string {
	if c.Host == "" || c.Port <= 0 {
		return ""
	}
	return c.Scheme + "://" + net.JoinHostPort(c.Host, itoa(c.Port))
}

// Validate 报告配置是否完整可用。
func (c ProxyConfig) Validate() error {
	switch c.Scheme {
	case "http", "https", "socks5":
	default:
		return fmt.Errorf("不支持的协议：%s（只支持 http / https / socks5）", c.Scheme)
	}
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("代理地址不能为空")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("端口 %d 不合法", c.Port)
	}
	return nil
}

// Normalized 返回去掉首尾空白后的副本。
func (c ProxyConfig) Normalized() ProxyConfig {
	c.Host = strings.TrimSpace(c.Host)
	return c
}

// DefaultProxyConfig 是没有存储时的默认值 —— 关。
var DefaultProxyConfig = ProxyConfig{Scheme: "http"}

// ProxyState 是代理的运行状态（给前端显示）。
type ProxyState struct {
	// Enabled 是代理开关。
	Enabled bool `json:"enabled"`
	// Scheme 是协议。
	Scheme string `json:"scheme"`
	// Host 是代理地址。
	Host string `json:"host"`
	// Port 是代理端口。
	Port int `json:"port"`
	// Address 是 `scheme://host:port`（空 = 未配置）。
	Address string `json:"address,omitempty"`
	// Up 是"最后一次探测代理本身能不能连通"。
	//
	// ⚠️ 它和 Egress.IP 是**两回事**：
	//
	//	Up    —— 代理服务器活着吗（本机 → 代理这一跳）
	//	Egress —— 经代理出去是什么 IP（代理 → 上游这一跳）
	Up bool `json:"up"`
	// LastCheckAt 是最后一次检测代理连通的时间（epoch ms）。
	LastCheckAt int64 `json:"lastCheckAt,omitempty"`
	// LastError 是最近一次连通失败的原因。
	LastError string `json:"lastError,omitempty"`
}

// ProxyManager 管理代理配置，并把配置应用到**共享 transport**。
//
// ⚠️ **为什么它必须长在 lane 包里** ——
//
//	所有上游流量都走 `Transport()` 里的那一个 `http.Transport`。
//	代理要"全局生效"，唯一的办法就是改**那一个 transport**，
//	而不是给每个调用方发一个带代理的新 client —— 那样的话，
//	已经建立起来的连接池、各车道自己缓存的 client 全都绕过代理。
type ProxyManager struct {
	// cfg 是当前配置（原子读写）。
	cfg atomic.Pointer[ProxyConfig]
	// mu 保护对 transport.Proxy 的替换。
	mu sync.Mutex
	// onApply 在配置应用后触发（持久化 + 通知）。
	onApply func(ProxyConfig)
	// lastCheck 是最近一次"代理本身能不能连通"的结果。
	lastCheck atomic.Pointer[ProxyState]
	// probeClient 用于检测代理连通性。
	//
	// ⚠️ **不能用共享的 Transport()** ——
	//   连通性检测要区分"代理挂了"和"上游挂了"，
	//   所以它自带一个**只走代理**的 client。
	//   共享 transport 在没配代理时是直连的，用它检测会得出错误结论。
	probeClient *http.Client
}

// NewProxyManager 建一个代理管理器。
func NewProxyManager() *ProxyManager {
	m := &ProxyManager{}
	m.cfg.Store(&DefaultProxyConfig)
	return m
}

// Config 返回当前配置。
func (m *ProxyManager) Config() ProxyConfig {
	return *m.cfg.Load()
}

// SetOnApply 注册"配置已应用"回调。
func (m *ProxyManager) SetOnApply(fn func(ProxyConfig)) {
	m.mu.Lock()
	m.onApply = fn
	m.mu.Unlock()
}

// Apply 应用一个新配置，并把它装到共享 transport 上。
//
// ⚠️ **必须重建整个 transport，而不是只改 Proxy 字段** ——
//
//	`http.Transport` 的 `Proxy` 在**建立连接时**读取。
//	在途连接不会换代理，这是对的（不能让一个请求半路换出口）；
//	但**空闲连接**如果留着，它们是直连的，
//	新请求会复用它们 → **绕过代理**。
//
//	所以配置变化时关闭旧 transport（掐掉所有空闲连接），
//	换上新的 —— 之后的连接才真的全部走代理。
//
// ⚠️ **从代理切回直连也要重建** —— 同样的理由：
//
//	留着走代理的空闲连接，"关掉代理"后请求仍会经代理出去。
func (m *ProxyManager) Apply(cfg ProxyConfig) error {
	cfg = cfg.Normalized()
	if cfg.Enabled {
		if err := cfg.Validate(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg.Store(&cfg)
	applyProxyToTransport(cfg)
	return nil
}

// proxyFromConfig 生成 transport 的 Proxy 函数。
//
// ⚠️ 返回 `(nil, nil)` 而不是 nil ——
//
//	`http.Transport.Proxy` 的约定是"返回 nil 错误且 URL 为 nil = 直连"。
func proxyFromConfig(cfg ProxyConfig) func(*http.Request) (*url.URL, error) {
	if !cfg.Enabled {
		return nil
	}
	addr := cfg.Address()
	if addr == "" {
		return nil
	}
	// ⚠️ **地址只解析一次**，不放进闭包每请求解析 ——
	//   每个上游请求都要走这个函数，解析失败会变成每请求的 overhead。
	u, err := url.Parse(addr)
	if err != nil {
		return nil
	}
	return func(*http.Request) (*url.URL, error) {
		return u, nil
	}
}

// proxyTransport 是当前生效的、带代理设置的 transport。
//
// ⚠️ 全局单例 —— `Transport()` 每次调用返回新 client，
//
//	但它们**共享同一个 transport**，代理设置才对所有调用方生效。
var proxyTransport atomic.Pointer[http.Transport]

// applyProxyToTransport 用新配置替换共享 transport。
//
// ⚠️ **替换前先 Close 旧的** —— 否则空闲连接继续存活，
//
//	新配置生效前建立的老连接仍按老路走。
func applyProxyToTransport(cfg ProxyConfig) {
	old := proxyTransport.Load()
	next := newTransportWithProxy(cfg)
	if old != nil {
		old.CloseIdleConnections()
	}
	proxyTransport.Store(next)
}

// currentTransport 返回当前生效的共享 transport。
func currentTransport() *http.Transport {
	if t := proxyTransport.Load(); t != nil {
		return t
	}
	t := newTransportWithProxy(DefaultProxyConfig)
	proxyTransport.Store(t)
	return t
}

// TestConns 检测代理本身能不能连通。
//
// ⚠️ 它检测的是**本机 → 代理**这一跳，不是"经代理能不能到上游"：
//
//	代理活着但上游挂了 → 这里返回成功（代理没问题）
//	代理挂了           → 这里返回失败（这才是它要报告的）
//
// 两跳分开测的理由：用户配完代理最想知道的是"我配对了没有"，
// 而不是"上游今天好不好"。上游好不好，探测和出口检测自会报告。
func (m *ProxyManager) TestConns(ctx context.Context) ProxyState {
	cfg := m.Config()
	state := ProxyState{
		Enabled:     cfg.Enabled,
		Scheme:      cfg.Scheme,
		Host:        cfg.Host,
		Port:        cfg.Port,
		Address:     cfg.Address(),
		LastCheckAt: time.Now().UnixMilli(),
	}
	if !cfg.Enabled {
		state.LastError = "代理未启用"
		m.lastCheck.Store(&state)
		return state
	}
	if err := cfg.Validate(); err != nil {
		state.LastError = err.Error()
		m.lastCheck.Store(&state)
		return state
	}

	// 检测客户端：只走这个代理（代理挂了这里才报错）。
	//
	// ⚠️ **超时要宽** —— 检测路径是"FMH → 代理 → 隧道 → 公网回显"，
	//   实测有代理建立隧道 + TLS 就要七八秒；8 秒会在慢链路上
	//   报 context deadline exceeded，让人以为代理挂了（其实没挂）。
	client := &http.Client{
		Transport: newTransportWithProxy(cfg),
		Timeout:   proxyTestTimeout,
	}

	_, err := connectThroughProxy(ctx, client, cfg)
	if err != nil {
		state.LastError = err.Error()
	} else {
		state.Up = true
	}
	m.lastCheck.Store(&state)
	return state
}

// LastCheck 返回最近一次连通性检测的结果。
func (m *ProxyManager) LastCheck() ProxyState {
	if s := m.lastCheck.Load(); s != nil {
		return *s
	}
	cfg := m.Config()
	return ProxyState{
		Enabled: cfg.Enabled,
		Scheme:  cfg.Scheme,
		Host:    cfg.Host,
		Port:    cfg.Port,
		Address: cfg.Address(),
	}
}

// proxyTestTimeout 是代理连通性检测的总超时。
//
// 路径是"FMH → 代理 → 隧道 → 公网回显"，慢代理建隧道就要七八秒，
// 8 秒不够；15 秒覆盖住，还留给"检测中…"的按钮一个合理的等待上限。
const proxyTestTimeout = 15 * time.Second

// connectThroughProxy 经代理发一个最小请求，验证代理能转发。
//
// ⚠️ 用 CONNECT 而不是 GET ——
//
//	GET 要代理自己去连目标，报错时你分不清
//	"代理坏了"还是"目标挂了"。
//	CONNECT 成功 = 代理确认了自己能转发，
//	失败也只会是代理的问题（目标地址是写死的可靠域名）。
func connectThroughProxy(ctx context.Context, client *http.Client, cfg ProxyConfig) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, proxyTestTimeout)
	defer cancel()

	// 目标用一个**稳定且轻量**的地址 —— 任何代理都放行的 443。
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://api.ipify.org", nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("连不上代理 %s: %v", cfg.Address(), cleanProxyErr(err))
	}
	defer resp.Body.Close()
	return "", nil
}

// cleanProxyErr 把连接错误里的地址信息剥掉 ——
// 报错时只留"原因"，不留内部 IP:Port（日志会存盘）。
func cleanProxyErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// url.Error 会带全量 URL —— 只留原因部分。
	if i := strings.LastIndex(msg, ": "); i >= 0 && i+2 < len(msg) {
		reason := msg[i+2:]
		if reason != "" && !strings.Contains(reason, "://") {
			return reason
		}
	}
	return msg
}

// ParseProxyConfig 从存储的 JSON 里解出配置（带兜底）。
func ParseProxyConfig(raw string) ProxyConfig {
	if strings.TrimSpace(raw) == "" {
		return DefaultProxyConfig
	}
	var cfg ProxyConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return DefaultProxyConfig
	}
	return cfg.Normalized()
}

// EncodeProxyConfig 把配置编码成 JSON（存进 settings 表）。
func EncodeProxyConfig(cfg ProxyConfig) string {
	data, err := json.Marshal(cfg)
	if err != nil {
		return ""
	}
	return string(data)
}
