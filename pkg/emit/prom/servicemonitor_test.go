package prom

import "testing"

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
