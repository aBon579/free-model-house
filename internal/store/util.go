package store

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
)

// 本文件是一些小的工具函数（避免到处 import）。

// hexDecode 解一个 hex 串。
func hexDecode(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s)%2 != 0 {
		return nil, errOddLength
	}
	return hex.DecodeString(s)
}

// errOddLength 是奇数长度的 hex。
var errOddLength = errString("hex 串长度必须是偶数")

// errString 是一个简单的错误类型。
type errString string

func (e errString) Error() string { return string(e) }

// replaceAll 替换所有出现。
func replaceAll(s, old, new string) string {
	if old == "" {
		return s
	}
	return strings.ReplaceAll(s, old, new)
}

// jsonMarshalString 把一个字符串 JSON 编码（带引号）。
func jsonMarshalString(s string) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// readFile 读一个文件（测试用）。
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// GenerateKey 生成一把带前缀的随机 Key。
//
// 前缀让它**自描述** —— 一眼看出它是本机 Key 还是 LAN Key，
// 也防止把 LAN Key 误填到只允许本机 Key 的地方。
//
// 20 字节 = 160 bit 熵。
func GenerateKey(prefix string) string {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		// 随机源失败 —— 这是极罕见的情况，但必须给出一个可用的 key。
		// 退化成时间派生（熵降低，但不至于让服务起不来）。
		now := timeNowUnixNano()
		for i := range b {
			b[i] = byte(now >> (8 * (i % 8)))
		}
	}
	return prefix + hex.EncodeToString(b)
}
