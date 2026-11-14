package credit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every test uses a synthetic RoundTripper. No real login, credentials, SDK execution,
// captcha completion, or inference takes place in this suite.
type zcodeTestTransport func(*http.Request) (*http.Response, error)

func (f zcodeTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func zcodeTestAdapter(f zcodeTestTransport) *zcodeAdapter {
	return NewZCode(&http.Client{Transport: f}).(*zcodeAdapter)
}
func zcodeTestResponse(status int, body any) *http.Response {
	b, _ := json.Marshal(body)
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(b)))}
}
func zcodeTestOK(data any) *http.Response {
	return zcodeTestResponse(200, map[string]any{"code": 0, "data": data})
}
func zcodeTestJWT(claims map[string]any) string {
	b, _ := json.Marshal(claims)
	return "eyJhbGciOiJIUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(b) + ".test-signature"
}
func zcodeTestCredential() Credential {
	return Credential{"accessToken": "test-opaque-access", "uid": "test-user", "machineId": "test-machine", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}
}
func zcodeTestCaptcha() map[string]any {
	return map[string]any{"configs": map[string]any{"captcha": map[string]any{"sceneId": "test-scene", "prefix": "test-prefix", "region": "test-region"}}}
}
func zcodeTestDecode(t *testing.T, r io.Reader) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		t.Fatal(err)
	}
	return m
}
func zcodeTestAPI(t *testing.T, err error, code string) *APIError {
	t.Helper()
	var api *APIError
	if !errors.As(err, &api) || api.Code != code {
		t.Fatalf("expected API code %q, got %v", code, err)
	}
	return api
}

func TestZCodeLoginFlowAndExistingSubscription(t *testing.T) {
	for _, provider := range []string{"bigmodel", "zai"} {
		t.Run(provider, func(t *testing.T) {
			var calls []string
			var secret string
			polls := 0
			exp := time.Now().Add(time.Hour).Unix()
			token := zcodeTestJWT(map[string]any{"userId": "test-user", "exp": exp})
			z := zcodeTestAdapter(func(r *http.Request) (*http.Response, error) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				switch r.URL.Path {
				case "/api/v1/oauth/cli/init":
					if r.Method != "POST" || zcodeTestDecode(t, r.Body)["provider"] != provider {
						t.Fatal("wrong init")
					}
					secret = r.Header.Get("Authorization")
					if len(strings.TrimPrefix(secret, "Bearer ")) != 64 {
						t.Fatal("flow secret is not 32 random bytes")
					}
					target := "https://bigmodel.cn/login?redirect=" + url.QueryEscape(zcodeOrigin+"/oauth/cli/callback/bigmodel")
					if provider == "zai" {
						target = "https://chat.z.ai/api/oauth/authorize?redirect_uri=" + url.QueryEscape(zcodeOrigin+"/oauth/cli/callback/zai")
					}
					return zcodeTestOK(map[string]any{"flow_id": "test-flow", "poll_token": "test-poll-token", "authorize_url": target, "expires_at": time.Now().Add(3 * time.Minute).Unix(), "poll_interval_sec": 3}), nil
				case "/api/v1/oauth/cli/poll/test-flow":
					if r.Method != "GET" || r.Header.Get("Authorization") != secret {
						t.Fatal("incorrect poll authorization")
					}
					polls++
					if polls == 1 {
						return zcodeTestOK(map[string]any{"status": "pending"}), nil
					}
					if polls == 2 {
						return zcodeTestResponse(503, map[string]any{"message": "secret-never-exposed"}), nil
					}
					return zcodeTestOK(map[string]any{"status": "ready", "token": token, "user": map[string]any{"user_id": "test-user", "name": "Test User"}, provider: map[string]any{"access_token": "test-provider-token"}}), nil
				case "/api/biz/customer/getCustomerInfo":
					if r.URL.Host != "api.z.ai" || r.Method != "GET" || r.Header.Get("Authorization") != "Bearer test-provider-token" {
						t.Fatal("incorrect read-only subscription authentication")
					}
					return zcodeTestOK(map[string]any{"organizations": []any{
						map[string]any{"organizationId": "other", "projects": []any{map[string]any{"projectId": "other"}}},
						map[string]any{"organizationId": "test-org", "organizationName": "默认机构", "projects": []any{
							map[string]any{"projectId": "excluded", "projectName": "默认项目", "projectType": "2"},
							map[string]any{"projectId": "also-excluded", "projectName": "默认项目", "projectType": 2},
							map[string]any{"projectId": "test-project", "projectName": "默认项目", "projectType": 1},
						}},
					}}), nil
				case "/api/biz/v1/organization/test-org/projects/test-project/api_keys":
					if r.Method != "GET" {
						t.Fatal("must never create a key")
					}
					list := []any{map[string]any{"name": "unrelated-key", "apiKey": "do-not-copy"}, map[string]any{"name": "zcode-api-key", "apiKey": "test-key"}}
					if provider == "zai" {
						return zcodeTestOK(list), nil
					}
					return zcodeTestResponse(200, list), nil
				case "/api/biz/v1/organization/test-org/projects/test-project/api_keys/copy/test-key":
					if r.Method != "GET" {
						t.Fatal("copy must be read-only")
					}
					if provider == "zai" {
						return zcodeTestOK(map[string]any{"secretKey": "test-secret"}), nil
					}
					return zcodeTestResponse(200, map[string]any{"secretKey": "test-secret"}), nil
				default:
					t.Fatalf("unexpected endpoint %s", r.URL.Path)
					return nil, nil
				}
			})
			login, err := z.StartLogin(context.Background(), provider)
			if err != nil {
				t.Fatal(err)
			}
			if login.Interval != 3 || login.State != "pending" || login.Provider != "zcode" || len(login.Data.Text("machineId")) != 36 {
				t.Fatal("invalid login metadata")
			}
			encoded, _ := json.Marshal(login)
			if strings.Contains(string(encoded), "test-poll-token") || strings.Contains(string(encoded), strings.TrimPrefix(secret, "Bearer ")) {
				t.Fatal("public login leaked flow credentials")
			}
			for i := 0; i < 2; i++ {
				c, ready, err := z.PollLogin(context.Background(), login, "")
				if err != nil || ready || c != nil {
					t.Fatalf("pending/retry failed: %v", err)
				}
			}
			c, ready, err := z.PollLogin(context.Background(), login, "")
			if err != nil || !ready {
				t.Fatalf("ready failed: %v", err)
			}
			if c.Text("accessToken") != token || c.Text("uid") != "test-user" || c.Text("codingPlanKey") != "test-key.test-secret" || c.Text("machineId") != login.Data.Text("machineId") || c["expiresAt"] != exp*1000 {
				t.Fatal("incorrect canonical credential")
			}
			if len(calls) != 7 {
				t.Fatalf("expected init + 3 polls + 3 read-only subscription requests, got %d", len(calls))
			}
		})
	}
}

