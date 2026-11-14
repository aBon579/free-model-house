package credit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Credential map[string]any

func (c Credential) Text(keys ...string) string {
	for _, k := range keys {
		if v, ok := c[k].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (c Credential) Clone() Credential {
	b, _ := json.Marshal(c)
	var out Credential
	_ = json.Unmarshal(b, &out)
	return out
}

type Model struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	ContextWindow int      `json:"contextWindow"`
	MaxOutput     int      `json:"maxOutput,omitempty"`
	Vision        bool     `json:"vision"`
	Reasoning     bool     `json:"reasoning"`
	Efforts       []string `json:"efforts,omitempty"`
	Multiplier    *float64 `json:"multiplier,omitempty"`
	Function      string   `json:"function,omitempty"`
	MaxMode       bool     `json:"maxMode,omitempty"`
}

type Balance struct {
	Name      string   `json:"name"`
	Total     *float64 `json:"total,omitempty"`
	Used      *float64 `json:"used,omitempty"`
	Remaining *float64 `json:"remaining,omitempty"`
	Unit      string   `json:"unit"`
	ExpiresAt int64    `json:"expiresAt,omitempty"`
}

type Campaign struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Amount    *float64 `json:"amount,omitempty"`
	Claimable bool     `json:"claimable"`
	Claimed   bool     `json:"claimed"`
	ExpiresAt int64    `json:"expiresAt,omitempty"`
}

type Credits struct {
	Balances  []Balance  `json:"balances"`
	Campaigns []Campaign `json:"campaigns"`
	UpdatedAt int64      `json:"updatedAt"`
	Warning   string     `json:"warning,omitempty"`
}

type ClaimResult struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Claimed        bool     `json:"claimed"`
	AlreadyClaimed bool     `json:"alreadyClaimed"`
	Amount         *float64 `json:"amount,omitempty"`
	Message        string   `json:"message"`
}

type Login struct {
	ID        string     `json:"id"`
	Provider  string     `json:"provider"`
	URL       string     `json:"url"`
	State     string     `json:"state"`
	Message   string     `json:"message,omitempty"`
	AccountID string     `json:"accountId,omitempty"`
	ExpiresAt int64      `json:"expiresAt"`
	Interval  int        `json:"interval"`
	Data      Credential `json:"-"`
}

type Inference struct {
	Model     Model
	Messages  []map[string]any
	Tools     []map[string]any
	MaxTokens int
	Effort    string
	MaxMode   bool
	// OfficialIdentity 由账号设置带入，目前只有 ZCode 适配器读它：
	// true 表示按官方客户端的形态带 system 身份块。
	OfficialIdentity bool
	SessionID        string
}

type Prepared struct {
	URL     string
	Headers map[string]string
	Body    []byte
	Wire    string
}

type Adapter interface {
	StartLogin(context.Context, string) (*Login, error)
	PollLogin(context.Context, *Login, string) (Credential, bool, error)
	Validate(Credential) error
	Refresh(context.Context, Credential) (Credential, error)
	Models(context.Context, Credential) ([]Model, error)
	Credits(context.Context, Credential) (Credits, error)
	Claim(context.Context, Credential, string, string) (ClaimResult, error)
	Prepare(context.Context, Credential, Inference) (Prepared, error)
}

type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string { return e.Message }

func JSONRequest(ctx context.Context, client *http.Client, method, endpoint string, headers map[string]string, body any, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(b))
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return errors.New("上游请求地址无效")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("无法连接上游，请检查出口代理或稍后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Message: fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out); err != nil {
		return errors.New("上游返回了无法解析的数据")
	}
	return nil
}

func Object(v any) map[string]any {
	m, _ := v.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}
func Array(v any) []any { a, _ := v.([]any); return a }
func Text(v any) string { s, _ := v.(string); return s }
func Number(v any) *float64 {
	switch n := v.(type) {
	case float64:
		return &n
	case json.Number:
		f, err := n.Float64()
		if err == nil {
			return &f
		}
	case string:
		f, err := strconv.ParseFloat(n, 64)
		if err == nil {
			return &f
		}
	}
	return nil
}
func Timestamp(v any) int64 {
	if n := Number(v); n != nil {
		t := int64(*n)
		if t > 0 && t < 100000000000 {
			t *= 1000
		}
		return t
	}
	if s := Text(v); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UnixMilli()
		}
	}
	return 0
}
