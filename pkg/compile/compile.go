// Package compile is the Lantern compiler core.
//
// Compile is a PURE FUNCTION. It performs no I/O: no cluster access, no
// network, no filesystem, no clock, no randomness. Everything impure — runtime
// detection, cluster capability discovery — is resolved by the caller and
// arrives in Facts.
//
// This is the load-bearing architectural decision of the whole project:
//
//   - The CLI and the operator share one implementation, so `lantern synth`
//     can never drift from what the operator applies.
//   - Golden-file tests pin every byte of output, which is what actually
//     breaks when one of six upstream CRD schemas changes.
//   - `lantern diff` is compile-then-compare, with no second code path.
//
// CI enforces the boundary: this package may not import k8s client libraries.
package compile

import (
	"fmt"
	"sort"
	"strings"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
	"github.com/VedantGuptaX/lantern/pkg/emit/otel"
	"github.com/VedantGuptaX/lantern/pkg/emit/prom"
	"github.com/VedantGuptaX/lantern/pkg/kube"
)

// Facts carries everything impure, pre-resolved by the caller.
type Facts struct {
	// Runtime is the detected application runtime.
	Runtime api.Runtime
	// RuntimeEvidence explains how Runtime was determined, for diagnostics.
	RuntimeEvidence string
}

// Diagnostic is a non-fatal compiler message.
type Diagnostic struct {
	Level   string // error | warn | info
	Service string
	Message string
}

func (d Diagnostic) String() string {
	if d.Service != "" {
		return fmt.Sprintf("%-5s %s: %s", d.Level, d.Service, d.Message)
	}
	return fmt.Sprintf("%-5s %s", d.Level, d.Message)
}

// Output is the compilation result.
type Output struct {
	Objects []*kube.Object
	Diags   []Diagnostic
}

// YAML renders the output as a multi-document stream.
func (o Output) YAML() string { return kube.Stream(o.Objects) }