func TestZCodeSubscriptionNeverCreatesKeys(t *testing.T) {
	for _, kind := range []string{"no-org", "no-key", "no-secret", "http-error"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			z := zcodeTestAdapter(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" {
					t.Fatal("key creation forbidden")
				}
				if kind == "http-error" {
					return zcodeTestResponse(403, map[string]any{"message": "test-secret"}), nil
				}
				if strings.HasSuffix(r.URL.Path, "getCustomerInfo") {
					if kind == "no-org" {
						return zcodeTestOK(map[string]any{}), nil
					}
					return zcodeTestOK(map[string]any{"organizations": []any{map[string]any{"organizationId": "org", "projects": []any{map[string]any{"projectId": "project"}}}}}), nil
				}
				if strings.HasSuffix(r.URL.Path, "api_keys") {
					name := "zcode-api-key"
					if kind == "no-key" {
						name = "other"
					}
					return zcodeTestResponse(200, []any{map[string]any{"name": name, "apiKey": "key"}}), nil
				}
				return zcodeTestResponse(200, map[string]any{}), nil
			})
			key, err := z.existingCodingKey(context.Background(), zcodeTestCredential(), "test-provider-token")
			if key != "" || calls > 3 {
				t.Fatal("unexpected secret or extra request")
			}
			if kind != "http-error" && err != nil {
				t.Fatal(err)
			}
			if err != nil && strings.Contains(err.Error(), "test-secret") {
				t.Fatal("secret leak")
			}
		})
	}
}

func TestZCodeAuthorizationURLSafety(t *testing.T) {
	bad := []string{"http://bigmodel.cn/login", "https://bigmodel.cn.evil.test/login", "https://evil.test/?bigmodel.cn", "https://bigmodel.cn@evil.test/", "https://user@bigmodel.cn/login", "https://bigmodel.cn:8443/login", "javascript:alert(1)", "https://bigmodel.cn/login?redirect=https%3A%2F%2Fevil.test", "https://bigmodel.cn/login?redirect_uri=http%3A%2F%2Fzcode.z.ai", "https://chat.z.ai/api/oauth/authorize"}
	for _, target := range bad {
		t.Run(target, func(t *testing.T) {
			z := zcodeTestAdapter(func(*http.Request) (*http.Response, error) {
				return zcodeTestOK(map[string]any{"flow_id": "flow", "authorize_url": target}), nil
			})
			_, err := z.StartLogin(context.Background(), "bigmodel")
			zcodeTestAPI(t, err, "invalid_authorization_url")
		})
	}
	z := zcodeTestAdapter(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid provider should not access network")
		return nil, nil
	})
	_, err := z.StartLogin(context.Background(), "unknown")
	zcodeTestAPI(t, err, "invalid_provider")
}

