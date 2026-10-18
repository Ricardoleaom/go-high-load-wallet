package wager

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/application/outboxevents"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ledger"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	domainwager "github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	domainwallet "github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wallet"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/postgres"
)

var (
	ErrOpeningNotAllowed      = errors.New("OPENING is reserved for internal wallet creation")
	ErrIdempotencyConflict    = errors.New("idempotency key reused with different payload")
	ErrIdempotencyKeyMismatch = errors.New("transaction already recorded under a different idempotency key")
	ErrWalletNotFound         = postgres.ErrWalletNotFound
)

const (
	FailureInsufficientBalance         = "INSUFFICIENT_BALANCE"
	FailureRollbackInsufficientBalance = "ROLLBACK_INSUFFICIENT_BALANCE"
	FailureCurrencyMismatch            = "CURRENCY_MISMATCH"
	FailureReferenceNotProcessed       = "REFERENCE_NOT_PROCESSED"
	FailureReferenceMismatch           = "REFERENCE_MISMATCH"
	FailureReferenceKindNotReversible  = "REFERENCE_KIND_NOT_REVERSIBLE"
	FailureReferenceAlreadyReversed    = "REFERENCE_ALREADY_REVERSED"
	FailureReferenceNotFoundTimeout    = "REFERENCE_NOT_FOUND_TIMEOUT"
)

// maxReferenceAttempts bounds how many times the retry worker re-checks a
// REFUND/ROLLBACK reference before giving up and rejecting it.
const maxReferenceAttempts = 8

type Service struct {
	db           *sql.DB
	wallets      *postgres.WalletRepository
	transactions *postgres.TransactionRepository
	ledger       *postgres.LedgerRepository
	outbox       *postgres.OutboxRepository

	onConcurrencyConflict func()
}

func NewService(
	db *sql.DB,
	wallets *postgres.WalletRepository,
	transactions *postgres.TransactionRepository,
	ledgerRepo *postgres.LedgerRepository,
	outbox *postgres.OutboxRepository,
) *Service {
	return &Service{
		db:           db,
		wallets:      wallets,
		transactions: transactions,
		ledger:       ledgerRepo,
		outbox:       outbox,
	}
}

// OnConcurrencyConflict registers a callback invoked whenever two concurrent
// submissions race on the same idempotency key (see errIdempotencyRace).
// Used to feed a metric; optional, nil-safe if never called.
func (s *Service) OnConcurrencyConflict(fn func()) {
	s.onConcurrencyConflict = fn
}

type SubmitInput struct {
	ProviderID          string
	ExternalID          string
	IdempotencyKey      string
	PlayerID            string
	WalletID            string
	RoundID             string
	GameID              string
	Kind                domainwager.Kind
	Amount              money.Money
	ReferenceExternalID string
	CorrelationID       string
}

type Result struct {
	TransactionID    string
	Status           domainwager.Status
	Balance          money.Money
	IdempotentReplay bool
	FailureCode      string
}

