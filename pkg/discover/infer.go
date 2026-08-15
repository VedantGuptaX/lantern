package discover

import (
	"fmt"
	"sort"
	"strings"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
	"github.com/VedantGuptaX/lantern/pkg/detect"
	"github.com/VedantGuptaX/lantern/pkg/kube"
	"github.com/VedantGuptaX/lantern/pkg/yamlx"
)

// SkipNamespaces are excluded by default. These hold platform components that
// a team does not own and should not be asked to define SLOs for; discovering
// them buries the twelve services the user actually cares about.
var SkipNamespaces = []string{
	"kube-system", "kube-public", "kube-node-lease",
	"cert-manager", "ingress-nginx", "istio-system", "linkerd",
	"monitoring", "observability", "logging",
	"flux-system", "argocd", "kube-flannel", "calico-system", "tigera-operator",
	"gatekeeper-system", "velero", "local-path-storage",
}

// Note records something the discoverer guessed rather than knew, so a human
// reviews it before the draft is applied.
type Note struct {
	Workload string
	Field    string
	Value    string
	Reason   string
	// LowConfidence marks a guess that is more likely than not to need
	// editing. These are surfaced prominently rather than mixed into the rest.
	LowConfidence bool
}

// Result is one discovered service plus the reasoning behind it.
type Result struct {
	Service api.ServiceObservability
	Notes   []Note
}

// Options tunes discovery.
type Options struct {
	// IncludeSystem disables the SkipNamespaces filter.
	IncludeSystem bool
	// Namespaces, when non-empty, restricts discovery to these namespaces.
	Namespaces []string
	// DefaultTeam is used when no ownership label can be found.
	DefaultTeam string
}

// Discover converts workloads into draft specs.
func Discover(workloads []Workload, opts Options) []Result {
	nsFilter := map[string]bool{}
	for _, n := range opts.Namespaces {
		nsFilter[n] = true
	}
	skip := map[string]bool{}
	if !opts.IncludeSystem {
		for _, n := range SkipNamespaces {
			skip[n] = true
		}
	}

	var out []Result
	for _, w := range workloads {
		if len(nsFilter) > 0 && !nsFilter[w.Namespace] {
			continue
		}
		if skip[w.Namespace] {
			continue
		}
		out = append(out, discoverOne(w, opts))
	}
	return out
}

func discoverOne(w Workload, opts Options) Result {
	var notes []Note
	note := func(field, value, reason string, low bool) {
		notes = append(notes, Note{Workload: w.String(), Field: field, Value: value, Reason: reason, LowConfidence: low})
	}

	// --- runtime -----------------------------------------------------------

	rt := detect.Runtime("", detect.Workload{
		Images:      w.Images(),
		Commands:    w.Commands(),
		Env:         w.Env(),
		Annotations: w.Annotations,
	})
	if rt.Runtime == api.RuntimeUnknown {
		note("instrumentation.runtime", "(unset)",
			"no runtime evidence in image or command; the compiler will fall back to eBPF", true)
	} else {
		note("instrumentation.runtime", string(rt.Runtime), rt.Evidence, false)
	}

	// --- service kind ------------------------------------------------------

	kind, kindWhy, kindLow := inferServiceKind(w)
	note("serviceKind", string(kind), kindWhy, kindLow)

	// --- team --------------------------------------------------------------

	team, teamWhy := inferTeam(w, opts.DefaultTeam)
	if team == "" {
		note("team", "(unset)",
			"no ownership label found; alerts cannot be routed until this is set", true)
	} else {
		note("team", team, teamWhy, false)
	}

	// --- metrics port ------------------------------------------------------

	metricsPort, portWhy, portLow := inferMetricsPort(w)
	if metricsPort == "" {
		note("target.metricsPort", "(unset)",
			"no port named metrics and no prometheus.io/port annotation; scrape config will guess", true)
	} else {
		note("target.metricsPort", metricsPort, portWhy, portLow)
	}

	// --- selector ----------------------------------------------------------

	selector := w.Selector
	if len(selector) == 0 {
		selector = map[string]string{"app": w.Name}
		note("target.selector", "app="+w.Name,
			"workload declared no selector; falling back to the conventional app label", true)
	}

	spec := api.ServiceObservability{
		APIVersion: api.GroupVersion,
		Kind:       api.KindServiceObservability,
		Metadata: api.ObjectMeta{
			Name:      w.Name,
			Namespace: w.Namespace,
			Annotations: map[string]string{
				"lantern.dev/discovered-from": w.Kind,
			},
		},
		Spec: api.ServiceObservabilitySpec{
			Target: api.Target{
				Kind:        w.Kind,
				Name:        w.Name,
				Selector:    selector,
				MetricsPort: metricsPort,
			},
			ServiceKind: kind,
			Team:        team,
			Instrumentation: api.InstrumentationCfg{
				Runtime: rt.Runtime,
			},
		},
	}

	// SLOs are deliberately left empty. The compiler fills them from the
	// ObservabilityStack defaults, and inventing per-service objectives from a
	// manifest would be a guess presented as a commitment.
	return Result{Service: spec, Notes: notes}
}

