package observability_test

import (
	"runtime"
	"testing"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/buildinfo"
	"github.com/Ricardoleaom/go-high-load-wallet/internal/observability"
)

func TestBuildInfoIsExposed(t *testing.T) {
	families, err := observability.New().Registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}

	for _, family := range families {
		if family.GetName() != "wallet_build_info" {
			continue
		}
		metric := family.GetMetric()[0]
		labels := map[string]string{}
		for _, pair := range metric.GetLabel() {
			labels[pair.GetName()] = pair.GetValue()
		}
		if labels["version"] != buildinfo.Version || labels["goversion"] != runtime.Version() {
			t.Fatalf("labels = %v, want version %q and goversion %q", labels, buildinfo.Version, runtime.Version())
		}
		if value := metric.GetGauge().GetValue(); value != 1 {
			t.Fatalf("value = %v, want 1", value)
		}
		return
	}
	t.Fatal("wallet_build_info is not registered")
}