// Submit is the single entry point HTTP and SQS both call, so the two
// transports share identical idempotency and processing guarantees.
func (s *Service) Submit(ctx context.Context, input SubmitInput) (Result, error) {
	if input.Kind == domainwager.KindOpening {
		return Result{}, ErrOpeningNotAllowed
	}

	payloadHash, err := ComputePayloadHash(input)
	if err != nil {
		return Result{}, fmt.Errorf("compute payload hash: %w", err)
	}

	existing, err := s.transactions.GetByIdempotencyKey(ctx, input.ProviderID, input.IdempotencyKey)
	if err == nil {
		return s.replay(ctx, existing, payloadHash)
	}
	if !errors.Is(err, postgres.ErrTransactionNotFound) {
		return Result{}, fmt.Errorf("check idempotency key: %w", err)
	}

	byExternal, err := s.transactions.GetByExternalID(ctx, input.ProviderID, input.ExternalID)
	if err == nil {
		if byExternal.IdempotencyKey() != input.IdempotencyKey {
			return Result{}, ErrIdempotencyKeyMismatch
		}
		return s.replay(ctx, byExternal, payloadHash)
	}
	if !errors.Is(err, postgres.ErrTransactionNotFound) {
		return Result{}, fmt.Errorf("check external ID: %w", err)
	}

	// A provider could otherwise send any walletId/playerId pair it can
	// guess or has seen leaked elsewhere, moving money on a wallet that
	// belongs to a different player than the one named in the request.
	wlt, err := s.wallets.GetByID(ctx, input.WalletID)
	if err != nil {
		if errors.Is(err, postgres.ErrWalletNotFound) {
			return Result{}, ErrWalletNotFound
		}
		return Result{}, fmt.Errorf("load wallet: %w", err)
	}
	if wlt.PlayerID() != input.PlayerID {
		return Result{}, ErrWalletNotFound
	}

	transaction, err := domainwager.New(domainwager.Input{
		ID:                  uuid.NewString(),
		ProviderID:          input.ProviderID,
		ExternalID:          input.ExternalID,
		IdempotencyKey:      input.IdempotencyKey,
		PayloadHash:         payloadHash,
		WalletID:            input.WalletID,
		PlayerID:            input.PlayerID,
		RoundID:             input.RoundID,
		GameID:              input.GameID,
		Kind:                input.Kind,
		Amount:              input.Amount,
		ReferenceExternalID: input.ReferenceExternalID,
	})
	if err != nil {
		return Result{}, err
	}

	correlationID := input.CorrelationID
	if correlationID == "" {
		correlationID = transaction.ID()
	}

	var result Result
	switch input.Kind {
	case domainwager.KindBet:
		result, err = s.processMovement(ctx, transaction, ledger.DirectionDebit, correlationID, FailureInsufficientBalance)
	case domainwager.KindWin:
		result, err = s.processMovement(ctx, transaction, ledger.DirectionCredit, correlationID, "")
	case domainwager.KindLoss:
		result, err = s.processLoss(ctx, transaction, wlt, correlationID)
	case domainwager.KindRefund, domainwager.KindRollback:
		result, err = s.processReversal(ctx, transaction, true, correlationID, 0)
	default:
		return Result{}, domainwager.ErrInvalidTransaction
	}

	// Two concurrent first-time submissions of the same idempotency key can
	// both pass the lookup above and race on the unique index; the loser
	// just replays what the winner persisted, instead of failing.
	if errors.Is(err, errIdempotencyRace) {
		if s.onConcurrencyConflict != nil {
			s.onConcurrencyConflict()
		}
		return s.resolveIdempotencyRace(ctx, input, payloadHash)
	}

	return result, err
}

// resolveIdempotencyRace answers a submission whose insert lost a race on a
// unique index to a concurrent one, the same way Submit's lookups would have
// answered it a moment later: a winner under the same key is replayed, and a
// winner with the same external ID under another key is a key mismatch.
func (s *Service) resolveIdempotencyRace(ctx context.Context, input SubmitInput, payloadHash string) (Result, error) {
	existing, err := s.transactions.GetByIdempotencyKey(ctx, input.ProviderID, input.IdempotencyKey)
	if err == nil {
		return s.replay(ctx, existing, payloadHash)
	}
	if !errors.Is(err, postgres.ErrTransactionNotFound) {
		return Result{}, fmt.Errorf("resolve idempotency race: %w", err)
	}

	byExternal, err := s.transactions.GetByExternalID(ctx, input.ProviderID, input.ExternalID)
	if err != nil {
		return Result{}, fmt.Errorf("resolve idempotency race: %w", err)
	}
	if byExternal.IdempotencyKey() != input.IdempotencyKey {
		return Result{}, ErrIdempotencyKeyMismatch
	}
	return s.replay(ctx, byExternal, payloadHash)
}

// errIdempotencyRace signals that Create lost a race against a concurrent
// submission with the same idempotency key.
var errIdempotencyRace = errors.New("idempotency key inserted concurrently")

// translateCreateErr maps a lost race on the idempotency unique index to
// errIdempotencyRace, so the caller replays the winner's result instead of
// failing the request.
func translateCreateErr(err error) error {
	if errors.Is(err, postgres.ErrTransactionConflict) {
		return errIdempotencyRace
	}
	return fmt.Errorf("create transaction: %w", err)
}

