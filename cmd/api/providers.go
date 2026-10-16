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

	db, err := postgres.Open(ctx, cfg.DatabaseURL)
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

// newAuthenticator retries JWKS discovery for a while, since Keycloak may
// still be starting up when this process does in docker compose.
func newAuthenticator(cfg config.Config, metrics *observability.Metrics) (*httpapi.Authenticator, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var lastErr error
	for attempt := 0; attempt < 12; attempt++ {
		auth, err := httpapi.NewAuthenticator(ctx, cfg.JWKSURL, metrics)
		if err == nil {
			return auth, nil
		}
		lastErr = err
		slog.Warn("waiting for IdP JWKS endpoint", "attempt", attempt+1, "error", err)
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("fetch JWKS: %w", ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}

	return nil, fmt.Errorf("fetch JWKS after retries: %w", lastErr)
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

func newSQSConsumer(
	cfg config.Config,
	client *sqs.Client,
	db *sql.DB,
	inbox *postgres.InboxRepository,
	wagerService *wagerapp.Service,
) *sqsx.Consumer {
	return sqsx.NewConsumer(client, cfg.TransactionsQueueURL, db, inbox, wagerService)
}

func newOutboxPublisher(
	cfg config.Config,
	client *sqs.Client,
	outbox *postgres.OutboxRepository,
	metrics *observability.Metrics,
) *sqsx.OutboxPublisher {
	return sqsx.NewOutboxPublisher(client, cfg.EventsQueueURL, outbox, metrics)
}

func newSQSReadiness(cfg config.Config, client *sqs.Client) *sqsx.Readiness {
	return sqsx.NewReadiness(client, cfg.TransactionsQueueURL)
}

func newReferenceRetryWorker(wagerService *wagerapp.Service, metrics *observability.Metrics) *wagerapp.RetryWorker {
	return wagerapp.NewRetryWorker(wagerService, metrics)
}

func newHTTPHandlerDeps(
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
	}
}
