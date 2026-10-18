package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

func TestOpenAppliesPoolOptions(t *testing.T) {
	url := pgtest.URL(t)

	tuned := openPool(t, url, postgres.WithMaxConns(3), postgres.WithMinConns(1)).Config()
	if tuned.MaxConns != 3 || tuned.MinConns != 1 {
		t.Errorf("tuned pool = %d/%d connections, want 3/1", tuned.MaxConns, tuned.MinConns)
	}

	plain := openPool(t, url).Config()
	if plain.MaxConns != 10 || plain.MinConns != 2 {
		t.Errorf("default pool = %d/%d connections, want 10/2", plain.MaxConns, plain.MinConns)
	}
	if plain.MaxConnLifetime != 30*time.Minute || plain.MaxConnLifetimeJitter != 3*time.Minute ||
		plain.MaxConnIdleTime != 5*time.Minute {
		t.Errorf("connection lifetimes = %s ±%s, idle %s; want 30m ±3m, idle 5m",
			plain.MaxConnLifetime, plain.MaxConnLifetimeJitter, plain.MaxConnIdleTime)
	}

	small := openPool(t, url, postgres.WithMaxConns(1)).Config()
	if small.MinConns != 1 {
		t.Errorf("a one-connection pool keeps %d connections, want 1", small.MinConns)
	}
}

func TestOpenTracesQueriesWhenAsked(t *testing.T) {
	url := pgtest.URL(t)
	recorder := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	pool := openPool(t, url, postgres.WithQueryTracing())

	// otelpgx only records queries under a recording parent span, so SQL
	// spans always hang off the request or job that issued them.
	ctx, parent := otel.Tracer("test").Start(testContext(t), "request")
	var one int
	if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("SELECT 1: %v", err)
	}
	parent.End()

	for _, span := range recorder.Ended() {
		for _, attr := range span.Attributes() {
			if attr.Key == attribute.Key("db.query.text") && strings.Contains(attr.Value.AsString(), "SELECT 1") {
				return
			}
		}
	}
	t.Fatalf("no span carried the query text; %d spans ended", len(recorder.Ended()))
}

func TestOpenAppliesSessionTimeouts(t *testing.T) {
	pool := openPool(t, pgtest.URL(t), postgres.WithSessionTimeouts(5*time.Second, 10*time.Second, 2*time.Second))

	for setting, want := range map[string]string{
		"statement_timeout":                   "5s",
		"idle_in_transaction_session_timeout": "10s",
		"lock_timeout":                        "2s",
	} {
		var got string
		if err := pool.QueryRow(testContext(t), "SHOW "+setting).Scan(&got); err != nil {
			t.Fatalf("SHOW %s: %v", setting, err)
		}
		if got != want {
			t.Errorf("%s = %s, want %s", setting, got, want)
		}
	}
}

// A request whose deadline passes mid-statement must stop the statement on
// the server, not just stop waiting for it, and must leave the connection
// usable rather than closed: under overload, reconnecting for every timed
// out request would add load exactly when there is least room for it.
func TestDeadlineCancelsTheStatementAndKeepsTheConnection(t *testing.T) {
	pool := openPool(t, pgtest.URL(t), postgres.WithMaxConns(1))
	opened := pool.Stat().NewConnsCount()

	ctx, cancel := context.WithTimeout(testContext(t), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := pool.Exec(ctx, "SELECT pg_sleep(10)")
	if !postgres.IsTimeout(err) {
		t.Fatalf("err = %v, want a timeout", err)
	}
	if took := time.Since(started); took > time.Second {
		t.Fatalf("the cancelled statement returned after %s", took)
	}

	var sleeping int
	if err := pool.QueryRow(testContext(t),
		`SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'SELECT pg_sleep(10)%' AND state = 'active'`,
	).Scan(&sleeping); err != nil {
		t.Fatalf("query after the timeout: %v", err)
	}
	if sleeping != 0 {
		t.Errorf("%d pg_sleep statements still running on the server", sleeping)
	}
	if reopened := pool.Stat().NewConnsCount() - opened; reopened != 0 {
		t.Errorf("the pool opened %d new connections after the timeout, want 0", reopened)
	}
}