// infraImages are off-the-shelf datastores and brokers. They are not
// application services: they do not emit OpenTelemetry HTTP semconv metrics,
// and generating an availability SLO against `http_server_request_duration`
// for a Redis pod produces an alert that can never fire. Recognising them and
// saying so is far more useful than a confident wrong guess.
var infraImages = map[string]string{
	"redis": "database", "valkey": "database", "memcached": "database",
	"postgres": "database", "mysql": "database", "mariadb": "database",
	"mongo": "database", "cassandra": "database", "elasticsearch": "database",
	"clickhouse": "database", "cockroach": "database", "etcd": "database",
	"rabbitmq": "worker", "kafka": "worker", "nats": "worker", "pulsar": "worker",
}

func matchInfraImage(images []string) (api.ServiceKind, string, bool) {
	for _, img := range images {
		l := strings.ToLower(img)
		// Compare against the image name only, so a registry path like
		// registry.internal/team-redis/checkout does not match.
		base := l
		if i := strings.LastIndex(base, "/"); i >= 0 {
			base = base[i+1:]
		}
		for name, kind := range infraImages {
			if base == name || strings.HasPrefix(base, name+":") || strings.HasPrefix(base, name+"-") {
				return api.ServiceKind(kind), fmt.Sprintf("image %q is an off-the-shelf %s", img, kind), true
			}
		}
	}
	return "", "", false
}

// inferServiceKind guesses what the workload does from its ports and name.
func inferServiceKind(w Workload) (api.ServiceKind, string, bool) {
	if w.Kind == "CronJob" {
		return api.KindCron, "workload is a CronJob", false
	}

	if kind, why, ok := matchInfraImage(w.Images()); ok {
		return kind, why + "; it will not emit OpenTelemetry HTTP metrics, so consider type: custom SLOs or an exporter", true
	}

	ports := w.Ports()
	for _, p := range ports {
		n := strings.ToLower(p.Name)
		if n == "grpc" || strings.HasPrefix(n, "grpc-") || p.Port == 50051 {
			return api.KindGRPC, fmt.Sprintf("container port %q", p.Name), false
		}
	}
	for _, p := range ports {
		n := strings.ToLower(p.Name)
		if n == "http" || n == "web" || n == "api" || strings.HasPrefix(n, "http-") {
			if n == "http-metrics" {
				continue
			}
			return api.KindHTTP, fmt.Sprintf("container port %q", p.Name), false
		}
	}

	name := strings.ToLower(w.Name)
	for _, hint := range []string{"worker", "consumer", "processor", "ingest", "queue"} {
		if strings.Contains(name, hint) {
			return api.KindWorker, fmt.Sprintf("workload name contains %q", hint), true
		}
	}

	// A workload with no serving port is most likely a background consumer.
	serving := 0
	for _, p := range ports {
		if !isMetricsPortName(p.Name) {
			serving++
		}
	}
	if serving == 0 {
		return api.KindWorker, "workload exposes no serving port", true
	}

	for _, p := range ports {
		if p.Port == 80 || p.Port == 8080 || p.Port == 8000 || p.Port == 3000 {
			return api.KindHTTP, fmt.Sprintf("container port %d is a conventional HTTP port", p.Port), true
		}
	}

	return api.KindHTTP, "defaulted; no conclusive port or name evidence", true
}

// teamLabels are checked in order of how deliberate they are as an ownership
// signal.
var teamLabels = []string{
	"team", "lantern.dev/team", "squad", "owner",
	"app.kubernetes.io/part-of", "backstage.io/owner",
}

func inferTeam(w Workload, fallback string) (string, string) {
	for _, key := range teamLabels {
		if v, ok := w.Labels[key]; ok && v != "" {
			return v, fmt.Sprintf("label %s=%s", key, v)
		}
	}
	for _, key := range teamLabels {
		if v, ok := w.Annotations[key]; ok && v != "" {
			return v, fmt.Sprintf("annotation %s=%s", key, v)
		}
	}
	if fallback != "" {
		return fallback, "--team flag"
	}
	return "", ""
}

func isMetricsPortName(n string) bool {
	l := strings.ToLower(n)
	return l == "metrics" || l == "prometheus" || l == "http-metrics" || l == "monitoring"
}

