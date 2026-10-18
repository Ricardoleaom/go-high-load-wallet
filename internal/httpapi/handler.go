package httpapi

import (
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	wagerapp "github.com/Ricardoleaom/go-high-load-wallet/internal/application/wager"
	walletapp "github.com/Ricardoleaom/go-high-load-wallet/internal/application/wallet"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/observability"
)

// ReadinessChecker reports whether a dependency is ready to serve traffic.
type ReadinessChecker interface {
	Name() string
	Ready(ctx context.Context) error
}

type Dependencies struct {
	DB                *sql.DB
	Wallets           *postgres.WalletRepository
	Transactions      *postgres.TransactionRepository
	Ledger            *postgres.LedgerRepository
	WalletService     *walletapp.Service
	WagerService      *wagerapp.Service
	Auth              *Authenticator
	Metrics           *observability.Metrics
	ReadinessCheckers []ReadinessChecker
}

type handler struct {
	deps Dependencies
}

func NewHandler(deps Dependencies) http.Handler {
	h := &handler{deps: deps}

	protected := http.NewServeMux()
	protected.HandleFunc("POST /wallets", h.openWallet)
	protected.HandleFunc("GET /wallets/{id}", h.getWallet)
	protected.HandleFunc("GET /wallets/{id}/ledger", h.getWalletLedger)
	protected.HandleFunc("POST /wallets/{id}/reconciliation", h.reconcileWallet)
	protected.HandleFunc("POST /wagering/transactions", h.submitWagerTransaction)
	protected.HandleFunc("GET /wagering/transactions/{id}", h.getTransaction)
	protected.HandleFunc(
		"GET /providers/{providerId}/wagering/transactions/{externalId}",
		h.getTransactionByExternalID,
	)

	mux := http.NewServeMux()
	mux.Handle("GET /health/live", metricsMiddleware(deps.Metrics, http.HandlerFunc(healthLive)))
	mux.Handle("GET /health/ready", metricsMiddleware(deps.Metrics, http.HandlerFunc(h.healthReady)))

	// Auth.Middleware calls r.WithContext to attach the authenticated
	// identity, which (per net/http's own semantics) hands `protected` a
	// copy of the request, not the original pointer. metricsMiddleware has
	// to wrap `protected` specifically, on the inside of that copy, or it
	// would only ever see the outer "/" catch-all pattern instead of the
	// actual matched route for every successfully authenticated request.
	// Auth itself records metrics for the requests it rejects, since those
	// never reach this inner wrapper at all.
	mux.Handle("/", deps.Auth.Middleware(metricsMiddleware(deps.Metrics, limitBody(protected))))

	return otelhttp.NewHandler(mux, "http.server")
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
