package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Readiness reports whether PostgreSQL answers through the pool.
type Readiness struct {
	pool *pgxpool.Pool
}

func NewReadiness(pool *pgxpool.Pool) *Readiness {
	return &Readiness{pool: pool}
}

func (*Readiness) Name() string { return "postgres" }

func (r *Readiness) Ready(ctx context.Context) error { return r.pool.Ping(ctx) }