func TestZCodeRedirectsCannotLeakCredentials(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: zcodeTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "zcode.z.ai" {
			t.Fatal("followed unsafe redirect")
		}
		resp := zcodeTestResponse(302, map[string]any{})
		resp.Header.Set("Location", "https://evil.test/collect")
		return resp, nil
	}), CheckRedirect: func(*http.Request, []*http.Request) error {
		t.Fatal("must replace unsafe redirect policy on a copy")
		return nil
	}}
	z := NewZCode(client)
	_, err := z.StartLogin(context.Background(), "bigmodel")
	if err == nil || calls != 1 || client.Timeout != 0 {
		t.Fatal("redirect guard or non-mutation failed")
	}
}

func TestZCodePollAndLoginErrors(t *testing.T) {
	for _, tc := range []struct{ status, want string }{{"failed", "login_denied"}, {"denied", "login_denied"}, {"expired", "login_expired"}, {"unknown", "invalid_response"}, {"ready", "invalid_credential"}} {
		t.Run(tc.status, func(t *testing.T) {
			z := zcodeTestAdapter(func(*http.Request) (*http.Response, error) {
				return zcodeTestOK(map[string]any{"status": tc.status, "message": "private-value"}), nil
			})
			login := &Login{Data: Credential{"flowID": "test", "flowSecret": "test-secret", "machineId": "test-machine", "oauthProvider": "zai"}}
			_, _, err := z.PollLogin(context.Background(), login, "")
			zcodeTestAPI(t, err, tc.want)
			if strings.Contains(err.Error(), "private-value") {
				t.Fatal("upstream message leaked")
			}
		})
	}
	z := zcodeTestAdapter(func(*http.Request) (*http.Response, error) { t.Fatal("expired flow must not poll"); return nil, nil })
	_, _, err := z.PollLogin(context.Background(), &Login{ExpiresAt: 1, Data: Credential{"flowID": "test", "flowSecret": "test-secret"}}, "")
	zcodeTestAPI(t, err, "login_expired")
	_, _, err = z.PollLogin(context.Background(), nil, "")
	zcodeTestAPI(t, err, "invalid_login")
}

func TestZCodeValidateAndJWTMetadata(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	for _, field := range []string{"userId", "uid"} {
		c := Credential{"token": zcodeTestJWT(map[string]any{field: "test-user", "exp": future}), "machineId": "test-machine"}
		z := NewZCode(nil)
		if err := z.Validate(c); err != nil {
			t.Fatal(err)
		}
		got, err := zcodeCredential(c)
		if err != nil || got.Text("uid") != "test-user" || got["expiresAt"] != future*1000 {
			t.Fatal("JWT metadata not normalized")
		}
		if c.Text("uid") != "" {
			t.Fatal("validation mutated input")
		}
	}
	invalid := []Credential{
		{}, {"accessToken": "opaque", "machineId": "machine"}, {"token": "opaque", "uid": "user"},
		{"token": "bad.bad.bad", "machineId": "machine", "uid": "user"},
		{"accessToken": "bad\r\nsecret", "uid": "user", "machineId": "machine"},
		{"token": zcodeTestJWT(map[string]any{"uid": "user", "exp": future}), "uid": "other", "machineId": "machine"},
	}
	for i, c := range invalid {
		t.Run(fmt.Sprint(i), func(t *testing.T) { zcodeTestAPI(t, NewZCode(nil).Validate(c), "invalid_credential") })
	}
	expired := Credential{"token": zcodeTestJWT(map[string]any{"uid": "user", "exp": 1}), "machineId": "machine"}
	zcodeTestAPI(t, NewZCode(nil).Validate(expired), "login_required")
}

func TestZCodeRefreshOnlyValidates(t *testing.T) {
	c := zcodeTestCredential()
	count := 0
	z := zcodeTestAdapter(func(r *http.Request) (*http.Response, error) {
		count++
		if r.Method != "GET" || r.URL.Path != "/api/v1/client/configs" {
			t.Fatal("invented refresh protocol")
		}
		return zcodeTestOK(map[string]any{}), nil
	})
	got, err := z.Refresh(context.Background(), c)
	if err != nil || count != 1 || got.Text("accessToken") != c.Text("accessToken") || got["expiresAt"] != c["expiresAt"] {
		t.Fatal("refresh changed token or expiry")
	}
	c["expiresAt"] = int64(1)
	_, err = z.Refresh(context.Background(), c)
	zcodeTestAPI(t, err, "login_required")
	if count != 1 {
		t.Fatal("expired credential must require a new login")
	}
	z = zcodeTestAdapter(func(*http.Request) (*http.Response, error) {
		return zcodeTestResponse(401, map[string]any{"msg": "test-private-token"}), nil
	})
	_, err = z.Refresh(context.Background(), zcodeTestCredential())
	zcodeTestAPI(t, err, "login_required")
	if strings.Contains(err.Error(), "test-private-token") {
		t.Fatal("upstream error leak")
	}
}

