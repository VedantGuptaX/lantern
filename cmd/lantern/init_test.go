package main

import (
	"bufio"
	"strings"
	"testing"
)

func TestPromptYesNoAcceptsEmptyAsDefault(t *testing.T) {
	cases := []struct {
		name  string
		input string
		def   bool
		want  bool
	}{
		{"empty accepts true default", "\n", true, true},
		{"empty accepts false default", "\n", false, false},
		{"explicit y", "y\n", false, true},
		{"explicit yes", "yes\n", false, true},
		{"explicit n", "n\n", true, false},
		{"explicit no", "no\n", true, false},
		{"case insensitive", "Y\n", false, true},
		{"whitespace trimmed", "  y  \n", false, true},
		{"bad input then valid", "maybe\ny\n", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := bufio.NewReader(strings.NewReader(c.input))
			got, err := promptYesNo(r, &strings.Builder{}, "question?", c.def)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestRunInitPromptsResolvesEachAnswer(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  initAnswers
	}{
		{
			name:  "everything, all defaults (hammer enter)",
			input: "\n\n\n\n\n",
			want:  initAnswers{Metrics: true, Logs: true, Traces: true, SDKAgent: false, GPU: false},
		},
		{
			name:  "metrics only",
			input: "y\nn\nn\nn\n",
			want:  initAnswers{Metrics: true, Logs: false, Traces: false, GPU: false},
		},
		{
			name:  "traces with SDK agent",
			input: "y\nn\ny\ny\nn\n",
			want:  initAnswers{Metrics: true, Logs: false, Traces: true, SDKAgent: true, GPU: false},
		},
		{
			name:  "traces declined skips the SDK-agent sub-question",
			input: "y\ny\nn\nn\n",
			want:  initAnswers{Metrics: true, Logs: true, Traces: false, SDKAgent: false, GPU: false},
		},
		{
			name:  "gpu on",
			input: "y\ny\ny\nn\ny\n",
			want:  initAnswers{Metrics: true, Logs: true, Traces: true, SDKAgent: false, GPU: true},
		},
		{
			// GPU monitoring is a bare ServiceMonitor/PrometheusRule with no
			// standalone value -- meaningless without a real Prometheus to
			// scrape and evaluate it, and (unlike logs/traces) there's no
			// trimmed sub-toggle path for it because its CRDs come from
			// kube-prometheus-stack's own nested crds subchart, which Helm
			// skips entirely when the parent's enabled condition is false.
			// The question must not even be asked when metrics is off --
			// if it were and this input answered "y" to it, GPU would come
			// back true here and the test would catch the regression.
			name:  "metrics declined skips the GPU question entirely",
			input: "n\nn\nn\n",
			want:  initAnswers{Metrics: false, Logs: false, Traces: false, SDKAgent: false, GPU: false},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := runInitPrompts(strings.NewReader(c.input), &strings.Builder{})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestCollectorNeeded(t *testing.T) {
	cases := []struct {
		name string
		a    initAnswers
		want bool
	}{
		{"metrics only, nothing else", initAnswers{Metrics: true}, false},
		{"logs pulls in the collector", initAnswers{Logs: true}, true},
		{"traces pulls in the collector", initAnswers{Traces: true}, true},
		{"SDK agent alone pulls in the collector", initAnswers{SDKAgent: true}, true},
		{"nothing at all", initAnswers{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.collectorNeeded(); got != c.want {
				t.Errorf("collectorNeeded() = %v, want %v", got, c.want)
			}
		})
	}
}

func TestRenderValuesTogglesMatchAnswers(t *testing.T) {
	// Metrics-only: this is the exact scenario that matters most -- a user
	// who wants zero OpenTelemetry footprint at all, not even the collector
	// or operator CRDs/webhook.
	a := initAnswers{Metrics: true}
	out := renderValues(a)

	mustContain(t, out, "kube-prometheus-stack:\n  enabled: true")
	mustContain(t, out, "loki:\n  enabled: false")
	mustContain(t, out, "logsCollector:\n  enabled: false")
	mustContain(t, out, "tempo:\n  enabled: false")
	mustContain(t, out, "collector:\n  enabled: false")
	mustContain(t, out, "opentelemetry-operator:\n  enabled: false")
	mustContain(t, out, "gpuMonitoring:\n  enabled: false")
	// The real bug this regression-tests: values-quickstart.yaml wires
	// Tempo/Loki into Grafana's additionalDataSources unconditionally, which
	// would point a real Grafana at services that were never installed.
	mustContain(t, out, "additionalDataSources: []")
}

func TestRenderValuesDataSourcesMatchEnabledBackends(t *testing.T) {
	cases := []struct {
		name      string
		a         initAnswers
		wantTempo bool
		wantLoki  bool
	}{
		{"traces only", initAnswers{Metrics: true, Traces: true}, true, false},
		{"logs only", initAnswers{Metrics: true, Logs: true}, false, true},
		{"both", initAnswers{Metrics: true, Traces: true, Logs: true}, true, true},
		{"neither", initAnswers{Metrics: true}, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := renderValues(c.a)
			hasTempo := strings.Contains(out, "name: Tempo")
			hasLoki := strings.Contains(out, "name: Loki")
			if hasTempo != c.wantTempo {
				t.Errorf("Tempo datasource present=%v, want %v", hasTempo, c.wantTempo)
			}
			if hasLoki != c.wantLoki {
				t.Errorf("Loki datasource present=%v, want %v", hasLoki, c.wantLoki)
			}
			// UIDs must match what values-quickstart.yaml itself uses, or
			// anything downstream that references tempo-main/loki-main
			// (per-service dashboards, the Investigate dashboard) breaks.
			if c.wantTempo {
				mustContain(t, out, "uid: tempo-main")
			}
			if c.wantLoki {
				mustContain(t, out, "uid: loki-main")
			}
		})
	}
}

// TestOperatorEnabledWheneverACollectorCRIsEmitted is the regression test for
// a real silent-failure bug: init enabled the OTel Collector (and the logs
// collector) for logs/traces while leaving opentelemetry-operator disabled
// unless SDK/agent injection was also chosen. Both of those are
// OpenTelemetryCollector *custom resources* -- without the operator there's
// no CRD and no reconciler, so templates/collector.yaml and
// templates/logs-collector.yaml skip them via lantern.otelCollectorCRDReady
// and the install "succeeds" with nothing shipping any telemetry at all.
func TestOperatorEnabledWheneverACollectorCRIsEmitted(t *testing.T) {
	cases := []struct {
		name string
		a    initAnswers
	}{
		{"logs without SDK agent", initAnswers{Metrics: true, Logs: true}},
		{"traces (eBPF) without SDK agent", initAnswers{Metrics: true, Traces: true}},
		{"logs and traces, still no SDK agent", initAnswers{Metrics: true, Logs: true, Traces: true}},
		{"logs only, no metrics either", initAnswers{Logs: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !c.a.collectorNeeded() {
				t.Fatalf("test setup wrong: expected this combination to need a collector")
			}
			if !c.a.operatorNeeded() {
				t.Errorf("operatorNeeded() = false, but a collector CR will be emitted -- " +
					"the CR would be silently skipped with no operator to own its CRD")
			}
			mustContain(t, renderValues(c.a), "opentelemetry-operator:\n  enabled: true")
		})
	}
}

// The other direction: no collector CR means the operator is real, avoidable
// footprint (CRDs, a webhook, a controller Deployment) and should stay off.
func TestOperatorDisabledWhenNoCollectorCRIsEmitted(t *testing.T) {
	for _, a := range []initAnswers{
		{Metrics: true},
		{Metrics: true, GPU: true},
		{},
	} {
		if a.operatorNeeded() {
			t.Errorf("%+v: operatorNeeded() = true with no collector CR to reconcile", a)
		}
		mustContain(t, renderValues(a), "opentelemetry-operator:\n  enabled: false")
	}
}

// TestGrafanaStaysAvailableForLogsOrTracesWithoutMetrics is the regression
// test for a real UX-breaking bug: Grafana ships ONLY inside the
// kube-prometheus-stack subchart, so answering "no" to the metrics question
// (worded "Prometheus + Grafana") for a logs- or traces-only install
// produced a fully working Loki/Tempo pipeline with no UI anywhere to look
// at it.
func TestGrafanaStaysAvailableForLogsOrTracesWithoutMetrics(t *testing.T) {
	cases := []struct {
		name string
		a    initAnswers
	}{
		{"logs without metrics", initAnswers{Logs: true}},
		{"traces without metrics", initAnswers{Traces: true}},
		{"both without metrics", initAnswers{Logs: true, Traces: true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !c.a.grafanaOnly() {
				t.Fatalf("test setup wrong: expected this combination to trigger grafanaOnly")
			}
			out := renderValues(c.a)
			mustContain(t, out, "kube-prometheus-stack:\n  enabled: true")
			mustContain(t, out, "prometheus:\n    enabled: false")
			mustContain(t, out, "alertmanager:\n    enabled: false")
			mustContain(t, out, "kubeStateMetrics:\n    enabled: false")
			// The upstream subchart's default Grafana datasource is gated
			// only on grafana.enabled, not on its own prometheus.enabled --
			// without this override Grafana would carry a datasource
			// pointed at a Prometheus this combination never installs.
			mustContain(t, out, "defaultDatasourceEnabled: false")
			// Exactly one "grafana:" key -- a second one would silently
			// clobber whichever came first in real YAML parsers.
			if n := strings.Count(out, "\n  grafana:\n"); n != 1 {
				t.Errorf("expected exactly one top-level grafana: key, found %d in:\n%s", n, out)
			}
		})
	}
}

func TestGrafanaOnlyFalseWhenMetricsAnsweredYes(t *testing.T) {
	a := initAnswers{Metrics: true, Logs: true}
	if a.grafanaOnly() {
		t.Error("grafanaOnly() should be false once Metrics is true -- the full bundle covers Grafana already")
	}
	if !a.grafanaNeeded() {
		t.Error("grafanaNeeded() should still be true")
	}
}

func TestGrafanaNotNeededWithNothingSelected(t *testing.T) {
	a := initAnswers{}
	if a.grafanaNeeded() {
		t.Error("grafanaNeeded() should be false with nothing selected at all")
	}
	mustContain(t, renderValues(a), "kube-prometheus-stack:\n  enabled: false")
}

// This used to assert the opposite: that answering "no" to metrics meant no
// Grafana at all, and therefore no additionalDataSources block. That was
// exactly the bug TestGrafanaStaysAvailableForLogsOrTracesWithoutMetrics now
// guards against -- Grafana stays installed via grafanaOnly(), so the
// datasources block correctly still appears. The genuinely-no-Grafana case
// is TestGrafanaNotNeededWithNothingSelected instead.
func TestRenderValuesDataSourcesBlockPresentWhenGrafanaOnly(t *testing.T) {
	out := renderValues(initAnswers{Metrics: false, Traces: true, Logs: true})
	mustContain(t, out, "additionalDataSources:")
	mustContain(t, out, "name: Tempo")
	mustContain(t, out, "name: Loki")
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected output to contain %q, got:\n%s", needle, haystack)
	}
}
