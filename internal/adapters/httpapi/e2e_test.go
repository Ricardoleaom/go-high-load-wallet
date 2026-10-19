package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/httpapi"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/wagering"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/jwttest"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// apiHarness runs the real handler, use case and PostgreSQL adapter against
// a real PostgreSQL, with jwttest standing in for Keycloak.
type apiHarness struct {
	server  *httptest.Server
	issuer  *jwttest.Issuer
	metrics *observability.Metrics
}

func newAPIHarness(t *testing.T, opts ...func(*httpapi.Dependencies)) *apiHarness {
	t.Helper()
	databaseURL := pgtest.URL(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	pool, err := postgres.Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	t.Cleanup(pool.Close)

	issuer := jwttest.NewIssuer(t)
	metrics := observability.New()
	auth, err := httpapi.NewAuthenticator(ctx, httpapi.AuthConfig{
		JWKSURL: issuer.JWKSURL(), RefreshInterval: time.Hour, Issuer: issuer.URL(), Audience: jwttest.Audience,
	}, metrics)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}

	deps := httpapi.Dependencies{
		Wagering: wagering.NewService(postgres.NewEngine(pool), postgres.NewReferenceQueue(pool)),
		Reader:   postgres.NewReader(pool),
		Auth:     auth,
		Metrics:  metrics,
	}
	for _, opt := range opts {
		opt(&deps)
	}
	server := httptest.NewServer(httpapi.NewHandler(deps))
	t.Cleanup(server.Close)

	return &apiHarness{server: server, issuer: issuer, metrics: metrics}
}

type apiResponse struct {
	status int
	body   map[string]any
}

func (h *apiHarness) do(t *testing.T, method, path, token string, headers map[string]string, body any) apiResponse {
	t.Helper()

	var payload io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode request body: %v", err)
		}
		payload = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, h.server.URL+path, payload)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}

	resp, err := h.server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("decode %s %s response: %v", method, path, err)
	}
	return apiResponse{status: resp.StatusCode, body: decoded}
}

func (h *apiHarness) openWallet(t *testing.T, balance string) (walletID, playerID string) {
	t.Helper()
	playerID = uuid.NewString()
	resp := h.do(t, http.MethodPost, "/wallets", h.issuer.InternalToken(t), nil, map[string]any{
		"playerId":       playerID,
		"initialBalance": map[string]string{"amount": balance, "currency": "BRL"},
	})
	requireStatus(t, resp, http.StatusCreated)
	return resp.body["id"].(string), playerID
}

// submit derives the Idempotency-Key from the external ID, so resending
// the same body is a replay and changing anything else is a conflict.
func (h *apiHarness) submit(t *testing.T, token string, body map[string]any) apiResponse {
	t.Helper()
	key := "e2e:" + body["externalTransactionId"].(string)
	return h.do(t, http.MethodPost, "/wagering/transactions", token, map[string]string{"Idempotency-Key": key}, body)
}

func wagerBody(walletID, playerID, kind, amount string) map[string]any {
	return map[string]any{
		"providerId":            "provider-a",
		"externalTransactionId": uuid.NewString(),
		"playerId":              playerID,
		"walletId":              walletID,
		"roundId":               "round-e2e",
		"gameId":                "game-e2e",
		"kind":                  kind,
		"money":                 map[string]string{"amount": amount, "currency": "BRL"},
	}
}

func requireStatus(t *testing.T, resp apiResponse, want int) {
	t.Helper()
	if resp.status != want {
		t.Fatalf("status = %d, want %d (body %v)", resp.status, want, resp.body)
	}
}

func requireField(t *testing.T, resp apiResponse, field string, want any) {
	t.Helper()
	if got := resp.body[field]; got != want {
		t.Fatalf("%s = %v, want %v (body %v)", field, got, want, resp.body)
	}
}

func requireBalance(t *testing.T, resp apiResponse, want string) {
	t.Helper()
	balance, ok := resp.body["balance"].(map[string]any)
	if !ok {
		t.Fatalf("response has no balance: %v", resp.body)
	}
	if got := balance["amount"]; got != want {
		t.Fatalf("balance = %v, want %s", got, want)
	}
}

