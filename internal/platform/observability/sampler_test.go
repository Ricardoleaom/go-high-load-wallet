package observability_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

const betRoute = "POST /wagering/transactions"

// fakeDatabase stands in for the collectors the PostgreSQL adapter and the
// outbox register in production, under the same names.
type fakeDatabase struct {
	pending, oldest, inUse, maxConns float64
	waits                            prometheus.Counter
}

func registerFakeDatabase(t *testing.T, m *observability.Metrics) *fakeDatabase {
	t.Helper()
	db := &fakeDatabase{
		maxConns: 10,
		waits:    prometheus.NewCounter(prometheus.CounterOpts{Name: "wallet_db_pool_acquire_waits_total", Help: "waits"}),
	}
	acquired := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "wallet_db_pool_connections", Help: "connections", ConstLabels: prometheus.Labels{"state": "acquired"},
	}, func() float64 { return db.inUse })
	m.Registry.MustRegister(
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "outbox_pending_events", Help: "pending"}, func() float64 { return db.pending }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "outbox_oldest_pending_age_seconds", Help: "oldest"}, func() float64 { return db.oldest }),
		acquired,
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "wallet_db_pool_max_connections", Help: "max"}, func() float64 { return db.maxConns }),
		db.waits,
	)
	return db
}

func serve(m *observability.Metrics, route, status string, n int, seconds float64) {
	class := status[:1] + "xx"
	for range n {
		m.HTTPRequestsTotal.WithLabelValues(route, "POST", status).Inc()
		m.HTTPRequestDuration.WithLabelValues(route, "POST", class).Observe(seconds)
	}
}

func onlyPoint(t *testing.T, s *observability.Sampler) observability.Point {
	t.Helper()
	points := s.Points()
	if len(points) != 1 {
		t.Fatalf("got %d points, want 1", len(points))
	}
	return points[0]
}

func TestSamplerFirstSampleIsABaseline(t *testing.T) {
	t.Parallel()
	m := observability.New()
	s := observability.NewSampler(m.Registry, 5*time.Second, 5*time.Minute)
	serve(m, betRoute, "200", 50, 0.002)

	if err := s.Sample(time.Unix(100, 0)); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	if got := s.Points(); len(got) != 0 {
		t.Fatalf("first sample produced %d points: a rate needs two readings", len(got))
	}
}