func TestZCodeDynamicModels(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint(nested), func(t *testing.T) {
			z := zcodeTestAdapter(func(r *http.Request) (*http.Response, error) {
				if r.URL.Query().Get("platform") != "unknown" || r.Header.Get("X-Device-Mid") == "" {
					t.Fatal("bad configs request")
				}
				models := map[string]any{"1": map[string]any{"modelId": "model-b", "modalities": map[string]any{"input": []string{"text", "image"}}}, "0": map[string]any{"modelId": "model-a", "name": "Model A", "contextWindow": 1000000, "maxCompletionTokens": 128000, "capabilities": map[string]any{"vision": true}, "multiplier": "0.5", "reasoning": map[string]any{"levels": map[string]any{"max": map[string]any{}, "high": map[string]any{}, "low": map[string]any{}, "custom": map[string]any{}}}}}
				d := map[string]any{"builtinModels": models}
				if nested {
					d = map[string]any{"configs": map[string]any{"builtinModels": []any{models["0"], models["1"]}}}
				}
				return zcodeTestOK(d), nil
			})
			c := zcodeTestCredential()
			c["codingPlanKey"] = "test-key.secret"
			models, err := z.Models(context.Background(), c)
			if err != nil || len(models) != 4 {
				t.Fatalf("models failed: %v", err)
			}
			m := models[0]
			if m.ID != "model-a" || m.ContextWindow != 1000000 || m.MaxOutput != 128000 || !m.Vision || !m.Reasoning || m.Multiplier == nil || *m.Multiplier != 0.5 || !reflect.DeepEqual(m.Efforts, []string{"low", "high", "max", "custom"}) {
				t.Fatal("dynamic model fields lost")
			}
			if models[1].ID != "coding/model-a" || models[1].Multiplier != nil || models[2].Multiplier != nil || models[2].ContextWindow != 0 || !models[2].Vision {
				t.Fatal("unknown/coding model metadata fabricated")
			}
		})
	}
}

func TestZCodeCreditsBalanceCampaignsAndDeduplication(t *testing.T) {
	var events []string
	z := zcodeTestAdapter(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer test-opaque-access" || r.Header.Get("X-Device-Mid") != "test-machine" {
			t.Fatal("credits auth missing")
		}
		switch r.URL.Path {
		case "/api/v1/zcode-plan/billing/balance":
			return zcodeTestOK(map[string]any{"balances": []any{
				map[string]any{"show_name": "Model A", "unit_type": "token", "total_units": "100", "used_units": 25, "remaining_units": 75, "available_units": 0, "expires_at": 2000000000},
				map[string]any{"show_name": "Unknown", "unit_type": "token", "remaining_units": nil},
			}}), nil
		case "/api/v1/event/report":
			d := zcodeTestDecode(t, r.Body)
			if d["device_mid"] != "test-machine" || r.Method != "POST" {
				t.Fatal("bad event")
			}
			events = append(events, Text(d["event"]))
			return zcodeTestOK(map[string]any{}), nil
		case "/api/v1/zcode-plan/billing/preview":
			return zcodeTestOK(map[string]any{"plans": []any{
				map[string]any{"plan_id": "plan-a", "name": "Daily", "entitlements": []any{map[string]any{"grant_units": 20, "unit_type": "token"}, map[string]any{"grant_units": 30, "unit_type": "token"}}},
				map[string]any{"plan_id": "plan-b", "claimed": true},
				map[string]any{"plan_id": "plan-c", "entitlements": []any{map[string]any{"grant_units": 1, "unit_type": "token"}, map[string]any{"grant_units": 1, "unit_type": "credit"}}},
			}}), nil
		default:
			t.Fatalf("unexpected endpoint %s", r.URL.Path)
			return nil, nil
		}
	})
	c := zcodeTestCredential()
	for i := 0; i < 2; i++ {
		credits, err := z.Credits(context.Background(), c)
		if err != nil {
			t.Fatal(err)
		}
		if len(credits.Balances) != 2 || *credits.Balances[0].Total != 100 || *credits.Balances[0].Remaining != 0 || credits.Balances[0].ExpiresAt != 2000000000000 || credits.Balances[1].Remaining != nil {
			t.Fatal("balance conversion wrong")
		}
		if len(credits.Campaigns) != 3 || *credits.Campaigns[0].Amount != 50 || !credits.Campaigns[0].Claimable || credits.Campaigns[1].Claimable || !credits.Campaigns[1].Claimed || credits.Campaigns[2].Amount != nil {
			t.Fatal("campaign conversion wrong")
		}
	}
	if !reflect.DeepEqual(events, []string{"app_launch", "app_daily_active"}) {
		t.Fatal("events were not deduplicated")
	}
	c["uid"] = "second-user"
	if _, err := z.Credits(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatal("dedup accidentally crossed account boundaries")
	}
}

