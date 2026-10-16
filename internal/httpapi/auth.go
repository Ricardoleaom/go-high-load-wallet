package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

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

// Authenticator validates bearer tokens against the IdP's JWKS.
type Authenticator struct {
	keyfunc keyfunc.Keyfunc
	metrics *observability.Metrics
}

func NewAuthenticator(ctx context.Context, jwksURL string, metrics *observability.Metrics) (*Authenticator, error) {
	kf, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS from %s: %w", jwksURL, err)
	}

	return &Authenticator{keyfunc: kf, metrics: metrics}, nil
}

// Middleware rejects requests without a valid bearer token and puts the
// authenticated identity (provider ID, or internal-service flag) in the
// request context for handlers to authorize against. Requests it rejects
// never reach metricsMiddleware wrapping the protected routes (see
// handler.go), so it records http_requests_total for them itself.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		tokenString, ok := strings.CutPrefix(header, "Bearer ")
		if !ok || tokenString == "" {
			a.rejectUnauthenticated(w, r, "missing bearer token")
			return
		}

		var parsed claims
		_, err := jwt.ParseWithClaims(
			tokenString, &parsed, a.keyfunc.Keyfunc,
			jwt.WithValidMethods([]string{"RS256"}),
		)
		if err != nil {
			a.rejectUnauthenticated(w, r, "invalid or expired token")
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

func (a *Authenticator) rejectUnauthenticated(w http.ResponseWriter, r *http.Request, message string) {
	writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", message)
	a.metrics.HTTPRequestsTotal.WithLabelValues(
		r.Pattern, normalizeMethod(r.Method), strconv.Itoa(http.StatusUnauthorized),
	).Inc()
}

func providerIDFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(ctxKeyProviderID).(string)
	return v, ok && v != ""
}

func isInternal(ctx context.Context) bool {
	v, _ := ctx.Value(ctxKeyInternal).(bool)
	return v
}
