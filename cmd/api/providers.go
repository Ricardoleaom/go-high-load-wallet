package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"go.uber.org/fx"

	wagerapp "github.com/Ricardoleaom/go-high-load-wallet/internal/application/wager"
	walletapp "github.com/Ricardoleaom/go-high-load-wallet/internal/application/wallet"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/config"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/httpapi"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/sqsx"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/observability"
)

func newDB(lc fx.Lifecycle, cfg config.Config) (*sql.DB, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	db, err := postgres.Open(ctx, cfg.DatabaseURL,
		postgres.WithMaxOpenConns(cfg.DBMaxOpenConns),
		postgres.WithMaxIdleConns(cfg.DBMaxIdleConns),
		postgres.WithQueryTracing(),
		postgres.WithSessionTimeouts(cfg.DBStatementTimeout, cfg.DBIdleInTransactionTimeout, cfg.DBLockTimeout),
	)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	lc.Append(fx.Hook{
		OnStop: func(context.Context) error {
			return db.Close()
		},
	})

	return db, nil
}

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
		auth, err := httpapi.NewAuthenticator(ctx, cfg.JWKSURL, cfg.JWKSRefreshInterval, metrics)
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

func newWagerService(
	db *sql.DB,
	wallets *postgres.WalletRepository,
	transactions *postgres.TransactionRepository,
	ledgerRepo *postgres.LedgerRepository,
	outbox *postgres.OutboxRepository,
	metrics *observability.Metrics,
) *wagerapp.Service {
	service := wagerapp.NewService(db, wallets, transactions, ledgerRepo, outbox)
	service.OnConcurrencyConflict(metrics.ConcurrencyConflictsTotal.Inc)
	return service
}

func newInboxRepository(db *sql.DB) *postgres.InboxRepository {
	return postgres.NewInboxRepository(db)
}

func newOutboxRepository(db *sql.DB) *postgres.OutboxRepository {
	return postgres.NewOutboxRepository(db)
}

// registerDBMetrics exposes the pool statistics and the outbox backlog. The
// backlog query runs on every scrape, bounded so a slow database cannot
// stall the scrape.
func registerDBMetrics(metrics *observability.Metrics, db *sql.DB, outbox *postgres.OutboxRepository) {
	metrics.RegisterDB(db)
	metrics.Registry.MustRegister(observability.NewOutboxCollector(outbox.Backlog, 2*time.Second))
}

func newSQSConsumer(
	cfg config.Config,
	client *sqs.Client,
	db *sql.DB,
	inbox *postgres.InboxRepository,
	wagerService *wagerapp.Service,
	metrics *observability.Metrics,
) *sqsx.Consumer {
	return sqsx.NewConsumer(client, cfg.TransactionsQueueURL, db, inbox, wagerService, metrics)
}

func newOutboxPublisher(
	cfg config.Config,
	client *sqs.Client,
	outbox *postgres.OutboxRepository,
	metrics *observability.Metrics,
) *sqsx.OutboxPublisher {
	return sqsx.NewOutboxPublisher(client, cfg.EventsQueueURL, outbox, metrics, cfg.OutboxBatchSize)
}

func newSQSReadiness(cfg config.Config, client *sqs.Client) *sqsx.Readiness {
	return sqsx.NewReadiness(client, cfg.TransactionsQueueURL)
}

func newReferenceRetryWorker(wagerService *wagerapp.Service, metrics *observability.Metrics) *wagerapp.RetryWorker {
	return wagerapp.NewRetryWorker(wagerService, metrics)
}

func newHTTPHandlerDeps(
	cfg config.Config,
	db *sql.DB,
	wallets *postgres.WalletRepository,
	transactions *postgres.TransactionRepository,
	ledgerRepo *postgres.LedgerRepository,
	walletService *walletapp.Service,
	wagerService *wagerapp.Service,
	auth *httpapi.Authenticator,
	metrics *observability.Metrics,
	sqsReadiness *sqsx.Readiness,
) httpapi.Dependencies {
	return httpapi.Dependencies{
		DB:                db,
		Wallets:           wallets,
		Transactions:      transactions,
		Ledger:            ledgerRepo,
		WalletService:     walletService,
		WagerService:      wagerService,
		Auth:              auth,
		Metrics:           metrics,
		ReadinessCheckers: []httpapi.ReadinessChecker{sqsReadiness},
		RequestTimeout:    cfg.RequestTimeout,
	}
}