func TestZCodeCreditsUnknownAndPreviewFailure(t *testing.T) {
	for _, kind := range []string{"unavailable", "enterprise", "empty", "preview-error"} {
		t.Run(kind, func(t *testing.T) {
			z := zcodeTestAdapter(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "balance") {
					if kind == "unavailable" {
						return zcodeTestResponse(503, map[string]any{"msg": "test-secret"}), nil
					}
					if kind == "enterprise" {
						return zcodeTestOK(map[string]any{"displayMode": "enterprise"}), nil
					}
					return zcodeTestOK(map[string]any{"balances": []any{}}), nil
				}
				if kind == "enterprise" {
					t.Fatal("enterprise must not report events or pretend campaigns exist")
				}
				if kind == "unavailable" && strings.HasSuffix(r.URL.Path, "report") {
					t.Fatal("do not report activity without authenticated balance")
				}
				if strings.HasSuffix(r.URL.Path, "preview") && kind == "preview-error" {
					return zcodeTestResponse(500, map[string]any{"msg": "test-secret"}), nil
				}
				return zcodeTestOK(map[string]any{}), nil
			})
			c, err := z.Credits(context.Background(), zcodeTestCredential())
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Balances) != 1 || c.Balances[0].Remaining != nil || c.Balances[0].Total != nil || c.Warning == "" || strings.Contains(c.Warning, "test-secret") {
				t.Fatal("unknown balance was made zero or error leaked")
			}
		})
	}
}

func TestZCodeActivityConcurrencyAndFailures(t *testing.T) {
	var count atomic.Int32
	z := zcodeTestAdapter(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "report") {
			count.Add(1)
			return zcodeTestResponse(500, map[string]any{}), nil
		}
		return zcodeTestOK(map[string]any{"balances": []any{}}), nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := z.Credits(context.Background(), zcodeTestCredential()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if count.Load() != 2 {
		t.Fatal("concurrent or failed event submissions were repeated")
	}
}

func TestZCodeClaimRequiresOfficialCaptchaParameter(t *testing.T) {
	z := zcodeTestAdapter(func(*http.Request) (*http.Response, error) {
		t.Fatal("missing captcha must not call upstream")
		return nil, nil
	})
	_, err := z.Claim(context.Background(), zcodeTestCredential(), "plan-a", "")
	api := zcodeTestAPI(t, err, "captcha_required")
	if api.Status != 409 || api.Message != "请在 Credit 页面完成人机验证" {
		t.Fatal("captcha error contract changed")
	}
	for _, tc := range []struct {
		name    string
		code    any
		status  int
		want    string
		already bool
	}{
		{"success", 0, 200, "", false}, {"already", "1003", 200, "", true}, {"already-http", 1003, 400, "", true},
		{"captcha", "3007", 400, "captcha_required", false}, {"blocked", "3012", 403, "unusual_activity", false},
		{"ineligible", 1004, 200, "not_eligible", false}, {"unknown", 9876, 200, "upstream_error", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := 0
			z := zcodeTestAdapter(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/v1/client/configs" {
					return zcodeTestOK(zcodeTestCaptcha()), nil
				}
				if r.URL.Path != "/api/v1/zcode-plan/billing/claim" {
					t.Fatal("unexpected endpoint")
				}
				claims++
				if r.Method != "POST" || r.Header.Get("x-aliyun-captcha-verify-param") != "test-user-completed-param" || r.Header.Get("x-aliyun-captcha-verify-region") != "test-region" || zcodeTestDecode(t, r.Body)["plan_id"] != "plan-a" {
					t.Fatal("claim must forward the given parameter and official region")
				}
				return zcodeTestResponse(tc.status, map[string]any{"code": tc.code, "msg": "test-secret-never-return", "data": map[string]any{"amount": 25}}), nil
			})
			result, err := z.Claim(context.Background(), zcodeTestCredential(), "plan-a", "test-user-completed-param")
			if claims != 1 {
				t.Fatal("claim or captcha automatically retried")
			}
			if tc.want != "" {
				zcodeTestAPI(t, err, tc.want)
				if strings.Contains(err.Error(), "test-secret") {
					t.Fatal("upstream secret leaked")
				}
				return
			}
			if err != nil || !result.Claimed || result.AlreadyClaimed != tc.already || strings.Contains(result.Message, "test-secret") {
				t.Fatalf("bad claim outcome: %v", err)
			}
			if !tc.already && (result.Amount == nil || *result.Amount != 25) {
				t.Fatal("claim amount missing")
			}
		})
	}
}