// Compile turns one ServiceObservability into its emitted artifacts.
func Compile(svc api.ServiceObservability, stack api.ObservabilityStack, facts Facts) (Output, error) {
	var out Output

	if err := validate(svc, stack); err != nil {
		return out, err
	}

	name := svc.Metadata.Name
	ns := svc.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}
	env := stack.Spec.Defaults.Environment

	labels := map[string]string{
		api.ManagedLabel: "true",
		api.ServiceLabel: name,
	}
	if svc.Spec.Team != "" {
		labels[api.TeamLabel] = svc.Spec.Team
	}

	// ---- resolve instrumentation mode -------------------------------------

	ebpfAvailable := stack.Spec.Instrumentation.EBPF.Enabled
	mode := svc.Spec.Instrumentation.Mode
	if mode == "" {
		mode = api.ModeAuto
	}
	if stack.Spec.Instrumentation.Provider == "none" {
		mode = api.ModeNone
	}

	resolvedMode, why, err := resolveMode(mode, facts.Runtime, ebpfAvailable)
	if err != nil {
		return out, fmt.Errorf("service %q: %w", name, err)
	}
	if mode == api.ModeAuto && resolvedMode != api.ModeNone {
		out.Diags = append(out.Diags, Diagnostic{
			Level:   "info",
			Service: name,
			Message: fmt.Sprintf("instrumentation mode auto resolved to %q (runtime %q from %s): %s",
				resolvedMode, facts.Runtime, facts.RuntimeEvidence, why),
		})
	}
	if svc.Spec.ServiceKind == api.KindInference && (resolvedMode == api.ModeAgent || resolvedMode == api.ModeEBPF) {
		// `lantern discover` sets mode: none for serviceKind: inference itself
		// (see pkg/discover), so this only fires for a hand-written spec that
		// left instrumentation.mode on auto. It's a warning rather than a
		// silent override because Compile does not second-guess an explicit
		// choice the caller made — but GPU model servers (vLLM, Triton, NIM,
		// ...) already export their own Prometheus metrics, and agent/eBPF
		// injection into a GPU-resident serving process is rarely what's
		// wanted.
		out.Diags = append(out.Diags, Diagnostic{
			Level:   "warn",
			Service: name,
			Message: fmt.Sprintf(
				"serviceKind inference resolved instrumentation mode to %q; GPU model servers usually export their own "+
					"Prometheus metrics and don't need OTel agent/eBPF injection — set instrumentation.mode: none explicitly if that's not wanted here",
				resolvedMode),
		})
	}

	// ---- policy: sampling rate --------------------------------------------

	tracesEnabled := api.BoolValue(svc.Spec.Signals.Traces.Enabled, true)
	sampling := api.FloatValue(svc.Spec.Signals.Traces.SamplingRate, 0.1)
	if p := stack.Spec.Policy.MinSamplingRate; p != nil && sampling < *p {
		out.Diags = append(out.Diags, Diagnostic{
			Level:   "warn",
			Service: name,
			Message: fmt.Sprintf("sampling rate %v is below the stack policy minimum %v; clamped", sampling, *p),
		})
		sampling = *p
	}
	if p := stack.Spec.Policy.MaxSamplingRate; p != nil && sampling > *p {
		out.Diags = append(out.Diags, Diagnostic{
			Level:   "warn",
			Service: name,
			Message: fmt.Sprintf("sampling rate %v exceeds the stack policy maximum %v; clamped", sampling, *p),
		})
		sampling = *p
	}

	// ---- instrumentation ---------------------------------------------------

	otlp := stack.Spec.Instrumentation.Collector
	if otlp == "" {
		otlp = stack.Spec.Backends.Traces.OTLPEndpoint
	}

	attrs := map[string]string{}
	for k, v := range svc.Spec.Attributes {
		attrs[k] = v
	}
	if env != "" {
		if _, set := attrs["deployment.environment"]; !set {
			attrs["deployment.environment"] = env
		}
	}

	otelIn := otel.Input{
		Service:             name,
		Namespace:           ns,
		Team:                svc.Spec.Team,
		Runtime:             facts.Runtime,
		Mode:                resolvedMode,
		OTLPEndpoint:        otlp,
		SamplingRate:        sampling,
		TracesEnabled:       tracesEnabled,
		Attributes:          attrs,
		Labels:              labels,
		InstrumentationName: "lantern-default",
	}

	if resolvedMode == api.ModeAgent || resolvedMode == api.ModeSDKOnly {
		instr, err := otel.BuildInstrumentation(otelIn)
		if err != nil {
			return out, err
		}
		out.Objects = append(out.Objects, instr)
	}

	patch, odiags, err := otel.BuildWorkloadPatch(otelIn)
	if err != nil {
		return out, err
	}
	out.Diags = append(out.Diags, liftOtel(name, odiags)...)
	if patch != nil {
		patch.Kind = targetKind(svc.Spec.Target)
		patch.Name = targetName(svc)
		out.Objects = append(out.Objects, otel.PatchObject(patch, labels))
	}

	// ---- metrics scrape ----------------------------------------------------

	if api.BoolValue(svc.Spec.Signals.Metrics, true) && stack.Spec.Backends.Metrics.Operator != "none" {
		sm, sdiags, err := prom.BuildServiceMonitor(prom.MonitorInput{
			Service:    name,
			Namespace:  ns,
			Selector:   targetSelector(svc),
			Port:       svc.Spec.Target.MetricsPort,
			DenyLabels: stack.Spec.Policy.DenyLabels,
			Labels:     labels,
		})
		if err != nil {
			return out, err
		}
		out.Diags = append(out.Diags, liftProm(name, sdiags)...)
		out.Objects = append(out.Objects, sm)
	}

	// ---- SLOs and burn-rate alerts -----------------------------------------

	slos := svc.Spec.SLOs
	if len(slos) == 0 {
		slos = defaultSLOs(stack)
		switch {
		case len(slos) == 0:
			// nothing to apply
		case !prom.HasSLITemplate(svc.Spec.ServiceKind):
			// Stack defaults are a convenience, not a request. Failing a whole
			// compile because a discovered CronJob has no HTTP semconv metrics
			// would make `lantern discover | lantern synth` unusable on any
			// real cluster, so skip and explain instead.
			out.Diags = append(out.Diags, Diagnostic{
				Level:   "warn",
				Service: name,
				Message: fmt.Sprintf(
					"serviceKind %q has no built-in SLI template, so the ObservabilityStack's default SLOs were skipped; "+
						"declare an SLO with type: custom and your own errorQuery/totalQuery to get burn-rate alerts",
					svc.Spec.ServiceKind),
			})
			slos = nil
		default:
			out.Diags = append(out.Diags, Diagnostic{
				Level:   "info",
				Service: name,
				Message: fmt.Sprintf("no SLOs declared; applying %d default(s) from the ObservabilityStack", len(slos)),
			})
		}
	}

	if len(slos) > 0 {
		selector, sdiag := prom.SeriesSelector(name, ns, env, resolvedMode)
		out.Diags = append(out.Diags, liftProm(name, []prom.Diagnostic{sdiag})...)

		rules, rdiags, err := prom.BuildRules(prom.RuleInput{
			Service:       name,
			Namespace:     ns,
			Team:          svc.Spec.Team,
			ServiceKind:   svc.Spec.ServiceKind,
			Selector:      selector,
			SLOs:          slos,
			Labels:        labels,
			RuleNamespace: stack.Spec.Backends.Metrics.RuleNamespace,
		})
		if err != nil {
			return out, fmt.Errorf("service %q: %w", name, err)
		}
		out.Diags = append(out.Diags, liftProm(name, rdiags)...)
		out.Objects = append(out.Objects, rules)
	}

	// ---- P1 placeholder ----------------------------------------------------

	if !svc.Spec.Dashboard.Disabled {
		out.Diags = append(out.Diags, Diagnostic{
			Level:   "info",
			Service: name,
			Message: "dashboard generation lands in P1; no GrafanaDashboard emitted yet",
		})
	}

	kube.Sort(out.Objects)
	sortDiags(out.Diags)
	return out, nil
}

