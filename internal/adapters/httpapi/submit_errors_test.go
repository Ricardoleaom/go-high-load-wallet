package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/wagering"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
)

func TestWriteSubmitErrorMapsEveryError(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{wagering.ErrIdempotencyConflict, http.StatusConflict, "IDEMPOTENCY_CONFLICT"},
		{wagering.ErrIdempotencyKeyMismatch, http.StatusConflict, "IDEMPOTENCY_KEY_MISMATCH"},
		{wagering.ErrWalletNotFound, http.StatusNotFound, "WALLET_NOT_FOUND"},
		{wagering.ErrInvalidID, http.StatusBadRequest, "INVALID_ID"},
		{wagering.ErrFieldTooLong, http.StatusBadRequest, "VALIDATION_ERROR"},
		{wager.ErrInvalidTransaction, http.StatusBadRequest, "VALIDATION_ERROR"},
		{wager.ErrInvalidAmount, http.StatusBadRequest, "VALIDATION_ERROR"},
		{money.ErrInvalidAmount, http.StatusBadRequest, "VALIDATION_ERROR"},
		{money.ErrInvalidCurrency, http.StatusBadRequest, "VALIDATION_ERROR"},
		{money.ErrCurrencyMismatch, http.StatusBadRequest, "VALIDATION_ERROR"},
		{wagering.ErrOpeningNotAllowed, http.StatusBadRequest, "VALIDATION_ERROR"},
		{fmt.Errorf("wrapped: %w", wagering.ErrIdempotencyConflict), http.StatusConflict, "IDEMPOTENCY_CONFLICT"},
		{fmt.Errorf("submit: %w", markedTimeout{}), http.StatusServiceUnavailable, "TIMEOUT"},
		{errors.New("unexpected"), http.StatusInternalServerError, "INTERNAL_ERROR"},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeSubmitError(recorder, httptest.NewRequest(http.MethodPost, "/wagering/transactions", nil), tc.err)
			assertError(t, recorder, tc.status, tc.code)
		})
	}

	t.Run("deadline exceeded", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()
		<-ctx.Done()
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/wagering/transactions", nil).WithContext(ctx)
		writeSubmitError(recorder, request, fmt.Errorf("move wallet: %w", ctx.Err()))
		assertError(t, recorder, http.StatusServiceUnavailable, "TIMEOUT")
		if recorder.Header().Get("Retry-After") == "" {
			t.Error("503 without Retry-After")
		}
	})
}

func assertError(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var body errorBody
	if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if recorder.Code != status || body.Code != code {
		t.Fatalf("got %d %s, want %d %s", recorder.Code, body.Code, status, code)
	}
}

// markedTimeout is how the storage adapter flags a statement or lock
// timeout, the way net.Error does.
type markedTimeout struct{}

func (markedTimeout) Error() string { return "canceling statement due to lock timeout" }
func (markedTimeout) Timeout() bool { return true }
