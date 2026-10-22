package lane

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// **上游 HTTP 传输层**。参数是实测调出来的。
// 三个关键点：
//  1. **ResponseHeaderTimeout: 90s** —— 一个接受 TCP 然后永不回应的出口，
//     会让请求**完全没有截止时间**。90 秒清掉了最慢的实测冷启动（62 秒），
//     同时仍能砍掉卡死的出口。
//  2. **MaxIdleConnsPerHost: 128** —— Go 的默认是 2，
//     意味着 6 个并发上游请求里有 4 个每轮都要重做 TLS 握手。
//  3. **形状比 Content-Type 可信** —— 上游有时用 `application/json`
//     的 Content-Type 回答流式请求。所以要嗅探前几个字节，
//     而且把嗅探到的字节 replay 回流里。
// 详见 docs/design/00-overview.md §5.11。

const (
	// MaxConcurrentUpstream 是全局并发上游请求上限。
	// 免费车道是共享资源，无界并发会把上游打挂。
	MaxConcurrentUpstream = 6

	// StreamIdleTimeout 是"多久没有任何新字节"的阈值。
	// ⚠️ 它限制的是**沉默**，不是**长度**。每次读到 chunk 就重置。
	// 健康回合每几秒给一个 chunk、7-62 秒完成；
	// 一次卡死的回合沉默超过 5 分钟，会把整个重试预算耗光，
	// 导致什么都没重试，客户端只看到自己的取消。
	StreamIdleTimeout = 90 * time.Second

	// upstreamHeadTimeout 是"从发出到收到响应头"的截止时间。
	upstreamHeadTimeout = 90 * time.Second

	// sniffBytes 是嗅探响应体形状要读的字节数。
	sniffBytes = 4096
)

// upstreamSem 是全局并发信号量。
var upstreamSem = make(chan struct{}, MaxConcurrentUpstream)

// Transport 是共享的上游 HTTP 客户端。
// 所有上游流量都走它，这样连接池才是共享的。
func Transport() *http.Client {
	return &http.Client{
		Transport: transport(),
		// ⚠️ 不要设 Client.Timeout —— 它会覆盖整个响应体读取，
		// 长回答会被砍掉。空闲检测由 streamReader 负责。
	}
}

func transport() *http.Transport {
	t := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: upstreamHeadTimeout,
	}
	// ⚠️ **显式指定 CA 证书池** ——
	//
	//   容器里降权后是 nobody，`/etc/ssl/certs/ca-certificates.crt`
	//   如果对它不可读，Go 就**没有证书池可用** → 所有 HTTPS 全挂。
	//
	//   `CAPool()` 在**降权前**把证书读进内存了，
	//   所以这里用内存里的那份，不再依赖文件权限。
	//
	//   非容器环境（Windows / 开发机）读不到那个文件 → 返回 nil →
	//   Go 退回系统默认证书库，行为不变。
	if pool := CAPool(); pool != nil {
		t.TLSClientConfig = &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS12,
		}
	}
	return t
}

// bodyShape 是响应体的形状。
type bodyShape string

const (
	shapeEmpty   bodyShape = "empty"
	shapeSSE     bodyShape = "sse"
	shapeJSON    bodyShape = "json"
	shapeUnknown bodyShape = "unknown"
)

// sniffShape 嗅探响应体的形状。
// ⚠️ 上游有时用 `application/json` 的 Content-Type 回答**流式**请求。
// 所以形状比头可信。
func sniffShape(head []byte) bodyShape {
	trimmed := strings.TrimLeft(string(head), " \t\r\n")
	if trimmed == "" {
		return shapeEmpty
	}
	if strings.HasPrefix(trimmed, ":") ||
		strings.HasPrefix(trimmed, "data:") ||
		strings.HasPrefix(trimmed, "event:") ||
		strings.HasPrefix(trimmed, "id:") ||
		strings.HasPrefix(trimmed, "retry:") {
		return shapeSSE
	}
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return shapeJSON
	}
	return shapeUnknown
}

// replayReader 把已经读走的头字节放回流的最前面。
// ⚠️ 不 replay 就会**丢掉第一个 token** —— 上游的第一个 SSE 帧
// 常常就在嗅探的那 4KB 里。
type replayReader struct {
	head []byte
	body io.Reader
	// closer 是原始 body 的 Close（要转发，否则连接不会释放）。
	closer io.Closer
}

func (r *replayReader) Read(p []byte) (int, error) {
	if len(r.head) > 0 {
		n := copy(p, r.head)
		r.head = r.head[n:]
		return n, nil
	}
	return r.body.Read(p)
}

func (r *replayReader) Close() error {
	if r.closer != nil {
		return r.closer.Close()
	}
	return nil
}

// idleReader 是一个"沉默即超时"的读取器。
// 它把阻塞的 body 变成带超时的读取：每次成功读到数据就重置计时器。
type idleReader struct {
	body    io.Reader
	timeout time.Duration
	// buf 是上一次读剩下的数据。
	buf []byte
	// lastActivity 是最后一次读到数据的时间。
	lastActivity time.Time
}