// CompileAll compiles a set of services, deduplicating shared objects such as
// the per-namespace Instrumentation CR.
func CompileAll(svcs []api.ServiceObservability, stack api.ObservabilityStack, facts map[string]Facts) (Output, error) {
	var all Output
	seen := map[string]bool{}

	ordered := append([]api.ServiceObservability(nil), svcs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Metadata.Namespace != ordered[j].Metadata.Namespace {
			return ordered[i].Metadata.Namespace < ordered[j].Metadata.Namespace
		}
		return ordered[i].Metadata.Name < ordered[j].Metadata.Name
	})

	for _, svc := range ordered {
		out, err := Compile(svc, stack, facts[svc.Metadata.Name])
		if err != nil {
			return all, err
		}
		for _, o := range out.Objects {
			if seen[o.Ref()] {
				continue
			}
			seen[o.Ref()] = true
			all.Objects = append(all.Objects, o)
		}
		all.Diags = append(all.Diags, out.Diags...)
	}

	kube.Sort(all.Objects)
	return all, nil
}

// ---------------------------------------------------------------------------

func resolveMode(requested api.InstrumentationMode, r api.Runtime, ebpf bool) (api.InstrumentationMode, string, error) {
	// Mirrors detect.Mode, kept here so Compile stays pure and importable
	// without the detection package's workload types.
	if requested != "" && requested != api.ModeAuto {
		if requested == api.ModeEBPF && !ebpf {
			return "", "", fmt.Errorf("instrumentation mode ebpf requested but instrumentation.ebpf.enabled is false in the ObservabilityStack")
		}
		return requested, "explicitly requested", nil
	}
	switch r {
	case api.RuntimePreInstr:
		return api.ModeSDKOnly, "workload is already instrumented; configuring the exporter only", nil
	case api.RuntimeJava, api.RuntimeNodeJS, api.RuntimePython, api.RuntimeDotNet:
		return api.ModeAgent, fmt.Sprintf("%s has a mature agent injector with deeper span coverage than eBPF", r), nil
	case api.RuntimeGo:
		if ebpf {
			return api.ModeEBPF, "Go agent injection is uprobe-based and needs OTEL_GO_AUTO_TARGET_EXE; eBPF is simpler", nil
		}
		return api.ModeAgent, "eBPF is disabled at the stack level, falling back to the Go agent", nil
	default:
		if ebpf {
			return api.ModeEBPF, "no agent exists for this runtime; eBPF still yields RED metrics for HTTP, gRPC and SQL", nil
		}
		return api.ModeNone, "runtime is unknown and eBPF is disabled; no instrumentation can be applied automatically", nil
	}
}

