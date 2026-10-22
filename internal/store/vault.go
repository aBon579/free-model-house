package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// 本文件是**凭据的密封存储**。
// 核心原则：**上游凭据永不离开本机**。
// 加密方案：AES-256-GCM。
// 密钥来自环境变量 `FMH_SECRET_KEY`，未设置时首次启动生成并写入
// `/data/.secret-key`（0600）。
// ⚠️ 丢失密钥 = 已存的上游凭据无法解密 —— 只能在管理台重新录入。
//   所以这个文件必须备份，而且**不能**和数据库分开存放（否则备份容易漏）。

// Vault 是密封的凭据存储。
type Vault struct {
	mu sync.RWMutex
	// key 是 AES-256 主密钥。
	key []byte
	// path 是密钥文件路径（生成时用）。
	path string
}

// ErrNoSecretKey 表示没有可用的解密密钥。
var ErrNoSecretKey = errors.New("没有可用的凭据加密密钥")

// NewVault 建一个密封存储。
// 优先用环境变量；没有就从 path 读；再没有就生成一个新的并写入。
func NewVault(dataDir string) (*Vault, error) {
	key, err := loadOrCreateSecretKey(dataDir)
	if err != nil {
		return nil, err
	}
	return &Vault{
		key:  key,
		path: filepath.Join(dataDir, secretKeyFile),
	}, nil
}

// secretKeyFile 是密钥文件名。
const secretKeyFile = ".secret-key"

// loadOrCreateSecretKey 取密钥：环境变量 → 文件 → 新生成。
func loadOrCreateSecretKey(dataDir string) ([]byte, error) {
	if env := os.Getenv("FMH_SECRET_KEY"); env != "" {
		return decodeSecretKey(env)
	}
	path := filepath.Join(dataDir, secretKeyFile)
	if raw, err := os.ReadFile(path); err == nil {
		key, err := decodeSecretKey(string(raw))
		if err != nil {
			return nil, fmt.Errorf("密钥文件 %s 无法解析: %w", path, err)
		}
		return key, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(key)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		return nil, fmt.Errorf("写入密钥文件失败: %w", err)
	}
	return key, nil
}

// decodeSecretKey 解析一个密钥（base64 或 hex）。
func decodeSecretKey(v string) ([]byte, error) {
	v = trimSpace(v)
	if v == "" {
		return nil, ErrNoSecretKey
	}
	// base64（标准或 URL 安全）。
	for _, enc := range []*base64.Encoding{
		base64.StdEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.RawURLEncoding,
	} {
		if b, err := enc.DecodeString(v); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	if b, err := hexDecode(v); err == nil && len(b) == 32 {
		return b, nil
	}
	return nil, fmt.Errorf("密钥必须是 32 字节的 base64 或 hex")
}

// Seal 加密一个明文。
// 每次调用都用**新的随机 nonce** —— 所以同一个明文两次加密的结果不同。
func (v *Vault) Seal(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	v.mu.RLock()
	key := v.key
	v.mu.RUnlock()
	if len(key) == 0 {
		return "", ErrNoSecretKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open 解密一个密文。
func (v *Vault) Open(sealed string) (string, error) {
	if sealed == "" {
		return "", nil
	}
	v.mu.RLock()
	key := v.key
	v.mu.RUnlock()
	if len(key) == 0 {
		return "", ErrNoSecretKey
	}
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil {
		return "", fmt.Errorf("密文不是合法 base64: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(raw) < gcm.NonceSize() {
		return "", errors.New("密文太短")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		// ⚠️ 认证失败 —— 密钥不对或密文被改过。
		// 绝不能返回一个"部分正确"的结果。
		return "", fmt.Errorf("解密失败（密钥可能不对）: %w", err)
	}
	return string(plain), nil
}

// HasKey 报告是否已经有一个可用的密钥。
func (v *Vault) HasKey() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.key) > 0
}

// Redact 从一段文本里抹掉一个凭据。
// ⚠️ **两种拼法都要搜** —— 原始的，和它的 JSON 转义形式
// （因为文本可能已经被 JSON 编码过了）。
// ⚠️ **脱敏要在截断之前做** —— 先切片可能把一个 key 切成两半，留下前半截。
func Redact(value, secret string) string {
	if secret == "" || value == "" {
		return value
	}
	out := replaceAll(value, secret, "‹已隐去›")
	// JSON 转义形式。
	escaped, err := jsonMarshalString(secret)
	if err == nil && escaped != secret {
		out = replaceAll(out, escaped, "‹已隐去›")
	}
	return out
}

// CredentialHint 生成凭据指纹（永不回显全文）。
// 让人能确认"配对了"，但不足以被用来调用。
func CredentialHint(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return "…"
	}
	return key[:8] + "…" + key[len(key)-4:]
}

// trimSpace 去空白。
func trimSpace(s string) string {
	start := 0
	end := len(s)
	for start < end && isSpace(s[start]) {
		start++
	}
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

// isSpace 判断是不是空白字符。
func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}
