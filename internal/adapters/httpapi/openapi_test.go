package httpapi

import (
	"io/fs"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/Ricardoleaom/go-high-load-wallet/internal/platform/observability"
)

const contractPath = "../../../docs/openapi.yaml"

// contract is the part of docs/openapi.yaml these tests hold to the code.
type contract struct {
	Paths      map[string]map[string]any `yaml:"paths"`
	Components struct {
		Schemas map[string]struct {
			Properties map[string]struct {
				Enum []string `yaml:"enum"`
			} `yaml:"properties"`
		} `yaml:"schemas"`
	} `yaml:"components"`
}

func loadContract(t *testing.T) contract {
	t.Helper()
	raw, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("read contract: %v", err)
	}
	var c contract
	if err := yaml.Unmarshal(raw, &c); err != nil {
		t.Fatalf("decode contract: %v", err)
	}
	return c
}

type noSummary struct{}

func (noSummary) Points() []observability.Point { return nil }
func (noSummary) Interval() time.Duration       { return time.Second }

// TestOpenAPIDescribesEveryRoute fails when a route is added, removed or
// renamed on one side only.
func TestOpenAPIDescribesEveryRoute(t *testing.T) {
	t.Parallel()
	c := loadContract(t)

	var described []string
	for path, operations := range c.Paths {
		for method := range operations {
			if method == "parameters" {
				continue
			}
			described = append(described, strings.ToUpper(method)+" "+path)
		}
	}

	h := &handler{deps: Dependencies{Summary: noSummary{}}}
	var registered []string
	for _, r := range append(publicRoutes(h), h.protectedRoutes()...) {
		registered = append(registered, r.pattern)
	}

	slices.Sort(described)
	slices.Sort(registered)
	if !slices.Equal(described, registered) {
		t.Fatalf("contract describes\n  %s\nthe handler registers\n  %s",
			strings.Join(described, "\n  "), strings.Join(registered, "\n  "))
	}
}

var errorCode = regexp.MustCompile(`(?:writeError\(w, http\.Status\w+, |apiError\{http\.Status\w+, )"([A-Z_]+)"`)

// TestOpenAPIListsEveryErrorCode fails when the code answers an error code
// the contract does not list, or the contract lists one nothing answers.
func TestOpenAPIListsEveryErrorCode(t *testing.T) {
	t.Parallel()
	c := loadContract(t)
	listed := slices.Clone(c.Components.Schemas["Error"].Properties["code"].Enum)

	// The package's own sources, read through a filesystem rooted here.
	fsys := os.DirFS(".")
	sources, err := fs.Glob(fsys, "*.go")
	if err != nil {
		t.Fatal(err)
	}
	var answered []string
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		raw, err := fs.ReadFile(fsys, source)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range errorCode.FindAllStringSubmatch(string(raw), -1) {
			if !slices.Contains(answered, match[1]) {
				answered = append(answered, match[1])
			}
		}
	}

	slices.Sort(listed)
	slices.Sort(answered)
	if !slices.Equal(listed, answered) {
		t.Fatalf("contract lists error codes\n  %v\nthe handlers answer\n  %v", listed, answered)
	}
}
