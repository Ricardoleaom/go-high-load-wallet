package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
)

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