func validate(svc api.ServiceObservability, stack api.ObservabilityStack) error {
	if svc.Kind != api.KindServiceObservability {
		return fmt.Errorf("expected kind %s, got %q", api.KindServiceObservability, svc.Kind)
	}
	if svc.Metadata.Name == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if svc.Spec.ServiceKind == "" {
		return fmt.Errorf("service %q: spec.serviceKind is required", svc.Metadata.Name)
	}
	if api.BoolValue(stack.Spec.Policy.RequireTeamLabel, false) && svc.Spec.Team == "" {
		return fmt.Errorf("service %q: spec.team is required by the ObservabilityStack policy", svc.Metadata.Name)
	}
	if svc.Spec.Target.Name == "" && len(svc.Spec.Target.Selector) == 0 {
		return fmt.Errorf("service %q: spec.target needs either a name or a selector", svc.Metadata.Name)
	}
	names := map[string]bool{}
	for _, slo := range svc.Spec.SLOs {
		if slo.Name == "" {
			return fmt.Errorf("service %q: every SLO needs a name", svc.Metadata.Name)
		}
		if names[slo.Name] {
			return fmt.Errorf("service %q: duplicate SLO name %q", svc.Metadata.Name, slo.Name)
		}
		names[slo.Name] = true
	}
	return nil
}

func defaultSLOs(stack api.ObservabilityStack) []api.SLO {
	var out []api.SLO
	if a := stack.Spec.Defaults.SLO.Availability; a != nil {
		out = append(out, api.SLO{Name: "availability", Type: api.SLOAvailability, Objective: *a, Window: "30d"})
	}
	if l := stack.Spec.Defaults.SLO.Latency; l != nil {
		out = append(out, api.SLO{Name: "latency", Type: api.SLOLatency, Objective: l.Objective, Threshold: l.Threshold, Window: "30d"})
	}
	return out
}

func targetKind(t api.Target) string {
	if t.Kind == "" {
		return "Deployment"
	}
	return t.Kind
}

func targetName(svc api.ServiceObservability) string {
	if svc.Spec.Target.Name != "" {
		return svc.Spec.Target.Name
	}
	return svc.Metadata.Name
}

// targetSelector falls back to the conventional `app: <name>` label when no
// explicit selector is given, which is what the zero-config path relies on.
func targetSelector(svc api.ServiceObservability) map[string]string {
	if len(svc.Spec.Target.Selector) > 0 {
		return svc.Spec.Target.Selector
	}
	return map[string]string{"app": targetName(svc)}
}

func liftProm(service string, ds []prom.Diagnostic) []Diagnostic {
	out := make([]Diagnostic, 0, len(ds))
	for _, d := range ds {
		out = append(out, Diagnostic{Level: d.Level, Service: service, Message: strings.TrimSpace(d.Message)})
	}
	return out
}

func liftOtel(service string, ds []otel.Diagnostic) []Diagnostic {
	out := make([]Diagnostic, 0, len(ds))
	for _, d := range ds {
		out = append(out, Diagnostic{Level: d.Level, Service: service, Message: strings.TrimSpace(d.Message)})
	}
	return out
}

// sortDiags orders diagnostics by severity so warnings are never buried under
// a wall of info lines.
func sortDiags(ds []Diagnostic) {
	rank := map[string]int{"error": 0, "warn": 1, "info": 2}
	sort.SliceStable(ds, func(i, j int) bool {
		ri, rj := rank[ds[i].Level], rank[ds[j].Level]
		if ri != rj {
			return ri < rj
		}
		return ds[i].Service < ds[j].Service
	})
}
