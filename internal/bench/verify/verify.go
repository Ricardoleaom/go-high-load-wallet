// Package verify checks the financial invariants every load run must leave
// intact. Each check is one SQL query returning the offending IDs; the
// report keeps the count and up to ten examples per check.
package verify

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

type Check struct {
	Name       string   `json:"name"`
	Invariant  string   `json:"invariant"`
	Violations int64    `json:"violations"`
	Samples    []string `json:"samples"`
}

type Report struct {
	Checks []Check `json:"checks"`
}

func (r Report) OK() bool {
	for _, c := range r.Checks {
		if c.Violations > 0 {
			return false
		}
	}
	return true
}

type definition struct {
	name      string
	invariant string
	query     string // returns one text ID per offending row
}

// movingKinds are the kinds whose PROCESSED transactions change a balance.
const movingKinds = `('OPENING', 'BET', 'WIN', 'REFUND', 'ROLLBACK')`

var definitions = []definition{
	{"non_negative_balance", "I1", `
		SELECT id::text FROM wallets WHERE balance_minor_units < 0`},
	{"balance_matches_ledger", "I2", `
		SELECT w.id::text
		FROM wallets w
		LEFT JOIN (
			SELECT wallet_id, sum(amount_delta_minor_units) AS total
			FROM ledger_entries GROUP BY wallet_id
		) l ON l.wallet_id = w.id
		WHERE w.balance_minor_units <> COALESCE(l.total, 0)`},
	{"processed_movement_has_one_entry", "I4", `
		SELECT t.id::text
		FROM transactions t
		LEFT JOIN ledger_entries le ON le.transaction_id = t.id
		WHERE t.status = 'PROCESSED' AND t.kind IN ` + movingKinds + ` AND t.amount_minor_units > 0
		GROUP BY t.id
		HAVING count(le.id) <> 1`},
	{"entry_points_to_processed_transaction", "I4", `
		SELECT le.id::text
		FROM ledger_entries le
		JOIN transactions t ON t.id = le.transaction_id
		WHERE t.status <> 'PROCESSED'`},
	{"loss_has_no_entry", "I4", `
		SELECT t.id::text
		FROM transactions t
		JOIN ledger_entries le ON le.transaction_id = t.id
		WHERE t.kind = 'LOSS'`},
	{"single_successful_reversal", "I5", `
		SELECT reference_id
		FROM transactions
		WHERE status = 'PROCESSED' AND kind IN ('REFUND', 'ROLLBACK') AND reference_id IS NOT NULL
		GROUP BY provider_id, reference_id
		HAVING count(*) > 1`},
	{"no_pending_transactions", "synchronous processing", `
		SELECT id::text FROM transactions WHERE status = 'PENDING'`},
	// A hash anti-join: the outbox has no index on the payload's transaction
	// ID, and a per-row lookup would be quadratic at benchmark volumes.
	{"terminal_transaction_has_event", "I6", `
		WITH events AS (
			SELECT DISTINCT event_type, payload->'data'->>'transactionId' AS transaction_id
			FROM outbox_events
			WHERE event_type IN ('WagerTransactionProcessed', 'WagerTransactionRejected')
		)
		SELECT t.id::text
		FROM transactions t
		LEFT JOIN events e
			ON e.transaction_id = t.id::text
			AND e.event_type = CASE t.status
				WHEN 'PROCESSED' THEN 'WagerTransactionProcessed'
				ELSE 'WagerTransactionRejected'
			END
		WHERE t.kind <> 'OPENING' AND t.status IN ('PROCESSED', 'REJECTED') AND e.transaction_id IS NULL`},
}

const maxSamples = 10

// Querier runs the checks: a pgx connection or pool.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Run executes every check. A violation is a finding, not an error: err is
// reserved for failing to run a check at all.
func Run(ctx context.Context, db Querier) (Report, error) {
	report := Report{Checks: make([]Check, 0, len(definitions))}
	for _, d := range definitions {
		check, err := run(ctx, db, d)
		if err != nil {
			return Report{}, err
		}
		report.Checks = append(report.Checks, check)
	}
	return report, nil
}

func run(ctx context.Context, db Querier, d definition) (Check, error) {
	//nolint:gosec // d.query is one of this package's constant definitions, never input
	query := fmt.Sprintf(`SELECT offender, count(*) OVER () FROM (%s) AS offenders(offender) LIMIT %d`, d.query, maxSamples)
	rows, err := db.Query(ctx, query)
	if err != nil {
		return Check{}, fmt.Errorf("check %s: %w", d.name, err)
	}
	defer rows.Close()

	check := Check{Name: d.name, Invariant: d.invariant, Samples: []string{}}
	for rows.Next() {
		var sample string
		if err := rows.Scan(&sample, &check.Violations); err != nil {
			return Check{}, fmt.Errorf("check %s: %w", d.name, err)
		}
		check.Samples = append(check.Samples, sample)
	}
	if err := rows.Err(); err != nil {
		return Check{}, fmt.Errorf("check %s: %w", d.name, err)
	}
	return check, nil
}
