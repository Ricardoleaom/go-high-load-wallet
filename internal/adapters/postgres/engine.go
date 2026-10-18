package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wallet"
)

// Engine runs each command as one call to a function of migration 13: one
// round trip, one commit, every lock taken and released inside it.
type Engine struct {
	pool *pgxpool.Pool
}

func NewEngine(pool *pgxpool.Pool) *Engine {
	return &Engine{pool: pool}
}

func (e *Engine) Execute(ctx context.Context, cmd engine.Command) (engine.Outcome, error) {
	var (
		out engine.Outcome
		err error
	)
	switch cmd := cmd.(type) {
	case engine.OpenWallet:
		out, err = e.openWallet(ctx, cmd)
	case engine.SubmitWager:
		out, err = e.submit(ctx, cmd)
	case engine.RetryReference:
		out, err = e.retry(ctx, cmd)
	default:
		err = fmt.Errorf("%w: %T", engine.ErrUnknownCommand, cmd)
	}
	return out, markTimeout(err)
}

func (e *Engine) openWallet(ctx context.Context, cmd engine.OpenWallet) (engine.Outcome, error) {
	walletID, transactionID, entryID, eventID, balanceEventID := newID(), newID(), newID(), newID(), newID()
	initial := cmd.InitialBalance

	var createdAt *time.Time
	if err := e.pool.QueryRow(ctx, `SELECT wallet_open($1, $2, $3, $4, $5, $6, $7, $8)`,
		walletID, cmd.PlayerID, initial.Currency(), initial.AmountInMinorUnits(),
		transactionID, entryID, eventID, balanceEventID,
	).Scan(&createdAt); err != nil {
		return engine.Outcome{}, fmt.Errorf("open wallet: %w", err)
	}
	if createdAt == nil {
		return engine.Outcome{}, engine.ErrWalletConflict
	}

	w, err := wallet.Rehydrate(walletID, cmd.PlayerID, initial, 1, *createdAt, *createdAt)
	if err != nil {
		return engine.Outcome{}, fmt.Errorf("open wallet: %w", err)
	}
	out := engine.Outcome{Wallet: w}
	if initial.AmountInMinorUnits() > 0 {
		out.TransactionID, out.Status, out.Balance = transactionID, wager.StatusProcessed, initial
	}
	return out, nil
}

const submitQuery = `
	SELECT outcome, transaction_id, status, failure_code, balance, currency, raced
	FROM wager_submit($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18)`

func (e *Engine) submit(ctx context.Context, cmd engine.SubmitWager) (engine.Outcome, error) {
	var r result
	if err := r.scan(e.pool.QueryRow(ctx, submitQuery,
		newID(), newID(), newID(), newID(),
		cmd.ProviderID, cmd.ExternalID, cmd.IdempotencyKey, cmd.PayloadHash,
		cmd.WalletID, cmd.PlayerID, cmd.RoundID, cmd.GameID,
		string(cmd.Kind), cmd.Amount.AmountInMinorUnits(), cmd.Amount.Currency(), cmd.ReferenceExternalID,
		cmd.CorrelationID, wager.ReferenceBackoff(1),
	)); err != nil {
		return engine.Outcome{}, fmt.Errorf("submit %s: %w", cmd.Kind, err)
	}
	return r.outcome()
}

const retryQuery = `
	SELECT outcome, transaction_id, status, failure_code, balance, currency, raced
	FROM wager_retry_reference($1, $2, $3, $4, $5, $6)`

func (e *Engine) retry(ctx context.Context, cmd engine.RetryReference) (engine.Outcome, error) {
	var r result
	if err := r.scan(e.pool.QueryRow(ctx, retryQuery,
		cmd.TransactionID, newID(), newID(), newID(),
		cmd.Attempts >= wager.MaxReferenceAttempts, wager.ReferenceBackoff(cmd.Attempts+1),
	)); err != nil {
		return engine.Outcome{}, fmt.Errorf("retry reference %s: %w", cmd.TransactionID, err)
	}
	return r.outcome()
}

// result is a row of the wager_result type the functions return.
type result struct {
	kind          string
	transactionID *string
	status        *string
	failureCode   *string
	balance       *int64
	currency      *string
	raced         *bool
}

type scanner interface{ Scan(dest ...any) error }

func (r *result) scan(row scanner) error {
	return row.Scan(&r.kind, &r.transactionID, &r.status, &r.failureCode, &r.balance, &r.currency, &r.raced)
}

func (r *result) outcome() (engine.Outcome, error) {
	switch r.kind {
	case "APPLIED", "REPLAY":
	case "IDEMPOTENCY_CONFLICT":
		return engine.Outcome{}, engine.ErrIdempotencyConflict
	case "KEY_MISMATCH":
		return engine.Outcome{}, engine.ErrIdempotencyKeyMismatch
	case "WALLET_NOT_FOUND":
		return engine.Outcome{}, engine.ErrWalletNotFound
	case "NOT_FOUND":
		return engine.Outcome{}, engine.ErrTransactionNotFound
	default:
		return engine.Outcome{}, fmt.Errorf("unexpected engine outcome %q", r.kind)
	}

	out := engine.Outcome{
		TransactionID: deref(r.transactionID),
		Status:        wager.Status(deref(r.status)),
		FailureCode:   deref(r.failureCode),
		Replay:        r.kind == "REPLAY",
		Raced:         r.raced != nil && *r.raced,
	}
	if r.balance != nil {
		balance, err := fromMinorUnits(*r.balance, deref(r.currency))
		if err != nil {
			return engine.Outcome{}, err
		}
		out.Balance = balance
	}
	return out, nil
}

// fromMinorUnits rebuilds the Money a stored, non-negative amount stands
// for.
func fromMinorUnits(units int64, currency string) (money.Money, error) {
	if units < 0 {
		return money.Money{}, fmt.Errorf("negative stored amount %d", units)
	}
	m, err := money.New(fmt.Sprintf("%d.%02d", units/100, units%100), currency)
	if err != nil {
		return money.Money{}, fmt.Errorf("rebuild amount %d %s: %w", units, currency, err)
	}
	return m, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// newID returns a time-ordered UUID, so new rows land at the right edge of
// their primary key index instead of on a random page.
func newID() string { return uuid.Must(uuid.NewV7()).String() }
