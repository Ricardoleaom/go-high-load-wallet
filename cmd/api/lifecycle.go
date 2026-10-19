package main

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"go.uber.org/fx"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/sqsx"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/relay"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/app/wagering"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/config"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/lifecycle"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

// hook runs c between Fx's start and stop.
func hook(lc fx.Lifecycle, c lifecycle.Component) {
	lc.Append(fx.Hook{OnStart: c.Start, OnStop: c.Stop})
}

// registerTracing installs the OpenTelemetry provider before anything else
// starts and flushes it last on the way down.
func registerTracing(lc fx.Lifecycle) error {
	shutdown, err := observability.SetupTracing(context.Background())
	if err != nil {
		return err
	}
	lc.Append(fx.Hook{
		OnStop: func(ctx context.Context) error {
			if err := shutdown(ctx); err != nil {
				slog.Warn("flush traces on shutdown", "error", err)
			}
			return nil
		},
	})
	return nil
}

// adminWriteTimeout leaves room for the longest admin response, a CPU
// profile capped at 60 seconds.
const adminWriteTimeout = 70 * time.Second

func registerHTTPServer(lc fx.Lifecycle, cfg config.Config, handler http.Handler) {
	// The response may still be written for a moment after the request's
	// own deadline, which answers 503 when it expires.
	hook(lc, lifecycle.Server("HTTP", lifecycle.NewServer(":"+cfg.HTTPPort, handler, cfg.RequestTimeout+5*time.Second)))
}

// registerAdminServer serves metrics (and pprof when enabled) on their own
// address, loopback by default, so they stay off the public network.
func registerAdminServer(lc fx.Lifecycle, cfg config.Config, metrics *observability.Metrics) {
	hook(lc, lifecycle.Server("admin",
		lifecycle.NewServer(cfg.AdminAddr, observability.AdminHandler(metrics, cfg.PprofEnabled), adminWriteTimeout)))
}

func registerSQSConsumer(lc fx.Lifecycle, consumer *sqsx.Consumer) {
	hook(lc, lifecycle.Background("sqs-consumer", consumer.Run))
}

func registerRelay(lc fx.Lifecycle, cfg config.Config, r *relay.Relay) {
	hook(lc, lifecycle.Background("outbox-relay", func(ctx context.Context) error {
		return r.Run(ctx, cfg.OutboxPublishInterval)
	}))
}

func registerReferenceRetryWorker(lc fx.Lifecycle, cfg config.Config, worker *wagering.RetryWorker) {
	hook(lc, lifecycle.Background("reference-retry-worker", func(ctx context.Context) error {
		return worker.Run(ctx, cfg.ReferenceRetryInterval)
	}))
}
