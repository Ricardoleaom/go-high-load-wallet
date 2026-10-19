package postgres

import (
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// poolCollector exports the pool's own statistics on every scrape. Waits
// are what matter under load: an acquire that found every connection busy
// is a request queueing in the application rather than in PostgreSQL.
type poolCollector struct {
	pool *pgxpool.Pool

	acquires, waits, waitSeconds, canceled, opened, connections, maxConnections *prometheus.Desc
}

func NewPoolCollector(pool *pgxpool.Pool) prometheus.Collector {
	return &poolCollector{
		pool:           pool,
		acquires:       prometheus.NewDesc("wallet_db_pool_acquires_total", "Connections acquired from the pool.", nil, nil),
		waits:          prometheus.NewDesc("wallet_db_pool_acquire_waits_total", "Acquires that waited because every connection was busy.", nil, nil),
		waitSeconds:    prometheus.NewDesc("wallet_db_pool_acquire_wait_seconds_total", "Time acquires spent waiting for a busy pool.", nil, nil),
		canceled:       prometheus.NewDesc("wallet_db_pool_acquires_canceled_total", "Pool acquires given up because their context ended.", nil, nil),
		opened:         prometheus.NewDesc("wallet_db_pool_connections_opened_total", "Connections the pool opened.", nil, nil),
		connections:    prometheus.NewDesc("wallet_db_pool_connections", "Pool connections by state.", []string{"state"}, nil),
		maxConnections: prometheus.NewDesc("wallet_db_pool_max_connections", "Most connections the pool opens.", nil, nil),
	}
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.acquires, c.waits, c.waitSeconds, c.canceled, c.opened, c.connections, c.maxConnections} {
		ch <- d
	}
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(c.acquires, prometheus.CounterValue, float64(s.AcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.waits, prometheus.CounterValue, float64(s.EmptyAcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.waitSeconds, prometheus.CounterValue, s.EmptyAcquireWaitTime().Seconds())
	ch <- prometheus.MustNewConstMetric(c.canceled, prometheus.CounterValue, float64(s.CanceledAcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.opened, prometheus.CounterValue, float64(s.NewConnsCount()))
	ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(s.AcquiredConns()), "acquired")
	ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(s.IdleConns()), "idle")
	ch <- prometheus.MustNewConstMetric(c.connections, prometheus.GaugeValue, float64(s.ConstructingConns()), "constructing")
	ch <- prometheus.MustNewConstMetric(c.maxConnections, prometheus.GaugeValue, float64(s.MaxConns()))
}
