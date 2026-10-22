package lane

import (
	"encoding/json"
	"time"
)

// 三个 encoder 共享的底座。关键机制：**延迟提交**。
//
// 把 `message_start` / role 前言延迟到第一个内容字节发出之前，
// 这样上游的 429 + Retry-After 还能作为真正的 HTTP 429 返回；
// 一旦发出首个字节，200 就已提交，失败只能以流内错误事件传递。
//
// 详见 docs/design/00-overview.md §5.10。

// Writer 是线上的写入目标。
type Writer interface {
	// WriteSSE 写一个 SSE 帧（name 可为空）。
	WriteSSE(name string, data any) error
	// WriteJSON 写一个 JSON 响应（非流式）。
	WriteJSON(status int, payload any) error
	// HeadersSent 报告响应头是否已经发出。
	HeadersSent() bool
}

// baseEncoder 是三个 encoder 的公共部分。
type baseEncoder struct {
	writer Writer
	// requestID 是本次请求的追踪 id。
	requestID string
	// modelName 是对外暴露的模型名。
	modelName string
	// upstreamModel 是实际服务的上游模型名。
	upstreamModel string
	state         *encoderState
	// rename 把发出去的工具名还原成调用方的名字。
	rename map[string]string
	// suppress 是要被压制的工具名（注入的占位工具）。
	suppress map[string]bool
	// callerTools 是调用方声明过的工具名。
	// 只有调用方**没声明**的占位工具才压制 ——
	// 调用方自己声明的同名工具必须正常返回。
	callerTools map[string]bool
}

// shouldSuppress 报告一个工具调用是否要被压制。
//
// ⚠️ 判据是"调用方没有声明过这个名字"：注入的占位工具是 read/shell，
// 但调用方自己声明了 read 就是它真的想要的功能，不能压制。
func (e *baseEncoder) shouldSuppress(name string) bool {
	if !e.suppress[name] {
		return false
	}
	return !e.callerTools[name]
}

func (e *baseEncoder) emit(name string, payload any) error {
	return e.writer.WriteSSE(name, payload)
}

func nowUnix() int64 { return time.Now().Unix() }

// renameTools 把线上发出的工具名还原成调用方的名字。
//
// 别名机制：调用方正好用了 `read`/`shell` 但定义不同时，
// 网关把它改名成 `read_client` 发出去，响应侧再还原回去，
// 否则调用方会收到一个它不认识的工具名。
func renameTools(rename map[string]string, name string) string {
	if orig, ok := rename[name]; ok {
		return orig
	}
	return name
}

func marshalJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}
