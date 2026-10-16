package observability

import (
	"net/http"
	"net/http/pprof"
	"strconv"
)

// maxProfileSeconds caps CPU profiles and execution traces, which run for
// as long as the caller asks.
const maxProfileSeconds = 60

// AdminHandler serves operator endpoints that must not face the public
// network: Prometheus metrics and, only when pprofEnabled, the Go profiler.
// The profiler omits cmdline and caps profile and trace durations.
func AdminHandler(m *Metrics, pprofEnabled bool) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	if !pprofEnabled {
		return mux
	}

	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", http.NotFound)
	mux.Handle("GET /debug/pprof/profile", capSeconds(http.HandlerFunc(pprof.Profile)))
	mux.Handle("GET /debug/pprof/trace", capSeconds(http.HandlerFunc(pprof.Trace)))
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	return mux
}

func capSeconds(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw := r.URL.Query().Get("seconds"); raw != "" {
			seconds, err := strconv.Atoi(raw)
			if err != nil || seconds <= 0 || seconds > maxProfileSeconds {
				http.Error(w, "seconds must be between 1 and "+strconv.Itoa(maxProfileSeconds), http.StatusBadRequest)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
