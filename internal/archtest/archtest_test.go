// Package archtest holds the dependency rule of the hexagonal layout (ADR
// 0013) as a test, so a wrong import fails the build instead of waiting
// for a reviewer to notice.
package archtest

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/Ricardoleaom/go-high-load-wallet/"

// rule says what the non-test files of the packages under dir may not
// import; allowed, when set, is the only third-party code they may use.
type rule struct {
	dir     string
	why     string
	denied  []string
	allowed []string
}

var rules = []rule{
	{
		dir:     "internal/domain",
		why:     "the domain is pure: no storage, transport or framework",
		denied:  []string{module},
		allowed: []string{module + "internal/domain", "github.com/google/uuid"},
	},
	{
		dir: "internal/app",
		why: "the app defines ports; adapters and the platform plug into it, not the other way round",
		denied: []string{
			module + "internal/adapters", module + "internal/platform", module + "cmd",
			"go.uber.org/fx", "github.com/jackc/", "database/sql", "net/http",
			"github.com/aws/", "github.com/prometheus/",
		},
	},
}

// adapters may not import one another: each plugs into the app on its own.
const adaptersDir = "internal/adapters"

// layers are the only top-level directories under internal/: the four
// layers plus test and benchmark support.
var layers = map[string]bool{
	"domain": true, "app": true, "adapters": true, "platform": true,
	"bench": true, "testsupport": true, "archtest": true,
}

func TestDependencyRule(t *testing.T) {
	root := repositoryRoot(t)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "tools" || name == "bench" || name == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if layer, ok := strings.CutPrefix(filepath.ToSlash(rel), "internal/"); ok && !layers[strings.Split(layer, "/")[0]] {
			t.Errorf("%s is outside the layers of ADR 0013 (domain, app, adapters, platform)", rel)
		}
		for _, imp := range imports(t, path) {
			if problem := violation(filepath.ToSlash(rel), imp); problem != "" {
				t.Errorf("%s imports %s: %s", rel, imp, problem)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func violation(file, imp string) string {
	for _, r := range rules {
		if !strings.HasPrefix(file, r.dir+"/") {
			continue
		}
		for _, allowed := range r.allowed {
			if strings.HasPrefix(imp, allowed) {
				return ""
			}
		}
		thirdParty := strings.Contains(strings.Split(imp, "/")[0], ".")
		if len(r.allowed) > 0 && thirdParty && !strings.HasPrefix(imp, module) {
			return r.why
		}
		for _, denied := range r.denied {
			if strings.HasPrefix(imp, denied) {
				return r.why
			}
		}
	}

	if strings.HasPrefix(file, adaptersDir+"/") && strings.HasPrefix(imp, module+adaptersDir+"/") {
		own := strings.Split(strings.TrimPrefix(file, adaptersDir+"/"), "/")[0]
		other := strings.Split(strings.TrimPrefix(imp, module+adaptersDir+"/"), "/")[0]
		if own != other {
			return "an adapter plugs into the app, never into another adapter"
		}
	}
	if strings.HasPrefix(imp, "go.uber.org/fx") && !strings.HasPrefix(file, "cmd/") {
		return "only cmd/ composes the application with Fx"
	}
	return ""
}

func imports(t *testing.T, path string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	var out []string
	for _, spec := range file.Imports {
		imp, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("%s: import %s: %v", path, spec.Path.Value, err)
		}
		out = append(out, imp)
	}
	return out
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// TestRulesCatchViolations keeps the rule itself honest.
func TestRulesCatchViolations(t *testing.T) {
	for _, tc := range []struct {
		file, imp string
		bad       bool
	}{
		{"internal/domain/wager/x.go", module + "internal/app/engine", true},
		{"internal/domain/wager/x.go", "github.com/jackc/pgx/v5", true},
		{"internal/domain/wager/x.go", module + "internal/domain/money", false},
		{"internal/domain/wager/x.go", "github.com/google/uuid", false},
		{"internal/domain/wager/x.go", "time", false},
		{"internal/app/wagering/x.go", module + "internal/adapters/postgres", true},
		{"internal/app/wagering/x.go", module + "internal/platform/observability", true},
		{"internal/app/wagering/x.go", "net/http", true},
		{"internal/app/wagering/x.go", "go.opentelemetry.io/otel", false},
		{"internal/app/wagering/x.go", module + "internal/domain/wager", false},
		{"internal/adapters/httpapi/x.go", module + "internal/adapters/postgres", true},
		{"internal/adapters/postgres/x.go", module + "internal/adapters/postgres/sub", false},
		{"internal/adapters/httpapi/x.go", module + "internal/app/wagering", false},
		{"internal/platform/config/x.go", "go.uber.org/fx", true},
		{"cmd/api/x.go", "go.uber.org/fx", false},
	} {
		if got := violation(tc.file, tc.imp) != ""; got != tc.bad {
			t.Errorf("%s importing %s: violation=%v, want %v", tc.file, tc.imp, got, tc.bad)
		}
	}
}