func TestZCodeCaptchaConfigSafety(t *testing.T) {
	for _, sdk := range []string{"", zcodeSDK + "?v=1", "http://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js", "https://o.alicdn.com.evil.test/captcha.js", "https://o.alicdn.com/other.js", "https://user@o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js"} {
		t.Run(sdk, func(t *testing.T) {
			z := zcodeTestAdapter(func(*http.Request) (*http.Response, error) {
				d := zcodeTestCaptcha()
				Object(Object(d["configs"])["captcha"])["sdkUrl"] = sdk
				return zcodeTestOK(d), nil
			})
			cfg, err := z.CaptchaConfig(context.Background(), zcodeTestCredential())
			if sdk == "" || sdk == zcodeSDK+"?v=1" {
				if err != nil || cfg["sceneId"] != "test-scene" || cfg["prefix"] != "test-prefix" || cfg["region"] != "test-region" {
					t.Fatal("config not read from upstream")
				}
			} else {
				zcodeTestAPI(t, err, "unsafe_captcha_sdk")
			}
		})
	}
	z := zcodeTestAdapter(func(*http.Request) (*http.Response, error) { return zcodeTestOK(map[string]any{}), nil })
	_, err := z.CaptchaConfig(context.Background(), zcodeTestCredential())
	zcodeTestAPI(t, err, "captcha_config_unavailable")
}

func TestZCodeOfficialIdentityIsOptIn(t *testing.T) {
	// 这个开关的作用是让上游把请求当成**官方客户端**发来的，不是修请求格式。
	// 所以两边都要钉住：默认必须一个字节都不带官方身份块；开启后必须
	// 逐字符等于提取来的模板（长度差一个字符就可能换成一次 3012 账号处罚）。
	z := zcodeTestAdapter(func(*http.Request) (*http.Response, error) {
		t.Fatal("prepare must not hit the network")
		return nil, nil
	})
	newIn := func() Inference {
		return Inference{Model: Model{ID: "dynamic-model", MaxOutput: 4096}, MaxTokens: 128,
			Messages: []map[string]any{
				{"role": "system", "content": "System instruction"},
				{"role": "user", "content": "Hello"},
			}}
	}

	plain, err := z.Prepare(context.Background(), zcodeTestCredential(), newIn())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain.Body), "You are ZCode, an interactive coding agent") {
		t.Fatal("默认就带上官方身份块 —— 这个开关必须是显式开启的")
	}

	in := newIn()
	in.OfficialIdentity = true
	on, err := z.Prepare(context.Background(), zcodeTestCredential(), in)
	if err != nil {
		t.Fatal(err)
	}
	body := zcodeTestDecode(t, strings.NewReader(string(on.Body)))
	system := Array(body["system"])
	if len(system) != 5 {
		t.Fatalf("system blocks = %d, want 5（cliPrefix / stable / environment / FMH 自述 / 调用方）", len(system))
	}
	first := Text(Object(system[0])["text"])
	if first != zcodeOfficialCLIPrefix || len([]rune(first)) != 42 {
		t.Errorf("blocks[0] = %q —— 官方检查看的是开头这段，必须逐字一致", first)
	}
	if got := len([]rune(zcodeIdentityText())); got != zcodeOfficialIdentityChars {
		t.Errorf("身份文本 %d 字符，与提取时的 %d 不一致", got, zcodeOfficialIdentityChars)
	}
	if !strings.Contains(Text(Object(system[1])["text"]), "# Working style") {
		t.Error("stable 三段没带全")
	}
	env := Text(Object(system[2])["text"])
	if !strings.HasPrefix(env, "# Environment") || !strings.Contains(env, "zcode/dynamic-model") {
		t.Errorf("environment 段 = %q", env)
	}
	// 模拟上游 ≠ 对最终用户隐瞒：FMH 的自述块要还在，调用方的 system 不能被吃掉。
	if !strings.Contains(Text(Object(system[3])["text"]), "Free Model House") {
		t.Error("FMH 自述块被丢了 —— 开身份模拟不等于对用户撒谎")
	}
	if Text(Object(system[4])["text"]) != "System instruction" {
		t.Error("调用方自己的 system 必须追加在官方块之后，不能替换掉")
	}
	// 断点只打最后一块：逐块打点会吃满单请求 4 个的预算，tools 反而打不上。
	if Object(system[4])["cache_control"] == nil {
		t.Error("最后一块该带 cache_control")
	}
	for i, raw := range system[:4] {
		if Object(raw)["cache_control"] != nil {
			t.Errorf("blocks[%d] 不该带 cache_control", i)
		}
	}

	// 日期块：官方逐字形态，且**幂等**（同一份 messages 再插一次不该出现两块）。
	messages := Array(body["messages"])
	if len(messages) != 1 || Object(messages[0])["role"] != "user" {
		t.Fatalf("messages = %v, want 一条 user（system 在顶层）", messages)
	}
	userBlocks := Array(Object(messages[0])["content"])
	head := Text(Object(userBlocks[0])["text"])
	if !strings.HasPrefix(head, "<system-reminder>") || !strings.Contains(head, "# currentDate") {
		t.Errorf("首轮缺日期块：%q", head)
	}
	if len(userBlocks) != 2 || Text(Object(userBlocks[1])["text"]) != "Hello" {
		t.Errorf("日期块没插在调用方内容之前：%v", userBlocks)
	}
	again := zcodeWithContextPrefix([]map[string]any{{"role": "user", "content": userBlocks}}, time.Now())
	if len(Array(again[0]["content"])) != 2 {
		t.Error("日期块插了两次")
	}

	// 模板与提取时不一致时必须**本地拒发**：发一份"差一点"的身份块不是退回裸请求，
	// 而是替账号多烧一发处罚。
	orig := zcodeOfficialStableSections
	zcodeOfficialStableSections = []string{orig[0]}
	defer func() { zcodeOfficialStableSections = orig }()
	if _, err := zcodeOfficialSystem(nil, "m"); err == nil {
		t.Error("模板被截短却照样发送")
	} else if !strings.Contains(err.Error(), "官方客户端身份") {
		t.Errorf("报错要告诉用户怎么退出这条路：%v", err)
	}
}

