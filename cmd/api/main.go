package main

import (
	"log/slog"
	"os"
	"runtime"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/httpapi"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/buildinfo"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/config"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

var configModule = fx.Module("config",
	fx.Provide(
		config.Load,
		observability.New,
	),
)

// persistenceModule owns the connection pool and every PostgreSQL adapter.
// Only cmd/ knows Fx: the adapters and the app layer are plain constructors.
var persistenceModule = fx.Module("persistence",
	fx.Provide(
		newPool,
		newEngine,
		newReader,
		postgres.NewOutboxStore,
		postgres.NewInbox,
		postgres.NewReferenceQueue,
		postgres.NewReadiness,
	),
)

// applicationModule wires the use cases on top of the ports above.
var applicationModule = fx.Module("application",
	fx.Provide(
		newWageringService,
		newReferenceRetryWorker,
		newRelay,
	),
)

var messagingModule = fx.Module("messaging",
	fx.Provide(
		newSQSClient,
		newSQSConsumer,
		newSQSSink,
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

// appInvokes starts everything with a lifecycle; the Fx composition test
// runs this exact list.
var appInvokes = fx.Invoke(
	registerTracing,
	registerDBMetrics,
	registerHTTPServer,
	registerAdminServer,
	registerSQSConsumer,
	registerRelay,
	registerReferenceRetryWorker,
)

func main() {
	// Set before Fx starts so its own lifecycle events and every component
	// logging through slog's default share the same JSON output.
	level, err := config.ParseLogLevel(os.Getenv("LOG_LEVEL"))
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}
	logger := slog.New(observability.NewTraceHandler(
		slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}),
	))
	slog.SetDefault(logger)
	logger.Info("starting wallet", "version", buildinfo.Version, "goVersion", runtime.Version())

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
		appInvokes,
	).Run()
}
