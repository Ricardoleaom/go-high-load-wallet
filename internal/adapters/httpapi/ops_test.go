package httpapi_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/httpapi"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

type fixedSummary struct{ points []observability.Point }

func (f fixedSummary) Points() []observability.Point { return f.points }
func (f fixedSummary) Interval() time.Duration       { return 5 * time.Second }

func withSummary(points ...observability.Point) func(*httpapi.Dependencies) {
	return func(d *httpapi.Dependencies) { d.Summary = fixedSummary{points: points} }
}

func TestOpsSummaryForOperators(t *testing.T) {
	t.Parallel()
	p50 := 3.25
	h := newAPIHarness(t, withSummary(
		observability.Point{
			At: time.Date(2026, 10, 18, 21, 0, 5, 0, time.UTC), RequestsPerSecond: 321.456, BetsPerSecond: 320,
			BetP50Ms: &p50, OutboxPending: 1200, PoolInUse: 4, PoolMax: 10,
		},
	))

	resp := h.do(t, http.MethodGet, "/ops/summary", h.issuer.OperatorToken(t), nil, nil)

	requireStatus(t, resp, http.StatusOK)
	requireField(t, resp, "intervalSeconds", float64(5))
	points, ok := resp.body["points"].([]any)
	if !ok || len(points) != 1 {
		t.Fatalf("points = %v, want one", resp.body["points"])
	}
	point := points[0].(map[string]any)
	for field, want := range map[string]any{
		"at":                "2026-10-18T21:00:05Z",
		"requestsPerSecond": 321.46,
		"betsPerSecond":     float64(320),
		"betP50Ms":          3.25,
		"betP99Ms":          nil,
		"outboxPending":     float64(1200),
		"poolInUse":         float64(4),
		"poolMax":           float64(10),
	} {
		got, present := point[field]
		if !present || got != want {
			t.Errorf("%s = %v (present %v), want %v", field, got, present, want)
		}
	}

	internal := h.do(t, http.MethodGet, "/ops/summary", h.issuer.InternalToken(t), nil, nil)
	requireStatus(t, internal, http.StatusOK)
}

func TestOpsSummaryBeforeTheFirstSample(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t, withSummary())

	resp := h.do(t, http.MethodGet, "/ops/summary", h.issuer.OperatorToken(t), nil, nil)

	requireStatus(t, resp, http.StatusOK)
	points, ok := resp.body["points"].([]any)
	if !ok || len(points) != 0 {
		t.Fatalf("points = %#v, want an empty list", resp.body["points"])
	}
}

func TestOpsSummaryRefusesProviders(t *testing.T) {
	t.Parallel()
	h := newAPIHarness(t, withSummary())

	resp := h.do(t, http.MethodGet, "/ops/summary", h.issuer.ProviderToken(t, "provider-a"), nil, nil)

	requireStatus(t, resp, http.StatusForbidden)
	requireField(t, resp, "code", "FORBIDDEN")
}
