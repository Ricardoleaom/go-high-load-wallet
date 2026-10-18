package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

type options struct {
	maxOpenConns int
	maxIdleConns int
	traceQueries bool

	statementTimeout         time.Duration
	idleInTransactionTimeout time.Duration
	lockTimeout              time.Duration
}

type Option func(*options)

func WithMaxOpenConns(n int) Option { return func(o *options) { o.maxOpenConns = n } }

func WithMaxIdleConns(n int) Option { return func(o *options) { o.maxIdleConns = n } }

// WithQueryTracing records a span per SQL statement through the global
// OpenTelemetry tracer provider.
func WithQueryTracing() Option { return func(o *options) { o.traceQueries = true } }

// WithSessionTimeouts has PostgreSQL cancel a statement that runs longer
// than statement, end a session idle inside a transaction for longer than
// idleInTransaction, and give up waiting for a lock after lock. Zero leaves
// a limit at the server's default.
func WithSessionTimeouts(statement, idleInTransaction, lock time.Duration) Option {
	return func(o *options) {
		o.statementTimeout = statement
		o.idleInTransactionTimeout = idleInTransaction
		o.lockTimeout = lock
	}
}

// IsTimeout reports whether err means an operation ran out of time: a
// cancelled context, or PostgreSQL cancelling a statement (57014) or giving
// up on a lock (55P03).
func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "57014" || pgErr.Code == "55P03")
}

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
	for setting, limit := range map[string]time.Duration{
		"statement_timeout":                   o.statementTimeout,
		"idle_in_transaction_session_timeout": o.idleInTransactionTimeout,
		"lock_timeout":                        o.lockTimeout,
	} {
		if limit > 0 {
			connConfig.RuntimeParams[setting] = strconv.FormatInt(limit.Milliseconds(), 10)
		}
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
