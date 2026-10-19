package observability

import (
	"context"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// betRoute is the route whose latency the summary reports.
const betRoute = "POST /wagering/transactions"

// Point is what the service did during one sampling interval: rates over
// the interval, gauges as read at its end. A quantile is nil when no bet
// was answered in the interval.
type Point struct {
	At                       time.Time
	RequestsPerSecond        float64
	BetsPerSecond            float64
	ServerErrorsPerSecond    float64
	TransactionsPerSecond    float64
	EventsPublishedPerSecond float64
	BetP50Ms                 *float64
	BetP99Ms                 *float64
	OutboxPending            float64
	OutboxOldestSeconds      float64
	PoolInUse                float64
	PoolMax                  float64
	PoolWaitsPerSecond       float64
}

// Sampler reads the service's own metrics every interval and keeps the
// last window of points, so a console can show live numbers without
// talking to Prometheus or the admin port.
type Sampler struct {
	gatherer prometheus.Gatherer
	interval time.Duration
	capacity int

	mu     sync.RWMutex
	points []Point
	last   *reading
}

// reading is one gather's cumulative totals and gauges.
type reading struct {
	at                                          time.Time
	requests, bets, serverErrors                float64
	transactions, published, poolWaits          float64
	betBuckets                                  map[float64]float64 // upper bound → cumulative count
	betCount                                    float64
	outboxPending, outboxOldest, poolIn, poolMx float64
}

func NewSampler(gatherer prometheus.Gatherer, interval, window time.Duration) *Sampler {
	capacity := max(int(window/interval), 1)
	return &Sampler{gatherer: gatherer, interval: interval, capacity: capacity}
}

func (s *Sampler) Interval() time.Duration { return s.interval }

// Points returns a copy of the window, oldest first.
func (s *Sampler) Points() []Point {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Point(nil), s.points...)
}

// Run samples every interval until ctx ends.
func (s *Sampler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-ticker.C:
			if err := s.Sample(now); err != nil {
				slog.Warn("ops sample failed", "error", err)
			}
		}
	}
}

// Sample reads the metrics once. The first reading only sets the baseline:
// a rate needs two.
func (s *Sampler) Sample(now time.Time) error {
	families, err := s.gatherer.Gather()
	if err != nil && len(families) == 0 {
		return err
	}
	current := read(families, now)

	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.last
	s.last = current
	if previous == nil {
		return nil
	}
	s.points = append(s.points, between(previous, current))
	if extra := len(s.points) - s.capacity; extra > 0 {
		s.points = append(s.points[:0], s.points[extra:]...)
	}
	return nil
}

func read(families []*dto.MetricFamily, at time.Time) *reading {
	r := &reading{at: at, betBuckets: map[float64]float64{}}
	for _, family := range families {
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, pair := range metric.GetLabel() {
				labels[pair.GetName()] = pair.GetValue()
			}
			switch family.GetName() {
			case "http_requests_total":
				value := metric.GetCounter().GetValue()
				if strings.HasPrefix(labels["path"], "GET /health/") {
					continue
				}
				r.requests += value
				if labels["path"] == betRoute {
					r.bets += value
				}
				if strings.HasPrefix(labels["status"], "5") {
					r.serverErrors += value
				}
			case "http_request_duration_seconds":
				if labels["route"] != betRoute {
					continue
				}
				histogram := metric.GetHistogram()
				r.betCount += float64(histogram.GetSampleCount())
				for _, bucket := range histogram.GetBucket() {
					r.betBuckets[bucket.GetUpperBound()] += float64(bucket.GetCumulativeCount())
				}
			case "wager_transactions_total":
				r.transactions += metric.GetCounter().GetValue()
			case "outbox_events_published_total":
				r.published += metric.GetCounter().GetValue()
			case "wallet_db_pool_acquire_waits_total":
				r.poolWaits += metric.GetCounter().GetValue()
			case "outbox_pending_events":
				r.outboxPending = metric.GetGauge().GetValue()
			case "outbox_oldest_pending_age_seconds":
				r.outboxOldest = metric.GetGauge().GetValue()
			case "wallet_db_pool_connections":
				if labels["state"] == "acquired" {
					r.poolIn = metric.GetGauge().GetValue()
				}
			case "wallet_db_pool_max_connections":
				r.poolMx = metric.GetGauge().GetValue()
			}
		}
	}
	return r
}

func between(previous, current *reading) Point {
	seconds := current.at.Sub(previous.at).Seconds()
	rate := func(before, after float64) float64 {
		if seconds <= 0 {
			return 0
		}
		return increase(before, after) / seconds
	}

	// A counter that went down was reset: as Prometheus does, count its
	// new total as the increase. The histogram resets with the counters.
	bets := increase(previous.betCount, current.betCount)
	buckets := map[float64]float64{}
	for bound, after := range current.betBuckets {
		if current.betCount < previous.betCount {
			buckets[bound] = after
		} else {
			buckets[bound] = after - previous.betBuckets[bound]
		}
	}

	return Point{
		At:                       current.at,
		RequestsPerSecond:        rate(previous.requests, current.requests),
		BetsPerSecond:            rate(previous.bets, current.bets),
		ServerErrorsPerSecond:    rate(previous.serverErrors, current.serverErrors),
		TransactionsPerSecond:    rate(previous.transactions, current.transactions),
		EventsPublishedPerSecond: rate(previous.published, current.published),
		PoolWaitsPerSecond:       rate(previous.poolWaits, current.poolWaits),
		BetP50Ms:                 quantileMs(0.50, buckets, bets),
		BetP99Ms:                 quantileMs(0.99, buckets, bets),
		OutboxPending:            current.outboxPending,
		OutboxOldestSeconds:      current.outboxOldest,
		PoolInUse:                current.poolIn,
		PoolMax:                  current.poolMx,
	}
}

func increase(before, after float64) float64 {
	if after < before {
		return after
	}
	return after - before
}

// quantileMs interpolates linearly inside the bucket the quantile falls
// in, as PromQL's histogram_quantile does; past the last finite bound it
// answers that bound.
func quantileMs(q float64, cumulative map[float64]float64, total float64) *float64 {
	if total <= 0 {
		return nil
	}
	bounds := make([]float64, 0, len(cumulative))
	for bound := range cumulative {
		bounds = append(bounds, bound)
	}
	sort.Float64s(bounds)

	rank := q * total
	lower, below := 0.0, 0.0
	for _, upper := range bounds {
		count := cumulative[upper]
		if count >= rank && count > below {
			value := (lower + (upper-lower)*(rank-below)/(count-below)) * 1000
			return &value
		}
		lower, below = upper, count
	}
	if len(bounds) == 0 || math.IsInf(lower, 1) {
		return nil
	}
	value := lower * 1000
	return &value
}
