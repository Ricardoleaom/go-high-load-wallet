package migrations_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

// TestEngineMigrationsRoundTrip takes the engine migrations (12 to 14) down
// and up again: down must leave the schema as phase 2a had it, up must
// restore the functions, the fillfactor, the dropped index and the lookup
// indexes led by their own column.
func TestEngineMigrationsRoundTrip(t *testing.T) {
	url := pgtest.FreshURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	requireEngineSchema(t, ctx, db, true)
	if err := pgtest.MigrateTo(ctx, url, 11); err != nil {
		t.Fatal(err)
	}
	requireEngineSchema(t, ctx, db, false)
	if err := pgtest.MigrateTo(ctx, url, 14); err != nil {
		t.Fatal(err)
	}
	requireEngineSchema(t, ctx, db, true)
}

func requireEngineSchema(t *testing.T, ctx context.Context, db *sql.DB, upgraded bool) {
	t.Helper()

	var functions int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_proc
		WHERE proname IN ('wallet_open', 'wager_submit', 'wager_retry_reference', 'wallet_money_text')`,
	).Scan(&functions); err != nil {
		t.Fatal(err)
	}
	var playerIndex sql.NullString
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('idx_wallets_player_id')::text`).Scan(&playerIndex); err != nil {
		t.Fatal(err)
	}
	var options sql.NullString
	if err := db.QueryRowContext(ctx, `
		SELECT array_to_string(reloptions, ',') FROM pg_class WHERE relname = 'wallets'`,
	).Scan(&options); err != nil {
		t.Fatal(err)
	}

	var keyIndex string
	if err := db.QueryRowContext(ctx, `SELECT pg_get_indexdef('idx_transactions_idempotency_unique'::regclass)`).Scan(&keyIndex); err != nil {
		t.Fatal(err)
	}
	keyFirst := strings.Contains(keyIndex, "(idempotency_key, provider_id)")
	hasFillfactor := strings.Contains(options.String, "fillfactor=80")
	if upgraded && (functions != 4 || playerIndex.Valid || !hasFillfactor || !keyFirst) {
		t.Errorf("upgraded schema: %d engine functions, idx_wallets_player_id=%v, reloptions=%q, key index %s",
			functions, playerIndex.Valid, options.String, keyIndex)
	}
	if !upgraded && (functions != 0 || !playerIndex.Valid || hasFillfactor || keyFirst) {
		t.Errorf("downgraded schema: %d engine functions, idx_wallets_player_id=%v, reloptions=%q, key index %s",
			functions, playerIndex.Valid, options.String, keyIndex)
	}
}
