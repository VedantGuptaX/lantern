// Package detect resolves the application runtime and the instrumentation
// mode that follows from it.
//
// Detection is impure by nature — it reads workload state — so it lives
// outside pkg/compile and produces a Facts value that the compiler consumes.
// This keeps Compile a pure function.
package detect

import (
	"fmt"
	"strings"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
)

// Workload is the subset of a pod spec detection needs. The CLI populates it
// from a manifest or from flags; the P2 operator will populate it from a live
// lookup.
type Workload struct {
	Images      []string
	Commands    []string
	Env         map[string]string
	Annotations map[string]string
}

// Result carries the detected runtime plus the evidence behind it, so the
// compiler can explain itself rather than appearing to guess.
type Result struct {
	Runtime  api.Runtime
	Evidence string
}

// Runtime resolves the application runtime from workload evidence, in priority
// order. The explicit override always wins; a wrong heuristic must always be
// correctable without a code change.
func Runtime(override api.Runtime, w Workload) Result {
	if override != api.RuntimeUnknown {
		return Result{Runtime: override, Evidence: "spec.instrumentation.runtime override"}
	}

	// Respect instrumentation that already exists rather than fighting it.
	for k := range w.Annotations {
		if strings.HasPrefix(k, "instrumentation.opentelemetry.io/inject-") {
			lang := strings.TrimPrefix(k, "instrumentation.opentelemetry.io/inject-")
			if r, ok := runtimeFromName(lang); ok {
				return Result{Runtime: r, Evidence: "existing annotation " + k}
			}
		}
	}

	// An app already exporting OTLP must not receive an agent on top.
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_SERVICE_NAME"} {
		if _, ok := w.Env[k]; ok {
			return Result{
				Runtime:  api.RuntimePreInstr,
				Evidence: fmt.Sprintf("%s is already set on the workload", k),
			}
		}
	}

	for _, img := range w.Images {
		if r, ok := runtimeFromImage(img); ok {
			return Result{Runtime: r, Evidence: "container image " + img}
		}
	}

	for _, cmd := range w.Commands {
		if r, ok := runtimeFromCommand(cmd); ok {
			return Result{Runtime: r, Evidence: "container command " + cmd}
		}
	}

	return Result{Runtime: api.RuntimeUnknown, Evidence: "no runtime evidence found"}
}

func runtimeFromName(s string) (api.Runtime, bool) {
	switch s {
	case "java":
		return api.RuntimeJava, true
	case "nodejs", "node":
		return api.RuntimeNodeJS, true
	case "python":
		return api.RuntimePython, true
	case "dotnet":
		return api.RuntimeDotNet, true
	case "go":
		return api.RuntimeGo, true
	case "sdk":
		return api.RuntimePreInstr, true
	}
	return "", false
}

func runtimeFromImage(img string) (api.Runtime, bool) {
	l := strings.ToLower(img)
	switch {
	case containsAny(l, "openjdk", "eclipse-temurin", "amazoncorretto", "/java", "jre", "jdk", "tomcat", "jetty", "quarkus", "spring"):
		return api.RuntimeJava, true
	case containsAny(l, "node:", "/node", "nodejs"):
		return api.RuntimeNodeJS, true
	case containsAny(l, "python:", "/python", "uvicorn", "gunicorn", "django"):
		return api.RuntimePython, true
	case containsAny(l, "dotnet", "aspnet", "mcr.microsoft.com/dotnet"):
		return api.RuntimeDotNet, true
	case containsAny(l, "golang:", "/golang"):
		return api.RuntimeGo, true
	}
	return "", false
}

func runtimeFromCommand(cmd string) (api.Runtime, bool) {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "", false
	}
	base := fields[0]
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	switch {
	case base == "java":
		return api.RuntimeJava, true
	case base == "node" || base == "npm" || base == "yarn" || base == "pnpm":
		return api.RuntimeNodeJS, true
	case strings.HasPrefix(base, "python") || base == "uvicorn" || base == "gunicorn":
		return api.RuntimePython, true
	case base == "dotnet":
		return api.RuntimeDotNet, true
	}
	return "", false
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// Mode resolves `auto` into a concrete instrumentation mode.
//
// The selection matrix, and why:
//
//	java, nodejs, python  -> agent  deepest framework coverage; mature injectors
//	dotnet                -> agent  OBI has no .NET support yet
//	go                    -> ebpf   the Go agent is uprobe-based anyway and
//	                                needs OTEL_GO_AUTO_TARGET_EXE; OBI is
//	                                simpler to operate
//	other, unknown        -> ebpf   no agent exists; eBPF still yields RED
//	                                metrics for HTTP/gRPC/SQL
//	already-instrumented  -> sdk-only  configure the exporter, inject nothing
//
// When eBPF is unavailable at the stack level, runtimes that would have used
// it fall back to no instrumentation rather than silently double-instrumenting
// or silently producing nothing.
func Mode(requested api.InstrumentationMode, r api.Runtime, ebpfAvailable bool) (api.InstrumentationMode, string, error) {
	if requested != "" && requested != api.ModeAuto {
		if requested == api.ModeEBPF && !ebpfAvailable {
			return "", "", fmt.Errorf(
				"instrumentation mode ebpf was requested but the ObservabilityStack has instrumentation.ebpf.enabled=false")
		}
		return requested, "explicitly requested", nil
	}

	switch r {
	case api.RuntimePreInstr:
		return api.ModeSDKOnly, "workload is already instrumented; configuring the exporter only", nil
	case api.RuntimeJava, api.RuntimeNodeJS, api.RuntimePython, api.RuntimeDotNet:
		return api.ModeAgent, fmt.Sprintf("%s has a mature agent injector with deeper span coverage than eBPF", r), nil
	case api.RuntimeGo:
		if ebpfAvailable {
			return api.ModeEBPF, "Go agent injection is uprobe-based and needs OTEL_GO_AUTO_TARGET_EXE; eBPF is simpler", nil
		}
		return api.ModeAgent, "eBPF is disabled at the stack level, falling back to the Go agent", nil
	default:
		if ebpfAvailable {
			return api.ModeEBPF, "no agent exists for this runtime; eBPF still yields RED metrics for HTTP, gRPC and SQL", nil
		}
		return api.ModeNone, "runtime is unknown and eBPF is disabled; no instrumentation can be applied automatically", nil
	}
}