func TestE2EOpenWalletAndRejectADuplicatePlayerCurrency(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, playerID := h.openWallet(t, "100.00")

	got := h.do(t, http.MethodGet, "/wallets/"+walletID, h.issuer.InternalToken(t), nil, nil)
	requireStatus(t, got, http.StatusOK)
	requireBalance(t, got, "100.00")

	duplicate := h.do(t, http.MethodPost, "/wallets", h.issuer.InternalToken(t), nil, map[string]any{
		"playerId":       playerID,
		"initialBalance": map[string]string{"amount": "5.00", "currency": "BRL"},
	})
	requireStatus(t, duplicate, http.StatusConflict)
	requireField(t, duplicate, "code", "WALLET_CONFLICT")
}

func TestE2EWalletEndpointsRequireTheInternalRole(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, _ := h.openWallet(t, "10.00")

	resp := h.do(t, http.MethodGet, "/wallets/"+walletID, h.issuer.ProviderToken(t, "provider-a"), nil, nil)
	requireStatus(t, resp, http.StatusForbidden)
	requireField(t, resp, "code", "FORBIDDEN")
}

func TestE2EBetIsProcessedOnceAndReplayed(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, playerID := h.openWallet(t, "100.00")
	token := h.issuer.ProviderToken(t, "provider-a")
	bet := wagerBody(walletID, playerID, "BET", "25.00")

	first := h.submit(t, token, bet)
	requireStatus(t, first, http.StatusOK)
	requireField(t, first, "status", "PROCESSED")
	requireField(t, first, "idempotentReplay", false)
	requireBalance(t, first, "75.00")

	replay := h.submit(t, token, bet)
	requireStatus(t, replay, http.StatusOK)
	requireField(t, replay, "idempotentReplay", true)
	requireField(t, replay, "transactionId", first.body["transactionId"])
	requireBalance(t, replay, "75.00")

	changed := maps.Clone(bet)
	changed["money"] = map[string]string{"amount": "30.00", "currency": "BRL"}
	conflict := h.submit(t, token, changed)
	requireStatus(t, conflict, http.StatusConflict)
	requireField(t, conflict, "code", "IDEMPOTENCY_CONFLICT")
}

func TestE2EBetWithoutAnIdempotencyKeyIsRejected(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, playerID := h.openWallet(t, "10.00")

	resp := h.do(t, http.MethodPost, "/wagering/transactions", h.issuer.ProviderToken(t, "provider-a"), nil,
		wagerBody(walletID, playerID, "BET", "1.00"))
	requireStatus(t, resp, http.StatusBadRequest)
	requireField(t, resp, "code", "MISSING_IDEMPOTENCY_KEY")
}

func TestE2EBetBeyondTheBalanceIsRejected(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, playerID := h.openWallet(t, "10.00")

	resp := h.submit(t, h.issuer.ProviderToken(t, "provider-a"), wagerBody(walletID, playerID, "BET", "25.00"))
	requireStatus(t, resp, http.StatusUnprocessableEntity)
	requireField(t, resp, "status", "REJECTED")
	requireField(t, resp, "failureCode", "INSUFFICIENT_BALANCE")
}

func TestE2EInvalidTokensAreUnauthenticatedQuickly(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, playerID := h.openWallet(t, "10.00")

	tokens := map[string]string{
		"missing":     "",
		"expired":     h.issuer.ExpiredProviderToken(t, "provider-a"),
		"hs256":       h.issuer.HS256ProviderToken(t, "provider-a"),
		"unknown key": h.issuer.UnknownKeyProviderToken(t, "provider-a"),
	}
	for name, token := range tokens {
		t.Run(name, func(t *testing.T) {
			started := time.Now()
			resp := h.submit(t, token, wagerBody(walletID, playerID, "BET", "1.00"))
			requireStatus(t, resp, http.StatusUnauthorized)
			requireField(t, resp, "code", "UNAUTHENTICATED")
			if elapsed := time.Since(started); elapsed > 2*time.Second {
				t.Fatalf("rejecting the token took %v", elapsed)
			}
		})
	}
}

func TestE2EProviderInTheBodyMustMatchTheToken(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, playerID := h.openWallet(t, "10.00")

	resp := h.submit(t, h.issuer.ProviderToken(t, "provider-b"), wagerBody(walletID, playerID, "BET", "1.00"))
	requireStatus(t, resp, http.StatusForbidden)
	requireField(t, resp, "code", "PROVIDER_MISMATCH")
}

