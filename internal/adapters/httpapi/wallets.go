package httpapi

import (
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/queries"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/wagering"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ids"
)

// requireWalletAccess admits the internal service and operators: wallets
// are not scoped to a provider, so providers never use these routes.
func requireWalletAccess(w http.ResponseWriter, r *http.Request) bool {
	if !isInternal(r.Context()) && !isOperator(r.Context()) {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "wallet operations need the internal service or an operator")
		return false
	}
	return true
}

type openWalletRequest struct {
	PlayerID       string   `json:"playerId"`
	InitialBalance moneyDTO `json:"initialBalance"`
}

type walletResponse struct {
	ID       string   `json:"id"`
	PlayerID string   `json:"playerId"`
	Balance  moneyDTO `json:"balance"`
	Version  int64    `json:"version"`
}

func (h *handler) openWallet(w http.ResponseWriter, r *http.Request) {
	if !requireWalletAccess(w, r) {
		return
	}

	var req openWalletRequest
	if !decodeBody(w, r, &req) {
		return
	}

	if !ids.Valid(req.PlayerID) {
		writeError(w, http.StatusBadRequest, "INVALID_PLAYER_ID", "playerId must be a UUID")
		return
	}

	initialBalance, err := req.InitialBalance.toDomain()
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_MONEY", err.Error())
		return
	}

	created, err := h.deps.Wagering.OpenWallet(r.Context(), req.PlayerID, initialBalance)
	if err != nil {
		if errors.Is(err, wagering.ErrWalletConflict) {
			writeError(w, http.StatusConflict, "WALLET_CONFLICT", "a wallet already exists for this player and currency")
			return
		}
		writeServerError(w, r, err, "could not open wallet")
		return
	}

	writeJSON(w, http.StatusCreated, walletResponse{
		ID:       created.ID(),
		PlayerID: created.PlayerID(),
		Balance:  toMoneyDTO(created.Balance()),
		Version:  created.Version(),
	})
}

func (h *handler) getWallet(w http.ResponseWriter, r *http.Request) {
	if !requireWalletAccess(w, r) {
		return
	}

	id, ok := pathID(w, r, "id")
	if !ok {
		return
	}
	wlt, err := h.deps.Reader.Wallet(r.Context(), id)
	if err != nil {
		if errors.Is(err, queries.ErrWalletNotFound) {
			writeError(w, http.StatusNotFound, "WALLET_NOT_FOUND", "wallet not found")
			return
		}
		writeServerError(w, r, err, "could not load wallet")
		return
	}

	writeJSON(w, http.StatusOK, walletResponse{
		ID:       wlt.ID(),
		PlayerID: wlt.PlayerID(),
		Balance:  toMoneyDTO(wlt.Balance()),
		Version:  wlt.Version(),
	})
}

type ledgerEntryResponse struct {
	ID            string   `json:"id"`
	TransactionID string   `json:"transactionId"`
	Direction     string   `json:"direction"`
	Amount        moneyDTO `json:"amount"`
	BalanceBefore moneyDTO `json:"balanceBefore"`
	BalanceAfter  moneyDTO `json:"balanceAfter"`
	CreatedAt     string   `json:"createdAt"`
}

type ledgerPageResponse struct {
	Entries    []ledgerEntryResponse `json:"entries"`
	NextCursor string                `json:"nextCursor,omitempty"`
}

func (h *handler) getWalletLedger(w http.ResponseWriter, r *http.Request) {
	if !requireWalletAccess(w, r) {
		return
	}

	walletID, ok := pathID(w, r, "id")
	if !ok {
		return
	}

	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > 500 {
			writeError(w, http.StatusBadRequest, "INVALID_LIMIT", "limit must be a positive integer up to 500")
			return
		}
		limit = parsed
	}

	var after queries.Cursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		var err error
		after.CreatedAt, after.ID, err = decodeLedgerCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_CURSOR", "cursor is malformed")
			return
		}
	}

	entries, err := h.deps.Reader.Ledger(r.Context(), walletID, after, limit)
	if err != nil {
		writeServerError(w, r, err, "could not load ledger")
		return
	}

	resp := ledgerPageResponse{Entries: make([]ledgerEntryResponse, 0, len(entries))}
	for _, e := range entries {
		resp.Entries = append(resp.Entries, ledgerEntryResponse{
			ID:            e.ID(),
			TransactionID: e.TransactionID(),
			Direction:     string(e.Direction()),
			Amount:        toMoneyDTO(e.Amount()),
			BalanceBefore: toMoneyDTO(e.BalanceBefore()),
			BalanceAfter:  toMoneyDTO(e.BalanceAfter()),
			CreatedAt:     e.CreatedAt().Format(time.RFC3339Nano),
		})
	}
	if len(entries) == limit {
		last := entries[len(entries)-1]
		resp.NextCursor = encodeLedgerCursor(last.CreatedAt(), last.ID())
	}

	writeJSON(w, http.StatusOK, resp)
}

func encodeLedgerCursor(createdAt time.Time, id string) string {
	raw := fmt.Sprintf("%s|%s", createdAt.Format(time.RFC3339Nano), id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeLedgerCursor(cursor string) (time.Time, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", err
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, "", errors.New("malformed cursor")
	}
	createdAt, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, "", err
	}
	if !ids.Valid(parts[1]) {
		return time.Time{}, "", errors.New("malformed cursor ID")
	}
	return createdAt, parts[1], nil
}

type reconciliationResponse struct {
	WalletID          string   `json:"walletId"`
	StoredBalance     moneyDTO `json:"storedBalance"`
	CalculatedBalance moneyDTO `json:"calculatedBalance"`
	Difference        moneyDTO `json:"difference"`
	Consistent        bool     `json:"consistent"`
	CheckedEntries    int      `json:"checkedEntries"`
}

// reconcileWallet rebuilds the balance from the ledger (including the
// opening credit) and compares it against the stored balance. It never
// writes anything.
func (h *handler) reconcileWallet(w http.ResponseWriter, r *http.Request) {
	if !requireWalletAccess(w, r) {
		return
	}

	walletID, ok := pathID(w, r, "id")
	if !ok {
		return
	}

	snapshot, err := h.deps.Reader.Reconciliation(r.Context(), walletID)
	if err != nil {
		if errors.Is(err, queries.ErrWalletNotFound) {
			writeError(w, http.StatusNotFound, "WALLET_NOT_FOUND", "wallet not found")
			return
		}
		writeServerError(w, r, err, "could not reconcile wallet")
		return
	}

	difference, err := snapshot.StoredBalance.Subtract(snapshot.CalculatedBalance)
	if err != nil {
		writeServerError(w, r, err, "could not compare balances")
		return
	}

	consistent := difference.AmountInMinorUnits() == 0
	if !consistent {
		h.deps.Metrics.ReconciliationMismatches.Inc()
		slog.Error("wallet reconciliation mismatch",
			"walletId", walletID,
			"storedBalance", snapshot.StoredBalance.String(),
			"calculatedBalance", snapshot.CalculatedBalance.String(),
		)
	}

	writeJSON(w, http.StatusOK, reconciliationResponse{
		WalletID:          walletID,
		StoredBalance:     toMoneyDTO(snapshot.StoredBalance),
		CalculatedBalance: toMoneyDTO(snapshot.CalculatedBalance),
		Difference:        toMoneyDTO(difference),
		Consistent:        consistent,
		CheckedEntries:    snapshot.CheckedEntries,
	})
}
