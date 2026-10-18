// Package wagering is the use case HTTP and SQS share: it validates a
// provider's transaction, gives it its identity and hands it to the
// engine, so both transports get the same idempotency and the same rules.
package wagering

import (
	"context"
	"errors"
	"fmt"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ids"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wallet"
)

var (
	ErrWalletNotFound         = engine.ErrWalletNotFound
	ErrWalletConflict         = engine.ErrWalletConflict
	ErrIdempotencyConflict    = engine.ErrIdempotencyConflict
	ErrIdempotencyKeyMismatch = engine.ErrIdempotencyKeyMismatch
)

type Service struct {
	engine     engine.Engine
	references ReferenceQueue

	onConcurrencyConflict func()
}

// NewService uses e for every command; references may be nil when the
// caller never retries pending reversals.
func NewService(e engine.Engine, references ReferenceQueue) *Service {
	return &Service{engine: e, references: references}
}

// OnConcurrencyConflict registers a callback invoked whenever a submission
// lost a race against a concurrent one with the same identity and was
// answered with the winner's result. Used to feed a metric.
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
	Kind                wager.Kind
	Amount              money.Money
	ReferenceExternalID string
	CorrelationID       string
}

type Result struct {
	TransactionID    string
	Status           wager.Status
	Balance          money.Money
	IdempotentReplay bool
	FailureCode      string
}

func (s *Service) Submit(ctx context.Context, in SubmitInput) (Result, error) {
	if err := Validate(in); err != nil {
		return Result{}, err
	}
	in.WalletID, in.PlayerID = ids.Canonical(in.WalletID), ids.Canonical(in.PlayerID)

	payloadHash, err := ComputePayloadHash(in)
	if err != nil {
		return Result{}, fmt.Errorf("compute payload hash: %w", err)
	}

	out, err := s.engine.Execute(ctx, engine.SubmitWager{
		ProviderID:          in.ProviderID,
		ExternalID:          in.ExternalID,
		IdempotencyKey:      in.IdempotencyKey,
		PayloadHash:         payloadHash,
		WalletID:            in.WalletID,
		PlayerID:            in.PlayerID,
		RoundID:             in.RoundID,
		GameID:              in.GameID,
		Kind:                in.Kind,
		Amount:              in.Amount,
		ReferenceExternalID: in.ReferenceExternalID,
		CorrelationID:       in.CorrelationID,
	})
	if err != nil {
		return Result{}, err
	}
	if out.Raced && s.onConcurrencyConflict != nil {
		s.onConcurrencyConflict()
	}
	return Result{
		TransactionID:    out.TransactionID,
		Status:           out.Status,
		Balance:          out.Balance,
		IdempotentReplay: out.Replay,
		FailureCode:      out.FailureCode,
	}, nil
}

// OpenWallet opens a wallet for playerID with initial as its balance.
func (s *Service) OpenWallet(ctx context.Context, playerID string, initial money.Money) (wallet.Wallet, error) {
	if !ids.Valid(playerID) {
		return wallet.Wallet{}, ErrInvalidID
	}
	out, err := s.engine.Execute(ctx, engine.OpenWallet{PlayerID: ids.Canonical(playerID), InitialBalance: initial})
	if err != nil {
		return wallet.Wallet{}, err
	}
	return out.Wallet, nil
}

// RetryPendingReferences retries the pending reversals that are due. Every
// claimed one is tried, so one that keeps failing cannot hold back the
// others; it returns how many were handled and the failures.
func (s *Service) RetryPendingReferences(ctx context.Context, limit int) (int, error) {
	claims, err := s.references.ClaimDue(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("claim pending references: %w", err)
	}

	handled := 0
	var errs []error
	for _, c := range claims {
		if _, err := s.engine.Execute(ctx, engine.RetryReference{TransactionID: c.TransactionID, Attempts: c.Attempts}); err != nil {
			errs = append(errs, fmt.Errorf("retry pending reference %s: %w", c.TransactionID, err))
			continue
		}
		handled++
	}
	return handled, errors.Join(errs...)
}

// MetricLabels bounds the labels a submission is measured under: an
// unknown kind from the caller's payload becomes OTHER, and a failed call
// is labelled "error".
func MetricLabels(kind wager.Kind, r Result, err error) (kindLabel, status string) {
	kindLabel = string(kind)
	if !kind.Known() {
		kindLabel = "OTHER"
	}
	if err != nil {
		return kindLabel, "error"
	}
	return kindLabel, string(r.Status)
}