// RetryPendingReferences resolves PENDING_REFERENCE transactions whose next
// attempt is due. Meant to be called by a background worker on a timer.
// Every claimed transaction is tried, so one that keeps failing cannot hold
// back the others; it returns how many were handled and the failures.
func (s *Service) RetryPendingReferences(ctx context.Context, limit int) (int, error) {
	due, err := s.transactions.ListDuePendingReferences(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("list due pending references: %w", err)
	}

	handled := 0
	var errs []error
	for _, d := range due {
		correlationID := d.Transaction.ID()
		if _, err := s.processReversal(ctx, d.Transaction, false, correlationID, d.Attempts); err != nil {
			errs = append(errs, fmt.Errorf("retry pending reference %s: %w", d.Transaction.ID(), err))
			continue
		}
		handled++
	}

	return handled, errors.Join(errs...)
}

func (s *Service) replay(ctx context.Context, existing domainwager.Transaction, payloadHash string) (Result, error) {
	if existing.PayloadHash() != payloadHash {
		return Result{}, ErrIdempotencyConflict
	}

	result := Result{
		TransactionID:    existing.ID(),
		Status:           existing.Status(),
		FailureCode:      existing.FailureCode(),
		IdempotentReplay: true,
	}

	if existing.Status() != domainwager.StatusProcessed {
		return result, nil
	}

	entry, err := s.ledger.GetByTransactionID(ctx, existing.ID())
	switch {
	case err == nil:
		result.Balance = entry.BalanceAfter()
	case errors.Is(err, postgres.ErrLedgerEntryNotFound):
		w, werr := s.wallets.GetByID(ctx, existing.WalletID())
		if werr != nil {
			return Result{}, fmt.Errorf("load wallet for replay: %w", werr)
		}
		result.Balance = w.Balance()
	default:
		return Result{}, fmt.Errorf("load ledger entry for replay: %w", err)
	}

	return result, nil
}

// processMovement handles BET (debit) and WIN (credit): both are simple,
// single-step movements with no reference resolution.
func (s *Service) processMovement(
	ctx context.Context,
	transaction domainwager.Transaction,
	direction ledger.Direction,
	correlationID string,
	insufficientFailureCode string,
) (Result, error) {
	dbTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin %s: %w", transaction.Kind(), err)
	}
	defer dbTx.Rollback()

	if err := s.transactions.Create(ctx, dbTx, transaction); err != nil {
		return Result{}, translateCreateErr(err)
	}

	newBalanceUnits, newVersion, err := moveWallet(ctx, dbTx, transaction, direction)
	if err != nil {
		_ = dbTx.Rollback()
		switch {
		case errors.Is(err, postgres.ErrInsufficientBalance) && insufficientFailureCode != "":
			return s.rejectFresh(ctx, transaction, insufficientFailureCode, correlationID)
		case errors.Is(err, postgres.ErrWalletCurrencyMismatch):
			return s.rejectFresh(ctx, transaction, FailureCurrencyMismatch, correlationID)
		case errors.Is(err, postgres.ErrWalletNotFound):
			return Result{}, ErrWalletNotFound
		default:
			return Result{}, fmt.Errorf("move wallet: %w", err)
		}
	}

	entry, newBalance, err := buildLedgerEntry(transaction, direction, newBalanceUnits)
	if err != nil {
		return Result{}, err
	}

	if err := s.ledger.Append(ctx, dbTx, entry); err != nil {
		return Result{}, fmt.Errorf("append ledger entry: %w", err)
	}

	if err := transaction.MarkProcessed(); err != nil {
		return Result{}, err
	}
	if err := s.transactions.UpdateStatus(ctx, dbTx, transaction); err != nil {
		return Result{}, fmt.Errorf("update status: %w", err)
	}

	if err := outboxevents.AppendProcessed(ctx, dbTx, s.outbox, transaction, &entry, newVersion, correlationID); err != nil {
		return Result{}, err
	}

	if err := dbTx.Commit(); err != nil {
		return Result{}, fmt.Errorf("commit %s: %w", transaction.Kind(), err)
	}

	return Result{TransactionID: transaction.ID(), Status: transaction.Status(), Balance: newBalance}, nil
}

