package prom

import (
	"strings"
	"testing"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
)

// TestServiceMonitorWarnsOnUnverifiedPortGuess is the regression test for a
// bug found running discover/synth against a real cluster: when
// target.metricsPort is unset, BuildServiceMonitor silently guessed the port
// name "metrics" at "info" level. `-strict` only fails the build on "warn"
// level diagnostics (see cmd/lantern/main.go), so that guess was invisible
// even under `-strict`, and a Service without a port actually named "metrics"
// got a ServiceMonitor that scrapes nothing, with no error anywhere.
func TestServiceMonitorWarnsOnUnverifiedPortGuess(t *testing.T) {
	obj, diags, err := BuildServiceMonitor(MonitorInput{
		Service:   "checkout",
		Namespace: "prod",
		Selector:  map[string]string{"app": "checkout"},
		// Port intentionally left unset.
	})
	if err != nil {
		t.Fatalf("BuildServiceMonitor: %v", err)
	}
	if obj == nil {
		t.Fatal("expected a ServiceMonitor object even when guessing the port")
	}

	found := false
	for _, d := range diags {
		if d.Level == "warn" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warn-level diagnostic when target.metricsPort is unset and Lantern must guess, got %+v", diags)
	}
}

// TestServiceMonitorNoWarningWhenPortIsExplicit proves the warning is scoped
// to the no-evidence case only — an explicit target.metricsPort must not
// trigger the same diagnostic.
func TestServiceMonitorNoWarningWhenPortIsExplicit(t *testing.T) {
	_, diags, err := BuildServiceMonitor(MonitorInput{
		Service:   "checkout",
		Namespace: "prod",
		Selector:  map[string]string{"app": "checkout"},
		Port:      "metrics",
	})
	if err != nil {
		t.Fatalf("BuildServiceMonitor: %v", err)
	}
	for _, d := range diags {
		if d.Level == "warn" {
			t.Errorf("did not expect a warn diagnostic when target.metricsPort is explicitly set, got %+v", diags)
		}
	}
}

// TestServiceMonitorRequiresSelector guards the existing error path.
func TestServiceMonitorRequiresSelector(t *testing.T) {
	if _, _, err := BuildServiceMonitor(MonitorInput{Service: "checkout", Namespace: "prod"}); err == nil {
		t.Error("expected an error when no selector is provided")
	}
}

// TestServiceMonitorAppliesSeriesLimit is the regression test for a bug found
// during a nuclear-test sweep: policy.maxSeriesPerService was never wired
// into anything the compiler emitted, despite the Policy struct's own doc
// comment promising "the compiler enforces these; it does not merely warn."
// A developer setting maxSeriesPerService got zero actual protection. This
// asserts SeriesLimit now becomes the ServiceMonitor's native sampleLimit
// field, plus the diagnostic that explains its fail-closed behavior.
func TestServiceMonitorAppliesSeriesLimit(t *testing.T) {
	obj, diags, err := BuildServiceMonitor(MonitorInput{
		Service:     "checkout",
		Namespace:   "prod",
		Selector:    map[string]string{"app": "checkout"},
		Port:        "metrics",
		SeriesLimit: 25000,
	})
	if err != nil {
		t.Fatalf("BuildServiceMonitor: %v", err)
	}
	got := obj.YAML()
	if !strings.Contains(got, "sampleLimit: 25000") {
		t.Errorf("expected spec.sampleLimit: 25000 in output, got:\n%s", got)
	}
	found := false
	for _, d := range diags {
		if d.Level == "info" && strings.Contains(d.Message, "sampleLimit") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected an info diagnostic explaining the sampleLimit fail-closed behavior, got %+v", diags)
	}
}

// TestServiceMonitorNoSeriesLimitWhenUnset guards against a regression where
// SeriesLimit: 0 (the zero value, meaning policy.maxSeriesPerService was
// never set) would emit sampleLimit: 0 -- which Prometheus would treat as an
// active zero-sample cap, not "unset", silently killing every scrape.
func TestServiceMonitorNoSeriesLimitWhenUnset(t *testing.T) {
	obj, _, err := BuildServiceMonitor(MonitorInput{
		Service:   "checkout",
		Namespace: "prod",
		Selector:  map[string]string{"app": "checkout"},
		Port:      "metrics",
	})
	if err != nil {
		t.Fatalf("BuildServiceMonitor: %v", err)
	}
	got := obj.YAML()
	if strings.Contains(got, "sampleLimit") {
		t.Errorf("did not expect sampleLimit in output when SeriesLimit is unset, got:\n%s", got)
	}
}

// The ServiceMonitor must pin the job label to "<namespace>/<service>" so it
// agrees with the SLI selector SeriesSelector builds for mode: none. If these
// two ever drift, the recording rules and burn-rate alerts select nothing and
// silently read as always-healthy -- so this test asserts they line up.
func TestServiceMonitorPinsJobToMatchSeriesSelector(t *testing.T) {
	obj, _, err := BuildServiceMonitor(MonitorInput{
		Service:   "llama-70b-server",
		Namespace: "ml",
		Selector:  map[string]string{"app": "llama-70b-server"},
		Port:      "metrics",
	})
	if err != nil {
		t.Fatalf("BuildServiceMonitor: %v", err)
	}
	got := obj.YAML()
	for _, want := range []string{"relabelings:", "action: replace", "targetLabel: job", "replacement: ml/llama-70b-server"} {
		if !strings.Contains(got, want) {
			t.Errorf("ServiceMonitor missing %q, got:\n%s", want, got)
		}
	}
	// The pinned job must be exactly the value the SLI selector matches on.
	sel, _ := SeriesSelector("llama-70b-server", "ml", "", api.ModeNone)
	if !strings.Contains(sel, `job="ml/llama-70b-server"`) {
		t.Errorf("SeriesSelector job does not match the pinned job label: %q", sel)
	}
}