func TestZCodePrepareAnthropicConversion(t *testing.T) {
	z := zcodeTestAdapter(func(*http.Request) (*http.Response, error) {
		t.Fatal("initial prepare must not request captcha or inference")
		return nil, nil
	})
	in := Inference{Model: Model{ID: "dynamic-model", MaxOutput: 4096, Efforts: []string{"low", "high"}}, MaxTokens: 5000, Effort: "high", Messages: []map[string]any{
		{"role": "system", "content": "System instruction"},
		{"role": "developer", "content": []map[string]any{{"type": "text", "text": "Developer instruction", "cache_control": map[string]any{"type": "ephemeral"}}}},
		{"role": "user", "content": []any{map[string]any{"type": "text", "text": "Look"}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64,aGVsbG8="}}, map[string]any{"type": "image_url", "image_url": "https://images.example.test/test.png"}}},
		{"role": "assistant", "reasoning_content": "Thinking from history", "content": []any{map[string]any{"type": "thinking", "thinking": "Signed history", "signature": "test-signature"}, map[string]any{"type": "redacted_thinking", "data": "test-redacted"}, map[string]any{"type": "text", "text": "Calling tools"}}, "tool_calls": []any{map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "read", "arguments": "{\"path\":\"test.go\"}"}}}},
		{"role": "tool", "tool_call_id": "call-1", "is_error": true, "content": []any{map[string]any{"type": "text", "text": "Not found"}, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "aGVsbG8="}}}},
		{"role": "user", "content": "Try again"},
		{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call-2", "name": "read", "input": map[string]any{"path": "other.go"}}}},
		{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-2", "content": "OK"}}},
	}, Tools: []map[string]any{
		{"type": "function", "function": map[string]any{"name": "read", "description": "Read a file", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []string{"path"}}}},
		{"name": "native", "input_schema": map[string]any{"type": "object"}, "cache_control": map[string]any{"type": "ephemeral"}},
	}}
	before, _ := json.Marshal(in)
	prepared, err := z.Prepare(context.Background(), zcodeTestCredential(), in)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(in)
	if string(before) != string(after) {
		t.Fatal("input messages or schemas were mutated")
	}
	if prepared.URL != zcodeOrigin+"/api/v1/zcode-plan/anthropic/v1/messages" || prepared.Wire != "messages" || prepared.Headers["x-aliyun-captcha-verify-param"] != "" {
		t.Fatal("wrong free wire or speculative captcha")
	}
	body := zcodeTestDecode(t, strings.NewReader(string(prepared.Body)))
	if body["stream"] != true || body["max_tokens"] != float64(4096) || Object(body["output_config"])["effort"] != "high" {
		t.Fatal("wrong inference controls")
	}
	system := Array(body["system"])
	if len(system) != 3 || !strings.Contains(Text(Object(system[0])["text"]), "Free Model House") || Object(system[2])["cache_control"] == nil {
		t.Fatal("FMH identity or supplied system lost")
	}
	messages := Array(body["messages"])
	if len(messages) != 5 {
		t.Fatalf("expected adjacent users/tools merged: %d", len(messages))
	}
	first := Array(Object(messages[0])["content"])
	if Object(Object(first[1])["source"])["media_type"] != "image/png" || Object(Object(first[2])["source"])["url"] != "https://images.example.test/test.png" {
		t.Fatal("images lost")
	}
	assistant := Array(Object(messages[1])["content"])
	if len(assistant) != 5 || Object(assistant[0])["thinking"] != "Thinking from history" || Object(assistant[1])["signature"] != "test-signature" || Object(assistant[2])["type"] != "redacted_thinking" || Object(Object(assistant[4])["input"])["path"] != "test.go" {
		t.Fatal("thinking/tool calls lost")
	}
	results := Array(Object(messages[2])["content"])
	if len(results) != 2 || Object(results[0])["tool_use_id"] != "call-1" || Object(results[0])["is_error"] != true || len(Array(Object(results[0])["content"])) != 2 {
		t.Fatal("tool result conversion lost data")
	}
	tools := Array(body["tools"])
	if Object(tools[0])["name"] != "read" || Object(tools[0])["function"] != nil || Object(tools[0])["input_schema"] == nil || Object(tools[1])["cache_control"] == nil {
		t.Fatal("tool schema conversion wrong")
	}
}

