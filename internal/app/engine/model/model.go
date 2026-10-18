// Package model is the reference engine: the business rules applied to
// in-memory state under one lock, built from the domain types alone. It is
// slow and forgets everything on restart; its job is to be obviously right,
// so the contract and property tests can hold faster engines to it.
package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/events"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ledger"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wallet"
)

type Engine struct {
	mu sync.Mutex

	wallets       map[string]wallet.Wallet
	walletByOwner map[string]string // playerID|currency -> wallet ID

	transactions map[string]wager.Transaction
	byKey        map[string]string // provider\x00key -> transaction ID
	byExternal   map[string]string // provider\x00externalID -> transaction ID
	reversedBy   map[string]string // original ID -> its PROCESSED reversal

	entries    map[string]ledger.Entry // transaction ID -> its entry
	ledgerSums map[string]int64        // wallet ID -> sum of signed deltas

	events []recorded
}

type recorded struct {
	transactionID string
	envelope      events.Envelope
}

func New() *Engine {
	return &Engine{
		wallets:       map[string]wallet.Wallet{},
		walletByOwner: map[string]string{},
		transactions:  map[string]wager.Transaction{},
		byKey:         map[string]string{},
		byExternal:    map[string]string{},
		reversedBy:    map[string]string{},
		entries:       map[string]ledger.Entry{},
		ledgerSums:    map[string]int64{},
	}
}

