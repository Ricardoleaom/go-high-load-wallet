package httpapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

type contextKey string

const (
	ctxKeyProviderID contextKey = "providerID"
	ctxKeyInternal   contextKey = "internal"
)

// claims are the token fields this API relies on. A provider's
// client_credentials token carries provider_id; the internal service's
// token carries the "internal" realm role instead.
type claims struct {
	jwt.RegisteredClaims
	ProviderID  string `json:"provider_id,omitempty"`
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

func (c claims) hasRole(role string) bool {
	for _, r := range c.RealmAccess.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// AuthConfig says where the signing keys are and which tokens count: the
// issuer that must have signed them and the audience they must be for.
type AuthConfig struct {
	JWKSURL         string
	RefreshInterval time.Duration
	Issuer          string
	Audience        string
}

// Authenticator validates bearer tokens against the IdP's JWKS and keeps
// what it verified in a cache, so a client reusing its token pays for the
// RSA signature check once rather than on every request.
type Authenticator struct {
	keyfunc keyfunc.Keyfunc
	metrics *observability.Metrics
	options []jwt.ParserOption

	cache *tokenCache
	// refresh bounds how long a verified token is trusted without being
	// verified again: a key the IdP revokes stops being trusted on the next
	// JWKS refresh, and the cache must not outlast that.
	refresh time.Duration
	now     func() time.Time
}

// tokenCacheCapacity bounds the cache: past it, tokens are still accepted
// but verified on every request.
const tokenCacheCapacity = 10_000

// unknownKeyRefreshTimeout bounds the JWKS re-fetch a token with an unknown
// kid triggers on the request path. The re-fetch is what picks up a key the
// IdP just rotated in, so it stays on, but a slow or hung IdP must cost the
// request at most this long instead of the library's one-minute default.
const unknownKeyRefreshTimeout = 2 * time.Second

// NewAuthenticator fetches the JWKS and fails if it cannot, so the caller
// decides whether to retry. ctx bounds the background refresh that re-reads
// the JWKS every refreshInterval, dropping keys the IdP revoked: it must
// live as long as the Authenticator, not just this call.
func NewAuthenticator(
	ctx context.Context,
	cfg AuthConfig,
	metrics *observability.Metrics,
) (*Authenticator, error) {
	if cfg.Issuer == "" || cfg.Audience == "" {
		return nil, errors.New("authenticator needs both the token issuer and audience")
	}

	failOnFirstFetch := false
	kf, err := keyfunc.NewDefaultOverrideCtx(ctx, []string{cfg.JWKSURL}, keyfunc.Override{
		NoErrorReturnFirstHTTPReq: &failOnFirstFetch,
		RefreshInterval:           cfg.RefreshInterval,
		RateLimitWaitMax:          unknownKeyRefreshTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS from %s: %w", cfg.JWKSURL, err)
	}

	a := &Authenticator{
		keyfunc: kf,
		metrics: metrics,
		cache:   newTokenCache(tokenCacheCapacity),
		refresh: cfg.RefreshInterval,
		now:     time.Now,
	}
	// A token signed by the right key is not enough: it must come from the
	// expected issuer, be meant for this API and expire.
	a.options = []jwt.ParserOption{
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(cfg.Issuer),
		jwt.WithAudience(cfg.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(func() time.Time { return a.now() }),
	}
	return a, nil
}

// Middleware rejects requests without a valid bearer token and puts the
// authenticated identity (provider ID, or internal-service flag) in the
// request context for handlers to authorize against. Requests it rejects
// never reach metricsMiddleware wrapping the protected routes (see
// handler.go), so it records http_requests_total for them itself.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		header := r.Header.Get("Authorization")
		tokenString, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || tokenString == "" {
			a.rejectUnauthenticated(w, r, "missing bearer token", started)
			return
		}

		id, ok := a.verify(r.Context(), tokenString)
		if !ok {
			a.rejectUnauthenticated(w, r, "invalid or expired token", started)
			return
		}

		ctx := r.Context()
		if id.providerID != "" {
			ctx = context.WithValue(ctx, ctxKeyProviderID, id.providerID)
		}
		if id.internal {
			ctx = context.WithValue(ctx, ctxKeyInternal, true)
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// identity is what a verified token says about its bearer.
type identity struct {
	providerID string
	internal   bool
}

// verify answers from the cache when it can, and otherwise checks the
// token's signature and claims, caching a valid one until the earlier of
// its expiry and one JWKS refresh interval from now. A cached token is only
// trusted while the key that signed it is still in the JWKS, so a key the
// IdP revokes stops being trusted at the next refresh, as without the
// cache.
func (a *Authenticator) verify(ctx context.Context, token string) (identity, bool) {
	now := a.now()
	if entry, ok := a.cache.get(token, now); ok {
		if _, err := a.keyfunc.Storage().KeyRead(ctx, entry.kid); err == nil {
			a.metrics.AuthTokenCacheTotal.WithLabelValues("hit").Inc()
			return entry.identity, true
		}
		a.cache.remove(token)
	}
	a.metrics.AuthTokenCacheTotal.WithLabelValues("miss").Inc()

	var parsed claims
	parsedToken, err := jwt.ParseWithClaims(token, &parsed, a.keyfunc.Keyfunc, a.options...)
	if err != nil {
		return identity{}, false
	}
	id := identity{providerID: parsed.ProviderID, internal: parsed.hasRole("internal")}
	expires := parsed.ExpiresAt.Time
	if limit := now.Add(a.refresh); limit.Before(expires) {
		expires = limit
	}
	if kid, ok := parsedToken.Header["kid"].(string); ok {
		a.cache.put(token, cachedIdentity{identity: id, kid: kid, until: expires}, now)
	}
	return id, true
}

// tokenCache maps the SHA-256 of a verified token, so the tokens themselves
// are not kept, to its identity and the moment it must be verified again.
type tokenCache struct {
	mu        sync.RWMutex
	entries   map[[sha256.Size]byte]cachedIdentity
	capacity  int
	lastPurge time.Time
}

type cachedIdentity struct {
	identity
	kid   string // the signing key, which must still be in the JWKS
	until time.Time
}

func newTokenCache(capacity int) *tokenCache {
	return &tokenCache{entries: map[[sha256.Size]byte]cachedIdentity{}, capacity: capacity}
}

func (c *tokenCache) get(token string, now time.Time) (cachedIdentity, bool) {
	key := sha256.Sum256([]byte(token))
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || !now.Before(entry.until) {
		return cachedIdentity{}, false
	}
	return entry, true
}

func (c *tokenCache) remove(token string) {
	key := sha256.Sum256([]byte(token))
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// put caches entry until entry.until. A full cache first drops what
// expired, at most once a second so a flood of new tokens cannot turn
// every request into a sweep; still full, it caches nothing.
func (c *tokenCache) put(token string, entry cachedIdentity, now time.Time) {
	if c.capacity <= 0 || !now.Before(entry.until) {
		return
	}
	key := sha256.Sum256([]byte(token))
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= c.capacity && now.Sub(c.lastPurge) >= time.Second {
		c.lastPurge = now
		for k, entry := range c.entries {
			if !now.Before(entry.until) {
				delete(c.entries, k)
			}
		}
	}
	if len(c.entries) >= c.capacity {
		return
	}
	c.entries[key] = entry
}

func (c *tokenCache) size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

func (a *Authenticator) rejectUnauthenticated(w http.ResponseWriter, r *http.Request, message string, started time.Time) {
	writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", message)
	recordRequest(a.metrics, r, http.StatusUnauthorized, started)
}

func providerIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyProviderID).(string)
	return v, ok && v != ""
}

func isInternal(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeyInternal).(bool)
	return v
}