// processLoss handles LOSS: no wallet movement, just a processed record and
// its event.
func (s *Service) processLoss(
	ctx context.Context,
	transaction domainwager.Transaction,
	w domainwallet.Wallet,
	correlationID string,
) (Result, error) {
	if w.Balance().Currency() != transaction.Amount().Currency() {
		return s.rejectFresh(ctx, transaction, FailureCurrencyMismatch, correlationID)
	}

	dbTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin loss: %w", err)
	}
	defer dbTx.Rollback()

	if err := s.transactions.Create(ctx, dbTx, transaction); err != nil {
		return Result{}, translateCreateErr(err)
	}

	if err := transaction.MarkProcessed(); err != nil {
		return Result{}, err
	}
	if err := s.transactions.UpdateStatus(ctx, dbTx, transaction); err != nil {
		return Result{}, fmt.Errorf("update loss status: %w", err)
	}

	if err := outboxevents.AppendProcessed(ctx, dbTx, s.outbox, transaction, nil, w.Version(), correlationID); err != nil {
		return Result{}, err
	}

	if err := dbTx.Commit(); err != nil {
		return Result{}, fmt.Errorf("commit loss: %w", err)
	}

	return Result{TransactionID: transaction.ID(), Status: transaction.Status(), Balance: w.Balance()}, nil
}

func (s *Service) rejectFresh(
	ctx context.Context,
	transaction domainwager.Transaction,
	failureCode string,
	correlationID string,
) (Result, error) {
	if err := transaction.Reject(failureCode); err != nil {
		return Result{}, err
	}

	dbTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin reject: %w", err)
	}
	defer dbTx.Rollback()

	if err := s.transactions.Create(ctx, dbTx, transaction); err != nil {
		return Result{}, translateCreateErr(err)
	}

	if err := outboxevents.AppendRejected(ctx, dbTx, s.outbox, transaction, correlationID); err != nil {
		return Result{}, err
	}

	if err := dbTx.Commit(); err != nil {
		return Result{}, fmt.Errorf("commit reject: %w", err)
	}

	return Result{TransactionID: transaction.ID(), Status: transaction.Status(), FailureCode: failureCode}, nil
}

func moveWallet(
	ctx context.Context,
	dbTx *sql.Tx,
	transaction domainwager.Transaction,
	direction ledger.Direction,
) (newBalanceUnits int64, newVersion int64, err error) {
	if direction == ledger.DirectionDebit {
		return postgres.DebitWallet(
			ctx, dbTx, transaction.WalletID(),
			transaction.Amount().AmountInMinorUnits(), transaction.Amount().Currency(),
		)
	}
	return postgres.CreditWallet(
		ctx, dbTx, transaction.WalletID(),
		transaction.Amount().AmountInMinorUnits(), transaction.Amount().Currency(),
	)
}

func buildLedgerEntry(
	transaction domainwager.Transaction,
	direction ledger.Direction,
	newBalanceUnits int64,
) (ledger.Entry, money.Money, error) {
	newBalance, err := money.New(minorUnitsToDecimal(newBalanceUnits), transaction.Amount().Currency())
	if err != nil {
		return ledger.Entry{}, money.Money{}, err
	}

	balanceBefore, err := reverseMove(newBalance, transaction.Amount(), direction)
	if err != nil {
		return ledger.Entry{}, money.Money{}, err
	}

	entry, err := ledger.New(
		uuid.NewString(), transaction.WalletID(), transaction.ID(),
		direction, transaction.Amount(), balanceBefore, newBalance,
		transaction.UpdatedAt(),
	)
	if err != nil {
		return ledger.Entry{}, money.Money{}, err
	}

	return entry, newBalance, nil
}

// reverseMove derives the balance before a movement from the balance after
// it, since the atomic wallet UPDATE only returns the resulting balance.
func reverseMove(after money.Money, amount money.Money, direction ledger.Direction) (money.Money, error) {
	if direction == ledger.DirectionDebit {
		return after.Add(amount)
	}
	return after.Subtract(amount)
}

func minorUnitsToDecimal(units int64) string {
	return fmt.Sprintf("%d.%02d", units/100, units%100)
}

func referenceBackoff(attempt int) time.Duration {
	const base = 5 * time.Second
	const maxBackoff = 5 * time.Minute
	const maxShift = 20 // base<<20 already dwarfs maxBackoff; caps the shift count

	if attempt <= 0 {
		return base
	}

	shift := attempt - 1
	if shift > maxShift {
		shift = maxShift
	}

	d := base << uint(shift)
	if d <= 0 || d > maxBackoff {
		return maxBackoff
	}
	return d
}

