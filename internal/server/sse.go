package server

import (
	"bufio"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// 本文件是 **SSE 写出器** —— 实现延迟提交。
//
// 核心问题：流式代理一上来就写 `200 OK` + `text/event-stream`，
// 然后上游才告诉你它被限流了 —— 这时你只能往流里塞一个错误事件，
// 而客户端的退避逻辑看不到 `429` 和 `Retry-After`，只会当成一次普通的失败。
//
// 正确做法：**把响应头延迟到第一个内容字节**。
// 在那之前，上游的 429 + Retry-After 还能作为**真正的 HTTP 429** 返回。
// 一旦发出了第一个字节，200 就已提交，失败只能以流内错误事件传递。
//
// 另一个细节：长时间无字节时发 SSE 注释行（心跳），防止中间代理掐断连接。
// 理由：一次调用可能沉默 70 秒，而上游已经在计费 3024 个推理 token 了。

// sseWriter 是一个延迟提交的 SSE 写出器。
type sseWriter struct {
	mu sync.Mutex
	w  http.ResponseWriter
	// flusher 用于立刻把数据推出去。
	flusher http.Flusher
	// committed 表示响应头已发出。
	committed bool
	// aborted 表示上游在提交前失败。
	aborted  bool
	finished bool
	// heartbeat 是心跳间隔；0 = 不发心跳。
	heartbeat time.Duration
	// stopHeartbeat 停心跳。
	stopHeartbeat chan struct{}
	// heartbeatOnce 保证只停一次。
	heartbeatOnce sync.Once
}

// newSSEWriter 建一个 SSE 写出器。
func newSSEWriter(w http.ResponseWriter, r *http.Request) *sseWriter {
	flusher, _ := w.(http.Flusher)
	s := &sseWriter{
		w:             w,
		flusher:       flusher,
		heartbeat:     15 * time.Second,
		stopHeartbeat: make(chan struct{}),
	}
	// 客户端断开 → 停心跳。
	if r != nil {
		go func() {
			<-r.Context().Done()
			s.stop()
		}()
	}
	go s.runHeartbeat()
	return s
}

// commit 提交响应头（写 200 + Content-Type）。
func (s *sseWriter) commit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed || s.aborted {
		return
	}
	s.committed = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	// ⚠️ 告诉 Nginx 不要缓冲。
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
	s.flush()
}

// abort 放弃这次响应（还没提交）——
// 调用方可以改为写一个真正的 HTTP 错误状态。
func (s *sseWriter) abort() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed {
		return
	}
	s.aborted = true
	s.stopHeartbeatOnce()
}

// WriteSSE 写一个 SSE 帧。
func (s *sseWriter) WriteSSE(name string, data any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.aborted {
		return nil
	}
	if !s.committed {
		// ⚠️ 没提交就写内容 —— 那就提交。
		s.committed = true
		h := s.w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		s.w.WriteHeader(http.StatusOK)
	}
	payload := marshalJSON(data)
	if name != "" {
		if _, err := fmt.Fprintf(s.w, "event: %s\n", name); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", payload); err != nil {
		return err
	}
	s.flush()
	return nil
}

// WriteJSON 实现 lane.Writer（非流式路径用不到，接口要求）。
func (s *sseWriter) WriteJSON(status int, payload any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed {
		return nil
	}
	s.committed = true
	h := s.w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	s.w.WriteHeader(status)
	_, err := s.w.Write(marshalJSON(payload))
	return err
}

// HeadersSent 实现 lane.Writer。
func (s *sseWriter) HeadersSent() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.committed
}

// finish 收尾（写 [DONE]）。
func (s *sseWriter) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished || s.aborted {
		return
	}
	s.finished = true
	s.stopHeartbeatOnce()
	if !s.committed {
		s.committed = true
		h := s.w.Header()
		h.Set("Content-Type", "text/event-stream; charset=utf-8")
		s.w.WriteHeader(http.StatusOK)
	}
	// Anthropic 线不写 [DONE]（它的终止帧是 message_stop）。
	_, _ = s.w.Write([]byte("data: [DONE]\n\n"))
	s.flush()
}

// flush 把缓冲推出去。
func (s *sseWriter) flush() {
	if s.flusher != nil {
		s.flusher.Flush()
	}
}

// runHeartbeat 定期发心跳。
func (s *sseWriter) runHeartbeat() {
	if s.heartbeat <= 0 {
		return
	}
	ticker := time.NewTicker(s.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-s.stopHeartbeat:
			return
		case <-ticker.C:
			s.mu.Lock()
			if !s.committed || s.finished {
				s.mu.Unlock()
				continue
			}
			// 注释行 —— 对端会忽略，但能证明连接还活着。
			_, _ = s.w.Write([]byte(": ping\n\n"))
			s.flush()
			s.mu.Unlock()
		}
	}
}

// stop 停心跳（客户端断开时）。
func (s *sseWriter) stop() {
	s.stopHeartbeatOnce()
}

// stopHeartbeatOnce 停心跳（幂等）。
func (s *sseWriter) stopHeartbeatOnce() {
	s.heartbeatOnce.Do(func() {
		close(s.stopHeartbeat)
	})
}

// marshalJSON 序列化。
func marshalJSON(v any) []byte {
	b, err := jsonMarshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

// bufioReaderPool 复用 bufio.Reader（省分配）。
var bufioReaderPool = sync.Pool{
	New: func() any { return bufio.NewReaderSize(nil, 4096) },
}
