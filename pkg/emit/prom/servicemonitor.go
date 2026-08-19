package prom

import (
	"fmt"
	"sort"
	"strings"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
	"github.com/VedantGuptaX/lantern/pkg/kube"
	"github.com/VedantGuptaX/lantern/pkg/yamlx"
)

// MonitorInput is the pre-resolved input for scrape configuration.
type MonitorInput struct {
	Service    string
	Namespace  string
	Selector   map[string]string
	Port       string
	Interval   string
	DenyLabels []string
	Labels     map[string]string
	// SeriesLimit maps to policy.maxSeriesPerService (0 means unset -- no
	// limit). See the comment above BuildServiceMonitor for why this exists.
	SeriesLimit int
}

// BuildServiceMonitor emits a Prometheus Operator ServiceMonitor.
//
// Cardinality control is a compiler responsibility, not a developer one: the
// stack policy's denyLabels are compiled into metricRelabelings here so a
// developer cannot accidentally ship a user_id label into Prometheus, and
// SeriesLimit (policy.maxSeriesPerService) becomes the ServiceMonitor's own
// native spec.sampleLimit.
//
// FOUND BY GREPPING FOR IT, NOT BY READING THE CODE: the Policy struct's own
// doc comment claims "guardrails developers cannot exceed. The compiler
// enforces these; it does not merely warn" -- but maxSeriesPerService and
// maxRouteCardinality were referenced nowhere in pkg/compile or pkg/emit at
// all. A developer setting maxSeriesPerService: 25000 got zero actual
// protection; a service could emit unbounded cardinality and nothing in
// Lantern would stop it, despite the explicit promise. sampleLimit is
// Prometheus Operator's own real mechanism for exactly this ("a per-scrape
// limit on the number of scraped samples that will be accepted" -- verified
// against the ServiceMonitor CRD's own schema before wiring this in, not
// assumed): when a scrape exceeds it, Prometheus marks the WHOLE scrape as
// failed rather than partially ingesting, which is the correct
// fail-closed behavior for a hard cap.
func BuildServiceMonitor(in MonitorInput) (*kube.Object, []Diagnostic, error) {
	if len(in.Selector) == 0 {
		return nil, nil, fmt.Errorf("service %q: cannot build a ServiceMonitor without a target selector", in.Service)
	}

	var diags []Diagnostic
	port := in.Port
	if port == "" {
		port = "metrics"
		// This is a guess with zero evidence behind it — Lantern has no
		// cluster access to check whether the target Service actually has a
		// port named "metrics" (see the compiler's I/O-free boundary). Get
		// this wrong and the ServiceMonitor scrapes nothing, silently: no
		// error, no missing resource, just an empty target list that looks
		// identical to "everything is fine" until someone checks Prometheus's
		// target page. That risk belongs at "warn" so `-strict` catches it,
		// not "info" where it reads as routine.
		diags = append(diags, Diagnostic{
			Level:   "warn",
			Message: fmt.Sprintf("target.metricsPort not set; guessing port name %q with no evidence it exists on the target Service — verify it exists (kubectl get svc -o yaml) or set target.metricsPort explicitly, or this ServiceMonitor may scrape nothing", port),
		})
	}
	interval := in.Interval
	if interval == "" {
		interval = "30s"
	}

	endpoint := yamlx.NewMap(
		"port", yamlx.S(port),
		"interval", yamlx.S(interval),
		"path", yamlx.S("/metrics"),
	)

	// Pin the `job` label to "<namespace>/<service>". This is not cosmetic: for
	// instrumentation mode: none, SeriesSelector below builds the SLI's label
	// matcher as job="<namespace>/<service>", but the Prometheus Operator's
	// default job label for a ServiceMonitor target is the scraped Service's
	// name (no namespace prefix) -- so without this relabeling the recording
	// rules and burn-rate alerts select nothing and read as "always healthy",
	// exactly the silent failure SeriesSelector's own comment warns about. A
	// replace action with no sourceLabels sets the target label to the literal
	// replacement. (For agent/eBPF modes the SLI selects on service_name
	// instead, so pinning job is harmless there and keeps the label consistent.)
	endpoint.Set("relabelings", yamlx.NewSeq(
		yamlx.NewMap(
			"action", yamlx.S("replace"),
			"targetLabel", yamlx.S("job"),
			"replacement", yamlx.S(in.Namespace+"/"+in.Service),
		),
	))

	if len(in.DenyLabels) > 0 {
		deny := append([]string(nil), in.DenyLabels...)
		sort.Strings(deny)
		endpoint.Set("metricRelabelings", yamlx.NewSeq(
			yamlx.NewMap(
				"action", yamlx.S("labeldrop"),
				"regex", yamlx.S(strings.Join(deny, "|")),
			),
		))
		diags = append(diags, Diagnostic{
			Level: "info",
			Message: fmt.Sprintf("dropping %d policy-denied label(s) at scrape time: %s",
				len(deny), strings.Join(deny, ", ")),
		})
	}

	spec := yamlx.NewMap(
		"selector", yamlx.NewMap("matchLabels", kube.SortedStringMap(in.Selector)),
		"namespaceSelector", yamlx.NewMap("matchNames", yamlx.Strings(in.Namespace)),
		"endpoints", yamlx.NewSeq(endpoint),
	)
	if in.SeriesLimit > 0 {
		spec.Set("sampleLimit", yamlx.I(in.SeriesLimit))
		// sampleLimit fails CLOSED: Prometheus drops the ENTIRE scrape, not
		// just the excess series, once the limit is exceeded -- correct for
		// a hard cap, but a service that legitimately has more series than
		// this goes completely dark rather than partially degraded, which
		// is worth surfacing rather than leaving as a silent policy effect.
		diags = append(diags, Diagnostic{
			Level: "info",
			Message: fmt.Sprintf(
				"sampleLimit set to %d from policy.maxSeriesPerService: if this service's real "+
					"series count ever exceeds it, Prometheus fails the WHOLE scrape rather than "+
					"truncating it, and this service's metrics go dark until it's back under the limit",
				in.SeriesLimit),
		})
	}

	obj := kube.New(
		"monitoring.coreos.com/v1", "ServiceMonitor",
		in.Service, in.Namespace, in.Labels,
	).Set("spec", spec)

	return obj, diags, nil
}

// SeriesSelector builds the PromQL label selector identifying this service.
//
// Which labels exist depends on how the metrics arrived: telemetry flowing
// through the OTel collector carries resource attributes (service_name,
// deployment_environment), whereas metrics scraped directly by Prometheus
// carry job/namespace. Choosing wrong produces alerts that silently never
// fire, so the choice is explicit and reported as a diagnostic.
func SeriesSelector(service, namespace, environment string, mode api.InstrumentationMode) (string, Diagnostic) {
	if mode == api.ModeNone {
		return fmt.Sprintf(`job="%s/%s", namespace="%s"`, namespace, service, namespace),
			Diagnostic{
				Level:   "info",
				Message: "instrumentation is disabled, so SLI queries select on job/namespace from the ServiceMonitor scrape",
			}
	}
	sel := fmt.Sprintf(`service_name="%s"`, service)
	if environment != "" {
		sel += fmt.Sprintf(`, deployment_environment="%s"`, environment)
	}
	return sel, Diagnostic{
		Level: "info",
		Message: fmt.Sprintf(
			"SLI queries select on OpenTelemetry resource attributes (%s); "+
				"ensure your collector exports these as Prometheus labels", sel),
	}
}
