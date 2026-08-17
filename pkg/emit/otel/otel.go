// Package otel emits OpenTelemetry Operator resources and the workload
// annotations that trigger agent injection.
package otel

import (
	"fmt"
	"sort"
	"strconv"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
	"github.com/VedantGuptaX/lantern/pkg/kube"
	"github.com/VedantGuptaX/lantern/pkg/yamlx"
)

// InjectAnnotation returns the pod annotation key that triggers injection for
// a runtime, per the OpenTelemetry Operator's documented annotation set.
func InjectAnnotation(r api.Runtime) (string, bool) {
	switch r {
	case api.RuntimeJava:
		return "instrumentation.opentelemetry.io/inject-java", true
	case api.RuntimeNodeJS:
		return "instrumentation.opentelemetry.io/inject-nodejs", true
	case api.RuntimePython:
		return "instrumentation.opentelemetry.io/inject-python", true
	case api.RuntimeDotNet:
		return "instrumentation.opentelemetry.io/inject-dotnet", true
	case api.RuntimeGo:
		return "instrumentation.opentelemetry.io/inject-go", true
	case api.RuntimePreInstr:
		// The app already ships an SDK; inject configuration only, never an
		// agent, or every span is recorded twice.
		return "instrumentation.opentelemetry.io/inject-sdk", true
	default:
		return "", false
	}
}

// Input is the pre-resolved input for the instrumentation emitter.
type Input struct {
	Service       string
	Namespace     string
	Team          string
	Runtime       api.Runtime
	Mode          api.InstrumentationMode
	OTLPEndpoint  string
	SamplingRate  float64
	TracesEnabled bool
	Attributes    map[string]string
	Labels        map[string]string
	// InstrumentationName is the shared Instrumentation CR name in this
	// namespace. One CR per namespace, referenced by every service in it.
	InstrumentationName string
}

// Diagnostic mirrors prom.Diagnostic; kept local so emitters stay independent.
type Diagnostic struct {
	Level   string
	Message string
}

// BuildInstrumentation emits the namespace-scoped Instrumentation CR that
// agents read their exporter, sampler, and resource configuration from.
func BuildInstrumentation(in Input) (*kube.Object, error) {
	if in.OTLPEndpoint == "" {
		return nil, fmt.Errorf("service %q: backends.traces.otlpEndpoint (or instrumentation.collector) must be set to inject instrumentation", in.Service)
	}

	sampler := yamlx.NewMap(
		"type", yamlx.S("parentbased_traceidratio"),
		"argument", yamlx.S(strconv.FormatFloat(in.SamplingRate, 'f', -1, 64)),
	)
	if !in.TracesEnabled {
		sampler = yamlx.NewMap(
			"type", yamlx.S("always_off"),
		)
	}

	attrs := map[string]string{}
	for k, v := range in.Attributes {
		attrs[k] = v
	}
	attrs["service.namespace"] = in.Namespace
	if in.Team != "" {
		attrs["team"] = in.Team
	}

	// Pin the injected agents to STABLE HTTP semantic conventions.
	//
	// This is one half of a contract with pkg/emit/prom/sli.go, which builds
	// every HTTP SLI query against the stable names
	// (http_server_request_duration_seconds_count, and the
	// http_response_status_code attribute). Auto-instrumentation agents
	// default to the OLD pre-1.23 convention instead
	// (http_server_duration_milliseconds_count, http_status_code) unless
	// explicitly opted in, so without this the two halves never meet.
	//
	// FOUND ON A REAL CLUSTER, and it is completely silent: agents export
	// happily, the collector forwards happily, Prometheus stores the series
	// happily -- under names nothing queries. Every SLI recording rule
	// evaluates to no data, so every per-service dashboard panel and every
	// SLO burn-rate alert stays permanently empty with no error anywhere to
	// explain why. Confirmed by querying Prometheus directly: 0 series for
	// the name the rules use, 6 for the name actually being written.
	//
	// "http" (not "http/dup") because nothing in this project queries the
	// old names, so paying double HTTP metric cardinality to keep emitting
	// them would be waste. Supported by the Node.js agent since 0.54.0 and
	// the equivalent releases of the Java/Python/.NET agents; on any agent
	// too old to recognise it the variable is simply ignored, which leaves
	// behaviour exactly as it was rather than breaking anything.
	// See https://opentelemetry.io/docs/specs/semconv/non-normative/http-migration/
	env := yamlx.NewSeq(
		yamlx.NewMap(
			"name", yamlx.S("OTEL_SEMCONV_STABILITY_OPT_IN"),
			"value", yamlx.S("http"),
		),
	)

	spec := yamlx.NewMap(
		"env", env,
		"exporter", yamlx.NewMap("endpoint", yamlx.S(in.OTLPEndpoint)),
		"propagators", yamlx.Strings("tracecontext", "baggage"),
		"sampler", sampler,
		"resource", yamlx.NewMap("resourceAttributes", kube.SortedStringMap(attrs)),
	)

	return kube.New(
		"opentelemetry.io/v1alpha1", "Instrumentation",
		in.InstrumentationName, in.Namespace, in.Labels,
	).Set("spec", spec), nil
}

