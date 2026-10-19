// Package postgres is the PostgreSQL adapter: the engine that runs each
// command as one function call, the read side, the outbox, the inbox and
// the pending-reference queue, all on one pgx connection pool.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
	"github.com/jackc/pgx/v5/pgxpool"
)

type options struct {
	maxConns     int32
	minConns     int32
	traceQueries bool

	statementTimeout         time.Duration
	idleInTransactionTimeout time.Duration
	lockTimeout              time.Duration
}

type Option func(*options)

func WithMaxConns(n int32) Option { return func(o *options) { o.maxConns = n } }

// WithMinConns keeps n connections open even when idle, so a burst after a
// quiet period does not pay for opening them.
func WithMinConns(n int32) Option { return func(o *options) { o.minConns = n } }

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

// Connections are recycled after half an hour, spread over three minutes so
// they do not all reconnect at once, and closed after five idle minutes.
const (
	maxConnLifetime       = 30 * time.Minute
	maxConnLifetimeJitter = 3 * time.Minute
	maxConnIdleTime       = 5 * time.Minute
	healthCheckPeriod     = 30 * time.Second

	// cancelDeadlineDelay is how long a query whose context ended may wait
	// for PostgreSQL to act on the cancel request before the connection is
	// closed instead.
	cancelDeadlineDelay = 2 * time.Second
)

// Open connects and pings PostgreSQL. Without options the pool keeps at
// most 10 and at least 2 connections.
func Open(ctx context.Context, databaseURL string, opts ...Option) (*pgxpool.Pool, error) {
	o := options{maxConns: 10, minConns: 2}
	for _, opt := range opts {
		opt(&o)
	}

	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse PostgreSQL URL: %w", err)
	}
	config.MaxConns = o.maxConns
	config.MinConns = min(o.minConns, o.maxConns)
	config.MaxConnLifetime = maxConnLifetime
	config.MaxConnLifetimeJitter = maxConnLifetimeJitter
	config.MaxConnIdleTime = maxConnIdleTime
	config.HealthCheckPeriod = healthCheckPeriod

	if o.traceQueries {
		config.ConnConfig.Tracer = otelpgx.NewTracer()
	}
	for setting, limit := range map[string]time.Duration{
		"statement_timeout":                   o.statementTimeout,
		"idle_in_transaction_session_timeout": o.idleInTransactionTimeout,
		"lock_timeout":                        o.lockTimeout,
	} {
		if limit > 0 {
			config.ConnConfig.RuntimeParams[setting] = strconv.FormatInt(limit.Milliseconds(), 10)
		}
	}
	// When a request's context ends mid-query, ask PostgreSQL to cancel the
	// statement, which rolls it back and keeps the connection usable,
	// instead of pgx's default of closing the connection under it.
	config.ConnConfig.BuildContextWatcherHandler = func(conn *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: conn, DeadlineDelay: cancelDeadlineDelay}
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	return pool, nil
}

// IsTimeout reports whether err means an operation ran out of time: a
// context deadline, or PostgreSQL cancelling a statement (57014) or giving
// up on a lock (55P03).
func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "57014" || pgErr.Code == "55P03")
}

// timeoutError marks an error that means an operation ran out of time, for
// callers that must not depend on this package: like net.Error, it answers
// Timeout() with true.
type timeoutError struct{ err error }

func (e timeoutError) Error() string { return e.err.Error() }
func (e timeoutError) Unwrap() error { return e.err }
func (timeoutError) Timeout() bool   { return true }

// markTimeout wraps err in timeoutError when IsTimeout says it is one.
func markTimeout(err error) error {
	if err != nil && IsTimeout(err) {
		return timeoutError{err: err}
	}
	return err
}