func inferMetricsPort(w Workload) (string, string, bool) {
	for _, p := range w.Ports() {
		if isMetricsPortName(p.Name) {
			return p.Name, fmt.Sprintf("container port named %q", p.Name), false
		}
	}

	// Honour the long-standing prometheus.io annotations if present.
	if portStr, ok := w.Annotations["prometheus.io/port"]; ok && portStr != "" {
		for _, p := range w.Ports() {
			if fmt.Sprint(p.Port) == portStr && p.Name != "" {
				return p.Name, fmt.Sprintf("prometheus.io/port=%s matches port %q", portStr, p.Name), false
			}
		}
		return "", "", true
	}

	// A single serving port is a reasonable guess, but only when that port
	// plausibly speaks HTTP. Assuming Redis on 6379 serves /metrics produces
	// a ServiceMonitor that scrapes a binary protocol and fails forever.
	var serving []Port
	for _, p := range w.Ports() {
		if !isMetricsPortName(p.Name) && p.Name != "" && looksHTTP(p) {
			serving = append(serving, p)
		}
	}
	if len(serving) == 1 {
		return serving[0].Name, fmt.Sprintf("only one HTTP-like port (%q); assuming it also serves /metrics", serving[0].Name), true
	}

	return "", "", true
}

// looksHTTP reports whether a port plausibly serves HTTP, and therefore might
// also serve a Prometheus endpoint.
func looksHTTP(p Port) bool {
	switch strings.ToLower(p.Name) {
	case "http", "web", "api", "admin", "management":
		return true
	}
	if strings.HasPrefix(strings.ToLower(p.Name), "http-") {
		return true
	}
	switch p.Port {
	case 80, 443, 3000, 4000, 5000, 8000, 8001, 8080, 8081, 8443, 9000:
		return true
	}
	return false
}

// Render emits draft specs as a multi-document YAML stream, with the
// discoverer's reasoning inline as comments. The comments matter: a spec a
// human never reads is a spec a human never corrects.
func Render(results []Result) string {
	var b strings.Builder
	b.WriteString("# Generated by `lantern discover`. Review before applying.\n")
	b.WriteString("# Every value below is inferred; low-confidence guesses are marked REVIEW.\n")

	for i, r := range results {
		if i > 0 {
			b.WriteString("---\n")
		} else {
			b.WriteString("---\n")
		}
		for _, n := range r.Notes {
			marker := "#"
			if n.LowConfidence {
				marker = "# REVIEW"
			}
			b.WriteString(fmt.Sprintf("%s %s: %s (%s)\n", marker, n.Field, n.Value, n.Reason))
		}
		b.WriteString(renderSpec(r.Service))
	}
	return b.String()
}

func renderSpec(s api.ServiceObservability) string {
	meta := yamlx.NewMap("name", yamlx.S(s.Metadata.Name))
	if s.Metadata.Namespace != "" {
		meta.Set("namespace", yamlx.S(s.Metadata.Namespace))
	}
	if len(s.Metadata.Annotations) > 0 {
		meta.Set("annotations", kube.SortedStringMap(s.Metadata.Annotations))
	}

	target := yamlx.NewMap("kind", yamlx.S(s.Spec.Target.Kind), "name", yamlx.S(s.Spec.Target.Name))
	if len(s.Spec.Target.Selector) > 0 {
		target.Set("selector", kube.SortedStringMap(s.Spec.Target.Selector))
	}
	if s.Spec.Target.MetricsPort != "" {
		target.Set("metricsPort", yamlx.S(s.Spec.Target.MetricsPort))
	}

	spec := yamlx.NewMap(
		"target", target,
		"serviceKind", yamlx.S(string(s.Spec.ServiceKind)),
	)
	if s.Spec.Team != "" {
		spec.Set("team", yamlx.S(s.Spec.Team))
	}
	if s.Spec.Instrumentation.Runtime != "" {
		spec.Set("instrumentation", yamlx.NewMap("runtime", yamlx.S(string(s.Spec.Instrumentation.Runtime))))
	}

	return yamlx.Encode(yamlx.NewMap(
		"apiVersion", yamlx.S(s.APIVersion),
		"kind", yamlx.S(s.Kind),
		"metadata", meta,
		"spec", spec,
	))
}

// Summary reports what needs human review, grouped so the output is short
// even across a hundred services.
func Summary(results []Result) []string {
	byField := map[string][]string{}
	for _, r := range results {
		for _, n := range r.Notes {
			if n.LowConfidence {
				byField[n.Field] = append(byField[n.Field], r.Service.Metadata.Name)
			}
		}
	}

	fields := make([]string, 0, len(byField))
	for f := range byField {
		fields = append(fields, f)
	}
	sort.Strings(fields)

	var out []string
	for _, f := range fields {
		svcs := byField[f]
		sort.Strings(svcs)
		shown := svcs
		suffix := ""
		if len(shown) > 5 {
			shown = shown[:5]
			suffix = fmt.Sprintf(" (+%d more)", len(svcs)-5)
		}
		out = append(out, fmt.Sprintf("%-26s needs review on %d service(s): %s%s",
			f, len(svcs), strings.Join(shown, ", "), suffix))
	}
	return out
}
