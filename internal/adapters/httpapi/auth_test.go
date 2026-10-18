package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/httpapi"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/jwttest"
)

func TestNewAuthenticatorFailsWhenJWKSIsUnavailable(t *testing.T) {
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	t.Cleanup(unavailable.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := httpapi.NewAuthenticator(ctx, httpapi.AuthConfig{JWKSURL: unavailable.URL, RefreshInterval: time.Hour, Issuer: "https://issuer.example", Audience: jwttest.Audience}, observability.New()); err == nil {
		t.Fatal("NewAuthenticator succeeded without a reachable JWKS")
	}
}

func TestAuthenticatorStopsTrustingRevokedKeys(t *testing.T) {
	issuer := jwttest.NewIssuer(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	auth, err := httpapi.NewAuthenticator(ctx, httpapi.AuthConfig{JWKSURL: issuer.JWKSURL(), RefreshInterval: 50 * time.Millisecond, Issuer: issuer.URL(), Audience: jwttest.Audience}, observability.New())
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	protected := auth.Middleware(noContent())

	oldToken := issuer.ProviderToken(t, "provider-a")
	if status := statusFor(protected, oldToken); status != http.StatusNoContent {
		t.Fatalf("token before rotation: status %d, want 204", status)
	}

	issuer.Rotate(t)

	waitForStatus(t, protected, oldToken, http.StatusUnauthorized)
	if status := statusFor(protected, issuer.ProviderToken(t, "provider-a")); status != http.StatusNoContent {
		t.Fatalf("token signed after rotation: status %d, want 204", status)
	}
}

// A token with an unknown kid makes the JWKS library re-fetch the keys on
// the request path. A hung IdP must not hold that request: it gets a 401
// within a short bound instead of the library's one-minute default.
func TestUnknownKeyIsRejectedQuicklyWhileTheIdPHangs(t *testing.T) {
	issuer := jwttest.NewIssuer(t)
	var hung atomic.Bool
	release := make(chan struct{})
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hung.Load() {
			select {
			case <-release:
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
			return
		}
		issuer.ServeHTTP(w, r)
	}))
	t.Cleanup(idp.Close)
	t.Cleanup(func() { close(release) })

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	auth, err := httpapi.NewAuthenticator(ctx, httpapi.AuthConfig{JWKSURL: idp.URL, RefreshInterval: time.Hour, Issuer: issuer.URL(), Audience: jwttest.Audience}, observability.New())
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	protected := auth.Middleware(noContent())

	hung.Store(true)
	started := time.Now()
	status := statusFor(protected, issuer.UnknownKeyProviderToken(t, "provider-a"))
	elapsed := time.Since(started)

	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("an unknown kid held the request for %v while the IdP hung", elapsed)
	}
}

func noContent() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
}

func statusFor(h http.Handler, token string) int {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// waitForStatus polls because the JWKS refresh runs on its own timer.
func waitForStatus(t *testing.T, h http.Handler, token string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		got := statusFor(h, token)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("status stayed %d, want %d", got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func strictAuthenticator(t *testing.T, issuer *jwttest.Issuer) *httpapi.Authenticator {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	auth, err := httpapi.NewAuthenticator(ctx, httpapi.AuthConfig{
		JWKSURL:         issuer.JWKSURL(),
		RefreshInterval: time.Hour,
		Issuer:          issuer.URL(),
		Audience:        jwttest.Audience,
	}, observability.New())
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	return auth
}

func TestAuthenticatorRejectsForeignIssuer(t *testing.T) {
	issuer := jwttest.NewIssuer(t)
	protected := strictAuthenticator(t, issuer).Middleware(noContent())
	token := issuer.ProviderTokenWith(t, "provider-a", jwt.MapClaims{"iss": "https://elsewhere.example/realms/other"})
	if status := statusFor(protected, token); status != http.StatusUnauthorized {
		t.Fatalf("token from another issuer: status %d, want 401", status)
	}
	if status := statusFor(protected, issuer.ProviderToken(t, "provider-a")); status != http.StatusNoContent {
		t.Fatalf("valid token: status %d, want 204", status)
	}
}

func TestAuthenticatorRejectsWrongAudience(t *testing.T) {
	issuer := jwttest.NewIssuer(t)
	protected := strictAuthenticator(t, issuer).Middleware(noContent())
	token := issuer.ProviderTokenWith(t, "provider-a", jwt.MapClaims{"aud": "another-api"})
	if status := statusFor(protected, token); status != http.StatusUnauthorized {
		t.Fatalf("token for another audience: status %d, want 401", status)
	}
}

func TestAuthenticatorRejectsTokensWithoutExpiry(t *testing.T) {
	issuer := jwttest.NewIssuer(t)
	protected := strictAuthenticator(t, issuer).Middleware(noContent())
	token := issuer.ProviderTokenWith(t, "provider-a", jwt.MapClaims{"exp": nil})
	if status := statusFor(protected, token); status != http.StatusUnauthorized {
		t.Fatalf("token without exp: status %d, want 401", status)
	}
}
