package compile

import (
	"strings"
	"testing"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
)

// goldenStack is a minimal but complete ObservabilityStack: prometheus-operator
// metrics with a datasource, plus grafana dashboards -- enough for Compile to
// emit a ServiceMonitor, a PrometheusRule and a dashboard. (This test is in the
// internal `compile` package to reach the unexported preset funcs, so it can't
// borrow golden_test.go's loadStack, which lives in the external test package.)
func goldenStack(*testing.T) api.ObservabilityStack {
	return api.ObservabilityStack{
		APIVersion: api.GroupVersion,
		Kind:       "ObservabilityStack",
		Metadata:   api.ObjectMeta{Name: "default"},
		Spec: api.ObservabilityStackSpec{
			Backends: api.Backends{
				Metrics: api.MetricsBackend{
					Type:       "prometheus",
					Operator:   "prometheus-operator",
					Datasource: "prom-main",
				},
				Dashboards: api.DashboardsBackend{Type: "grafana"},
			},
		},
	}
}

func TestInferencePresetFor(t *testing.T) {
	cases := []struct {
		server    api.InferenceServer
		ok        bool
		wantSLOs  []string // SLO names, in order
		wantNote  bool
		metricsIn []string // metric names that must appear across the SLOs
	}{
		{api.InferenceVLLM, true, []string{"ttft", "inter-token-latency", "queue-depth"}, false,
			[]string{"vllm:time_to_first_token_seconds", "vllm:time_per_output_token_seconds", "vllm:num_requests_waiting"}},
		{api.InferenceNIM, true, []string{"ttft", "inter-token-latency", "queue-depth"}, true,
			[]string{"vllm:time_to_first_token_seconds"}},
		{api.InferenceTriton, true, []string{"queue-depth"}, true,
			[]string{"nv_inference_pending_request_count"}},
		{api.InferenceTGI, true, []string{"inter-token-latency", "queue-depth"}, true,
			[]string{"tgi_request_mean_time_per_token_duration", "tgi_queue_size"}},
		{api.InferenceServer("sglang"), false, nil, false, nil},
		{api.InferenceServer(""), false, nil, false, nil},
	}
	for _, c := range cases {
		t.Run(string(c.server), func(t *testing.T) {
			p, ok := inferencePresetFor(c.server)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v", ok, c.ok)
			}
			if !ok {
				return
			}
			var gotNames []string
			var gotMetrics []string
			for _, s := range p.slos {
				gotNames = append(gotNames, s.Name)
				gotMetrics = append(gotMetrics, s.Metric)
				// Every preset SLO must carry an explicit metric -- that's the
				// whole point (inference has no semconv family to fall back on).
				if strings.TrimSpace(s.Metric) == "" {
					t.Errorf("SLO %q has no explicit metric", s.Name)
				}
			}
			if strings.Join(gotNames, ",") != strings.Join(c.wantSLOs, ",") {
				t.Errorf("SLO names = %v, want %v", gotNames, c.wantSLOs)
			}
			for _, m := range c.metricsIn {
				found := false
				for _, gm := range gotMetrics {
					if gm == m {
						found = true
					}
				}
				if !found {
					t.Errorf("expected metric %q among preset SLOs, got %v", m, gotMetrics)
				}
			}
			if (p.note != "") != c.wantNote {
				t.Errorf("note present = %v, want %v (note=%q)", p.note != "", c.wantNote, p.note)
			}
		})
	}
}

func TestValidInferenceServer(t *testing.T) {
	for _, s := range []api.InferenceServer{api.InferenceVLLM, api.InferenceTriton, api.InferenceNIM, api.InferenceTGI} {
		if !validInferenceServer(s) {
			t.Errorf("%q should be valid", s)
		}
	}
	for _, s := range []api.InferenceServer{"", "sglang", "openai"} {
		if validInferenceServer(s) {
			t.Errorf("%q should be invalid", s)
		}
	}
}

func inferenceSvc(server api.InferenceServer, kind api.ServiceKind) api.ServiceObservability {
	return api.ServiceObservability{
		APIVersion: api.GroupVersion,
		Kind:       api.KindServiceObservability,
		Metadata:   api.ObjectMeta{Name: "llm", Namespace: "ml"},
		Spec: api.ServiceObservabilitySpec{
			Target:          api.Target{Kind: "Deployment", Name: "llm", MetricsPort: "metrics"},
			ServiceKind:     kind,
			InferenceServer: server,
			Team:            "ml",
		},
	}
}

// A serviceKind: inference service with a known server and no SLOs must get the
// preset's SLOs turned into a PrometheusRule + a dashboard, plus a review warn.
func TestCompileAppliesInferencePreset(t *testing.T) {
	out, err := Compile(inferenceSvc(api.InferenceVLLM, api.KindInference), goldenStack(t), Facts{})
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	y := out.YAML()
	for _, want := range []string{"vllm:time_to_first_token_seconds", "vllm:num_requests_waiting", "PrometheusRule"} {
		if !strings.Contains(y, want) {
			t.Errorf("compiled output missing %q", want)
		}
	}
	var sawInfo, sawWarn bool
	for _, d := range out.Diags {
		if d.Level == "info" && strings.Contains(d.Message, "built-in preset") {
			sawInfo = true
		}
		if d.Level == "warn" && strings.Contains(d.Message, "thresholds are defaults") {
			sawWarn = true
		}
	}
	if !sawInfo {
		t.Error("expected an info diagnostic noting the preset was applied")
	}
	if !sawWarn {
		t.Error("expected a warn diagnostic telling the user to review the default thresholds")
	}
}

// inferenceServer on a non-inference serviceKind, or an unknown server, must be
// rejected at validation rather than silently ignored.
func TestCompileRejectsMisusedInferenceServer(t *testing.T) {
	if _, err := Compile(inferenceSvc(api.InferenceVLLM, api.KindHTTP), goldenStack(t), Facts{}); err == nil {
		t.Error("expected error: inferenceServer set on serviceKind: http")
	}
	if _, err := Compile(inferenceSvc(api.InferenceServer("sglang"), api.KindInference), goldenStack(t), Facts{}); err == nil {
		t.Error("expected error: unknown inferenceServer")
	}
}