func (e *Engine) Execute(ctx context.Context, cmd engine.Command) (engine.Outcome, error) {
	if err := ctx.Err(); err != nil {
		return engine.Outcome{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	// Every command builds what it writes in a change first and applies it
	// only once nothing can fail, so a failed command leaves no trace.
	c := &change{}
	var (
		out engine.Outcome
		err error
	)
	switch cmd := cmd.(type) {
	case engine.OpenWallet:
		out, err = e.openWallet(c, cmd)
	case engine.SubmitWager:
		out, err = e.submit(c, cmd)
	case engine.RetryReference:
		out, err = e.retry(c, cmd)
	default:
		return engine.Outcome{}, fmt.Errorf("%w: %T", engine.ErrUnknownCommand, cmd)
	}
	if err != nil {
		return engine.Outcome{}, err
	}
	e.apply(c)
	return out, nil
}

// change is everything one command writes.
type change struct {
	wallet      *wallet.Wallet
	transaction *wager.Transaction
	entry       *ledger.Entry
	reverses    string // original the transaction reverses
	events      []recorded
}

func (e *Engine) apply(c *change) {
	if w := c.wallet; w != nil {
		if _, exists := e.wallets[w.ID()]; !exists {
			e.walletByOwner[owner(w.PlayerID(), w.Balance().Currency())] = w.ID()
		}
		e.wallets[w.ID()] = *w
	}
	if tx := c.transaction; tx != nil {
		e.transactions[tx.ID()] = *tx
		if tx.Kind() != wager.KindOpening {
			e.byKey[pair(tx.ProviderID(), tx.IdempotencyKey())] = tx.ID()
			e.byExternal[pair(tx.ProviderID(), tx.ExternalID())] = tx.ID()
		}
	}
	if entry := c.entry; entry != nil {
		e.entries[entry.TransactionID()] = *entry
		delta := entry.Amount().AmountInMinorUnits()
		if entry.Direction() == ledger.DirectionDebit {
			delta = -delta
		}
		e.ledgerSums[entry.WalletID()] += delta
	}
	if c.reverses != "" {
		e.reversedBy[c.reverses] = c.transaction.ID()
	}
	e.events = append(e.events, c.events...)
}

func (e *Engine) openWallet(c *change, cmd engine.OpenWallet) (engine.Outcome, error) {
	initial := cmd.InitialBalance
	if _, taken := e.walletByOwner[owner(cmd.PlayerID, initial.Currency())]; taken {
		return engine.Outcome{}, engine.ErrWalletConflict
	}
	w, err := wallet.New(newID(), cmd.PlayerID, initial)
	if err != nil {
		return engine.Outcome{}, err
	}
	c.wallet = &w
	out := engine.Outcome{Wallet: w}
	if initial.AmountInMinorUnits() == 0 {
		return out, nil
	}

	opening, err := wager.New(wager.Input{
		ID: newID(), WalletID: w.ID(), PlayerID: w.PlayerID(), Kind: wager.KindOpening, Amount: initial,
	})
	if err != nil {
		return engine.Outcome{}, err
	}
	zero, err := money.Zero(initial.Currency())
	if err != nil {
		return engine.Outcome{}, err
	}
	if err := e.process(c, &opening, w, w, ledger.DirectionCredit, zero, opening.ID()); err != nil {
		return engine.Outcome{}, err
	}
	out.TransactionID, out.Status, out.Balance = opening.ID(), opening.Status(), initial
	return out, nil
}

func (e *Engine) submit(c *change, cmd engine.SubmitWager) (engine.Outcome, error) {
	if err := wager.ValidateRequest(request(cmd)); err != nil {
		return engine.Outcome{}, err
	}
	if id, ok := e.byKey[pair(cmd.ProviderID, cmd.IdempotencyKey)]; ok {
		existing := e.transactions[id]
		if existing.PayloadHash() != cmd.PayloadHash {
			return engine.Outcome{}, engine.ErrIdempotencyConflict
		}
		return e.replay(existing), nil
	}
	if _, ok := e.byExternal[pair(cmd.ProviderID, cmd.ExternalID)]; ok {
		return engine.Outcome{}, engine.ErrIdempotencyKeyMismatch
	}
	w, ok := e.wallets[cmd.WalletID]
	if !ok || w.PlayerID() != cmd.PlayerID {
		return engine.Outcome{}, engine.ErrWalletNotFound
	}

	tx, err := wager.New(wager.Input{
		ID:                  newID(),
		ProviderID:          cmd.ProviderID,
		ExternalID:          cmd.ExternalID,
		IdempotencyKey:      cmd.IdempotencyKey,
		PayloadHash:         cmd.PayloadHash,
		WalletID:            cmd.WalletID,
		PlayerID:            cmd.PlayerID,
		RoundID:             cmd.RoundID,
		GameID:              cmd.GameID,
		Kind:                cmd.Kind,
		Amount:              cmd.Amount,
		ReferenceExternalID: cmd.ReferenceExternalID,
	})
	if err != nil {
		return engine.Outcome{}, err
	}
	correlationID := cmd.CorrelationID
	if correlationID == "" {
		correlationID = tx.ID()
	}

	switch cmd.Kind {
	case wager.KindBet, wager.KindWin:
		if cmd.Amount.Currency() != w.Balance().Currency() {
			return e.reject(c, &tx, wager.FailureCurrencyMismatch, correlationID)
		}
		direction := ledger.DirectionCredit
		if cmd.Kind == wager.KindBet {
			direction = ledger.DirectionDebit
		}
		return e.move(c, &tx, w, direction, wager.FailureInsufficientBalance, correlationID)
	case wager.KindLoss:
		if cmd.Amount.Currency() != w.Balance().Currency() {
			return e.reject(c, &tx, wager.FailureCurrencyMismatch, correlationID)
		}
		if err := e.process(c, &tx, w, w, "", money.Money{}, correlationID); err != nil {
			return engine.Outcome{}, err
		}
		return engine.Outcome{TransactionID: tx.ID(), Status: tx.Status(), Balance: w.Balance()}, nil
	default:
		return e.settleReversal(c, &tx, true, 0, correlationID)
	}
}

// request is the part of a submission the domain validates.
func request(cmd engine.SubmitWager) wager.Input {
	return wager.Input{
		ProviderID:          cmd.ProviderID,
		ExternalID:          cmd.ExternalID,
		IdempotencyKey:      cmd.IdempotencyKey,
		WalletID:            cmd.WalletID,
		PlayerID:            cmd.PlayerID,
		RoundID:             cmd.RoundID,
		GameID:              cmd.GameID,
		Kind:                cmd.Kind,
		Amount:              cmd.Amount,
		ReferenceExternalID: cmd.ReferenceExternalID,
	}
}

func (e *Engine) retry(c *change, cmd engine.RetryReference) (engine.Outcome, error) {
	tx, ok := e.transactions[cmd.TransactionID]
	if !ok {
		return engine.Outcome{}, engine.ErrTransactionNotFound
	}
	if tx.Status() != wager.StatusPendingReference {
		return e.replay(tx), nil
	}
	return e.settleReversal(c, &tx, false, cmd.Attempts, tx.ID())
}

// settleReversal decides a REFUND or ROLLBACK: a fresh one (isNew) or one
// already waiting for its original after attempts tries.
func (e *Engine) settleReversal(c *change, tx *wager.Transaction, isNew bool, attempts int, correlationID string) (engine.Outcome, error) {
	originalID, found := e.byExternal[pair(tx.ProviderID(), tx.ReferenceExternalID())]
	original := e.transactions[originalID]

	decision, failureCode := wager.DecideReversal(original, found, *tx)
	switch decision {
	case wager.ReversalWait:
		if !isNew && attempts >= wager.MaxReferenceAttempts {
			return e.reject(c, tx, wager.FailureReferenceNotFoundTimeout, correlationID)
		}
		if isNew {
			if err := tx.MarkPendingReference(); err != nil {
				return engine.Outcome{}, err
			}
			c.transaction = tx
			if err := c.emit(tx.ID(), events.TypeWagerTransactionPendingReference, tx.ID(), correlationID, "",
				events.WagerTransactionPendingReferenceData{
					TransactionID:                  tx.ID(),
					ReferenceExternalTransactionID: tx.ReferenceExternalID(),
				}); err != nil {
				return engine.Outcome{}, err
			}
		}
		return engine.Outcome{TransactionID: tx.ID(), Status: wager.StatusPendingReference}, nil
	case wager.ReversalReject:
		if err := tx.ResolveReference(original.ID()); err != nil {
			return engine.Outcome{}, err
		}
		return e.reject(c, tx, failureCode, correlationID)
	}

	if err := tx.ResolveReference(original.ID()); err != nil {
		return engine.Outcome{}, err
	}
	if _, reversed := e.reversedBy[original.ID()]; reversed {
		return e.reject(c, tx, wager.FailureReferenceAlreadyReversed, correlationID)
	}
	c.reverses = original.ID()
	return e.move(c, tx, e.wallets[tx.WalletID()], wager.ReversalDirection(tx.Kind(), original.Kind()),
		wager.FailureRollbackInsufficientBalance, correlationID)
}

// move applies tx's amount to w in direction, rejecting it with
// insufficientCode when a debit is not covered.
func (e *Engine) move(c *change, tx *wager.Transaction, w wallet.Wallet, direction ledger.Direction,
	insufficientCode, correlationID string) (engine.Outcome, error) {
	moved := w
	var err error
	if direction == ledger.DirectionDebit {
		err = moved.Debit(tx.Amount())
	} else {
		err = moved.Credit(tx.Amount())
	}
	if errors.Is(err, wallet.ErrInsufficientBalance) {
		c.reverses = ""
		return e.reject(c, tx, insufficientCode, correlationID)
	}
	if err != nil {
		return engine.Outcome{}, fmt.Errorf("move wallet %s: %w", w.ID(), err)
	}
	if err := e.process(c, tx, w, moved, direction, w.Balance(), correlationID); err != nil {
		return engine.Outcome{}, err
	}
	return engine.Outcome{TransactionID: tx.ID(), Status: tx.Status(), Balance: moved.Balance()}, nil
}

// process marks tx PROCESSED with its WagerTransactionProcessed event and,
// when direction is set, the ledger entry from before to after and the
// WalletBalanceChanged event.
func (e *Engine) process(c *change, tx *wager.Transaction, before, after wallet.Wallet,
	direction ledger.Direction, balanceBefore money.Money, correlationID string) error {
	if err := tx.MarkProcessed(); err != nil {
		return err
	}
	c.transaction = tx
	processedID, err := c.emitID(tx.ID(), events.TypeWagerTransactionProcessed, tx.ID(), correlationID, "",
		events.WagerTransactionProcessedData{
			TransactionID: tx.ID(),
			WalletID:      tx.WalletID(),
			Kind:          string(tx.Kind()),
			Money:         events.DTO(tx.Amount()),
		})
	if err != nil || direction == "" {
		return err
	}

	entry, err := ledger.New(newID(), before.ID(), tx.ID(), direction, tx.Amount(), balanceBefore, after.Balance(), time.Now().UTC())
	if err != nil {
		return err
	}
	c.entry = &entry
	c.wallet = &after
	return c.emit(tx.ID(), events.TypeWalletBalanceChanged, after.ID(), correlationID, processedID,
		events.WalletBalanceChangedData{
			WalletID:      after.ID(),
			TransactionID: tx.ID(),
			Direction:     string(direction),
			Money:         events.DTO(tx.Amount()),
			BalanceBefore: events.DTO(balanceBefore),
			BalanceAfter:  events.DTO(after.Balance()),
			WalletVersion: after.Version(),
		})
}

func (e *Engine) reject(c *change, tx *wager.Transaction, failureCode, correlationID string) (engine.Outcome, error) {
	if err := tx.Reject(failureCode); err != nil {
		return engine.Outcome{}, err
	}
	c.transaction = tx
	if err := c.emit(tx.ID(), events.TypeWagerTransactionRejected, tx.ID(), correlationID, "",
		events.WagerTransactionRejectedData{TransactionID: tx.ID(), FailureCode: failureCode}); err != nil {
		return engine.Outcome{}, err
	}
	return engine.Outcome{TransactionID: tx.ID(), Status: tx.Status(), FailureCode: failureCode}, nil
}

// replay answers what tx's first application answered.
func (e *Engine) replay(tx wager.Transaction) engine.Outcome {
	out := engine.Outcome{TransactionID: tx.ID(), Status: tx.Status(), FailureCode: tx.FailureCode(), Replay: true}
	if tx.Status() == wager.StatusProcessed {
		if entry, moved := e.entries[tx.ID()]; moved {
			out.Balance = entry.BalanceAfter()
		} else {
			out.Balance = e.wallets[tx.WalletID()].Balance()
		}
	}
	return out
}

func (c *change) emit(transactionID, eventType, aggregateID, correlationID, causationID string, data any) error {
	_, err := c.emitID(transactionID, eventType, aggregateID, correlationID, causationID, data)
	return err
}

func (c *change) emitID(transactionID, eventType, aggregateID, correlationID, causationID string, data any) (string, error) {
	id, payload, _, err := events.Build(eventType, aggregateID, correlationID, causationID, data)
	if err != nil {
		return "", err
	}
	var envelope events.Envelope
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", fmt.Errorf("read back %s: %w", eventType, err)
	}
	c.events = append(c.events, recorded{transactionID: transactionID, envelope: envelope})
	return id, nil
}

// Events returns the events about a transaction, oldest first.
func (e *Engine) Events(transactionID string) []events.Envelope {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []events.Envelope
	for _, r := range e.events {
		if r.transactionID == transactionID {
			out = append(out, r.envelope)
		}
	}
	return out
}

func (e *Engine) Transaction(id string) (wager.Transaction, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	tx, ok := e.transactions[id]
	return tx, ok
}

func (e *Engine) WalletBalance(id string) (money.Money, int64, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	w, ok := e.wallets[id]
	return w.Balance(), w.Version(), ok
}

// LedgerSum adds up a wallet's signed ledger deltas, opening included.
func (e *Engine) LedgerSum(walletID string) int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ledgerSums[walletID]
}

func (e *Engine) LedgerCount(transactionID string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.entries[transactionID]; ok {
		return 1
	}
	return 0
}

func newID() string { return uuid.Must(uuid.NewV7()).String() }

func pair(provider, id string) string { return provider + "\x00" + id }

func owner(playerID, currency string) string { return playerID + "|" + currency }
