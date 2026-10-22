package main

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

// attachLogFile 让日志**同时**写进 <数据目录>/fmh.log。
//
// ⚠️ **为什么需要它** ——
//
//	stdout 只进 docker logs，容器一重建就没了。
//	而排障最需要的恰恰是"**当时发生了什么**"——
//	比如"探测 27 个模型几秒就完"这种症状，
//	只有把那一轮的日志捞出来才看得见。
//
//	数据目录是 bind mount，容器外直接就能拿到这个文件。
//
// ⚠️ **失败不算错** ——
//
//	写不了日志不该让网关起不来。
//	数据目录只读、磁盘满、权限 weird —— 这些情况下
//	只提示一句，继续用 stdout。
func attachLogFile(logger *log.Logger, dataDir string) {
	if dataDir == "" {
		return
	}
	path := filepath.Join(dataDir, "fmh.log")

	// ⚠️ **目录可能还不存在** ——
	//
	//	首次启动时数据目录是空的（数据库是后面才建的）。
	//	不开这个就会 "no such file or directory"，日志文件永远建不出来。
	//
	//	⚠️ **降权之后建目录** —— 属主天然是 nobody，
	//	后面不需要 chown（降权后就改不了属主了）。
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		logger.Printf("⚠️ 创建数据目录失败（日志只用 stdout）: %v", err)
		return
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		logger.Printf("⚠️ 打开日志文件失败（只用 stdout）: %v", err)
		return
	}
	// ⚠️ **限制大小** —— 免得日志把磁盘写满。
	//
	//	10MB 足够看清几千轮探测的上下文。
	limitLogFile(path, 10<<20)

	// ⚠️ **每行都刷** —— 排障时正在跑的那一轮最重要，
	//   缓冲着就看不到"卡在哪一步"。
	writer := &syncWriter{w: f}
	logger.SetOutput(io.MultiWriter(os.Stdout, writer))
	logger.Printf("日志文件 %s", path)
}

// limitLogFile 把日志文件裁到 maxSize 以内（超出就从头截断）。
func limitLogFile(path string, maxSize int64) {
	st, err := os.Stat(path)
	if err != nil || st.Size() <= maxSize {
		return
	}
	// ⚠️ **保留尾部** —— 最近的内容才是排障要看的。
	data, err := os.ReadFile(path)
	if err != nil || int64(len(data)) <= maxSize {
		return
	}
	tail := data[int64(len(data))-maxSize:]
	// ⚠️ 从第一个完整行开始切 —— 半行会让日志看起来像坏了。
	if i := indexByte(tail, '\n'); i >= 0 && i+1 < len(tail) {
		tail = tail[i+1:]
	}
	_ = os.WriteFile(path, tail, 0o644)
}

// indexByte 找字节在切片里的位置（没有就返回 -1）。
func indexByte(b []byte, c byte) int {
	for i := range b {
		if b[i] == c {
			return i
		}
	}
	return -1
}

// syncWriter 是一个并发安全的 writer。
//
// ⚠️ log.Logger 本身是并发安全的，但**它会并发调 Write** ——
//
//	而 os.File 的 Write 是并发安全的，所以其实不需要这层。
//	但 MultiWriter 会把同一次写入拆给多个 writer，
//	保持一个明确的同步点更安全（探测线程 + HTTP 线程同时写）。
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
