package httpapi

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

type readinessResult struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func (h *handler) healthReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	checks := make(map[string]string)
	ready := true

	for _, checker := range h.deps.ReadinessCheckers {
		if err := checker.Ready(ctx); err != nil {
			slog.Error("readiness check failed", "dependency", checker.Name(), "error", err)
			checks[checker.Name()] = "unavailable"
			ready = false
		} else {
			checks[checker.Name()] = "ok"
		}
	}

	status := http.StatusOK
	result := readinessResult{Status: "ok", Checks: checks}
	if !ready {
		status = http.StatusServiceUnavailable
		result.Status = "unavailable"
	}

	writeJSON(w, status, result)
}
