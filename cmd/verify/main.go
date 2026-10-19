// Command verify checks the financial invariants of a wallet database and
// prints the report as JSON. Exit status: 0 when every invariant holds, 1
// when any is violated, 2 when the checks could not run.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/bench/verify"
)

func main() {
	databaseURL := flag.String("database-url", os.Getenv("DATABASE_URL"), "PostgreSQL URL (defaults to DATABASE_URL)")
	timeout := flag.Duration("timeout", 10*time.Minute, "give up after this long")
	flag.Parse()

	report, err := run(*databaseURL, *timeout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(2)
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(struct {
		OK bool `json:"ok"`
		verify.Report
	}{report.OK(), report}); err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(2)
	}
	if !report.OK() {
		os.Exit(1)
	}
}

func run(databaseURL string, timeout time.Duration) (verify.Report, error) {
	if databaseURL == "" {
		return verify.Report{}, fmt.Errorf("no database: set DATABASE_URL or -database-url")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return verify.Report{}, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()

	return verify.Run(ctx, conn)
}
