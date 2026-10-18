package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ids"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/postgres"
)

// maxBodyBytes bounds every request body; the largest valid one is a small
// fraction of it.
const maxBodyBytes = 64 << 10

// limitBody makes reading past maxBodyBytes fail, so a client cannot make
// the server buffer an arbitrarily large body.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		next.ServeHTTP(w, r)
	})
}

// decodeBody decodes the JSON body into dst, answering 413 for a body over
// the limit and 400 for anything else that does not decode.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	err := json.NewDecoder(r.Body).Decode(dst)
	if err == nil {
		return true
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE",
			fmt.Sprintf("the request body is limited to %d bytes", maxBodyBytes))
		return false
	}
	writeError(w, http.StatusBadRequest, "INVALID_BODY", "malformed JSON body")
	return false
}

// pathID returns the named path value when it is a well-formed ID and
// answers 400 otherwise.
func pathID(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	id := r.PathValue(name)
	if !ids.Valid(id) {
		writeError(w, http.StatusBadRequest, "INVALID_ID", name+" must be a UUID")
		return "", false
	}
	return id, true
}

// writeServerError answers a failure the client did not cause: 503 when the
// request ran out of time, so the client knows a retry may succeed, and 500
// otherwise.
func writeServerError(w http.ResponseWriter, r *http.Request, err error, message string) {
	if errors.Is(r.Context().Err(), context.DeadlineExceeded) || postgres.IsTimeout(err) {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "TIMEOUT", "the request did not finish within its time budget")
		return
	}
	writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", message)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if body == nil {
		return
	}
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("encode response body", "error", err)
	}
}

type errorBody struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Code: code, Error: message})
}

type moneyDTO struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

func toMoneyDTO(m money.Money) moneyDTO {
	return moneyDTO{Amount: m.DecimalString(), Currency: m.Currency()}
}

func (d moneyDTO) toDomain() (money.Money, error) {
	return money.New(d.Amount, d.Currency)
}
