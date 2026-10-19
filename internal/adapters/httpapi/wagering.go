package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/queries"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/wagering"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
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

	input, apiErr := decodeSubmitRequest(r.Body, providerID, r.Header.Get("Idempotency-Key"))
	if apiErr != nil {
		writeError(w, apiErr.status, apiErr.code, apiErr.message)
		return
	}

	started := time.Now()
	result, err := h.deps.Wagering.Submit(r.Context(), input)
	kindLabel, statusLabel := wagering.MetricLabels(input.Kind, result, err)
	observability.ObserveSubmit(r.Context(), h.deps.Metrics, "http", kindLabel, statusLabel, started)
	logger := slog.With("transport", "http", "providerId", providerID, "walletId", input.WalletID)
	if err != nil {
		logger.ErrorContext(r.Context(), "submit wager transaction", "externalTransactionId", input.ExternalID, "error", err)
		writeSubmitError(w, r, err)
		return
	}

	// Without an inbound correlation ID, the events are correlated by
	// transaction ID, so the log uses the same value.
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

// apiError is a client mistake found while reading a request.
type apiError struct {
	status  int
	code    string
	message string
}

// decodeSubmitRequest turns a submission's body and headers into the use
// case's input, or the 4xx the client gets instead. It checks what only
// HTTP knows (the key header, the authenticated provider, the JSON); the
// use case validates the rest.
func decodeSubmitRequest(body io.Reader, providerID, idempotencyKey string) (wagering.SubmitInput, *apiError) {
	if idempotencyKey == "" {
		return wagering.SubmitInput{}, &apiError{http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY", "the Idempotency-Key header is required"}
	}

	var req submitTransactionRequest
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return wagering.SubmitInput{}, &apiError{http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE",
				fmt.Sprintf("the request body is limited to %d bytes", maxBodyBytes)}
		}
		return wagering.SubmitInput{}, &apiError{http.StatusBadRequest, "INVALID_BODY", "malformed JSON body"}
	}

	if req.ProviderID != "" && req.ProviderID != providerID {
		return wagering.SubmitInput{}, &apiError{http.StatusForbidden, "PROVIDER_MISMATCH", "authenticated provider does not match the request body"}
	}
	if wager.Kind(req.Kind) == wager.KindOpening {
		return wagering.SubmitInput{}, &apiError{http.StatusBadRequest, "OPENING_NOT_ALLOWED", "OPENING is reserved for internal wallet creation"}
	}
	amount, err := req.Money.toDomain()
	if err != nil {
		return wagering.SubmitInput{}, &apiError{http.StatusBadRequest, "INVALID_MONEY", err.Error()}
	}

	return wagering.SubmitInput{
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
	}, nil
}

func writeSubmitError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, wagering.ErrIdempotencyConflict):
		writeError(w, http.StatusConflict, "IDEMPOTENCY_CONFLICT", "idempotency key reused with a different payload")
	case errors.Is(err, wagering.ErrIdempotencyKeyMismatch):
		writeError(w, http.StatusConflict, "IDEMPOTENCY_KEY_MISMATCH", "this operation was already recorded under a different idempotency key")
	case errors.Is(err, wagering.ErrWalletNotFound):
		writeError(w, http.StatusNotFound, "WALLET_NOT_FOUND", "wallet not found")
	case errors.Is(err, wagering.ErrInvalidID):
		writeError(w, http.StatusBadRequest, "INVALID_ID", err.Error())
	case errors.Is(err, wagering.ErrInvalidInput), errors.Is(err, wagering.ErrOpeningNotAllowed):
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	case errors.Is(err, wager.ErrInvalidTransaction),
		errors.Is(err, wager.ErrInvalidAmount),
		errors.Is(err, money.ErrInvalidAmount),
		errors.Is(err, money.ErrInvalidCurrency),
		errors.Is(err, money.ErrCurrencyMismatch):
		writeError(w, http.StatusBadRequest, "VALIDATION_ERROR", err.Error())
	default:
		writeServerError(w, r, err, "could not process the transaction")
	}
}

func statusCodeForResult(result wagering.Result) int {
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

func toSubmitResponse(result wagering.Result) submitTransactionResponse {
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

func toTransactionResponse(view queries.TransactionView) transactionResponse {
	tx := view.Transaction
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
	if view.Balance.Currency() != "" {
		dto := toMoneyDTO(view.Balance)
		resp.Balance = &dto
	}
	return resp
}

func (h *handler) getTransaction(w http.ResponseWriter, r *http.Request) {
	providerID, ok := requireProvider(w, r)
	if !ok {
		return
	}

	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	view, err := h.deps.Reader.Transaction(r.Context(), id)
	if err != nil {
		if errors.Is(err, queries.ErrTransactionNotFound) {
			writeError(w, http.StatusNotFound, "TRANSACTION_NOT_FOUND", "transaction not found")
			return
		}
		writeServerError(w, r, err, "could not load transaction")
		return
	}

	if view.Transaction.ProviderID() != providerID {
		writeError(w, http.StatusNotFound, "TRANSACTION_NOT_FOUND", "transaction not found")
		return
	}

	writeJSON(w, http.StatusOK, toTransactionResponse(view))
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

	view, err := h.deps.Reader.TransactionByExternalID(r.Context(), providerID, r.PathValue("externalId"))
	if err != nil {
		if errors.Is(err, queries.ErrTransactionNotFound) {
			writeError(w, http.StatusNotFound, "TRANSACTION_NOT_FOUND", "transaction not found")
			return
		}
		writeServerError(w, r, err, "could not load transaction")
		return
	}

	writeJSON(w, http.StatusOK, toTransactionResponse(view))
}
