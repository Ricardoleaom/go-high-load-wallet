package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	wagerapp "github.com/Ricardoleaom/go-high-load-wallet/internal/application/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/postgres"
)

func requireProvider(w http.ResponseWriter, r *http.Request) (string, bool) {
	providerID, ok := providerIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "a provider identity is required")
		return "", false
	}
	return providerID, true
}

type submitTransactionRequest struct {
	ProviderID                     string   `json:"providerId"`
	ExternalTransactionID          string   `json:"externalTransactionId"`
	PlayerID                       string   `json:"playerId"`
	WalletID                       string   `json:"walletId"`
	RoundID                        string   `json:"roundId"`
	GameID                         string   `json:"gameId"`
	Kind                           string   `json:"kind"`
	Money                          moneyDTO `json:"money"`
	ReferenceExternalTransactionID string   `json:"referenceExternalTransactionId,omitempty"`
}

type submitTransactionResponse struct {
	TransactionID    string    `json:"transactionId"`
	Status           string    `json:"status"`
	Balance          *moneyDTO `json:"balance,omitempty"`
	IdempotentReplay bool      `json:"idempotentReplay"`
	FailureCode      string    `json:"failureCode,omitempty"`
}

func (h *handler) submitWagerTransaction(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireProvider(w, r)
	if !ok {
		return
	}

	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		writeError(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY", "the Idempotency-Key header is required")
		return
	}

	var req submitTransactionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY", "malformed JSON body")
		return
	}

	if req.ProviderID != "" && req.ProviderID != providerID {
		writeError(w, http.StatusForbidden, "PROVIDER_MISMATCH", "authenticated provider does not match the request body")
		return
	}

	if wager.Kind(req.Kind) == wager.KindOpening {
		writeError(w, http.StatusBadRequest, "OPENING_NOT_ALLOWED", "OPENING is reserved for internal wallet creation")
		return
	}

	amount, err := req.Money.toDomain()
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_MONEY", err.Error())
		return
	}

	result, err := h.deps.WagerService.Submit(r.Context(), wagerapp.SubmitInput{
		ProviderID:          providerID,
		ExternalID:          req.ExternalTransactionID,
		IdempotencyKey:      idempotencyKey,
		PlayerID:            req.PlayerID,
		WalletID:            req.WalletID,
		RoundID:             req.RoundID,
		GameID:              req.GameID,
		Kind:                wager.Kind(req.Kind),
		Amount:              amount,
		ReferenceExternalID: req.ReferenceExternalTransactionID,
	})
	logger := slog.With("transport", "http", "providerId", providerID, "walletId", req.WalletID)
	if err != nil {
		logger.ErrorContext(r.Context(), "submit wager transaction", "externalTransactionId", req.ExternalTransactionID, "error", err)
		writeSubmitError(w, err)
		return
	}

	// Without an inbound correlation ID, the use case correlates the outbox
	// events by transaction ID, so the log uses the same value.
	logger.InfoContext(r.Context(), "wager transaction handled",
		"correlationId", result.TransactionID,
		"transactionId", result.TransactionID,
		"status", result.Status,
		"failureCode", result.FailureCode,
		"idempotentReplay", result.IdempotentReplay,
	)

	h.deps.Metrics.WagerTransactionsTotal.WithLabelValues(string(result.Status)).Inc()
	if result.IdempotentReplay {
		h.deps.Metrics.IdempotentReplaysTotal.Inc()
	}

	writeJSON(w, statusCodeForResult(result), toSubmitResponse(result))
}

func writeSubmitError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, wagerapp.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "idempotency key reused with a different payload")
	case errors.Is(err, wagerapp.ErrIdempotencyKeyMismatch):
		writeError(w, http.StatusConflict, "IDEMPOTENCY_KEY_MISMATCH", "this operation was already recorded under a different idempotency key")
	case errors.Is(err, wagerapp.ErrWalletNotFound):
		writeError(w, http.StatusNotFound, "WALLET_NOT_FOUND", "wallet not found")
	case errors.Is(err, wager.ErrInvalidTransaction),
		errors.Is(err, wager.ErrInvalidAmount),
		errors.Is(err, money.ErrInvalidAmount),
		errors.Is(err, money.ErrInvalidCurrency),
		errors.Is(err, money.ErrCurrencyMismatch):
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not process the transaction")
	}
}

