// Package qoderwasm prepares Qoder requests using the embedded official WASM.
// It performs no network requests and reads no credentials from the host.
//
// qoder-auth-official.wasm 是上游客户端的**原始二进制**，未反编译、未修改，
// 也不是本项目的作品 —— 带上它只因为请求的准备工作必须由这段代码自己完成。
// 它的完整性由 TestOfficialWASMIntegrity 里的 SHA-256 固定：换文件必测试红。
package qoderwasm

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tetratelabs/wazero"
)

//go:embed qoder-auth-official.wasm
var officialWASM []byte

// Only compiled code is shared. Linear memory, JS objects and credentials are
// isolated in a new runtime for every call, including concurrent calls.
var compilationCache = wazero.NewCompilationCache()

// Request is a prepared request, not an executed HTTP operation.
type Request struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Body    string            `json:"body"`
}

type userInfo struct {
	UID                string   `json:"uid"`
	SecurityOauthToken string   `json:"securityOauthToken"`
	OrganizationID     string   `json:"organizationId,omitempty"`
	OrganizationTags   []string `json:"organizationTags,omitempty"`
	DataPolicyAgreed   bool     `json:"dataPolicyAgreed,omitempty"`
}

type authFields struct {
	EncryptUserInfo string `json:"encrypt_user_info"`
	Key             string `json:"key"`
}

// Prepare generates authentication fields and an inference request entirely in
// process. host may be an HTTPS origin or a bare host[:port]; paths, userinfo,
// queries and fragments are rejected. Errors never include user or WASM text.
func Prepare(ctx context.Context, userJSON []byte, machineID, version, host, model string, payload []byte) (req Request, err error) {
	if err = ctx.Err(); err != nil {
		return Request{}, err
	}
	// Normalize cancellation, and never return a partially prepared request.
	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			req = Request{}
		}
	}()
	origin, err := validateHost(host)
	if err != nil {
		return Request{}, err
	}
	if len(userJSON) > 1<<20 || len(payload) > 16<<20 {
		return Request{}, errors.New("qoderwasm: input exceeds size limit")
	}
	var user userInfo
	if !utf8.Valid(userJSON) || json.Unmarshal(userJSON, &user) != nil || strings.TrimSpace(user.UID) == "" || strings.TrimSpace(user.SecurityOauthToken) == "" {
		return Request{}, errors.New("qoderwasm: invalid user JSON or missing uid/token")
	}
	for _, s := range []string{machineID, version, model} {
		if strings.TrimSpace(s) == "" || len(s) > 4096 || !utf8.ValidString(s) || strings.ContainsAny(s, "\r\n\x00") {
			return Request{}, errors.New("qoderwasm: invalid machine ID, version or model")
		}
	}
	if !utf8.Valid(payload) || !json.Valid(payload) {
		return Request{}, errors.New("qoderwasm: payload must be UTF-8 JSON")
	}

	rt := wazero.NewRuntimeWithConfig(ctx, wazero.NewRuntimeConfig().
		WithCompilationCache(compilationCache).
		WithMemoryLimitPages(4096).
		WithCloseOnContextDone(true))
	defer rt.Close(context.Background())
	compiled, err := rt.CompileModule(ctx, officialWASM)
	if err != nil {
		return Request{}, errors.New("qoderwasm: compile official module failed")
	}
	// Do not close compiled code here: the process-lifetime compilation cache
	// owns it. Closing a request runtime still releases its instance and heap.
	heap := newObjectHeap()
	if err = heap.instantiate(ctx, rt, compiled); err != nil {
		return Request{}, errors.New("qoderwasm: instantiate host imports failed")
	}
	mod, err := rt.InstantiateModule(ctx, compiled, wazero.NewModuleConfig().WithStartFunctions())
	if err != nil {
		return Request{}, errors.New("qoderwasm: instantiate official module failed")
	}
	bridge := wasmBridge{ctx: ctx, mod: mod, heap: heap}
	canonicalUser, _ := json.Marshal(user)
	authJSON, err := bridge.stringResult("generate_runtime_auth_fields", nil, []string{string(canonicalUser)}, true)
	if err != nil {
		return Request{}, err
	}
	var auth authFields
	if json.Unmarshal([]byte(authJSON), &auth) != nil || auth.EncryptUserInfo == "" || auth.Key == "" {
		return Request{}, errors.New("qoderwasm: invalid authentication result")
	}
	tags := user.OrganizationTags
	if tags == nil {
		tags = []string{}
	}
	contextUser, _ := json.Marshal(struct {
		UID             string   `json:"uid"`
		EncryptUserInfo string   `json:"encrypt_user_info"`
		Key             string   `json:"key"`
		OrganizationID  string   `json:"organization_id"`
		Tags            []string `json:"organization_tags"`
		Policy          bool     `json:"data_policy_agreed"`
	}{user.UID, auth.EncryptUserInfo, auth.Key, user.OrganizationID, tags, user.DataPolicyAgreed})
	const metadata = `{"client_type":"5","business_product":"cli","business_type":"agent","scene":"assistant"}`
	contextPtr, err := bridge.pointerResult("qodercontext_new", nil, []string{machineID, version, string(contextUser), metadata})
	if err != nil {
		return Request{}, err
	}
	defer bridge.release("__wbg_qodercontext_free", uint64(contextPtr), 0)
	resultPtr, err := bridge.pointerResult("qodercontext_prepareInferRequest", []uint64{uint64(contextPtr)}, []string{origin, string(payload), model, "system"})
	if err != nil {
		return Request{}, err
	}
	defer bridge.release("__wbg_requestresult_free", uint64(resultPtr), 0)
	args := []uint64{uint64(resultPtr)}
	if req.URL, err = bridge.stringResult("requestresult_url", args, nil, false); err != nil {
		return Request{}, err
	}
	if req.Body, err = bridge.stringResult("requestresult_body", args, nil, false); err != nil {
		return Request{}, err
	}
	headerResult, err := bridge.call("requestresult_headers", args...)
	if err != nil {
		return Request{}, err
	}
	headers, ok := heap.take(uint32(headerResult[0])).(map[string]string)
	if !ok {
		return Request{}, errors.New("qoderwasm: headers were not a Map")
	}
	req.Headers = make(map[string]string, len(headers))
	for k, v := range headers {
		if !validHeaderName(k) || strings.ContainsAny(v, "\r\n\x00") {
			return Request{}, errors.New("qoderwasm: invalid request header")
		}
		req.Headers[k] = v
	}
	if err = validateRequest(req, origin, user.SecurityOauthToken); err != nil {
		return Request{}, err
	}
	return req, nil
}

