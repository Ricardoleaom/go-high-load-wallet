package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

type options struct {
	maxOpenConns int
	maxIdleConns int
	traceQueries bool
}

type Option func(*options)

func WithMaxOpenConns(n int) Option { return func(o *options) { o.maxOpenConns = n } }

func WithMaxIdleConns(n int) Option { return func(o *options) { o.maxIdleConns = n } }

// WithQueryTracing records a span per SQL statement through the global
// OpenTelemetry tracer provider.
func WithQueryTracing() Option { return func(o *options) { o.traceQueries = true } }

// Open connects and pings PostgreSQL. Without options it keeps the pool the
// service shipped with: 10 open, 5 idle connections.
func Open(ctx context.Context, databaseURL string, opts ...Option) (*sql.DB, error) {
	o := options{maxOpenConns: 10, maxIdleConns: 5}
	for _, opt := range opts {
		opt(&o)
	}

	connConfig, err := pgx.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL URL: %w", err)
	}
	if o.traceQueries {
		connConfig.Tracer = otelpgx.NewTracer()
	}

	db := stdlib.OpenDB(*connConfig)
	db.SetMaxOpenConns(o.maxOpenConns)
	db.SetMaxIdleConns(o.maxIdleConns)
	db.SetConnMaxLifetime(30 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}

	return db, nil
}
