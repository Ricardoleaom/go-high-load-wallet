package postgres_test

import (
	"fmt"
	"math"
	"testing"

	"pgregory.net/rapid"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/domain/money"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// Events built in SQL render amounts with wallet_money_text and events
// built in Go with money.DecimalString: the two must never disagree.
func TestMoneyTextMatchesDecimalString(t *testing.T) {
	pool := openPool(t, pgtest.URL(t))
	ctx := testContext(t)

	sqlText := func(t interface{ Fatalf(string, ...any) }, units int64) string {
		var got string
		if err := pool.QueryRow(ctx, `SELECT wallet_money_text($1)`, units).Scan(&got); err != nil {
			t.Fatalf("wallet_money_text(%d): %v", units, err)
		}
		return got
	}

	if got := sqlText(t, math.MinInt64); got != "-92233720368547758.08" {
		t.Errorf("wallet_money_text(MinInt64) = %s", got)
	}

	rapid.Check(t, func(rt *rapid.T) {
		units := rapid.OneOf(
			rapid.Int64Range(-1_000, 1_000),
			rapid.Int64Range(math.MinInt64+1, math.MaxInt64),
			rapid.SampledFrom([]int64{0, 1, 99, 100, 101, math.MaxInt64, -1, -100}),
		).Draw(rt, "units")

		abs := units
		if abs < 0 {
			abs = -abs
		}
		m, err := money.New(fmt.Sprintf("%d.%02d", abs/100, abs%100), "BRL")
		if err != nil {
			rt.Fatalf("money.New: %v", err)
		}
		if units < 0 {
			if m, err = m.Negate(); err != nil {
				rt.Fatalf("Negate: %v", err)
			}
		}
		if got, want := sqlText(rt, units), m.DecimalString(); got != want {
			rt.Fatalf("wallet_money_text(%d) = %s, DecimalString = %s", units, got, want)
		}
	})
}
