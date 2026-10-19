package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/queries"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/wagering"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

// ReadinessChecker reports whether a dependency is ready to serve traffic.
type ReadinessChecker interface {
	Name() string
	Ready(ctx context.Context) error
}

type Dependencies struct {
	Wagering          *wagering.Service
	Reader            queries.Reader
	Auth              *Authenticator
	Metrics           *observability.Metrics
	ReadinessCheckers []ReadinessChecker

	// RequestTimeout bounds every authenticated request; zero means none.
	RequestTimeout time.Duration

	// AllowedOrigins are the browser origins CORS admits; none turns it off.
	AllowedOrigins []string

	// Summary serves GET /ops/summary; nil leaves the route out.
	Summary OpsSummary
}

type handler struct {
	deps Dependencies
}

// route is one pattern and its handler. The two route lists are the
// API's surface; docs/openapi.yaml must describe exactly these
// (TestOpenAPIDescribesEveryRoute).
type route struct {
	pattern string
	handle  http.HandlerFunc
}

// publicRoutes need no token.
func publicRoutes(h *handler) []route {
	return []route{
		{"GET /health/live", healthLive},
		{"GET /health/ready", h.healthReady},
	}
}

// protectedRoutes sit behind the authenticator; each authorizes its caller.
func (h *handler) protectedRoutes() []route {
	routes := []route{
		{"POST /wallets", h.openWallet},
		{"GET /wallets/{id}", h.getWallet},
		{"GET /wallets/{id}/ledger", h.getWalletLedger},
		{"POST /wallets/{id}/reconciliation", h.reconcileWallet},
		{"POST /wagering/transactions", h.submitWagerTransaction},
		{"GET /wagering/transactions/{id}", h.getTransaction},
		{"GET /providers/{providerId}/wagering/transactions/{externalId}", h.getTransactionByExternalID},
	}
	if h.deps.Summary != nil {
		routes = append(routes, route{"GET /ops/summary", h.getOpsSummary})
	}
	return routes
}

func NewHandler(deps Dependencies) http.Handler {
	h := &handler{deps: deps}

	protected := http.NewServeMux()
	for _, r := range h.protectedRoutes() {
		protected.HandleFunc(r.pattern, r.handle)
	}

	mux := http.NewServeMux()
	for _, r := range publicRoutes(h) {
		mux.Handle(r.pattern, metricsMiddleware(deps.Metrics, r.handle))
	}

	// Auth.Middleware calls r.WithContext to attach the authenticated
	// identity, which (per net/http's own semantics) hands `protected` a
	// copy of the request, not the original pointer. metricsMiddleware has
	// to wrap `protected` specifically, on the inside of that copy, or it
	// would only ever see the outer "/" catch-all pattern instead of the
	// actual matched route for every successfully authenticated request.
	// Auth itself records metrics for the requests it rejects, since those
	// never reach this inner wrapper at all.
	// The deadline goes outside metricsMiddleware: it hands on a copy of the
	// request, as Auth does, and metricsMiddleware must sit on the same
	// request `protected` matches.
	mux.Handle("/", deps.Auth.Middleware(withRequestTimeout(deps.RequestTimeout,
		metricsMiddleware(deps.Metrics, limitBody(protected)))))

	return otelhttp.NewHandler(withCORS(deps.AllowedOrigins, deps.Metrics, mux), "http.server")
}

// withRequestTimeout gives each request a deadline that every database call
// it makes inherits; zero leaves requests unbounded.
func withRequestTimeout(timeout time.Duration, next http.Handler) http.Handler {
	if timeout <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// metricsMiddleware records every response by route pattern, method and
// status, feeding http_requests_total and http_request_duration_seconds.
func metricsMiddleware(metrics *observability.Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		recordRequest(metrics, r, recorder.status, started)
	})
}

// recordRequest labels by r.Pattern, which is always a registered mux
// pattern or empty, so it can't be used to inflate cardinality. r.Method is
// whatever string the client sent on the request line: an attacker could
// otherwise mint an unbounded number of label values by varying it, so it's
// normalized to a known method or "OTHER" first.
func recordRequest(metrics *observability.Metrics, r *http.Request, status int, started time.Time) {
	method := normalizeMethod(r.Method)
	metrics.HTTPRequestsTotal.WithLabelValues(r.Pattern, method, strconv.Itoa(status)).Inc()
	observability.ObserveDuration(r.Context(),
		metrics.HTTPRequestDuration.WithLabelValues(r.Pattern, method, strconv.Itoa(status/100)+"xx"),
		time.Since(started).Seconds(),
	)
}

func normalizeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}
