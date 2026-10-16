package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"go.uber.org/fx"

	wagerapp "github.com/Ricardoleaom/go-high-load-wallet/internal/application/wager"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/config"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/infrastructure/sqsx"
)

// runBackground starts fn in a goroutine on OnStart and, on OnStop, cancels
// its context and waits (bounded by the shutdown context's deadline) for it
// to actually return, so shutdown is observable rather than fire-and-forget.
func runBackground(lc fx.Lifecycle, name string, fn func(ctx context.Context) error) {
	var cancel context.CancelFunc
	done := make(chan struct{})

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			var ctx context.Context
			ctx, cancel = context.WithCancel(context.Background()) //nolint:gosec // cancel is called in OnStop below, not leaked

			go func() {
				defer close(done)
				if err := fn(ctx); err != nil {
					slog.Error("background worker stopped with error", "worker", name, "error", err)
				}
			}()

			return nil
		},
		OnStop: func(ctx context.Context) error {
			cancel()
			select {
			case <-done:
				slog.Info("background worker stopped", "worker", name)
				return nil
			case <-ctx.Done():
				return fmt.Errorf("worker %s did not stop before shutdown deadline", name)
			}
		},
	})
}

func registerHTTPServer(lc fx.Lifecycle, cfg config.Config, handler http.Handler) {
	server := &http.Server{
		Addr:              ":" + cfg.HTTPPort,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			listener, err := net.Listen("tcp", server.Addr)
			if err != nil {
				return fmt.Errorf("listen on %s: %w", server.Addr, err)
			}

			go func() {
				slog.Info("HTTP server listening", "addr", server.Addr)
				if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					slog.Error("HTTP server stopped with error", "error", err)
				}
			}()

			return nil
		},
		OnStop: func(ctx context.Context) error {
			return server.Shutdown(ctx)
		},
	})
}

func registerSQSConsumer(lc fx.Lifecycle, consumer *sqsx.Consumer) {
	runBackground(lc, "sqs-consumer", consumer.Run)
}

func registerOutboxPublisher(lc fx.Lifecycle, cfg config.Config, publisher *sqsx.OutboxPublisher) {
	runBackground(lc, "outbox-publisher", func(ctx context.Context) error {
		return publisher.Run(ctx, cfg.OutboxPublishInterval)
	})
}

func registerReferenceRetryWorker(lc fx.Lifecycle, cfg config.Config, worker *wagerapp.RetryWorker) {
	runBackground(lc, "reference-retry-worker", func(ctx context.Context) error {
		return worker.Run(ctx, cfg.ReferenceRetryInterval)
	})
}
