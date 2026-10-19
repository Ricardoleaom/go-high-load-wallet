package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestOperatorReadsAndOpensWallets(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	operator := h.issuer.OperatorToken(t)

	opened := h.do(t, http.MethodPost, "/wallets", operator, nil, map[string]any{
		"playerId":       uuid.NewString(),
		"initialBalance": map[string]string{"amount": "25.00", "currency": "BRL"},
	})
	requireStatus(t, opened, http.StatusCreated)
	walletID := opened.body["id"].(string)

	requireStatus(t, h.do(t, http.MethodGet, "/wallets/"+walletID, operator, nil, nil), http.StatusOK)
	requireStatus(t, h.do(t, http.MethodGet, "/wallets/"+walletID+"/ledger", operator, nil, nil), http.StatusOK)
	requireStatus(t, h.do(t, http.MethodPost, "/wallets/"+walletID+"/reconciliation", operator, nil, nil), http.StatusOK)
}

func TestOperatorReadsAnyProvidersTransactions(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, playerID := h.openWallet(t, "50.00")
	bet := wagerBody(walletID, playerID, "BET", "5.00")
	created := h.submit(t, h.issuer.ProviderToken(t, "provider-a"), bet)
	requireStatus(t, created, http.StatusOK)
	operator := h.issuer.OperatorToken(t)

	byID := h.do(t, http.MethodGet, "/wagering/transactions/"+created.body["transactionId"].(string), operator, nil, nil)
	requireStatus(t, byID, http.StatusOK)
	requireField(t, byID, "providerId", "provider-a")

	byExternalID := "/providers/provider-a/wagering/transactions/" + bet["externalTransactionId"].(string)
	requireStatus(t, h.do(t, http.MethodGet, byExternalID, operator, nil, nil), http.StatusOK)

	missing := h.do(t, http.MethodGet, "/wagering/transactions/"+uuid.NewString(), operator, nil, nil)
	requireStatus(t, missing, http.StatusNotFound)
	requireField(t, missing, "code", "TRANSACTION_NOT_FOUND")
}

func TestOperatorCannotSubmitWagers(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, playerID := h.openWallet(t, "50.00")

	resp := h.submit(t, h.issuer.OperatorToken(t), wagerBody(walletID, playerID, "BET", "5.00"))
	requireStatus(t, resp, http.StatusForbidden)
	requireField(t, resp, "code", "FORBIDDEN")

	wallet := h.do(t, http.MethodGet, "/wallets/"+walletID, h.issuer.InternalToken(t), nil, nil)
	requireBalance(t, wallet, "50.00")
}

func TestProviderCannotUseWalletOperations(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t)
	walletID, _ := h.openWallet(t, "10.00")
	provider := h.issuer.ProviderToken(t, "provider-a")

	for _, call := range []struct{ method, path string }{
		{http.MethodPost, "/wallets"},
		{http.MethodGet, "/wallets/" + walletID + "/ledger"},
		{http.MethodPost, "/wallets/" + walletID + "/reconciliation"},
	} {
		resp := h.do(t, call.method, call.path, provider, nil, map[string]any{})
		requireStatus(t, resp, http.StatusForbidden)
		requireField(t, resp, "code", "FORBIDDEN")
	}
}
