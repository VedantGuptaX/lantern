package otel

import (
	"strings"
	"testing"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
)

func baseInput() Input {
	return Input{
		Service:             "checkout-api",
		Namespace:           "shop",
		Team:                "payments",
		Runtime:             api.RuntimeJava,
		Mode:                api.ModeAgent,
		OTLPEndpoint:        "http://collector:4318",
		SamplingRate:        0.1,
		TracesEnabled:       true,
		InstrumentationName: "lantern-default",
	}
}

// TestEBPFModeLeavesWorkloadUnannotated is the double-instrumentation guard.
//
// If an eBPF-instrumented workload also carried an inject-* annotation, the
// OpenTelemetry Operator would attach an agent on top of the eBPF probe and
// every request would produce two spans and doubled RED metrics — silently.
func TestEBPFModeLeavesWorkloadUnannotated(t *testing.T) {
	in := baseInput()
	in.Mode = api.ModeEBPF

	patch, diags, err := BuildWorkloadPatch(in)
	if err != nil {
		t.Fatalf("BuildWorkloadPatch: %v", err)
	}
	if patch != nil {
		t.Fatalf("eBPF mode must not annotate the workload, got %+v", patch.Annotations)
	}
	if len(diags) == 0 {
		t.Error("the exclusion must be reported, not silent")
	}
}

func TestAgentModeAnnotatesCorrectRuntime(t *testing.T) {
	cases := map[api.Runtime]string{
		api.RuntimeJava:     "instrumentation.opentelemetry.io/inject-java",
		api.RuntimeNodeJS:   "instrumentation.opentelemetry.io/inject-nodejs",
		api.RuntimePython:   "instrumentation.opentelemetry.io/inject-python",
		api.RuntimeDotNet:   "instrumentation.opentelemetry.io/inject-dotnet",
		api.RuntimeGo:       "instrumentation.opentelemetry.io/inject-go",
		api.RuntimePreInstr: "instrumentation.opentelemetry.io/inject-sdk",
	}
	for runtime, wantKey := range cases {
		in := baseInput()
		in.Runtime = runtime
		patch, _, err := BuildWorkloadPatch(in)
		if err != nil {
			t.Fatalf("runtime %q: %v", runtime, err)
		}
		if patch == nil {
			t.Fatalf("runtime %q: expected a workload patch", runtime)
		}
		if _, ok := patch.Annotations[wantKey]; !ok {
			t.Errorf("runtime %q: missing annotation %s, got %v", runtime, wantKey, patch.Annotations)
		}
		if len(patch.Annotations) != 1 {
			t.Errorf("runtime %q: exactly one inject annotation expected, got %v", runtime, patch.Annotations)
		}
	}
}

// TestSDKOnlyNeverInjectsAnAgent: an app that already ships an OTel SDK must
// receive configuration only.
func TestSDKOnlyNeverInjectsAnAgent(t *testing.T) {
	in := baseInput()
	in.Mode = api.ModeSDKOnly
	in.Runtime = api.RuntimeJava // deliberately a runtime with an agent available

	patch, _, err := BuildWorkloadPatch(in)
	if err != nil {
		t.Fatalf("BuildWorkloadPatch: %v", err)
	}
	if _, ok := patch.Annotations["instrumentation.opentelemetry.io/inject-sdk"]; !ok {
		t.Errorf("sdk-only must use inject-sdk, got %v", patch.Annotations)
	}
	for k := range patch.Annotations {
		if strings.Contains(k, "inject-java") {
			t.Error("sdk-only must never inject a language agent")
		}
	}
}

func TestAgentModeRejectsUnknownRuntime(t *testing.T) {
	in := baseInput()
	in.Runtime = api.RuntimeUnknown
	_, _, err := BuildWorkloadPatch(in)
	if err == nil {
		t.Fatal("agent mode with an unknown runtime must be an error, not a silent no-op")
	}
	if !strings.Contains(err.Error(), "spec.instrumentation.runtime") {
		t.Errorf("the error should tell the user how to fix it, got: %v", err)
	}
}

func TestInstrumentationRequiresAnEndpoint(t *testing.T) {
	in := baseInput()
	in.OTLPEndpoint = ""
	if _, err := BuildInstrumentation(in); err == nil {
		t.Fatal("expected an error when no OTLP endpoint is configured")
	}
}

func TestTracesDisabledUsesAlwaysOffSampler(t *testing.T) {
	in := baseInput()
	in.TracesEnabled = false
	obj, err := BuildInstrumentation(in)
	if err != nil {
		t.Fatalf("BuildInstrumentation: %v", err)
	}
	if !strings.Contains(obj.YAML(), "always_off") {
		t.Error("disabling traces must produce an always_off sampler")
	}
}