// WorkloadPatch describes the annotations that must be applied to the target
// workload's pod template.
//
// P0 emits this as a strategic-merge patch document rather than mutating the
// Deployment, because the CLI must not assume it owns the workload manifest.
// The P2 operator applies the same annotation set directly.
type WorkloadPatch struct {
	Kind        string
	Name        string
	Namespace   string
	Annotations map[string]string
}

// BuildWorkloadPatch resolves the injection annotations for a workload.
//
// The double-instrumentation guard lives here: agent and eBPF modes are
// mutually exclusive, and violating that is an error rather than a warning.
// An agent and an eBPF probe instrumenting the same handler produce two spans
// per request and doubled RED metrics — silent, expensive, and it destroys
// trust in the data on day one.
func BuildWorkloadPatch(in Input) (*WorkloadPatch, []Diagnostic, error) {
	var diags []Diagnostic

	switch in.Mode {
	case api.ModeNone:
		return nil, []Diagnostic{{
			Level:   "info",
			Message: fmt.Sprintf("service %q: instrumentation disabled; emitting scrape and rules only", in.Service),
		}}, nil

	case api.ModeEBPF:
		// eBPF is configured at the collector/OBI level, not by annotating the
		// pod. Emit the exclusion diagnostic so the operator (P2) knows to
		// keep this workload out of any agent-injection selector.
		return nil, []Diagnostic{{
			Level: "info",
			Message: fmt.Sprintf(
				"service %q: using eBPF instrumentation; the workload is deliberately left un-annotated so the "+
					"OpenTelemetry Operator will not also inject an agent",
				in.Service),
		}}, nil

	case api.ModeAgent, api.ModeSDKOnly:
		runtime := in.Runtime
		if in.Mode == api.ModeSDKOnly {
			runtime = api.RuntimePreInstr
		}
		key, ok := InjectAnnotation(runtime)
		if !ok {
			return nil, nil, fmt.Errorf(
				"service %q: instrumentation mode %q requires a known runtime, but runtime is %q; "+
					"set spec.instrumentation.runtime explicitly or use mode: ebpf",
				in.Service, in.Mode, runtime)
		}
		annotations := map[string]string{
			key: in.InstrumentationName,
		}
		if runtime == api.RuntimeGo {
			diags = append(diags, Diagnostic{
				Level: "warn",
				Message: fmt.Sprintf(
					"service %q: Go agent injection requires OTEL_GO_AUTO_TARGET_EXE to name the binary path, "+
						"and runs privileged. Consider mode: ebpf for Go workloads.",
					in.Service),
			})
		}
		return &WorkloadPatch{
			Kind:        "Deployment",
			Name:        in.Service,
			Namespace:   in.Namespace,
			Annotations: annotations,
		}, diags, nil

	default:
		return nil, nil, fmt.Errorf("service %q: unresolved instrumentation mode %q (compiler bug: auto should be resolved before emit)", in.Service, in.Mode)
	}
}

// PatchObject renders a WorkloadPatch as an applyable strategic-merge patch.
func PatchObject(p *WorkloadPatch, labels map[string]string) *kube.Object {
	keys := make([]string, 0, len(p.Annotations))
	for k := range p.Annotations {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	ann := yamlx.NewMap()
	for _, k := range keys {
		ann.Set(k, yamlx.S(p.Annotations[k]))
	}

	obj := kube.New("apps/v1", p.Kind, p.Name, p.Namespace, labels)
	obj.Set("spec", yamlx.NewMap(
		"template", yamlx.NewMap(
			"metadata", yamlx.NewMap("annotations", ann),
		),
	))
	return obj
}
