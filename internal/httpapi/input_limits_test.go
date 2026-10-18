package httpapi_test

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func requireError(t *testing.T, resp apiResponse, status int, code string) {
	t.Helper()
	if resp.status != status || resp.body["code"] != code {
		t.Fatalf("response = %d %v, want %d with code %s", resp.status, resp.body, status, code)
	}
}

// Identifiers are UUIDs: anything else is the client's mistake, never a 500.
func TestMalformedIdentifiersReturn400(t *testing.T) {
	h := newAPIHarness(t)
	internal := h.issuer.InternalToken(t)
	provider := h.issuer.ProviderToken(t, "provider-a")
	walletID, playerID := h.openWallet(t, "100.00")

	requireError(t, h.do(t, http.MethodGet, "/wallets/not-a-uuid", internal, nil, nil), 400, "INVALID_ID")
	requireError(t, h.do(t, http.MethodGet, "/wallets/not-a-uuid/ledger", internal, nil, nil), 400, "INVALID_ID")
	requireError(t, h.do(t, http.MethodPost, "/wallets/not-a-uuid/reconciliation", internal, nil, nil), 400, "INVALID_ID")
	requireError(t, h.do(t, http.MethodGet, "/wagering/transactions/not-a-uuid", provider, nil, nil), 400, "INVALID_ID")

	requireError(t, h.do(t, http.MethodPost, "/wallets", internal, nil, map[string]any{
		"playerId":       "abc",
		"initialBalance": map[string]string{"amount": "1.00", "currency": "BRL"},
	}), 400, "INVALID_PLAYER_ID")

	badWallet := wagerBody("abc", playerID, "BET", "1.00")
	requireError(t, h.submit(t, provider, badWallet), 400, "INVALID_ID")
	badPlayer := wagerBody(walletID, "abc", "BET", "1.00")
	requireError(t, h.submit(t, provider, badPlayer), 400, "INVALID_ID")

	cursor := base64.RawURLEncoding.EncodeToString([]byte("2026-01-01T00:00:00Z|not-a-uuid"))
	requireError(t, h.do(t, http.MethodGet, "/wallets/"+walletID+"/ledger?cursor="+cursor, internal, nil, nil), 400, "INVALID_CURSOR")
}

func TestOversizedBodiesAreRejected(t *testing.T) {
	h := newAPIHarness(t)
	padding := strings.Repeat("x", 65<<10)

	requireError(t, h.do(t, http.MethodPost, "/wallets", h.issuer.InternalToken(t), nil, map[string]any{
		"playerId":       uuid.NewString(),
		"initialBalance": map[string]string{"amount": "1.00", "currency": "BRL"},
		"padding":        padding,
	}), http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE")

	walletID, playerID := h.openWallet(t, "100.00")
	body := wagerBody(walletID, playerID, "BET", "1.00")
	body["gameId"] = padding
	requireError(t, h.submit(t, h.issuer.ProviderToken(t, "provider-a"), body), http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE")
}

func TestLongIdempotencyKeyIsRejected(t *testing.T) {
	h := newAPIHarness(t)
	walletID, playerID := h.openWallet(t, "100.00")
	resp := h.do(t, http.MethodPost, "/wagering/transactions", h.issuer.ProviderToken(t, "provider-a"),
		map[string]string{"Idempotency-Key": strings.Repeat("k", 256)},
		wagerBody(walletID, playerID, "BET", "1.00"))
	requireError(t, resp, http.StatusBadRequest, "VALIDATION_ERROR")
}
