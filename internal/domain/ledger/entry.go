package ledger

import (
	"errors"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
)

var (
	ErrInvalidEntry    = errors.New("invalid ledger entry")
	ErrBalanceMismatch = errors.New("ledger entry balance does not match direction and amount")
)

type Direction string

const (
	DirectionDebit  Direction = "DEBIT"
	DirectionCredit Direction = "CREDIT"
)

// Entry is an immutable, append-only ledger record. Construction validates
// balanceAfter = balanceBefore ± amount, according to direction.
type Entry struct {
	id            string
	walletID      string
	transactionID string
	direction     Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	createdAt     time.Time
}

func New(
	id, walletID, transactionID string,
	direction Direction,
	amount, balanceBefore, balanceAfter money.Money,
	createdAt time.Time,
) (Entry, error) {
	if id == "" || walletID == "" || transactionID == "" || createdAt.IsZero() {
		return Entry{}, ErrInvalidEntry
	}

	if direction != DirectionDebit && direction != DirectionCredit {
		return Entry{}, ErrInvalidEntry
	}

	if amount.AmountInMinorUnits() <= 0 {
		return Entry{}, ErrInvalidEntry
	}

	if balanceBefore.AmountInMinorUnits() < 0 || balanceAfter.AmountInMinorUnits() < 0 {
		return Entry{}, ErrInvalidEntry
	}

	var expected money.Money
	var err error
	switch direction {
	case DirectionDebit:
		expected, err = balanceBefore.Subtract(amount)
	case DirectionCredit:
		expected, err = balanceBefore.Add(amount)
	}
	if err != nil {
		return Entry{}, err
	}

	cmp, err := expected.Compare(balanceAfter)
	if err != nil {
		return Entry{}, err
	}
	if cmp != 0 {
		return Entry{}, ErrBalanceMismatch
	}

	return Entry{
		id:            id,
		walletID:      walletID,
		transactionID: transactionID,
		direction:     direction,
		amount:        amount,
		balanceBefore: balanceBefore,
		balanceAfter:  balanceAfter,
		createdAt:     createdAt,
	}, nil
}

func (e Entry) ID() string                 { return e.id }
func (e Entry) WalletID() string           { return e.walletID }
func (e Entry) TransactionID() string      { return e.transactionID }
func (e Entry) Direction() Direction       { return e.direction }
func (e Entry) Amount() money.Money        { return e.amount }
func (e Entry) BalanceBefore() money.Money { return e.balanceBefore }
func (e Entry) BalanceAfter() money.Money  { return e.balanceAfter }
func (e Entry) CreatedAt() time.Time       { return e.createdAt }
