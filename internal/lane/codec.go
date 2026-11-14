package lane

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// 这一层是给**包外的车道**用的（目前是 internal/creditlane）。
//
// 内置车道能直接用 doUpstream / decodeSSE / newDecoder 这些私有件，
// 包外自己重写一遍的话，并发闸、空闲超时、错误分类、切帧规则
// 就会在两个地方各说一套 —— 那种分歧只会在半夜的上游抖动里暴露。
// 所以这里把**同一套**实现暴露出去，而不是复制一份。

type StreamDecoder struct{ decoder *decoder }

func NewStreamDecoder(wire string, emit func(Chunk)) (*StreamDecoder, error) {
	if wire != "chat" && wire != "messages" && wire != "responses" {
		return nil, fmt.Errorf("unsupported stream protocol: %s", wire)
	}
	return &StreamDecoder{decoder: newDecoder(wireKind(wire), nil, emit)}, nil
}

func (s *StreamDecoder) Feed(frame []byte) *Error { return s.decoder.feed(frame) }

// Decode 从流里一路读到正常结束。
// 调用方只给 wire 名，切帧与错误信封的判断都走内置车道那套。
func (s *StreamDecoder) Decode(r io.Reader) (*Usage, FinishReason, *Error) {
	if err := ScanSSE(r, func(_ string, frame []byte) *Error { return s.Feed(frame) }); err != nil {
		return &s.decoder.usage, FinishError, err
	}
	return s.Result()
}

func (s *StreamDecoder) Result() (*Usage, FinishReason, *Error) {
	d := s.decoder
	if !d.sawFinish {
		return &d.usage, FinishError, &Error{Code: CodeTransport, Message: "上游流未正常结束"}
	}
	return &d.usage, d.finish, nil
}

func ChatWireTurn(turn *Turn) ([]map[string]any, []map[string]any) {
	messages := []map[string]any{}
	for _, v := range toChatMessages(turn.Messages) {
		messages = append(messages, v.(map[string]any))
	}
	tools := []map[string]any{}
	if !turn.SuppressTools {
		for _, v := range toolDefsFor(ZenStyleChat, turn.Tools) {
			tools = append(tools, v.(map[string]any))
		}
	}
	return messages, tools
}

// ProbeStateForError 把一次失败折成面板状态。
//
// ⚠️ 暴露它是因为"什么错算限流、什么错算不可用"这套判断**只能有一份**。
//
//	包外车道自己再写一遍 switch，两边就会对同一个错误给出不同结论 ——
//	用户看到的是"这个模型一会儿被限流、一会儿不可用"。
//
// ⚠️ 只返回状态，**不返回那句话** —— 措辞是车道自己的事
//
//	（免密车道的"换出口"和凭据车道的"重新登录"不是一回事）。
func ProbeStateForError(uerr *Error) ProbeState {
	state, _ := probeStateFromError(uerr)
	return state
}

// ScanSSE 从流里切出 SSE 帧，规则与内置车道完全一致：
// 空行与 `:` 注释行跳过，非 `data:` 字段行不当事件内容，`[DONE]` 收尾。
//
// ⚠️ 额外把 `event:` 的名字一起交出去 ——
//
//	内置车道能只看 data（Anthropic 把类型写在 data 里），
//	但 TRAE 那一类协议**同一个 data 形状会出现在不同事件名下**
//	（`output` / `token_usage` / `done` / `error`），
//	丢了事件名就只能靠猜，而猜错的直接后果是把错误帧当内容丢掉。
//
// onFrame 返回非 nil 的 *Error 就中止扫描。
func ScanSSE(r io.Reader, onFrame func(event string, data []byte) *Error) *Error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	event := ""
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r\n")
		switch {
		case line == "" || strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
			continue
		case strings.HasPrefix(line, "data:"):
		default:
			continue
		}
		frame := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if frame == "" {
			continue
		}
		if frame == "[DONE]" {
			return nil
		}
		if err := onFrame(event, []byte(frame)); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return &Error{Code: CodeTransport, Message: "upstream stream read failed: " + err.Error()}
	}
	return nil
}

// OpenStream 用与内置车道**同一条**上游通道发一次请求
// （并发闸、错误分类、形状嗅探、空闲超时都复用同一份实现），
// 返回可读的响应体。调用方负责 Close。
func OpenStream(ctx context.Context, client *http.Client, method, endpoint string, headers map[string]string, body []byte) (io.ReadCloser, *Error) {
	res, uerr := doUpstream(ctx, client, method, endpoint, headers, body)
	if res != nil && res.Body != nil {
		if uerr != nil {
			res.Body.Close()
			return nil, uerr
		}
		return &streamBody{Reader: newIdleReader(res.Body, StreamIdleTimeout), closer: res.Body}, nil
	}
	return nil, uerr
}

// streamBody 把「带空闲超时的读」与「关闭」拼成一个 io.ReadCloser
// —— idleReader 只管读，原始 body 仍然要有人关。
type streamBody struct {
	io.Reader
	closer io.Closer
}

func (s *streamBody) Close() error { return s.closer.Close() }
