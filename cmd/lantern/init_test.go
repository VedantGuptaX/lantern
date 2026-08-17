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

func TestRenderValuesNoDataSourcesBlockWhenMetricsOff(t *testing.T) {
	out := renderValues(initAnswers{Metrics: false, Traces: true, Logs: true})
	if strings.Contains(out, "additionalDataSources") {
		t.Errorf("additionalDataSources should not appear when Grafana itself isn't installed:\n%s", out)
	}
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("expected output to contain %q, got:\n%s", needle, haystack)
	}
}
