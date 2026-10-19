package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/httpapi"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/sqsx"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/engine"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/queries"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/relay"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/wagering"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/config"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

func newPool(lc fx.Lifecycle, cfg config.Config) (*pgxpool.Pool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := postgres.Open(ctx, cfg.DatabaseURL,
		postgres.WithMaxConns(int32(min(cfg.DBMaxOpenConns, 1<<20))), //nolint:gosec // bounded above
		postgres.WithMinConns(int32(min(cfg.DBMinConns, 1<<20))),     //nolint:gosec // bounded above
		postgres.WithQueryTracing(),
		postgres.WithSessionTimeouts(cfg.DBStatementTimeout, cfg.DBIdleInTransactionTimeout, cfg.DBLockTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	lc.Append(fx.Hook{
		OnStop: func(context.Context) error {
			pool.Close()
			return nil
		},
	})
	return pool, nil
}

func newEngine(pool *pgxpool.Pool) engine.Engine { return postgres.NewEngine(pool) }

func newReader(pool *pgxpool.Pool) queries.Reader { return postgres.NewReader(pool) }

// Overridden by tests; production waits up to two minutes for the IdP,
// which takes a while on a cold docker compose up.
var (
	jwksStartupTimeout = 2 * time.Minute
	jwksRetryDelay     = 5 * time.Second
)

// newAuthenticator retries the first JWKS fetch while the IdP may still be
// starting. Each attempt gets its own context: a failed attempt's refresh
// goroutine stops right away, and the successful one keeps re-reading the
// JWKS until Fx stops the app, so keys revoked at the IdP stop being
// trusted without a restart.
func newAuthenticator(lc fx.Lifecycle, cfg config.Config, metrics *observability.Metrics) (*httpapi.Authenticator, error) {
	deadline := time.Now().Add(jwksStartupTimeout)

	for attempt := 1; ; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		auth, err := httpapi.NewAuthenticator(ctx, httpapi.AuthConfig{
			JWKSURL:         cfg.JWKSURL,
			RefreshInterval: cfg.JWKSRefreshInterval,
			Issuer:          cfg.JWTIssuer,
			Audience:        cfg.JWTAudience,
		}, metrics)
		if err == nil {
			lc.Append(fx.Hook{OnStop: func(context.Context) error {
				cancel()
				return nil
			}})
			return auth, nil
		}
		cancel()

		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("fetch JWKS after %d attempts: %w", attempt, err)
		}
		slog.Warn("waiting for IdP JWKS endpoint", "attempt", attempt, "error", err)
		time.Sleep(jwksRetryDelay)
	}
}

func newSQSClient(cfg config.Config) (*sqs.Client, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return sqsx.NewClient(ctx, sqsx.Config{
		Region:          cfg.AWSRegion,
		Endpoint:        cfg.SQSEndpoint,
		AccessKeyID:     cfg.AWSAccessKeyID,
		SecretAccessKey: cfg.AWSSecretAccessKey,
	})
}

func newWageringService(e engine.Engine, references *postgres.ReferenceQueue, metrics *observability.Metrics) *wagering.Service {
	service := wagering.NewService(e, references)
	service.OnConcurrencyConflict(metrics.ConcurrencyConflictsTotal.Inc)
	return service
}

func newReferenceRetryWorker(service *wagering.Service, metrics *observability.Metrics) *wagering.RetryWorker {
	return wagering.NewRetryWorker(service, func(n int) { metrics.ReferenceRetriesTotal.Add(float64(n)) })
}

func newRelay(cfg config.Config, store *postgres.OutboxStore, sink *sqsx.Sink, metrics *observability.Metrics) *relay.Relay {
	return relay.New(store, sink, cfg.OutboxBatchSize, observability.NewRelayObserver(metrics))
}

// registerDBMetrics exposes the pool statistics and the outbox backlog. The
// backlog query runs on every scrape, bounded so a slow database cannot
// stall the scrape.
func registerDBMetrics(metrics *observability.Metrics, pool *pgxpool.Pool, outbox *postgres.OutboxStore) {
	metrics.Registry.MustRegister(postgres.NewPoolCollector(pool))
	metrics.Registry.MustRegister(observability.NewOutboxCollector(outbox.Backlog, 2*time.Second))
}

func newSQSConsumer(
	cfg config.Config,
	client *sqs.Client,
	inbox *postgres.Inbox,
	service *wagering.Service,
	metrics *observability.Metrics,
) *sqsx.Consumer {
	return sqsx.NewConsumer(client, cfg.TransactionsQueueURL, inbox, service, metrics)
}

func newSQSSink(cfg config.Config, client *sqs.Client) *sqsx.Sink {
	return sqsx.NewSink(client, cfg.EventsQueueURL)
}

func newSQSReadiness(cfg config.Config, client *sqs.Client) *sqsx.Readiness {
	return sqsx.NewReadiness(client, cfg.TransactionsQueueURL)
}

func newHTTPHandlerDeps(
	cfg config.Config,
	service *wagering.Service,
	reader queries.Reader,
	auth *httpapi.Authenticator,
	metrics *observability.Metrics,
	database *postgres.Readiness,
	queue *sqsx.Readiness,
) httpapi.Dependencies {
	return httpapi.Dependencies{
		Wagering:          service,
		Reader:            reader,
		Auth:              auth,
		Metrics:           metrics,
		ReadinessCheckers: []httpapi.ReadinessChecker{database, queue},
		RequestTimeout:    cfg.RequestTimeout,
		AllowedOrigins:    cfg.CORSAllowedOrigins,
	}
}