func TestE2ETransactionsAreIsolatedByProvider(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, playerID := h.openWallet(t, "50.00")
	bet := wagerBody(walletID, playerID, "BET", "5.00")

	created := h.submit(t, h.issuer.ProviderToken(t, "provider-a"), bet)
	requireStatus(t, created, http.StatusOK)
	transactionID := created.body["transactionId"].(string)

	own := h.do(t, http.MethodGet, "/wagering/transactions/"+transactionID, h.issuer.ProviderToken(t, "provider-a"), nil, nil)
	requireStatus(t, own, http.StatusOK)
	requireField(t, own, "status", "PROCESSED")

	foreign := h.do(t, http.MethodGet, "/wagering/transactions/"+transactionID, h.issuer.ProviderToken(t, "provider-b"), nil, nil)
	requireStatus(t, foreign, http.StatusNotFound)
	requireField(t, foreign, "code", "TRANSACTION_NOT_FOUND")

	byExternalID := "/providers/provider-a/wagering/transactions/" + bet["externalTransactionId"].(string)
	crossProvider := h.do(t, http.MethodGet, byExternalID, h.issuer.ProviderToken(t, "provider-b"), nil, nil)
	requireStatus(t, crossProvider, http.StatusForbidden)
}

func TestE2ERefundBeforeItsReferenceIsPending(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, playerID := h.openWallet(t, "20.00")

	refund := wagerBody(walletID, playerID, "REFUND", "5.00")
	refund["referenceExternalTransactionId"] = uuid.NewString()

	resp := h.submit(t, h.issuer.ProviderToken(t, "provider-a"), refund)
	requireStatus(t, resp, http.StatusAccepted)
	requireField(t, resp, "status", "PENDING_REFERENCE")
}

func TestE2ELedgerPagesWithoutGapsOrRepeats(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, playerID := h.openWallet(t, "100.00")
	provider := h.issuer.ProviderToken(t, "provider-a")
	for range 3 {
		requireStatus(t, h.submit(t, provider, wagerBody(walletID, playerID, "BET", "10.00")), http.StatusOK)
	}

	seen := map[string]bool{}
	path := "/wallets/" + walletID + "/ledger?limit=2"
	for range 4 { // 4 entries in pages of 2 need at most 3 requests
		page := h.do(t, http.MethodGet, path, h.issuer.InternalToken(t), nil, nil)
		requireStatus(t, page, http.StatusOK)

		for _, raw := range page.body["entries"].([]any) {
			id := raw.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("ledger entry %s returned twice", id)
			}
			seen[id] = true
		}

		cursor, _ := page.body["nextCursor"].(string)
		if cursor == "" {
			break
		}
		path = "/wallets/" + walletID + "/ledger?limit=2&cursor=" + url.QueryEscape(cursor)
	}

	if len(seen) != 4 {
		t.Fatalf("ledger returned %d distinct entries, want 4 (opening + 3 bets)", len(seen))
	}
}

func TestE2EReconciliationIsConsistentAfterMovements(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, playerID := h.openWallet(t, "100.00")
	requireStatus(t, h.submit(t, h.issuer.ProviderToken(t, "provider-a"), wagerBody(walletID, playerID, "BET", "30.00")), http.StatusOK)

	resp := h.do(t, http.MethodPost, "/wallets/"+walletID+"/reconciliation", h.issuer.InternalToken(t), nil, nil)
	requireStatus(t, resp, http.StatusOK)
	requireField(t, resp, "consistent", true)
	requireField(t, resp, "checkedEntries", float64(2))
}

func TestE2ESubmitLatencyIsRecordedPerKindAndStatus(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, playerID := h.openWallet(t, "100.00")

	requireStatus(t, h.submit(t, h.issuer.ProviderToken(t, "provider-a"), wagerBody(walletID, playerID, "BET", "10.00")), http.StatusOK)

	if got := histogramCount(t, h.metrics, "wager_submit_duration_seconds",
		map[string]string{"transport": "http", "kind": "BET", "status": "PROCESSED"}); got != 1 {
		t.Fatalf("submit observations = %d, want 1", got)
	}
	if got := histogramCount(t, h.metrics, "http_request_duration_seconds",
		map[string]string{"route": "POST /wagering/transactions", "method": "POST", "status_class": "2xx"}); got != 1 {
		t.Fatalf("HTTP observations = %d, want 1", got)
	}
}

// histogramCount returns the sample count of the series whose labels match.
func histogramCount(t *testing.T, metrics *observability.Metrics, name string, labels map[string]string) uint64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
	series:
		for _, metric := range family.GetMetric() {
			got := map[string]string{}
			for _, pair := range metric.GetLabel() {
				got[pair.GetName()] = pair.GetValue()
			}
			for key, value := range labels {
				if got[key] != value {
					continue series
				}
			}
			return metric.GetHistogram().GetSampleCount()
		}
	}
	return 0
}
