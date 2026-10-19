package wagering

import (
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/ids"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/wager"
)

// MaxFieldLength bounds every free-text field of a submission, idempotency
// key included.
const MaxFieldLength = 255

var (
	ErrOpeningNotAllowed = errors.New("OPENING is reserved for internal wallet creation")

	// ErrInvalidInput marks a submission the caller has to fix before
	// sending it again; ErrInvalidID and ErrFieldTooLong are both one.
	ErrInvalidInput = errors.New("invalid input")
	ErrInvalidID    = fmt.Errorf("%w: walletId and playerId must be UUIDs", ErrInvalidInput)
	ErrFieldTooLong = fmt.Errorf("%w: text fields are limited to %d characters", ErrInvalidInput, MaxFieldLength)
)

// Validate judges everything a submission can be judged on without
// storage, over HTTP and SQS alike, so a request that can never succeed
// never reaches the engine.
func Validate(in SubmitInput) error {
	if in.Kind == wager.KindOpening {
		return ErrOpeningNotAllowed
	}
	if !ids.Valid(in.WalletID) || !ids.Valid(in.PlayerID) {
		return ErrInvalidID
	}
	for _, field := range []string{
		in.ProviderID, in.ExternalID, in.IdempotencyKey,
		in.RoundID, in.GameID, in.ReferenceExternalID,
	} {
		if utf8.RuneCountInString(field) > MaxFieldLength {
			return ErrFieldTooLong
		}
	}
	return wager.ValidateRequest(wager.Input{
		ProviderID:          in.ProviderID,
		ExternalID:          in.ExternalID,
		IdempotencyKey:      in.IdempotencyKey,
		WalletID:            in.WalletID,
		PlayerID:            in.PlayerID,
		RoundID:             in.RoundID,
		GameID:              in.GameID,
		Kind:                in.Kind,
		Amount:              in.Amount,
		ReferenceExternalID: in.ReferenceExternalID,
	})
}
