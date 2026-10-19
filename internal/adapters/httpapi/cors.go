package httpapi

import (
	"net/http"
	"slices"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

// withCORS lets the listed browser origins call the API. It answers their
// preflights itself, before authentication, since a browser never sends a
// token on one; it refuses a preflight from any other origin; and it marks
// every response to a listed origin as readable by it. No credentials are
// allowed: the console authenticates with a bearer token, not cookies. An
// empty list leaves the API exactly as it was without CORS.
func withCORS(origins []string, metrics *observability.Metrics, next http.Handler) http.Handler {
	if len(origins) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := origin != "" && slices.Contains(origins, origin)
		header := w.Header()
		header.Add("Vary", "Origin")

		if r.Method == http.MethodOptions && origin != "" && r.Header.Get("Access-Control-Request-Method") != "" {
			started := time.Now()
			if !allowed {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "origin not allowed")
				recordRequest(metrics, r, http.StatusForbidden, started)
				return
			}
			header.Set("Access-Control-Allow-Origin", origin)
			header.Set("Access-Control-Allow-Methods", "GET, POST")
			header.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key")
			header.Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			recordRequest(metrics, r, http.StatusNoContent, started)
			return
		}

		if allowed {
			header.Set("Access-Control-Allow-Origin", origin)
			header.Set("Access-Control-Expose-Headers", "Retry-After")
		}
		next.ServeHTTP(w, r)
	})
}
