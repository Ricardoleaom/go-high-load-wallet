package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/adapters/postgres"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/testsupport/pgtest"
)

type fakeChecker struct {
	name string
	err  error
}

func (c fakeChecker) Name() string                { return c.name }
func (c fakeChecker) Ready(context.Context) error { return c.err }

func TestReadinessReportsEachDependency(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := postgres.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	defer pool.Close()
	closed, err := postgres.Open(ctx, pgtest.URL(t))
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	closed.Close()
	live, down := postgres.NewReadiness(pool), postgres.NewReadiness(closed)

	for _, tc := range []struct {
		name    string
		deps    Dependencies
		status  int
		overall string
		checks  map[string]string
	}{
		{"all ready", Dependencies{ReadinessCheckers: []ReadinessChecker{live, fakeChecker{name: "sqs"}}},
			http.StatusOK, "ok", map[string]string{"postgres": "ok", "sqs": "ok"}},
		{"queue down", Dependencies{ReadinessCheckers: []ReadinessChecker{live, fakeChecker{name: "sqs", err: errors.New("down")}}},
			http.StatusServiceUnavailable, "unavailable", map[string]string{"postgres": "ok", "sqs": "unavailable"}},
		{"database down", Dependencies{ReadinessCheckers: []ReadinessChecker{down, fakeChecker{name: "sqs"}}},
			http.StatusServiceUnavailable, "unavailable", map[string]string{"postgres": "unavailable", "sqs": "ok"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			(&handler{deps: tc.deps}).healthReady(recorder, httptest.NewRequest(http.MethodGet, "/health/ready", nil))

			var body readinessResult
			if err := json.NewDecoder(recorder.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if recorder.Code != tc.status || body.Status != tc.overall {
				t.Fatalf("readiness = %d %q, want %d %q", recorder.Code, body.Status, tc.status, tc.overall)
			}
			for dependency, want := range tc.checks {
				if got := body.Checks[dependency]; got != want {
					t.Errorf("check %s = %q, want %q", dependency, got, want)
				}
			}
		})
	}
}
