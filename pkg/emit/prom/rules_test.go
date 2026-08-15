package prom

import (
	"strings"
	"testing"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
)

// TestBurnRateThresholds checks the arithmetic behind every generated alert.
//
// A burn-rate threshold that is off by a factor of ten produces an alert that
// either never fires or pages constantly. Neither failure is visible by
// reading the YAML, so it is checked numerically here.
func TestBurnRateThresholds(t *testing.T) {
	// 99.9% availability over 30d => error budget 0.1% = 0.001.
	const budget = 0.001

	want := map[string]string{
		"5m/1h":  "0.0144", // 14.4 * 0.001
		"30m/6h": "0.006",  // 6    * 0.001
		"2h/1d":  "0.003",  // 3    * 0.001
		"6h/3d":  "0.001",  // 1    * 0.001
	}

	for _, bw := range DefaultBurnWindows {
		key := bw.Short + "/" + bw.Long
		got := trimFloat(bw.Factor * budget)
		if want[key] != got {
			t.Errorf("burn window %s: threshold = %s, want %s", key, got, want[key])
		}
	}
}

// TestTrimFloatRemovesBinaryNoise guards against 14.4*0.001 rendering as
// 0.014400000000000003 in emitted PromQL.
func TestTrimFloatRemovesBinaryNoise(t *testing.T) {
	cases := map[float64]string{
		14.4 * 0.001: "0.0144",
		6 * 0.001:    "0.006",
		0.999:        "0.999",
		30:           "30",
		0.001:        "0.001",
	}
	for in, want := range cases {
		if got := trimFloat(in); got != want {
			t.Errorf("trimFloat(%v) = %q, want %q", in, got, want)
		}
	}
}

// TestPromLabelNamesAreValid is the regression test for a bug that would have
// made Prometheus reject the entire generated rule file: Kubernetes label
// keys like `lantern.dev/team` are not valid Prometheus label names.
func TestPromLabelNamesAreValid(t *testing.T) {
	obj, _, err := BuildRules(RuleInput{
		Service:     "checkout-api",
		Namespace:   "shop",
		Team:        "payments",
		ServiceKind: api.KindHTTP,
		Selector:    `service_name="checkout-api"`,
		SLOs:        []api.SLO{{Name: "availability", Type: api.SLOAvailability, Objective: 99.9, Window: "30d"}},
	})
	if err != nil {
		t.Fatalf("BuildRules: %v", err)
	}

	for _, line := range strings.Split(obj.YAML(), "\n") {
		trimmed := strings.TrimSpace(line)
		// Only inspect lines inside a `labels:` block, which are `key: value`.
		if !strings.Contains(trimmed, ": ") || strings.HasPrefix(trimmed, "- ") {
			continue
		}
		key := strings.TrimSpace(strings.SplitN(trimmed, ":", 2)[0])
		if strings.HasPrefix(key, "lantern") && strings.Contains(key, ".") {
			t.Errorf("emitted an invalid Prometheus label name: %q", key)
		}
	}

	if strings.Contains(obj.YAML(), api.TeamLabel+":") {
		t.Errorf("Kubernetes-style team label leaked into PromQL rule labels")
	}
}

// TestLatencyBucketBound checks the `le` selector matches how Prometheus
// renders histogram bucket bounds. A mismatch here silently selects nothing,
// which reads as "the service is perfectly fast".
func TestLatencyBucketBound(t *testing.T) {
	cases := map[string]string{
		"300ms": "0.3",
		"1s":    "1",
		"2.5s":  "2.5",
		"50ms":  "0.05",
	}
	for in, want := range cases {
		got, err := bucketBound(in)
		if err != nil {
			t.Fatalf("bucketBound(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("bucketBound(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := bucketBound("nonsense"); err == nil {
		t.Error("expected an error for an unparseable threshold")
	}
	if _, err := bucketBound("-1s"); err == nil {
		t.Error("expected an error for a negative threshold")
	}
}

func TestParseWindowSupportsDays(t *testing.T) {
	d, err := ParseWindow("30d")
	if err != nil {
		t.Fatalf("ParseWindow: %v", err)
	}
	if d.Hours() != 720 {
		t.Errorf("30d = %v hours, want 720", d.Hours())
	}
	if _, err := ParseWindow("bogus"); err == nil {
		t.Error("expected an error for an invalid window")
	}
}

func TestCustomSLORequiresBothQueries(t *testing.T) {
	_, err := BuildSLI(api.SLO{Name: "x", Type: api.SLOCustom, ErrorQuery: "a"}, api.KindCustom, "")
	if err == nil {
		t.Fatal("expected an error when totalQuery is missing")
	}
}

func TestUnsupportedServiceKindIsRejected(t *testing.T) {
	_, err := BuildSLI(api.SLO{Name: "x", Type: api.SLOAvailability, Objective: 99}, api.KindCustom, "")
	if err == nil {
		t.Fatal("expected an error: serviceKind custom has no built-in SLI template")
	}
	if !strings.Contains(err.Error(), "type: custom") {
		t.Errorf("error should point the user at the custom SLO escape hatch, got: %v", err)
	}
}

func TestObjectiveOutOfRangeIsRejected(t *testing.T) {
	for _, obj := range []float64{0, 100, 100.5, -1} {
		_, _, err := BuildRules(RuleInput{
			Service:     "s",
			ServiceKind: api.KindHTTP,
			SLOs:        []api.SLO{{Name: "a", Type: api.SLOAvailability, Objective: obj}},
		})
		if err == nil {
			t.Errorf("objective %v should have been rejected", obj)
		}
	}
}
