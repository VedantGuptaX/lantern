// Command lantern compiles ServiceObservability specs into Kubernetes
// manifests: OpenTelemetry Instrumentation, Prometheus ServiceMonitors, and
// SLO burn-rate alert rules.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
	"github.com/VedantGuptaX/lantern/pkg/compile"
	"github.com/VedantGuptaX/lantern/pkg/detect"
	"github.com/VedantGuptaX/lantern/pkg/discover"
	"github.com/VedantGuptaX/lantern/pkg/yamlx"
)

const usage = `lantern — observability as code

Usage:
  lantern discover [flags] <path|->...      draft specs from existing workloads
  lantern synth    [flags] <spec.yaml>...   compile specs to Kubernetes manifests
  lantern validate [flags] <spec.yaml>...   parse and check specs, emit no output
  lantern version

Flags:
  -stack <file>    ObservabilityStack to compile against (synth, validate)
  -o <dir>         write one file per object instead of a stream on stdout
  -quiet           suppress diagnostics on stderr
  -strict          treat warnings as errors

discover flags:
  -team <name>     ownership fallback when no team label can be found
  -ns <a,b>        restrict discovery to these namespaces
  -all             include platform namespaces (kube-system and friends)

Examples:
  kubectl get deploy,statefulset,cronjob -A -o yaml | lantern discover -
  lantern discover ./manifests -team platform > services.yaml
  lantern synth -stack stack.yaml services.yaml | kubectl apply -f -
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	stackPath := fs.String("stack", "", "path to an ObservabilityStack manifest")
	outDir := fs.String("o", "", "write manifests into this directory")
	quiet := fs.Bool("quiet", false, "suppress diagnostics")
	strict := fs.Bool("strict", false, "treat warnings as errors")
	team := fs.String("team", "", "ownership fallback for discover")
	namespaces := fs.String("ns", "", "comma-separated namespaces to restrict discovery to")
	includeSystem := fs.Bool("all", false, "include platform namespaces in discovery")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }

	switch cmd {
	case "synth", "validate":
		_ = fs.Parse(os.Args[2:])
		if err := run(cmd, *stackPath, *outDir, fs.Args(), *quiet, *strict); err != nil {
			fmt.Fprintf(os.Stderr, "lantern: %v\n", err)
			os.Exit(1)
		}
	case "discover":
		_ = fs.Parse(os.Args[2:])
		opts := discover.Options{
			IncludeSystem: *includeSystem,
			DefaultTeam:   *team,
		}
		if *namespaces != "" {
			opts.Namespaces = strings.Split(*namespaces, ",")
		}
		if err := runDiscover(fs.Args(), opts, *quiet); err != nil {
			fmt.Fprintf(os.Stderr, "lantern: %v\n", err)
			os.Exit(1)
		}
	case "version", "-v", "--version":
		fmt.Println("lantern 0.1.0-p0")
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "lantern: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
}

func run(cmd, stackPath, outDir string, paths []string, quiet, strict bool) error {
	if len(paths) == 0 {
		return errors.New("no spec files given")
	}

	stack, err := loadStack(stackPath)
	if err != nil {
		return err
	}

	var svcs []api.ServiceObservability
	for _, p := range paths {
		loaded, err := loadServices(p)
		if err != nil {
			return err
		}
		svcs = append(svcs, loaded...)
	}
	if len(svcs) == 0 {
		return fmt.Errorf("no %s documents found in %s", api.KindServiceObservability, strings.Join(paths, ", "))
	}

	// Runtime detection is the impure step; it happens here, outside the
	// compiler, and its result is passed in as Facts.
	facts := map[string]compile.Facts{}
	for _, svc := range svcs {
		r := detect.Runtime(svc.Spec.Instrumentation.Runtime, detect.Workload{})
		facts[svc.Metadata.Name] = compile.Facts{Runtime: r.Runtime, RuntimeEvidence: r.Evidence}
	}

	out, err := compile.CompileAll(svcs, stack, facts)
	if err != nil {
		return err
	}

	if !quiet {
		for _, d := range out.Diags {
			fmt.Fprintln(os.Stderr, d.String())
		}
	}
	if strict {
		for _, d := range out.Diags {
			if d.Level == "warn" {
				return fmt.Errorf("warnings present and -strict is set")
			}
		}
	}

	if cmd == "validate" {
		fmt.Fprintf(os.Stderr, "ok: %d service(s), %d object(s) would be emitted\n", len(svcs), len(out.Objects))
		return nil
	}

	if outDir == "" {
		fmt.Print(out.YAML())
		return nil
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	for _, o := range out.Objects {
		fname := fmt.Sprintf("%s-%s.yaml", strings.ToLower(o.Kind), o.Name)
		if err := os.WriteFile(filepath.Join(outDir, fname), []byte(o.YAML()), 0o644); err != nil {
			return err
		}
	}
	fmt.Fprintf(os.Stderr, "wrote %d object(s) to %s\n", len(out.Objects), outDir)
	return nil
}

// runDiscover reads workloads from files, directories, or stdin and writes
// draft specs to stdout.
func runDiscover(paths []string, opts discover.Options, quiet bool) error {
	if len(paths) == 0 {
		return errors.New("no input given; pass a path, a directory, or - to read stdin")
	}

	var workloads []discover.Workload
	for _, p := range paths {
		data, err := readInput(p)
		if err != nil {
			return err
		}
		ws, err := discover.ParseWorkloads(data)
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		workloads = append(workloads, ws...)
	}

	if len(workloads) == 0 {
		return errors.New("no Deployment, StatefulSet, DaemonSet or CronJob found in the input")
	}

	results := discover.Discover(workloads, opts)
	if len(results) == 0 {
		return fmt.Errorf("found %d workload(s), but all were filtered out; use -all or -ns to widen the search", len(workloads))
	}

	fmt.Print(discover.Render(results))

	if !quiet {
		fmt.Fprintf(os.Stderr, "\ndiscovered %d service(s) from %d workload(s)\n", len(results), len(workloads))
		summary := discover.Summary(results)
		if len(summary) == 0 {
			fmt.Fprintln(os.Stderr, "nothing flagged for review")
			return nil
		}
		fmt.Fprintln(os.Stderr, "\nreview before applying:")
		for _, line := range summary {
			fmt.Fprintln(os.Stderr, "  "+line)
		}
	}
	return nil
}

// readInput accepts `-` for stdin, a file, or a directory of manifests.
func readInput(path string) ([]byte, error) {
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return os.ReadFile(path)
	}

	var buf []byte
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if ext := filepath.Ext(e.Name()); ext != ".yaml" && ext != ".yml" {
			continue
		}
		names = append(names, e.Name())
	}
	// Sorted so directory iteration order never changes the output.
	sort.Strings(names)
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(path, n))
		if err != nil {
			return nil, err
		}
		buf = append(buf, data...)
		buf = append(buf, []byte("\n---\n")...)
	}
	return buf, nil
}

func loadStack(path string) (api.ObservabilityStack, error) {
	if path == "" {
		return defaultStack(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return api.ObservabilityStack{}, fmt.Errorf("reading stack: %w", err)
	}
	var stack api.ObservabilityStack
	if err := yamlx.Unmarshal(data, &stack); err != nil {
		return stack, fmt.Errorf("%s: %w", path, err)
	}
	if stack.Kind != api.KindObservabilityStack {
		return stack, fmt.Errorf("%s: expected kind %s, got %q", path, api.KindObservabilityStack, stack.Kind)
	}
	return stack, nil
}

// defaultStack is a minimal in-cluster-ish default so `lantern synth` works
// without a stack file during evaluation. It is deliberately conservative:
// no eBPF, no policy.
func defaultStack() api.ObservabilityStack {
	return api.ObservabilityStack{
		APIVersion: api.GroupVersion,
		Kind:       api.KindObservabilityStack,
		Metadata:   api.ObjectMeta{Name: "default"},
		Spec: api.ObservabilityStackSpec{
			Backends: api.Backends{
				Metrics: api.MetricsBackend{Type: "prometheus", Operator: "prometheus-operator"},
				Traces:  api.TracesBackend{Type: "tempo", OTLPEndpoint: "http://otel-collector.observability:4318"},
			},
			Instrumentation: api.StackInstrumentation{Provider: "otel-operator"},
		},
	}
}

func loadServices(path string) ([]api.ServiceObservability, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	docs, err := yamlx.ParseAll(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	var out []api.ServiceObservability
	for i, doc := range docs {
		m, ok := doc.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := m["kind"].(string); kind != api.KindServiceObservability {
			continue
		}
		// Round-trip through the typed decoder so json tags govern field
		// names and unknown fields are rejected loudly.
		re, err := reencode(m)
		if err != nil {
			return nil, fmt.Errorf("%s document %d: %w", path, i+1, err)
		}
		var svc api.ServiceObservability
		if err := yamlx.UnmarshalValue(re, &svc); err != nil {
			return nil, fmt.Errorf("%s document %d: %w", path, i+1, err)
		}
		out = append(out, svc)
	}

	sort.SliceStable(out, func(i, j int) bool { return out[i].Metadata.Name < out[j].Metadata.Name })
	return out, nil
}

func reencode(m map[string]any) (any, error) { return m, nil }
