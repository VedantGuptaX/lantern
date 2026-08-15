package detect

import (
	"testing"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
)

func TestRuntimeDetectionPriority(t *testing.T) {
	tests := []struct {
		name     string
		override api.Runtime
		workload Workload
		want     api.Runtime
	}{
		{
			name:     "explicit override always wins",
			override: api.RuntimePython,
			workload: Workload{Images: []string{"openjdk:21"}},
			want:     api.RuntimePython,
		},
		{
			name:     "existing annotation is respected, not fought",
			workload: Workload{Annotations: map[string]string{"instrumentation.opentelemetry.io/inject-nodejs": "true"}},
			want:     api.RuntimeNodeJS,
		},
		{
			name:     "an app already exporting OTLP is never given an agent",
			workload: Workload{Env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://c:4318"}, Images: []string{"openjdk:21"}},
			want:     api.RuntimePreInstr,
		},
		{name: "java from image", workload: Workload{Images: []string{"eclipse-temurin:21-jre"}}, want: api.RuntimeJava},
		{name: "node from image", workload: Workload{Images: []string{"node:22-alpine"}}, want: api.RuntimeNodeJS},
		{name: "python from image", workload: Workload{Images: []string{"python:3.12-slim"}}, want: api.RuntimePython},
		{name: "dotnet from image", workload: Workload{Images: []string{"mcr.microsoft.com/dotnet/aspnet:9.0"}}, want: api.RuntimeDotNet},
		{name: "java from command", workload: Workload{Commands: []string{"java -jar /app.jar"}}, want: api.RuntimeJava},
		{name: "python from gunicorn", workload: Workload{Commands: []string{"gunicorn app:main"}}, want: api.RuntimePython},
		{name: "unknown when there is no evidence", workload: Workload{}, want: api.RuntimeUnknown},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Runtime(tc.override, tc.workload)
			if got.Runtime != tc.want {
				t.Errorf("Runtime = %q (evidence: %s), want %q", got.Runtime, got.Evidence, tc.want)
			}
			if got.Evidence == "" {
				t.Error("detection must always report its evidence so the compiler can explain itself")
			}
		})
	}
}

// TestModeSelectionMatrix pins the agent-vs-eBPF decision table from the
// design document. Getting this wrong is how you end up double-instrumenting.
func TestModeSelectionMatrix(t *testing.T) {
	tests := []struct {
		runtime api.Runtime
		ebpf    bool
		want    api.InstrumentationMode
	}{
		{api.RuntimeJava, true, api.ModeAgent},
		{api.RuntimeNodeJS, true, api.ModeAgent},
		{api.RuntimePython, true, api.ModeAgent},
		{api.RuntimeDotNet, true, api.ModeAgent}, // OBI has no .NET support yet
		{api.RuntimeGo, true, api.ModeEBPF},
		{api.RuntimeGo, false, api.ModeAgent}, // falls back when eBPF is off
		{api.RuntimeOther, true, api.ModeEBPF},
		{api.RuntimeUnknown, true, api.ModeEBPF},
		{api.RuntimeUnknown, false, api.ModeNone},
		{api.RuntimePreInstr, true, api.ModeSDKOnly},
	}

	for _, tc := range tests {
		got, why, err := Mode(api.ModeAuto, tc.runtime, tc.ebpf)
		if err != nil {
			t.Fatalf("runtime %q ebpf=%v: %v", tc.runtime, tc.ebpf, err)
		}
		if got != tc.want {
			t.Errorf("runtime %q ebpf=%v: mode = %q, want %q", tc.runtime, tc.ebpf, got, tc.want)
		}
		if why == "" {
			t.Errorf("runtime %q: mode selection must explain itself", tc.runtime)
		}
	}
}

func TestExplicitEBPFRequiresStackSupport(t *testing.T) {
	if _, _, err := Mode(api.ModeEBPF, api.RuntimeGo, false); err == nil {
		t.Fatal("requesting ebpf while the stack disables it must be an error, not a silent fallback")
	}
	if _, _, err := Mode(api.ModeEBPF, api.RuntimeGo, true); err != nil {
		t.Fatalf("ebpf should be allowed when the stack enables it: %v", err)
	}
}
