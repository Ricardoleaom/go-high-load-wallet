package observability_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/observability"
)

func TestAdminHandlerServesMetricsAndPprof(t *testing.T) {
	server := httptest.NewServer(observability.AdminHandler(observability.New()))
	t.Cleanup(server.Close)

	for path, want := range map[string]string{
		"/metrics":      "wallet_build_info",
		"/debug/pprof/": "goroutine",
	} {
		resp, err := server.Client().Get(server.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, resp.StatusCode)
		}
		if !strings.Contains(string(body), want) {
			t.Fatalf("GET %s: body does not mention %q", path, want)
		}
	}
}
