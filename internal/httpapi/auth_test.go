package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/httpapi"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/observability"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/jwttest"
)

func TestNewAuthenticatorFailsWhenJWKSIsUnavailable(t *testing.T) {
	unavailable := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "starting", http.StatusServiceUnavailable)
	}))
	t.Cleanup(unavailable.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	if _, err := httpapi.NewAuthenticator(ctx, unavailable.URL, time.Hour, observability.New()); err == nil {
		t.Fatal("NewAuthenticator succeeded without a reachable JWKS")
	}
}

func TestAuthenticatorStopsTrustingRevokedKeys(t *testing.T) {
	issuer := jwttest.NewIssuer(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	auth, err := httpapi.NewAuthenticator(ctx, issuer.JWKSURL(), 50*time.Millisecond, observability.New())
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
