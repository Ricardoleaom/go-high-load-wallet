package pgtest_test

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

func TestURLReturnsMigratedDatabase(t *testing.T) {
	url := pgtest.URL(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := sql.Open("pgx", url)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer func() { _ = db.Close() }()

	var tables int
	err = db.QueryRowContext(ctx, `
		SELECT count(*)
		FROM information_schema.tables
		WHERE table_schema = 'public'
		  AND table_name IN ('wallets', 'transactions', 'ledger_entries', 'inbox_messages', 'outbox_events')
	`).Scan(&tables)
	if err != nil {
		t.Fatalf("count tables: %v", err)
	}
	if tables != 5 {
		t.Fatalf("found %d of the 5 application tables", tables)
	}
}

// Several test binaries migrate the same DATABASE_URL at once when
// `go test ./...` runs packages in parallel against the compose database.
func TestMigrateIsIdempotentAndSafeConcurrently(t *testing.T) {
	url := pgtest.URL(t)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	errs := make(chan error, 3)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() { errs <- pgtest.Migrate(ctx, url) })
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("Migrate on an up-to-date database: %v", err)
		}
	}
}