// processReversal handles REFUND and ROLLBACK. isNew distinguishes a fresh
// submission (HTTP/SQS, transaction not persisted yet) from a retry by the
// pending-reference worker (transaction already persisted as
// PENDING_REFERENCE).
func (s *Service) processReversal(
	ctx context.Context,
	transaction domainwager.Transaction,
	isNew bool,
	correlationID string,
	attempts int,
) (Result, error) {
	original, err := s.transactions.GetByExternalID(ctx, transaction.ProviderID(), transaction.ReferenceExternalID())
	if errors.Is(err, postgres.ErrTransactionNotFound) {
		return s.deferReversal(ctx, transaction, isNew, correlationID, attempts)
	}
	if err != nil {
		return Result{}, fmt.Errorf("resolve reference: %w", err)
	}

	if original.Status() == domainwager.StatusPending || original.Status() == domainwager.StatusPendingReference {
		return s.deferReversal(ctx, transaction, isNew, correlationID, attempts)
	}

	if original.Status() != domainwager.StatusProcessed {
		return s.rejectReversal(ctx, transaction, isNew, correlationID, FailureReferenceNotProcessed)
	}

	if !referenceMatches(original, transaction) {
		return s.rejectReversal(ctx, transaction, isNew, correlationID, FailureReferenceMismatch)
	}

	if !reversibleKind(transaction.Kind(), original.Kind()) {
		return s.rejectReversal(ctx, transaction, isNew, correlationID, FailureReferenceKindNotReversible)
	}

	return s.applyReversal(ctx, transaction, isNew, correlationID, original)
}

func referenceMatches(original, reversal domainwager.Transaction) bool {
	if original.ProviderID() != reversal.ProviderID() ||
		original.PlayerID() != reversal.PlayerID() ||
		original.WalletID() != reversal.WalletID() ||
		original.RoundID() != reversal.RoundID() ||
		original.Amount().Currency() != reversal.Amount().Currency() {
		return false
	}

	cmp, err := original.Amount().Compare(reversal.Amount())
	return err == nil && cmp == 0
}

func reversibleKind(reversalKind, originalKind domainwager.Kind) bool {
	switch reversalKind {
	case domainwager.KindRefund:
		return originalKind == domainwager.KindBet
	case domainwager.KindRollback:
		return originalKind == domainwager.KindBet ||
			originalKind == domainwager.KindWin ||
			originalKind == domainwager.KindRefund
	default:
		return false
	}
}

// reversalDirection reports which way money moves to undo the original
// transaction: REFUND always credits back a BET; ROLLBACK undoes whatever
// the original movement was.
func reversalDirection(reversalKind, originalKind domainwager.Kind) ledger.Direction {
	if reversalKind == domainwager.KindRefund {
		return ledger.DirectionCredit
	}
	if originalKind == domainwager.KindBet {
		return ledger.DirectionCredit
	}
	return ledger.DirectionDebit
}

// deferReversal persists (or reschedules) a REFUND/ROLLBACK whose reference
// is still unresolved, giving up after maxReferenceAttempts.
func (s *Service) deferReversal(
	ctx context.Context,
	transaction domainwager.Transaction,
	isNew bool,
	correlationID string,
	attempts int,
) (Result, error) {
	if attempts >= maxReferenceAttempts {
		return s.rejectReversal(ctx, transaction, isNew, correlationID, FailureReferenceNotFoundTimeout)
	}

	nextAttempt := attempts + 1
	nextAttemptAt := time.Now().UTC().Add(referenceBackoff(nextAttempt))

	dbTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin defer reversal: %w", err)
	}
	defer dbTx.Rollback()

	if isNew {
		if err := transaction.MarkPendingReference(); err != nil {
			return Result{}, err
		}
		if err := s.transactions.Create(ctx, dbTx, transaction); err != nil {
			return Result{}, translateCreateErr(err)
		}
		if err := outboxevents.AppendPendingReference(ctx, dbTx, s.outbox, transaction, correlationID); err != nil {
			return Result{}, err
		}
	}

	if err := s.transactions.ScheduleReferenceRetry(ctx, dbTx, transaction.ID(), nextAttempt, nextAttemptAt); err != nil {
		return Result{}, err
	}

	if err := dbTx.Commit(); err != nil {
		return Result{}, fmt.Errorf("commit defer reversal: %w", err)
	}

	return Result{TransactionID: transaction.ID(), Status: domainwager.StatusPendingReference}, nil
}