func newIdleReader(body io.Reader, timeout time.Duration) *idleReader {
	return &idleReader{
		body:         body,
		timeout:      timeout,
		lastActivity: time.Now(),
	}
}

// Read 实现 io.Reader，沉默超过 timeout 就返回超时错误。
func (r *idleReader) Read(p []byte) (int, error) {
	if len(r.buf) > 0 {
		n := copy(p, r.buf)
		r.buf = r.buf[n:]
		return n, nil
	}
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := r.body.Read(p)
		ch <- result{n, err}
	}()

	select {
	case res := <-ch:
		if res.n > 0 {
			r.lastActivity = time.Now()
		}
		return res.n, res.err
	case <-time.After(r.timeout):
		return 0, &Error{
			Code:    CodeTimeout,
			Message: "upstream stream idle past its deadline",
		}
	}
}

// requestResult 是一次上游请求的结果。
type requestResult struct {
	// Status 是 HTTP 状态码。
	Status int
	Header http.Header
	// Body 是响应体（已 sniff + replay）。
	Body  io.ReadCloser
	Shape bodyShape
	// RetryAfterSec 是上游给的退避秒数。
	RetryAfterSec int
}

// doUpstream 发一个上游请求。
// 它负责：并发限流、响应体形状嗅探、错误分类。
// 流的内容由调用方用 decoder 解析。
func doUpstream(ctx context.Context, client *http.Client, method, url string, headers map[string]string, body []byte) (*requestResult, *Error) {
	select {
	case upstreamSem <- struct{}{}:
		defer func() { <-upstreamSem }()
	case <-ctx.Done():
		return nil, &Error{Code: CodeAborted, Message: "cancelled"}
	}

	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, &Error{Code: CodeClient, Message: err.Error()}
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, &Error{Code: CodeAborted, Message: "cancelled"}
		}
		return nil, &Error{Code: CodeTransport, Message: transportCause(err)}
	}

	result := &requestResult{
		Status:        resp.StatusCode,
		Header:        resp.Header,
		RetryAfterSec: parseRetryAfter(resp.Header.Get("Retry-After")),
	}

	// 非 2xx：读出错误体并分类。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		payload := extractErrorPayload(data)
		return result, ClassifyFailure(resp.StatusCode, payload, result.RetryAfterSec)
	}

	// 嗅探形状。
	// ⚠️ 必须用 **Read**（消费）而不是 **Peek**（不消费）：
	// Peek 之后字节仍在缓冲区里，如果再把它们 replay 一次，
	// 上游的第一个 SSE 帧就会被**读两遍** —— 客户端会收到重复的内容。
	// ⚠️ Read 一次只拿到"当前可用的"字节（不阻塞等满），
	// 所以对流式响应不会因为嗅探而卡住。
	br := bufio.NewReaderSize(resp.Body, 64*1024)
	head := make([]byte, sniffBytes)
	n, _ := br.Read(head)
	head = head[:n]
	result.Shape = sniffShape(head)
	if len(head) > 0 {
		result.Body = &replayReader{
			head:   head,
			body:   br,
			closer: resp.Body,
		}
	} else {
		result.Body = &replayReader{
			body:   br,
			closer: resp.Body,
		}
	}
	return result, nil
}

// parseRetryAfter 解析 Retry-After 头（秒）。
func parseRetryAfter(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 && n < 3600 {
		return n
	}
	// 也接受 HTTP 日期。
	if t, err := http.ParseTime(v); err == nil {
		d := int(time.Until(t).Seconds())
		if d > 0 && d < 3600 {
			return d
		}
	}
	return 0
}

// transportCause 从错误链里挖出真正的原因。
// Go 的 http.Client 把底层错误包成 `*url.Error`，
// 消息是 "fetch failed" 之类的笼统说法。
func transportCause(err error) string {
	if err == nil {
		return ""
	}
	cause := err
	for i := 0; i < 3; i++ {
		uerr, ok := cause.(interface{ Unwrap() error })
		if !ok {
			break
		}
		next := uerr.Unwrap()
		if next == nil {
			break
		}
		cause = next
	}
	if errors.Is(cause, context.DeadlineExceeded) {
		return "upstream request timed out"
	}
	msg := cause.Error()
	if msg == "" {
		return err.Error()
	}
	return msg
}

// getJSON 发一个 GET 请求并解析出 JSON。
func getJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, out any) *Error {
	res, uerr := doUpstream(ctx, client, http.MethodGet, url, headers, nil)
	if uerr != nil {
		return uerr
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return &Error{Code: CodeTransport, Message: "reading upstream response failed: " + err.Error()}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return &Error{
				Code:    CodeServer,
				Message: "upstream returned a non-JSON body: " + truncate(string(data), 200),
			}
		}
	}
	return nil
}

// postJSON 发一个 POST 请求并解析出 JSON。
func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body []byte, out any) *Error {
	res, uerr := doUpstream(ctx, client, http.MethodPost, url, headers, body)
	if uerr != nil {
		return uerr
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return &Error{Code: CodeTransport, Message: "reading upstream response failed: " + err.Error()}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return &Error{
				Code:    CodeServer,
				Message: "upstream returned a non-JSON body: " + truncate(string(data), 200),
			}
		}
	}
	return nil
}
