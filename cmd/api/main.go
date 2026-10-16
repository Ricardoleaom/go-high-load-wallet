package main

import (
	"log/slog"
	"os"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	walletapp "github.com/Ricardoleaom/go-high-load-wallet/internal/application/wallet"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/config"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/httpapi"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/observability"
)

var configModule = fx.Module("config",
	fx.Provide(
		config.Load,
		observability.New,
	),
)

// persistenceModule owns the database connection and every repository.
// Repositories are infrastructure, not domain: the domain packages
// (internal/domain/...) never import this module or Fx itself.
var persistenceModule = fx.Module("persistence",
	fx.Provide(
		newDB,
		postgres.NewWalletRepository,
		postgres.NewTransactionRepository,
		postgres.NewLedgerRepository,
		newInboxRepository,
		newOutboxRepository,
	),
)

// applicationModule wires the use cases. These depend on the domain and on
// the repositories above, never on HTTP, SQS or Fx types.
var applicationModule = fx.Module("application",
	fx.Provide(
		walletapp.NewService,
		newWagerService,
		newReferenceRetryWorker,
	),
)

var messagingModule = fx.Module("messaging",
	fx.Provide(
		newSQSClient,
		newSQSConsumer,
		newOutboxPublisher,
		newSQSReadiness,
	),
)

var httpModule = fx.Module("http",
	fx.Provide(
		newAuthenticator,
		newHTTPHandlerDeps,
		httpapi.NewHandler,
	),
)

func main() {
	// Set before Fx starts so its own lifecycle events and every component
	// logging through slog's default share the same JSON output.
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	fx.New(
		fx.WithLogger(func() fxevent.Logger {
			fxLogger := &fxevent.SlogLogger{Logger: logger}
			fxLogger.UseLogLevel(slog.LevelDebug)
			return fxLogger
		}),
		configModule,
		persistenceModule,
		applicationModule,
		messagingModule,
		httpModule,
		fx.Invoke(
			registerHTTPServer,
			registerSQSConsumer,
			registerOutboxPublisher,
			registerReferenceRetryWorker,
		),
	).Run()
}
