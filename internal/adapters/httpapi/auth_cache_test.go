package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/jwttest"
)

// cacheHarness is an authenticator on a clock the test moves.
type cacheHarness struct {
	auth    *Authenticator
	issuer  *jwttest.Issuer
	metrics *observability.Metrics
	now     time.Time
	handler http.Handler
}

func newCacheHarness(t testing.TB, refresh time.Duration) *cacheHarness {
	t.Helper()
	issuer := jwttest.NewIssuer(t)
	metrics := observability.New()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	auth, err := NewAuthenticator(ctx, AuthConfig{
		JWKSURL: issuer.JWKSURL(), RefreshInterval: refresh, Issuer: issuer.URL(), Audience: jwttest.Audience,
	}, metrics)
	if err != nil {
		t.Fatal(err)
	}
	h := &cacheHarness{auth: auth, issuer: issuer, metrics: metrics, now: time.Now()}
	auth.now = func() time.Time { return h.now }
	h.handler = auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	return h
}

func (h *cacheHarness) status(token string) int {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/wallets/x", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.handler.ServeHTTP(recorder, req)
	return recorder.Code
}

func (h *cacheHarness) lookups(result string) float64 {
	return testutil.ToFloat64(h.metrics.AuthTokenCacheTotal.WithLabelValues(result))
}

func TestVerifiedTokenIsServedFromTheCache(t *testing.T) {
	h := newCacheHarness(t, time.Hour)
	token := h.issuer.ProviderToken(t, "provider-a")

	for range 3 {
		if status := h.status(token); status != http.StatusNoContent {
			t.Fatalf("status = %d", status)
		}
	}
	if hits, misses := h.lookups("hit"), h.lookups("miss"); hits != 2 || misses != 1 {
		t.Errorf("cache hits=%v misses=%v, want 2 and 1", hits, misses)
	}
}

// The cache never stretches a token past its own expiry.
func TestCachedTokenStopsAtItsExpiry(t *testing.T) {
	h := newCacheHarness(t, time.Hour)
	token := h.issuer.ProviderTokenWith(t, "provider-a", jwt.MapClaims{"exp": h.now.Add(2 * time.Minute).Unix()})

	if status := h.status(token); status != http.StatusNoContent {
		t.Fatalf("fresh token: status %d", status)
	}
	h.now = h.now.Add(3 * time.Minute)
	if status := h.status(token); status != http.StatusUnauthorized {
		t.Fatalf("token past its expiry: status %d, want 401", status)
	}
}

// A cached token is verified again after one JWKS refresh interval, so a
// key the IdP revoked stops being trusted as soon as it would have without
// the cache.
func TestCachedTokenStopsAtTheRefreshWindow(t *testing.T) {
	h := newCacheHarness(t, time.Minute)
	token := h.issuer.ProviderToken(t, "provider-a")

	h.status(token)
	h.now = h.now.Add(30 * time.Second)
	h.status(token)
	h.now = h.now.Add(time.Minute)
	if status := h.status(token); status != http.StatusNoContent {
		t.Fatalf("still-valid token after the window: status %d", status)
	}
	if hits, misses := h.lookups("hit"), h.lookups("miss"); hits != 1 || misses != 2 {
		t.Errorf("cache hits=%v misses=%v, want 1 and 2 (verified again after the window)", hits, misses)
	}
}

// A cached token stops being trusted as soon as its signing key leaves the
// JWKS, however long its cache entry has left: the clock here never moves,
// so only the key check can evict it.
func TestCachedTokenStopsWhenItsKeyLeavesTheJWKS(t *testing.T) {
	h := newCacheHarness(t, 50*time.Millisecond)
	token := h.issuer.ProviderToken(t, "provider-a")
	if status := h.status(token); status != http.StatusNoContent {
		t.Fatalf("fresh token: status %d", status)
	}

	h.issuer.Rotate(t)
	deadline := time.Now().Add(3 * time.Second)
	for h.status(token) != http.StatusUnauthorized {
		if time.Now().After(deadline) {
			t.Fatal("a token signed by a revoked key was still served from the cache")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status := h.status(h.issuer.ProviderToken(t, "provider-a")); status != http.StatusNoContent {
		t.Fatalf("token signed by the new key: status %d", status)
	}
}

// Every valid token gets in until the cache is full; past that, tokens
// are still accepted but verified every time.
func TestCacheStaysBoundedUnderManyDistinctTokens(t *testing.T) {
	h := newCacheHarness(t, time.Hour)
	h.auth.cache.capacity = 50

	for i := range 150 {
		token := h.issuer.ProviderToken(t, fmt.Sprintf("provider-%d", i))
		if status := h.status(token); status != http.StatusNoContent {
			t.Fatalf("token %d: status %d", i, status)
		}
	}
	if size := h.auth.cache.size(); size > 50 {
		t.Errorf("cache holds %d entries, capacity 50", size)
	}

	// Expired entries make room.
	h.now = h.now.Add(2 * time.Hour)
	if status := h.status(h.issuer.ProviderTokenWith(t, "late", jwt.MapClaims{"exp": h.now.Add(time.Minute).Unix()})); status != http.StatusNoContent {
		t.Fatalf("token after the others expired: status %d", status)
	}
	if size := h.auth.cache.size(); size != 1 {
		t.Errorf("cache holds %d entries after the others expired, want 1", size)
	}
}

func TestRejectedTokensAreNotCached(t *testing.T) {
	h := newCacheHarness(t, time.Hour)
	wrongAudience := h.issuer.ProviderTokenWith(t, "provider-a", jwt.MapClaims{"aud": "another-api"})

	for range 2 {
		if status := h.status(wrongAudience); status != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", status)
		}
	}
	if size := h.auth.cache.size(); size != 0 {
		t.Errorf("cache holds %d rejected tokens", size)
	}
}

// BenchmarkMiddleware compares a request whose token is cached with one
// that is verified (RSA signature, claims) every time.
func BenchmarkMiddleware(b *testing.B) {
	for _, cached := range []bool{true, false} {
		b.Run(fmt.Sprintf("cached=%v", cached), func(b *testing.B) {
			h := newCacheHarness(b, time.Hour)
			if !cached {
				h.auth.cache.capacity = 0
			}
			token := h.issuer.ProviderToken(b, "provider-a")
			b.ResetTimer()
			for range b.N {
				if status := h.status(token); status != http.StatusNoContent {
					b.Fatalf("status %d", status)
				}
			}
		})
	}
}