func TestZCodePrepareCaptchaIsCallerOwnedAndCodingIsExplicit(t *testing.T) {
	calls := 0
	z := zcodeTestAdapter(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/api/v1/client/configs" {
			t.Fatal("prepare must not execute inference")
		}
		return zcodeTestOK(zcodeTestCaptcha()), nil
	})
	c := zcodeTestCredential()
	c["captchaVerifyParam"] = "test-interactive-param"
	in := Inference{Model: Model{ID: "model-a"}, Messages: []map[string]any{{"role": "user", "content": "hello"}}}
	p, err := z.Prepare(context.Background(), c, in)
	if err != nil || p.Headers["x-aliyun-captcha-verify-param"] != "test-interactive-param" || p.Headers["x-aliyun-captcha-verify-region"] != "test-region" || calls != 1 {
		t.Fatalf("real supplied captcha was not forwarded: %v", err)
	}
	if c.Text("captchaVerifyParam") == "" {
		t.Fatal("adapter mutated caller-owned credential")
	}
	// Simulate the stream owner consuming the one-time value before sending.
	delete(c, "captchaVerifyParam")
	p, err = z.Prepare(context.Background(), c, in)
	if err != nil || p.Headers["x-aliyun-captcha-verify-param"] != "" || calls != 1 {
		t.Fatal("captcha was retained or regenerated")
	}
	in.Model.ID = "coding/model-a"
	_, err = z.Prepare(context.Background(), c, in)
	zcodeTestAPI(t, err, "coding_plan_unavailable")
	c["codingPlanKey"] = "test-coding.secret"
	c["captchaVerifyParam"] = "not-for-coding"
	p, err = z.Prepare(context.Background(), c, in)
	if err != nil || p.URL != zcodeBizOrigin+"/api/anthropic/v1/messages" || p.Headers["Authorization"] != "Bearer test-coding.secret" || p.Headers["HTTP-Referer"] != "" || p.Headers["x-aliyun-captcha-verify-param"] != "" || calls != 1 {
		t.Fatal("incorrect coding channel")
	}
	if zcodeTestDecode(t, strings.NewReader(string(p.Body)))["model"] != "model-a" {
		t.Fatal("routing prefix leaked upstream")
	}
}

func TestZCodePrepareRejectsMalformedMessages(t *testing.T) {
	cases := []map[string]any{
		{"role": "other", "content": "bad"},
		{"role": "assistant", "tool_calls": []any{map[string]any{"id": "x", "function": map[string]any{"name": "f", "arguments": "not-json-secret"}}}},
		{"role": "tool", "content": "missing correlation id"},
		{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": "data:image/svg+xml;base64,aGVsbG8="}}},
		{"role": "user", "content": []any{map[string]any{"type": "image_url", "image_url": "file:///private.png"}}},
		{"role": "user", "content": []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "!invalid!"}}}},
		{"role": "user", "content": []any{map[string]any{"type": "thinking", "thinking": "wrong role"}}},
	}
	z := zcodeTestAdapter(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid message made a request")
		return nil, nil
	})
	for i, message := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			_, err := z.Prepare(context.Background(), zcodeTestCredential(), Inference{Model: Model{ID: "model"}, Messages: []map[string]any{message}})
			zcodeTestAPI(t, err, "invalid_messages")
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("invalid input leaked")
			}
		})
	}
}

func TestZCodeRequestErrorSanitizationAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   any
		want   string
	}{
		{"numeric-string", 200, map[string]any{"code": "3012", "msg": "test-secret"}, "unusual_activity"},
		{"bad-code", 200, map[string]any{"code": "test-secret"}, "invalid_response"},
		{"missing-code", 200, map[string]any{"msg": "test-secret"}, "invalid_response"},
		{"http", 500, map[string]any{"message": "test-secret"}, "upstream_http_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			z := zcodeTestAdapter(func(*http.Request) (*http.Response, error) { return zcodeTestResponse(tc.status, tc.body), nil })
			_, err := z.StartLogin(context.Background(), "zai")
			zcodeTestAPI(t, err, tc.want)
			if strings.Contains(err.Error(), "test-secret") {
				t.Fatal("response error leaked")
			}
		})
	}
	z := zcodeTestAdapter(func(*http.Request) (*http.Response, error) { return nil, errors.New("transport test-secret") })
	_, err := z.StartLogin(context.Background(), "bigmodel")
	zcodeTestAPI(t, err, "upstream_unavailable")
	if strings.Contains(err.Error(), "test-secret") {
		t.Fatal("transport error leaked")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = z.StartLogin(ctx, "bigmodel")
	if !errors.Is(err, context.Canceled) {
		t.Fatal("context cancellation lost")
	}
}
