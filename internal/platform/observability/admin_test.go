package observability_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

func get(t *testing.T, server *httptest.Server, path string) (int, string) {
	t.Helper()
	resp, err := server.Client().Get(server.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, string(body)
}

func TestAdminHandlerServesMetricsAndPprof(t *testing.T) {
	server := httptest.NewServer(observability.AdminHandler(observability.New(), true))
	t.Cleanup(server.Close)

	for path, want := range map[string]string{
		"/metrics":      "wallet_build_info",
		"/debug/pprof/": "goroutine",
	} {
		status, body := get(t, server, path)
		if status != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, status)
		}
		if !strings.Contains(body, want) {
			t.Fatalf("GET %s: body does not mention %q", path, want)
		}
	}
}

// The profiler is opt-in: by default the admin port serves metrics only.
func TestAdminHandlerServesPprofOnlyWhenEnabled(t *testing.T) {
	server := httptest.NewServer(observability.AdminHandler(observability.New(), false))
	t.Cleanup(server.Close)

	if status, _ := get(t, server, "/metrics"); status != http.StatusOK {
		t.Fatalf("GET /metrics: status %d", status)
	}
	for _, path := range []string{"/debug/pprof/", "/debug/pprof/profile?seconds=1", "/debug/pprof/heap"} {
		if status, _ := get(t, server, path); status != http.StatusNotFound {
			t.Fatalf("GET %s with pprof disabled: status %d, want 404", path, status)
		}
	}
}

func TestAdminHandlerLimitsWhatPprofExposes(t *testing.T) {
	server := httptest.NewServer(observability.AdminHandler(observability.New(), true))
	t.Cleanup(server.Close)

	if status, _ := get(t, server, "/debug/pprof/cmdline"); status != http.StatusNotFound {
		t.Fatalf("GET /debug/pprof/cmdline: status %d, want 404", status)
	}
	for _, path := range []string{"/debug/pprof/profile?seconds=600", "/debug/pprof/trace?seconds=600", "/debug/pprof/profile?seconds=abc"} {
		if status, _ := get(t, server, path); status != http.StatusBadRequest {
			t.Fatalf("GET %s: status %d, want 400", path, status)
		}
	}
}
