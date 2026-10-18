package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

func TestOpenAppliesPoolOptions(t *testing.T) {
	url := pgtest.URL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tuned, err := Open(ctx, url, WithMaxOpenConns(3), WithMaxIdleConns(2))
	if err != nil {
		t.Fatalf("Open with options: %v", err)
	}
	defer func() { _ = tuned.Close() }()
	if got := tuned.Stats().MaxOpenConnections; got != 3 {
		t.Fatalf("MaxOpenConnections = %d, want 3", got)
	}

	plain, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("Open without options: %v", err)
	}
	defer func() { _ = plain.Close() }()
	if got := plain.Stats().MaxOpenConnections; got != 10 {
		t.Fatalf("default MaxOpenConnections = %d, want 10", got)
	}
}

func TestOpenTracesQueriesWhenAsked(t *testing.T) {
	url := pgtest.URL(t)
	recorder := tracetest.NewSpanRecorder()
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	t.Cleanup(func() { otel.SetTracerProvider(previous) })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := Open(ctx, url, WithQueryTracing())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	// otelpgx only records queries under a recording parent span, so SQL
	// spans always hang off the request or job that issued them.
	parentCtx, parent := otel.Tracer("test").Start(ctx, "request")
	var one int
	if err := db.QueryRowContext(parentCtx, "SELECT 1").Scan(&one); err != nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := Open(ctx, pgtest.URL(t), WithSessionTimeouts(5*time.Second, 10*time.Second, 2*time.Second))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	for setting, want := range map[string]string{
		"statement_timeout":                   "5s",
		"idle_in_transaction_session_timeout": "10s",
		"lock_timeout":                        "2s",
	} {
		var got string
		if err := db.QueryRowContext(ctx, "SHOW "+setting).Scan(&got); err != nil {
			t.Fatalf("SHOW %s: %v", setting, err)
		}
		if got != want {
			t.Errorf("%s = %s, want %s", setting, got, want)
		}
	}
}
