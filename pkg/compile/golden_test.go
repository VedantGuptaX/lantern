package compile_test

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
	"github.com/VedantGuptaX/lantern/pkg/compile"
	"github.com/VedantGuptaX/lantern/pkg/detect"
	"github.com/VedantGuptaX/lantern/pkg/yamlx"
)

var update = flag.Bool("update", false, "rewrite golden files")

// TestGolden pins the compiler's output byte-for-byte.
//
// This is the real contract test. Six upstream projects own the schemas we
// emit into; when one of them changes, the symptom is a golden diff, not a
// silent behaviour change discovered in production three weeks later.
func TestGolden(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "golden")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading golden dir: %v", err)
	}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join(root, name)

			stack := loadStack(t, filepath.Join(dir, "stack.yaml"))
			svcs := loadServices(t, filepath.Join(dir, "service.yaml"))

			facts := map[string]compile.Facts{}
			for _, s := range svcs {
				r := detect.Runtime(s.Spec.Instrumentation.Runtime, detect.Workload{})
				facts[s.Metadata.Name] = compile.Facts{Runtime: r.Runtime, RuntimeEvidence: r.Evidence}
			}

			out, err := compile.CompileAll(svcs, stack, facts)
			if err != nil {
				t.Fatalf("compile: %v", err)
			}

			got := out.YAML()
			goldenPath := filepath.Join(dir, "expected.yaml")

			if *update {
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil {
					t.Fatalf("writing golden: %v", err)
				}
				t.Logf("updated %s", goldenPath)
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("reading golden (run with -update to create): %v", err)
			}
			if got != string(want) {
				t.Errorf("output does not match %s\n%s", goldenPath, firstDiff(string(want), got))
			}
		})
	}
}

// TestDeterminism guards the property the whole architecture rests on: the
// same input must produce byte-identical output, run after run. Go randomises
// map iteration order deliberately, so an accidental range-over-map anywhere
// in the emitters would surface here rather than as a spurious GitOps diff.
func TestDeterminism(t *testing.T) {
	stack := loadStack(t, filepath.Join("..", "..", "examples", "stack.yaml"))
	svcs := loadServices(t, filepath.Join("..", "..", "examples", "checkout-api.yaml"))
	facts := map[string]compile.Facts{
		"checkout-api": {Runtime: api.RuntimeJava, RuntimeEvidence: "test"},
	}

	first, err := compile.CompileAll(svcs, stack, facts)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	baseline := first.YAML()

	for i := 0; i < 50; i++ {
		out, err := compile.CompileAll(svcs, stack, facts)
		if err != nil {
			t.Fatalf("compile iteration %d: %v", i, err)
		}
		if out.YAML() != baseline {
			t.Fatalf("output differed on iteration %d\n%s", i, firstDiff(baseline, out.YAML()))
		}
	}
}

// ---------------------------------------------------------------------------

func loadStack(t *testing.T, path string) api.ObservabilityStack {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var s api.ObservabilityStack
	if err := yamlx.Unmarshal(data, &s); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return s
}

func loadServices(t *testing.T, path string) []api.ServiceObservability {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	docs, err := yamlx.ParseAll(data)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	var out []api.ServiceObservability
	for _, d := range docs {
		m, ok := d.(map[string]any)
		if !ok {
			continue
		}
		if k, _ := m["kind"].(string); k != api.KindServiceObservability {
			continue
		}
		var svc api.ServiceObservability
		if err := yamlx.UnmarshalValue(m, &svc); err != nil {
			t.Fatalf("decoding %s: %v", path, err)
		}
		out = append(out, svc)
	}
	return out
}

func firstDiff(want, got string) string {
	w := strings.Split(want, "\n")
	g := strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl != gl {
			return "line " + itoa(i+1) + ":\n  want: " + wl + "\n  got:  " + gl
		}
	}
	return "(no line differs; trailing whitespace?)"
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
