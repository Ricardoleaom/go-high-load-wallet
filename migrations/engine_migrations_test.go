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

// TestEngineMigrationsRoundTrip takes the two engine migrations (12 and 13)
// down and up again: down must leave the schema as phase 2a had it, up must
// restore the functions, the fillfactor and the dropped index.
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
	if err := pgtest.MigrateTo(ctx, url, 13); err != nil {
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

	hasFillfactor := strings.Contains(options.String, "fillfactor=80")
	if upgraded && (functions != 4 || playerIndex.Valid || !hasFillfactor) {
		t.Errorf("upgraded schema: %d engine functions, idx_wallets_player_id=%v, reloptions=%q",
			functions, playerIndex.Valid, options.String)
	}
	if !upgraded && (functions != 0 || !playerIndex.Valid || hasFillfactor) {
		t.Errorf("downgraded schema: %d engine functions, idx_wallets_player_id=%v, reloptions=%q",
			functions, playerIndex.Valid, options.String)
	}
}
