package httpapi

import (
	"math"
	"net/http"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

// OpsSummary is the live window of service metrics the console draws.
type OpsSummary interface {
	Points() []observability.Point
	Interval() time.Duration
}

type opsSummaryResponse struct {
	IntervalSeconds float64            `json:"intervalSeconds"`
	Points          []opsPointResponse `json:"points"`
}

type opsPointResponse struct {
	At                       string   `json:"at"`
	RequestsPerSecond        float64  `json:"requestsPerSecond"`
	BetsPerSecond            float64  `json:"betsPerSecond"`
	ServerErrorsPerSecond    float64  `json:"serverErrorsPerSecond"`
	TransactionsPerSecond    float64  `json:"transactionsPerSecond"`
	EventsPublishedPerSecond float64  `json:"eventsPublishedPerSecond"`
	BetP50Ms                 *float64 `json:"betP50Ms"`
	BetP99Ms                 *float64 `json:"betP99Ms"`
	OutboxPending            float64  `json:"outboxPending"`
	OutboxOldestSeconds      float64  `json:"outboxOldestSeconds"`
	PoolInUse                float64  `json:"poolInUse"`
	PoolMax                  float64  `json:"poolMax"`
	PoolWaitsPerSecond       float64  `json:"poolWaitsPerSecond"`
}

func (h *handler) getOpsSummary(w http.ResponseWriter, r *http.Request) {
	if !isInternal(r.Context()) && !isOperator(r.Context()) {
		writeError(w, http.StatusForbidden, "FORBIDDEN", "the summary needs the internal service or an operator")
		return
	}

	points := h.deps.Summary.Points()
	resp := opsSummaryResponse{
		IntervalSeconds: h.deps.Summary.Interval().Seconds(),
		Points:          make([]opsPointResponse, 0, len(points)),
	}
	for _, p := range points {
		resp.Points = append(resp.Points, opsPointResponse{
			At:                       p.At.UTC().Format(time.RFC3339),
			RequestsPerSecond:        round2(p.RequestsPerSecond),
			BetsPerSecond:            round2(p.BetsPerSecond),
			ServerErrorsPerSecond:    round2(p.ServerErrorsPerSecond),
			TransactionsPerSecond:    round2(p.TransactionsPerSecond),
			EventsPublishedPerSecond: round2(p.EventsPublishedPerSecond),
			BetP50Ms:                 round2Ptr(p.BetP50Ms),
			BetP99Ms:                 round2Ptr(p.BetP99Ms),
			OutboxPending:            p.OutboxPending,
			OutboxOldestSeconds:      round2(p.OutboxOldestSeconds),
			PoolInUse:                p.PoolInUse,
			PoolMax:                  p.PoolMax,
			PoolWaitsPerSecond:       round2(p.PoolWaitsPerSecond),
		})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

func round2Ptr(v *float64) *float64 {
	if v == nil {
		return nil
	}
	rounded := round2(*v)
	return &rounded
}