func validateHost(host string) (string, error) {
	invalid := errors.New("qoderwasm: host must be an HTTPS origin or bare authority")
	if host == "" || strings.TrimSpace(host) != host || strings.ContainsAny(host, "\\?#@\r\n\x00") {
		return "", invalid
	}
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return "", invalid
	}
	hostname := u.Hostname()
	if hostname == "" || strings.HasSuffix(u.Host, ":") {
		return "", invalid
	}
	if net.ParseIP(hostname) == nil {
		if strings.ContainsAny(u.Host, "[]") || len(hostname) > 253 {
			return "", invalid
		}
		for _, label := range strings.Split(hostname, ".") {
			if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return "", invalid
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
					return "", invalid
				}
			}
		}
	} else if strings.Contains(hostname, ":") && !strings.HasPrefix(u.Host, "[") {
		return "", invalid
	}
	if port := u.Port(); port != "" {
		p, e := strconv.Atoi(port)
		if e != nil || p < 1 || p > 65535 {
			return "", invalid
		}
	}
	return "https://" + u.Host, nil
}

func validHeaderName(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return false
		}
	}
	return true
}

func validateRequest(req Request, origin, token string) error {
	u, err := url.Parse(req.URL)
	if err != nil || u.Scheme != "https" || "https://"+u.Host != origin || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return errors.New("qoderwasm: generated URL escaped the specified HTTPS host")
	}
	authorized := false
	for k, v := range req.Headers {
		if strings.EqualFold(k, "authorization") {
			// The official module emits "Bearer COSY.<encrypted payload>".
			authorized = strings.HasPrefix(v, "Bearer COSY.") && len(v) > len("Bearer COSY.")
		}
		if strings.Contains(k, token) || strings.Contains(v, token) {
			return errors.New("qoderwasm: plaintext token in request")
		}
	}
	if !authorized {
		return errors.New("qoderwasm: missing COSY authorization")
	}
	if strings.Contains(req.URL, token) || strings.Contains(req.Body, token) {
		return errors.New("qoderwasm: plaintext token in request")
	}
	return nil
}