func (s *Service) rejectReversal(
	ctx context.Context,
	transaction domainwager.Transaction,
	isNew bool,
	correlationID string,
	failureCode string,
) (Result, error) {
	if err := transaction.Reject(failureCode); err != nil {
		return Result{}, err
	}

	dbTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin reject reversal: %w", err)
	}
	defer dbTx.Rollback()

	if isNew {
		if err := s.transactions.Create(ctx, dbTx, transaction); err != nil {
			return Result{}, translateCreateErr(err)
		}
	} else if err := s.transactions.UpdateStatus(ctx, dbTx, transaction); err != nil {
		return Result{}, fmt.Errorf("update rejected reversal: %w", err)
	}

	if err := outboxevents.AppendRejected(ctx, dbTx, s.outbox, transaction, correlationID); err != nil {
		return Result{}, err
	}

	if err := dbTx.Commit(); err != nil {
		return Result{}, fmt.Errorf("commit reject reversal: %w", err)
	}

	return Result{TransactionID: transaction.ID(), Status: transaction.Status(), FailureCode: failureCode}, nil
}

// applyReversal performs the actual wallet movement once the reference has
// resolved to a PROCESSED, matching, reversible original transaction.
func (s *Service) applyReversal(
	ctx context.Context,
	transaction domainwager.Transaction,
	isNew bool,
	correlationID string,
	original domainwager.Transaction,
) (Result, error) {
	dbTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin apply reversal: %w", err)
	}
	defer dbTx.Rollback()

	if _, err := s.transactions.LockByID(ctx, dbTx, original.ID()); err != nil {
		return Result{}, fmt.Errorf("lock reference: %w", err)
	}

	alreadyReversed, err := s.transactions.HasSuccessfulReversal(ctx, dbTx, transaction.ProviderID(), original.ID())
	if err != nil {
		return Result{}, err
	}
	if alreadyReversed {
		_ = dbTx.Rollback()
		return s.rejectReversal(ctx, transaction, isNew, correlationID, FailureReferenceAlreadyReversed)
	}

	if err := transaction.ResolveReference(original.ID()); err != nil {
		return Result{}, err
	}

	if isNew {
		if err := s.transactions.Create(ctx, dbTx, transaction); err != nil {
			return Result{}, translateCreateErr(err)
		}
	} else if err := s.transactions.UpdateStatus(ctx, dbTx, transaction); err != nil {
		return Result{}, fmt.Errorf("persist resolved reference: %w", err)
	}

	direction := reversalDirection(transaction.Kind(), original.Kind())
	insufficientFailureCode := ""
	if direction == ledger.DirectionDebit {
		insufficientFailureCode = FailureRollbackInsufficientBalance
	}

	newBalanceUnits, newVersion, err := moveWallet(ctx, dbTx, transaction, direction)
	if err != nil {
		_ = dbTx.Rollback()
		if errors.Is(err, postgres.ErrInsufficientBalance) && insufficientFailureCode != "" {
			return s.rejectReversal(ctx, transaction, isNew, correlationID, insufficientFailureCode)
		}
		return Result{}, fmt.Errorf("move wallet for reversal: %w", err)
	}

	entry, newBalance, err := buildLedgerEntry(transaction, direction, newBalanceUnits)
	if err != nil {
		return Result{}, err
	}

	if err := s.ledger.Append(ctx, dbTx, entry); err != nil {
		return Result{}, fmt.Errorf("append reversal ledger entry: %w", err)
	}

	if err := transaction.MarkProcessed(); err != nil {
		return Result{}, err
	}
	if err := s.transactions.UpdateStatus(ctx, dbTx, transaction); err != nil {
		if errors.Is(err, postgres.ErrTransactionConflict) {
			_ = dbTx.Rollback()
			return s.rejectReversal(ctx, transaction, isNew, correlationID, FailureReferenceAlreadyReversed)
		}
		return Result{}, fmt.Errorf("update reversal status: %w", err)
	}

	if err := outboxevents.AppendProcessed(ctx, dbTx, s.outbox, transaction, &entry, newVersion, correlationID); err != nil {
		return Result{}, err
	}

	if err := dbTx.Commit(); err != nil {
		return Result{}, fmt.Errorf("commit reversal: %w", err)
	}

	return Result{TransactionID: transaction.ID(), Status: transaction.Status(), Balance: newBalance}, nil
}
