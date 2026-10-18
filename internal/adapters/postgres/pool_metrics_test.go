package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

func TestPoolCollectorExportsPoolStats(t *testing.T) {
	pool := openPool(t, pgtest.URL(t), postgres.WithMaxConns(1), postgres.WithMinConns(0))
	registry := prometheus.NewRegistry()
	registry.MustRegister(postgres.NewPoolCollector(pool))

	held, err := pool.Acquire(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	// With the only connection held, this acquire waits and gives up.
	waitCtx, cancel := context.WithTimeout(testContext(t), 50*time.Millisecond)
	defer cancel()
	if extra, err := pool.Acquire(waitCtx); err == nil {
		extra.Release()
		t.Fatal("a second connection was acquired from a pool of one")
	}

	if err := testutil.GatherAndCompare(registry, strings.NewReader(`
# HELP wallet_db_pool_acquires_canceled_total Pool acquires given up because their context ended.
# TYPE wallet_db_pool_acquires_canceled_total counter
wallet_db_pool_acquires_canceled_total 1
# HELP wallet_db_pool_connections Pool connections by state.
# TYPE wallet_db_pool_connections gauge
wallet_db_pool_connections{state="acquired"} 1
wallet_db_pool_connections{state="constructing"} 0
wallet_db_pool_connections{state="idle"} 0
# HELP wallet_db_pool_max_connections Most connections the pool opens.
# TYPE wallet_db_pool_max_connections gauge
wallet_db_pool_max_connections 1
`), "wallet_db_pool_acquires_canceled_total", "wallet_db_pool_connections", "wallet_db_pool_max_connections"); err != nil {
		t.Error(err)
	}

	// This one waits for the held connection and gets it.
	acquired := make(chan error, 1)
	go func() {
		conn, err := pool.Acquire(testContext(t))
		if err == nil {
			conn.Release()
		}
		acquired <- err
	}()
	time.Sleep(30 * time.Millisecond)
	held.Release()
	if err := <-acquired; err != nil {
		t.Fatalf("waiting acquire: %v", err)
	}

	for name, atLeast := range map[string]float64{
		"wallet_db_pool_acquires_total":             2,
		"wallet_db_pool_acquire_waits_total":        1,
		"wallet_db_pool_acquire_wait_seconds_total": 0.01,
		"wallet_db_pool_connections_opened_total":   1,
	} {
		if got := gauge(t, registry, name); got < atLeast {
			t.Errorf("%s = %v, want at least %v", name, got, atLeast)
		}
	}
}

func gauge(t *testing.T, g prometheus.Gatherer, name string) float64 {
	t.Helper()
	families, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != name {
			continue
		}
		m := f.GetMetric()[0]
		if c := m.GetCounter(); c != nil {
			return c.GetValue()
		}
		return m.GetGauge().GetValue()
	}
	t.Fatalf("metric %s not exported", name)
	return 0
}
