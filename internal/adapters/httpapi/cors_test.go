package httpapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/httpapi"
)

const consoleOrigin = "http://localhost:5173"

func withOrigins(origins ...string) func(*httpapi.Dependencies) {
	return func(d *httpapi.Dependencies) { d.AllowedOrigins = origins }
}

// rawRequest sends a request and returns the response with its headers,
// which apiHarness.do does not keep.
func (h *apiHarness) rawRequest(t *testing.T, method, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.server.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func preflight(origin string) map[string]string {
	return map[string]string{
		"Origin":                         origin,
		"Access-Control-Request-Method":  http.MethodPost,
		"Access-Control-Request-Headers": "authorization, content-type, idempotency-key",
	}
}

func TestCORSPreflightFromAnAllowedOrigin(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t, withOrigins(consoleOrigin))

	resp := h.rawRequest(t, http.MethodOptions, "/wallets", preflight(consoleOrigin))

	// No token was sent: a 204 means the preflight never reached the
	// authenticator.
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	for name, want := range map[string]string{
		"Access-Control-Allow-Origin":  consoleOrigin,
		"Access-Control-Allow-Methods": "GET, POST",
		"Access-Control-Allow-Headers": "Authorization, Content-Type, Idempotency-Key",
		"Access-Control-Max-Age":       "600",
		"Vary":                         "Origin",
	} {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want none: the console sends a bearer token, not cookies", got)
	}
}

func TestCORSPreflightFromAnotherOrigin(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t, withOrigins(consoleOrigin))

	resp := h.rawRequest(t, http.MethodOptions, "/wallets", preflight("http://evil.example"))

	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("a refused preflight reached the authenticator")
	}
	for name := range resp.Header {
		if strings.HasPrefix(name, "Access-Control-") {
			t.Errorf("refused preflight carries %s", name)
		}
	}
}

func TestCORSAddsTheOriginToResponses(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t, withOrigins(consoleOrigin))
	walletID, _ := h.openWallet(t, "10.00")

	resp := h.rawRequest(t, http.MethodGet, "/wallets/"+walletID, map[string]string{
		"Origin":        consoleOrigin,
		"Authorization": "Bearer " + h.issuer.OperatorToken(t),
	})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != consoleOrigin {
		t.Errorf("Access-Control-Allow-Origin = %q, want %q", got, consoleOrigin)
	}
	if got := resp.Header.Get("Access-Control-Expose-Headers"); got != "Retry-After" {
		t.Errorf("Access-Control-Expose-Headers = %q, want Retry-After", got)
	}
	if got := resp.Header.Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin", got)
	}

	unauthenticated := h.rawRequest(t, http.MethodGet, "/wallets/"+walletID, map[string]string{"Origin": consoleOrigin})
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", unauthenticated.StatusCode)
	}
	if got := unauthenticated.Header.Get("Access-Control-Allow-Origin"); got != consoleOrigin {
		t.Errorf("a 401 lacks Access-Control-Allow-Origin, so the console cannot read why it failed")
	}
}

func TestCORSOffByDefault(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)

	resp := h.rawRequest(t, http.MethodOptions, "/wallets", preflight(consoleOrigin))

	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 as before CORS existed", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q with no origins configured", got)
	}
}
