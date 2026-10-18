package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/fx/fxtest"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/config"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/jwttest"
)

func TestNewAuthenticatorKeepsRefreshingAfterStartup(t *testing.T) {
	issuer := jwttest.NewIssuer(t)
	lc := fxtest.NewLifecycle(t)
	cfg := config.Config{JWKSURL: issuer.JWKSURL(), JWKSRefreshInterval: 50 * time.Millisecond, JWTIssuer: issuer.URL(), JWTAudience: jwttest.Audience}

	auth, err := newAuthenticator(lc, cfg, observability.New())
	if err != nil {
		t.Fatalf("newAuthenticator: %v", err)
	}
	lc.RequireStart()
	defer lc.RequireStop()

	protected := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	oldToken := issuer.ProviderToken(t, "provider-a")

	issuer.Rotate(t)

	deadline := time.Now().Add(3 * time.Second)
	for statusFor(protected, oldToken) != http.StatusUnauthorized {
		if time.Now().After(deadline) {
			t.Fatal("a revoked key was still trusted after the refresh interval")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestNewAuthenticatorRetriesUntilJWKSIsUp(t *testing.T) {
	shortenJWKSStartup(t, 2*time.Second, 20*time.Millisecond)
	issuer := jwttest.NewIssuer(t)

	var calls atomic.Int32
	starting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) <= 2 {
			http.Error(w, "starting", http.StatusServiceUnavailable)
			return
		}
		issuer.ServeHTTP(w, r)
	}))
	t.Cleanup(starting.Close)

	lc := fxtest.NewLifecycle(t)
	cfg := config.Config{JWKSURL: starting.URL, JWKSRefreshInterval: time.Hour, JWTIssuer: issuer.URL(), JWTAudience: jwttest.Audience}
	if _, err := newAuthenticator(lc, cfg, observability.New()); err != nil {
		t.Fatalf("gave up while the JWKS was still coming up: %v", err)
	}
	lc.RequireStart()
	lc.RequireStop()

	if got := calls.Load(); got < 3 {
		t.Fatalf("JWKS fetched %d times, want at least 3", got)
	}
}

func TestNewAuthenticatorGivesUpAfterTheStartupTimeout(t *testing.T) {
	shortenJWKSStartup(t, 200*time.Millisecond, 20*time.Millisecond)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	t.Cleanup(down.Close)

	started := time.Now()
	_, err := newAuthenticator(fxtest.NewLifecycle(t), config.Config{JWKSURL: down.URL, JWTIssuer: "https://issuer.example", JWTAudience: jwttest.Audience}, observability.New())
	if err == nil {
		t.Fatal("newAuthenticator succeeded without a JWKS")
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("gave up after %v, want close to the 200ms startup timeout", elapsed)
	}
}

func shortenJWKSStartup(t *testing.T, timeout, delay time.Duration) {
	t.Helper()
	previousTimeout, previousDelay := jwksStartupTimeout, jwksRetryDelay
	jwksStartupTimeout, jwksRetryDelay = timeout, delay
	t.Cleanup(func() { jwksStartupTimeout, jwksRetryDelay = previousTimeout, previousDelay })
}

func statusFor(h http.Handler, token string) int {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}
