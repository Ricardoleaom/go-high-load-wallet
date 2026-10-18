package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/observability"
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

// Authenticator validates bearer tokens against the IdP's JWKS.
type Authenticator struct {
	keyfunc keyfunc.Keyfunc
	metrics *observability.Metrics
	options []jwt.ParserOption
}

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

	return &Authenticator{
		keyfunc: kf,
		metrics: metrics,
		// A token signed by the right key is not enough: it must come from
		// the expected issuer, be meant for this API and expire.
		options: []jwt.ParserOption{
			jwt.WithValidMethods([]string{"RS256"}),
			jwt.WithIssuer(cfg.Issuer),
			jwt.WithAudience(cfg.Audience),
			jwt.WithExpirationRequired(),
		},
	}, nil
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

		var parsed claims
		_, err := jwt.ParseWithClaims(tokenString, &parsed, a.keyfunc.Keyfunc, a.options...)
		if err != nil {
			a.rejectUnauthenticated(w, r, "invalid or expired token", started)
			return
		}

		ctx := r.Context()
		if parsed.ProviderID != "" {
			ctx = context.WithValue(ctx, ctxKeyProviderID, parsed.ProviderID)
		}
		if parsed.hasRole("internal") {
			ctx = context.WithValue(ctx, ctxKeyInternal, true)
		}

		next.ServeHTTP(w, r.WithContext(ctx))
	})
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
