package main

import (
	"context"
	"testing"
	"time"

	"go.uber.org/fx"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/jwttest"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/sqstest"
)

// TestFxAppStartsAndStopsCleanly builds the exact module graph main() runs,
// starts it against a real PostgreSQL and LocalStack (containers) and an
// in-process JWKS, then stops it. It checks that every provider resolves,
// every OnStart hook succeeds, and every background worker (SQS consumer,
// outbox publisher, reference retry worker) actually exits during OnStop
// instead of leaking a goroutine.
func TestFxAppStartsAndStopsCleanly(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	databaseURL := pgtest.URL(t)
	issuer := jwttest.NewIssuer(t)
	endpoint := sqstest.Start(ctx, t)

	t.Setenv("DATABASE_URL", databaseURL)
	t.Setenv("JWKS_URL", issuer.JWKSURL())
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("SQS_ENDPOINT", endpoint)
	t.Setenv("SQS_TRANSACTIONS_QUEUE_URL", sqstest.CreateFIFOQueue(ctx, t, endpoint, "wager-transactions.fifo"))
	t.Setenv("SQS_EVENTS_QUEUE_URL", sqstest.CreateFIFOQueue(ctx, t, endpoint, "wager-events.fifo"))
	// Port 0 avoids colliding with an app container already bound to 8080.
	t.Setenv("HTTP_PORT", "0")
	t.Setenv("ADMIN_ADDR", "127.0.0.1:0")

	app := fx.New(
		configModule,
		persistenceModule,
		applicationModule,
		messagingModule,
		httpModule,
		appInvokes,
		fx.NopLogger,
	)

	startCtx, cancelStart := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStart()
	if err := app.Start(startCtx); err != nil {
		t.Fatalf("app.Start: %v", err)
	}

	stopCtx, cancelStop := context.WithTimeout(ctx, 30*time.Second)
	defer cancelStop()
	if err := app.Stop(stopCtx); err != nil {
		t.Fatalf("app.Stop: %v", err)
	}
}
