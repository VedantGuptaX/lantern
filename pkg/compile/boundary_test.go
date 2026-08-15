package compile_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/VedantGuptaX/lantern/pkg/yamlx"
)

// forbiddenInCompiler are imports that would make Compile impure. The design
// rests on Compile being a pure function: same inputs, byte-identical output,
// no I/O. If it can reach a cluster, a clock, or the network, then the CLI and
// the operator can silently diverge, golden tests stop being a contract, and
// `lantern diff` needs a second code path.
//
// This test is the enforcement mechanism for that rule.
var forbiddenInCompiler = []string{
	"k8s.io/client-go",
	"sigs.k8s.io/controller-runtime",
	"net/http",
	"net",
	"os/exec",
	"math/rand",
	"time",
	"os",
}

func TestCompilerPurityBoundary(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package dir: %v", err)
	}

	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, imp := range f.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			for _, bad := range forbiddenInCompiler {
				if path == bad || strings.HasPrefix(path, bad+"/") {
					t.Errorf("%s imports %q: pkg/compile must stay pure — move this into Facts, resolved by the caller", name, path)
				}
			}
		}
	}
}

// TestGoldenOutputIsParseable closes the loop: everything the compiler emits
// must be valid YAML that can be read back. An emitter bug that produced
// subtly malformed indentation would otherwise only surface at `kubectl apply`
// time.
func TestGoldenOutputIsParseable(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "golden")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading golden dir: %v", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), "expected.yaml")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}

		docs, err := yamlx.ParseAll(data)
		if err != nil {
			t.Fatalf("%s: emitted YAML does not parse: %v", path, err)
		}
		if len(docs) == 0 {
			t.Fatalf("%s: no documents", path)
		}

		for i, d := range docs {
			m, ok := d.(map[string]any)
			if !ok {
				t.Errorf("%s document %d is not a mapping", path, i+1)
				continue
			}
			for _, required := range []string{"apiVersion", "kind", "metadata"} {
				if _, ok := m[required]; !ok {
					t.Errorf("%s document %d is missing %q", path, i+1, required)
				}
			}
			meta, ok := m["metadata"].(map[string]any)
			if !ok || meta["name"] == "" || meta["name"] == nil {
				t.Errorf("%s document %d has no metadata.name", path, i+1)
			}
		}
	}
}