func statusCodeForResult(result wagerapp.Result) int {
	switch result.Status {
	case wager.StatusProcessed:
		return http.StatusOK
	case wager.StatusPendingReference:
		return http.StatusAccepted
	case wager.StatusRejected, wager.StatusFailed:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusOK
	}
}

func toSubmitResponse(result wagerapp.Result) submitTransactionResponse {
	resp := submitTransactionResponse{
		TransactionID:    result.TransactionID,
		Status:           string(result.Status),
		IdempotentReplay: result.IdempotentReplay,
		FailureCode:      result.FailureCode,
	}
	if result.Balance.Currency() != "" {
		dto := toMoneyDTO(result.Balance)
		resp.Balance = &dto
	}
	return resp
}

type transactionResponse struct {
	TransactionID                  string    `json:"transactionId"`
	ProviderID                     string    `json:"providerId,omitempty"`
	ExternalTransactionID          string    `json:"externalTransactionId,omitempty"`
	WalletID                       string    `json:"walletId"`
	Kind                           string    `json:"kind"`
	Status                         string    `json:"status"`
	Money                          moneyDTO  `json:"money"`
	Balance                        *moneyDTO `json:"balance,omitempty"`
	ReferenceExternalTransactionID string    `json:"referenceExternalTransactionId,omitempty"`
	FailureCode                    string    `json:"failureCode,omitempty"`
}

func (h *handler) buildTransactionResponse(r *http.Request, tx wager.Transaction) transactionResponse {
	resp := transactionResponse{
		TransactionID:                  tx.ID(),
		ProviderID:                     tx.ProviderID(),
		ExternalTransactionID:          tx.ExternalID(),
		WalletID:                       tx.WalletID(),
		Kind:                           string(tx.Kind()),
		Status:                         string(tx.Status()),
		Money:                          toMoneyDTO(tx.Amount()),
		ReferenceExternalTransactionID: tx.ReferenceExternalID(),
		FailureCode:                    tx.FailureCode(),
	}

	if tx.Status() == wager.StatusProcessed {
		if entry, err := h.deps.Ledger.GetByTransactionID(r.Context(), tx.ID()); err == nil {
			dto := toMoneyDTO(entry.BalanceAfter())
			resp.Balance = &dto
		} else if w, err := h.deps.Wallets.GetByID(r.Context(), tx.WalletID()); err == nil {
			dto := toMoneyDTO(w.Balance())
			resp.Balance = &dto
		}
	}

	return resp
}

func (h *handler) getTransaction(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireProvider(w, r)
	if !ok {
		return
	}

	id := r.PathValue("id")
	tx, err := h.deps.Transactions.GetByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, postgres.ErrTransactionNotFound) {
			writeError(w, http.StatusNotFound, "TRANSACTION_NOT_FOUND", "transaction not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load transaction")
		return
	}

	if tx.ProviderID() != providerID {
		writeError(w, http.StatusNotFound, "TRANSACTION_NOT_FOUND", "transaction not found")
		return
	}

	writeJSON(w, http.StatusOK, h.buildTransactionResponse(r, tx))
}

func (h *handler) getTransactionByExternalID(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireProvider(w, r)
	if !ok {
		return
	}

	pathProviderID := r.PathValue("providerId")
	if pathProviderID != providerID {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "cannot access another provider's transactions")
		return
	}

	externalID := r.PathValue("externalId")
	tx, err := h.deps.Transactions.GetByExternalID(r.Context(), providerID, externalID)
	if err != nil {
		if errors.Is(err, postgres.ErrTransactionNotFound) {
			writeError(w, http.StatusNotFound, "TRANSACTION_NOT_FOUND", "transaction not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "could not load transaction")
		return
	}

	writeJSON(w, http.StatusOK, h.buildTransactionResponse(r, tx))
}
