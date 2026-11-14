package qoderwasm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const fixtureToken = "qoderwasm-fixture-token-not-a-real-credential-47bd26"
const fixtureUID = "fixture-user-7042"
const fixturePayload = `{"messages":[{"role":"user","content":"offline fixture"}],"stream":false}`

func fixtureUser() []byte {
	b, _ := json.Marshal(userInfo{UID: fixtureUID, SecurityOauthToken: fixtureToken})
	return b
}

func prepareFixture(ctx context.Context, host string) (Request, error) {
	return Prepare(ctx, fixtureUser(), "fixture-machine-01", "0.1.0", host, "fixture-model", []byte(fixturePayload))
}

func TestOfficialWASMIntegrity(t *testing.T) {
	if len(officialWASM) != 320863 {
		t.Fatalf("unexpected WASM size: %d", len(officialWASM))
	}
	sum := sha256.Sum256(officialWASM)
	if hex.EncodeToString(sum[:]) != "c7a14b11d6c23b94ac39f6868ebd3dd6a70492efe87c864c33857adadb9f0951" {
		t.Fatal("official WASM digest mismatch")
	}
}

func TestPrepareOfficialWASM(t *testing.T) {
	for _, host := range []string{"fixture.example", "https://fixture.example", "https://fixture.example:8443/", "[::1]:8443"} {
		t.Run(host, func(t *testing.T) {
			req, err := prepareFixture(context.Background(), host)
			if err != nil {
				t.Fatal(err)
			}
			origin, _ := validateHost(host)
			assertRequest(t, req, origin, fixtureToken)
		})
	}
}

func assertRequest(t *testing.T, req Request, origin, token string) {
	t.Helper()
	u, err := url.Parse(req.URL)
	if err != nil || u.Scheme != "https" || "https://"+u.Host != origin || u.User != nil {
		t.Fatalf("request does not target the specified HTTPS host: %q", req.URL)
	}
	if u.Path == "" || u.Path == "/" {
		t.Fatal("inference endpoint is missing")
	}
	if len(req.Headers) < 2 {
		t.Fatal("expected populated headers from the WASM Map.set import")
	}
	authorized := false
	for k, v := range req.Headers {
		if strings.EqualFold(k, "authorization") {
			authorized = strings.HasPrefix(v, "Bearer COSY.") && len(v) > len("Bearer COSY.")
		}
	}
	if !authorized {
		t.Fatal("missing COSY authentication")
	}
	encoded, err := json.Marshal(req)
	if err != nil || strings.Contains(string(encoded), token) {
		t.Fatal("plaintext token leaked in generated request")
	}
	if req.Body == "" {
		t.Fatal("missing request body")
	}
}

func TestPrepareOptionalUserFieldsAndRandomness(t *testing.T) {
	user := userInfo{UID: fixtureUID, SecurityOauthToken: fixtureToken, OrganizationID: "fixture-org", OrganizationTags: []string{"fixture-tag"}, DataPolicyAgreed: true}
	userJSON, _ := json.Marshal(user)
	var previous string
	for i := 0; i < 2; i++ {
		req, err := Prepare(context.Background(), userJSON, "fixture-machine-01", "0.1.0", "fixture.example", "fixture-model", []byte(fixturePayload))
		if err != nil {
			t.Fatal(err)
		}
		assertRequest(t, req, "https://fixture.example", fixtureToken)
		data, _ := json.Marshal(req)
		if string(data) == previous {
			t.Fatal("two requests reused identical encrypted authentication")
		}
		previous = string(data)
	}
}

func TestPrepareRejectsUnsafeHosts(t *testing.T) {
	for _, host := range []string{"", "http://fixture.example", "ftp://fixture.example", "//fixture.example", "https://fixture.example/redirect", "https://user@fixture.example", "https://fixture.example?x=1", "https://fixture.example#x", "fixture.example?", "fixture.example#", "fixture.example\\@other.example", "fixture.example\r\nX-Test: x", "fixture.example:", "fixture.example:0", "fixture.example:65536", " fixture.example", "fixture.example ", "fixture..example", "-bad.example", "[fixture.example]", "::1"} {
		t.Run(fmt.Sprintf("%q", host), func(t *testing.T) {
			req, err := prepareFixture(context.Background(), host)
			if err == nil || req.URL != "" || req.Headers != nil || req.Body != "" {
				t.Fatal("unsafe host accepted or partial request returned")
			}
		})
	}
}

func TestPrepareInvalidInputs(t *testing.T) {
	for _, user := range []string{"", "null", "{}", `{"uid":"x"}`, `{"securityOauthToken":"x"}`, `{"uid":1,"securityOauthToken":"x"}`, `{"uid":"x","securityOauthToken":"` + fixtureToken + `","organizationTags":123}`} {
		req, err := Prepare(context.Background(), []byte(user), "machine", "version", "fixture.example", "model", []byte(fixturePayload))
		if err == nil || req.URL != "" {
			t.Fatal("invalid user accepted")
		}
		if strings.Contains(err.Error(), fixtureToken) {
			t.Fatal("error leaked credentials")
		}
	}
	for _, payload := range []string{"", "not JSON", "{", "\xff"} {
		if _, err := Prepare(context.Background(), fixtureUser(), "machine", "version", "fixture.example", "model", []byte(payload)); err == nil {
			t.Fatal("invalid payload accepted")
		}
	}
}

func TestPrepareCancellation(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		var ctx context.Context
		var cancel context.CancelFunc
		want := context.Canceled
		if deadline {
			ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			want = context.DeadlineExceeded
		} else {
			ctx, cancel = context.WithCancel(context.Background())
			cancel()
		}
		req, err := prepareFixture(ctx, "fixture.example")
		cancel()
		if !errors.Is(err, want) || req.URL != "" || req.Headers != nil || req.Body != "" {
			t.Fatalf("cancellation returned %v, wanted %v", err, want)
		}
	}
}

func TestPrepareConcurrentIsolation(t *testing.T) {
	const count = 6
	var wg sync.WaitGroup
	errorsCh := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			host := fmt.Sprintf("fixture-%d.example", i)
			token := fmt.Sprintf("%s-%d", fixtureToken, i)
			user, _ := json.Marshal(userInfo{UID: fmt.Sprintf("fixture-user-%d", i), SecurityOauthToken: token})
			req, err := Prepare(context.Background(), user, fmt.Sprintf("fixture-machine-%d", i), "0.1.0", host, "fixture-model", []byte(fixturePayload))
			if err == nil {
				err = validateRequest(req, "https://"+host, token)
			}
			errorsCh <- err
		}(i)
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Error(err)
		}
	}
}