func TestSamplerTurnsCountersIntoRates(t *testing.T) {
	t.Parallel()
	m := observability.New()
	db := registerFakeDatabase(t, m)
	s := observability.NewSampler(m.Registry, 5*time.Second, 5*time.Minute)
	start := time.Unix(100, 0)
	if err := s.Sample(start); err != nil {
		t.Fatalf("Sample: %v", err)
	}

	serve(m, betRoute, "200", 100, 0.002)
	serve(m, betRoute, "503", 5, 0.002)
	serve(m, "GET /wallets/{id}", "200", 10, 0.001)
	serve(m, "GET /health/ready", "200", 50, 0.001)
	m.WagerTransactionsTotal.WithLabelValues("PROCESSED").Add(100)
	m.OutboxPublishedTotal.Add(50)
	db.waits.Add(15)
	db.pending, db.oldest, db.inUse = 1200, 42, 7

	if err := s.Sample(start.Add(5 * time.Second)); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	p := onlyPoint(t, s)

	checks := []struct {
		name      string
		got, want float64
	}{
		{"RequestsPerSecond (health probes excluded)", p.RequestsPerSecond, 23},
		{"BetsPerSecond", p.BetsPerSecond, 21},
		{"ServerErrorsPerSecond", p.ServerErrorsPerSecond, 1},
		{"TransactionsPerSecond", p.TransactionsPerSecond, 20},
		{"EventsPublishedPerSecond", p.EventsPublishedPerSecond, 10},
		{"PoolWaitsPerSecond", p.PoolWaitsPerSecond, 3},
		{"OutboxPending", p.OutboxPending, 1200},
		{"OutboxOldestSeconds", p.OutboxOldestSeconds, 42},
		{"PoolInUse", p.PoolInUse, 7},
		{"PoolMax", p.PoolMax, 10},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	if !p.At.Equal(start.Add(5 * time.Second)) {
		t.Errorf("At = %v, want the second reading's time", p.At)
	}
}

func TestSamplerEstimatesBetQuantilesFromBucketDeltas(t *testing.T) {
	t.Parallel()
	m := observability.New()
	s := observability.NewSampler(m.Registry, 5*time.Second, 5*time.Minute)
	// Before the baseline, so only the interval's own bets may count.
	serve(m, betRoute, "200", 1000, 4)
	if err := s.Sample(time.Unix(100, 0)); err != nil {
		t.Fatalf("Sample: %v", err)
	}

	serve(m, betRoute, "200", 90, 0.004)
	serve(m, betRoute, "200", 10, 0.2)
	if err := s.Sample(time.Unix(105, 0)); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	p := onlyPoint(t, s)

	if p.BetP50Ms == nil || *p.BetP50Ms <= 2.5 || *p.BetP50Ms > 5 {
		t.Errorf("BetP50Ms = %v, want within (2.5, 5]", deref(p.BetP50Ms))
	}
	if p.BetP99Ms == nil || *p.BetP99Ms <= 100 || *p.BetP99Ms > 250 {
		t.Errorf("BetP99Ms = %v, want within (100, 250]", deref(p.BetP99Ms))
	}
}

func TestSamplerReportsNoQuantileWithoutBets(t *testing.T) {
	t.Parallel()
	m := observability.New()
	s := observability.NewSampler(m.Registry, 5*time.Second, 5*time.Minute)
	serve(m, betRoute, "200", 10, 0.004)
	if err := s.Sample(time.Unix(100, 0)); err != nil {
		t.Fatalf("Sample: %v", err)
	}
	serve(m, "GET /wallets/{id}", "200", 10, 0.001)
	if err := s.Sample(time.Unix(105, 0)); err != nil {
		t.Fatalf("Sample: %v", err)
	}

	p := onlyPoint(t, s)
	if p.BetP50Ms != nil || p.BetP99Ms != nil {
		t.Fatalf("quantiles = %v/%v for an interval without bets, want none", deref(p.BetP50Ms), deref(p.BetP99Ms))
	}
	if p.BetsPerSecond != 0 {
		t.Fatalf("BetsPerSecond = %v, want 0", p.BetsPerSecond)
	}
}

func TestSamplerNeverReportsANegativeRate(t *testing.T) {
	t.Parallel()
	first := observability.New()
	current := first.Registry
	s := observability.NewSampler(prometheus.GathererFunc(func() ([]*dto.MetricFamily, error) {
		return current.Gather()
	}), 5*time.Second, 5*time.Minute)
	serve(first, betRoute, "200", 100, 0.004)
	if err := s.Sample(time.Unix(100, 0)); err != nil {
		t.Fatalf("Sample: %v", err)
	}

	// The same gatherer reporting lower totals: a counter that reset.
	reset := observability.New()
	serve(reset, betRoute, "200", 10, 0.004)
	current = reset.Registry
	if err := s.Sample(time.Unix(105, 0)); err != nil {
		t.Fatalf("Sample: %v", err)
	}

	p := onlyPoint(t, s)
	if p.BetsPerSecond != 2 || p.RequestsPerSecond != 2 {
		t.Fatalf("after a reset rates = %v bets/s, %v req/s; want the new totals over the interval, 2 each",
			p.BetsPerSecond, p.RequestsPerSecond)
	}
}

func TestSamplerKeepsOnlyTheWindow(t *testing.T) {
	t.Parallel()
	m := observability.New()
	s := observability.NewSampler(m.Registry, time.Second, 10*time.Second)

	for i := range 16 {
		if err := s.Sample(time.Unix(int64(100+i), 0)); err != nil {
			t.Fatalf("Sample: %v", err)
		}
	}

	points := s.Points()
	if len(points) != 10 {
		t.Fatalf("kept %d points, want 10", len(points))
	}
	if first, last := points[0].At.Unix(), points[9].At.Unix(); first != 106 || last != 115 {
		t.Fatalf("window = %d..%d, want the newest ten, 106..115", first, last)
	}
	if s.Interval() != time.Second {
		t.Fatalf("Interval = %v, want 1s", s.Interval())
	}
}

func deref(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}
