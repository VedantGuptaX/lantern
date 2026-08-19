package discover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/VedantGuptaX/lantern/api/v1alpha1"
)

func loadFixture(t *testing.T) []Workload {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "examples", "cluster", "workloads.yaml"))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	ws, err := ParseWorkloads(data)
	if err != nil {
		t.Fatalf("ParseWorkloads: %v", err)
	}
	return ws
}

func byName(results []Result) map[string]Result {
	m := map[string]Result{}
	for _, r := range results {
		m[r.Service.Metadata.Name] = r
	}
	return m
}

func TestParsesAllWorkloadKinds(t *testing.T) {
	ws := loadFixture(t)
	kinds := map[string]int{}
	for _, w := range ws {
		kinds[w.Kind]++
	}
	if kinds["Deployment"] == 0 || kinds["StatefulSet"] == 0 || kinds["CronJob"] == 0 {
		t.Errorf("expected Deployments, StatefulSets and CronJobs, got %v", kinds)
	}
}

// TestCronJobPodTemplateIsFound: CronJob nests its pod template two levels
// deeper than every other workload kind, which is easy to miss and silently
// yields a service with no containers and therefore no runtime detection.
func TestCronJobPodTemplateIsFound(t *testing.T) {
	for _, w := range loadFixture(t) {
		if w.Kind != "CronJob" {
			continue
		}
		if len(w.Containers) == 0 {
			t.Fatalf("CronJob %s: pod template was not reached", w.Name)
		}
		if len(w.Commands()) == 0 {
			t.Errorf("CronJob %s: container command not parsed", w.Name)
		}
		return
	}
	t.Fatal("no CronJob in the fixture")
}

func TestSystemNamespacesAreSkipped(t *testing.T) {
	results := Discover(loadFixture(t), Options{})
	for _, r := range results {
		if r.Service.Metadata.Namespace == "kube-system" {
			t.Errorf("kube-system workload %q should have been skipped", r.Service.Metadata.Name)
		}
	}

	all := Discover(loadFixture(t), Options{IncludeSystem: true})
	if len(all) <= len(results) {
		t.Error("-all should widen the result set")
	}
}

func TestNamespaceFilter(t *testing.T) {
	results := Discover(loadFixture(t), Options{Namespaces: []string{"web"}})
	if len(results) == 0 {
		t.Fatal("expected results in namespace web")
	}
	for _, r := range results {
		if r.Service.Metadata.Namespace != "web" {
			t.Errorf("%s is in %q, expected only web", r.Service.Metadata.Name, r.Service.Metadata.Namespace)
		}
	}
}

func TestServiceKindInference(t *testing.T) {
	got := byName(Discover(loadFixture(t), Options{}))

	want := map[string]api.ServiceKind{
		"checkout-api":      api.KindHTTP,      // port named http
		"pricing-svc":       api.KindGRPC,      // port named grpc
		"settlement-worker": api.KindWorker,    // name contains "worker", no ports
		"nightly-reconcile": api.KindCron,      // CronJob
		"storefront":        api.KindHTTP,      // port named http
		"session-store":     api.KindDatabase,  // redis image
		"llama-70b-server":  api.KindInference, // vllm/vllm-openai image + nvidia.com/gpu limit
	}
	for name, wantKind := range want {
		r, ok := got[name]
		if !ok {
			t.Errorf("%s was not discovered", name)
			continue
		}
		if r.Service.Spec.ServiceKind != wantKind {
			t.Errorf("%s: serviceKind = %q, want %q", name, r.Service.Spec.ServiceKind, wantKind)
		}
	}
}

// TestOffTheShelfDatastoreIsNotCalledHTTP guards a bad guess that would
// generate an availability SLO against http_server_request_duration for a
// Redis pod — an alert that can never fire.
func TestOffTheShelfDatastoreIsNotCalledHTTP(t *testing.T) {
	r := byName(Discover(loadFixture(t), Options{}))["session-store"]
	if r.Service.Spec.ServiceKind == api.KindHTTP {
		t.Error("a redis image must not be inferred as an HTTP service")
	}
	if r.Service.Spec.Target.MetricsPort == "redis" {
		t.Error("the redis protocol port must not be guessed as a Prometheus scrape target")
	}

	var flagged bool
	for _, n := range r.Notes {
		if n.Field == "serviceKind" && n.LowConfidence {
			flagged = true
		}
	}
	if !flagged {
		t.Error("an off-the-shelf datastore guess must be flagged for review")
	}
}

// TestGPUWorkloadGetsInferenceKindAndNoAgentInjection guards two things:
// serviceKind inference must be inferred from a known GPU model-serving
// image, and the draft spec must default instrumentation.mode to none so
// `lantern synth` never tries to inject an OTel agent into a GPU-resident
// serving process.
func TestGPUWorkloadGetsInferenceKindAndNoAgentInjection(t *testing.T) {
	r, ok := byName(Discover(loadFixture(t), Options{}))["llama-70b-server"]
	if !ok {
		t.Fatal("llama-70b-server was not discovered")
	}
	if r.Service.Spec.ServiceKind != api.KindInference {
		t.Fatalf("serviceKind = %q, want inference", r.Service.Spec.ServiceKind)
	}
	if r.Service.Spec.Instrumentation.Mode != api.ModeNone {
		t.Errorf("instrumentation.mode = %q, want none", r.Service.Spec.Instrumentation.Mode)
	}
	// A recognised vLLM image must also pin inferenceServer, which is what
	// unlocks the compiler's built-in SLO preset for this service.
	if r.Service.Spec.InferenceServer != api.InferenceVLLM {
		t.Errorf("inferenceServer = %q, want vllm", r.Service.Spec.InferenceServer)
	}

	var flagged, serverNoted bool
	for _, n := range r.Notes {
		if n.Field == "serviceKind" && n.LowConfidence {
			flagged = true
		}
		if n.Field == "inferenceServer" && n.Value == "vllm" {
			serverNoted = true
		}
	}
	if !flagged {
		t.Error("an inference guess must be flagged for review like any other inferred serviceKind")
	}
	if !serverNoted {
		t.Error("a detected inferenceServer must be recorded as a note so the user reviews the preset's default thresholds")
	}
}

// TestGPUResourceRequestAloneIsEnough checks the fallback path: a workload
// with no recognisable inference-server image but a real GPU request should
// still be flagged as inference rather than silently defaulting to http.
func TestGPUResourceRequestAloneIsEnough(t *testing.T) {
	ws, err := ParseWorkloads([]byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: custom-gpu-server
  namespace: ml
spec:
  selector:
    matchLabels:
      app: custom-gpu-server
  template:
    metadata:
      labels:
        app: custom-gpu-server
    spec:
      containers:
        - name: server
          image: registry.internal/our-own-inference-server:1.0.0
          ports:
            - name: http
              containerPort: 8080
          resources:
            limits:
              nvidia.com/gpu: "2"
`))
	if err != nil {
		t.Fatalf("ParseWorkloads: %v", err)
	}
	results := Discover(ws, Options{})
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Service.Spec.ServiceKind != api.KindInference {
		t.Errorf("serviceKind = %q, want inference (nvidia.com/gpu limit was set)", results[0].Service.Spec.ServiceKind)
	}
	// Classified inference by the GPU request alone -- the server is unknown, so
	// inferenceServer must stay empty (no preset guessed for an unknown image).
	if results[0].Service.Spec.InferenceServer != "" {
		t.Errorf("inferenceServer = %q, want empty (image is unrecognised; only the GPU request triggered inference)", results[0].Service.Spec.InferenceServer)
	}
}

func TestRuntimeInference(t *testing.T) {
	got := byName(Discover(loadFixture(t), Options{}))

	want := map[string]api.Runtime{
		"checkout-api":             api.RuntimeJava,     // eclipse-temurin image
		"settlement-worker":        api.RuntimePython,   // python image
		"storefront":               api.RuntimeNodeJS,   // existing inject-nodejs annotation
		"already-instrumented-svc": api.RuntimePreInstr, // OTEL_EXPORTER_OTLP_ENDPOINT set
		"pricing-svc":              api.RuntimeUnknown,  // opaque internal image
	}
	for name, wantRT := range want {
		r, ok := got[name]
		if !ok {
			t.Errorf("%s was not discovered", name)
			continue
		}
		if r.Service.Spec.Instrumentation.Runtime != wantRT {
			t.Errorf("%s: runtime = %q, want %q", name, r.Service.Spec.Instrumentation.Runtime, wantRT)
		}
	}
}

func TestTeamInferenceSources(t *testing.T) {
	got := byName(Discover(loadFixture(t), Options{}))

	want := map[string]string{
		"checkout-api":  "payments", // team label
		"pricing-svc":   "pricing",  // app.kubernetes.io/part-of
		"session-store": "platform", // squad label
	}
	for name, wantTeam := range want {
		if got[name].Service.Spec.Team != wantTeam {
			t.Errorf("%s: team = %q, want %q", name, got[name].Service.Spec.Team, wantTeam)
		}
	}

	if got["storefront"].Service.Spec.Team != "" {
		t.Error("storefront has no ownership label; team must stay empty rather than be invented")
	}

	withFallback := byName(Discover(loadFixture(t), Options{DefaultTeam: "unassigned"}))
	if withFallback["storefront"].Service.Spec.Team != "unassigned" {
		t.Error("-team fallback was not applied")
	}
	if withFallback["checkout-api"].Service.Spec.Team != "payments" {
		t.Error("-team fallback must not override a real ownership label")
	}
}

func TestMetricsPortInference(t *testing.T) {
	got := byName(Discover(loadFixture(t), Options{}))

	if got["checkout-api"].Service.Spec.Target.MetricsPort != "metrics" {
		t.Errorf("checkout-api: metricsPort = %q, want metrics", got["checkout-api"].Service.Spec.Target.MetricsPort)
	}
	// storefront has prometheus.io/port=3000, which matches its `http` port.
	if got["storefront"].Service.Spec.Target.MetricsPort != "http" {
		t.Errorf("storefront: metricsPort = %q, want http (from prometheus.io/port)", got["storefront"].Service.Spec.Target.MetricsPort)
	}
	if got["settlement-worker"].Service.Spec.Target.MetricsPort != "" {
		t.Error("a workload with no ports must not have a metrics port invented for it")
	}
}

func TestSelectorIsTakenFromTheWorkload(t *testing.T) {
	got := byName(Discover(loadFixture(t), Options{}))
	sel := got["pricing-svc"].Service.Spec.Target.Selector
	if sel["app"] != "pricing-svc" || sel["tier"] != "backend" {
		t.Errorf("pricing-svc selector = %v, want the workload's own matchLabels", sel)
	}
}

// TestNoSLOsAreInvented: objectives are commitments. Inferring 99.9% from a
// manifest would present a guess as a promise.
func TestNoSLOsAreInvented(t *testing.T) {
	for _, r := range Discover(loadFixture(t), Options{}) {
		if len(r.Service.Spec.SLOs) != 0 {
			t.Errorf("%s: discovery must not invent SLOs, got %v", r.Service.Metadata.Name, r.Service.Spec.SLOs)
		}
	}
}

func TestRenderIsDeterministicAndAnnotated(t *testing.T) {
	results := Discover(loadFixture(t), Options{DefaultTeam: "unassigned"})
	first := Render(results)
	for i := 0; i < 25; i++ {
		if Render(results) != first {
			t.Fatal("Render is not deterministic")
		}
	}
	if !strings.Contains(first, "# REVIEW") {
		t.Error("low-confidence guesses must be marked REVIEW in the output")
	}
	if !strings.Contains(first, "Review before applying") {
		t.Error("output must carry a review header")
	}
}

func TestSummaryGroupsByField(t *testing.T) {
	summary := Summary(Discover(loadFixture(t), Options{}))
	if len(summary) == 0 {
		t.Fatal("expected review items in the fixture")
	}
	for _, line := range summary {
		if !strings.Contains(line, "needs review on") {
			t.Errorf("unexpected summary line: %q", line)
		}
	}
}

func TestKubectlListWrapperIsUnwrapped(t *testing.T) {
	src := []byte(`
apiVersion: v1
kind: List
items:
  - apiVersion: apps/v1
    kind: Deployment
    metadata:
      name: from-list
      namespace: shop
    spec:
      selector:
        matchLabels:
          app: from-list
      template:
        metadata:
          labels:
            app: from-list
        spec:
          containers:
            - name: app
              image: node:22
              ports:
                - name: http
                  containerPort: 8080
`)
	ws, err := ParseWorkloads(src)
	if err != nil {
		t.Fatalf("ParseWorkloads: %v", err)
	}
	if len(ws) != 1 || ws[0].Name != "from-list" {
		t.Fatalf("kubectl `kind: List` output was not unwrapped, got %v", ws)
	}
}

func TestNonWorkloadKindsAreIgnored(t *testing.T) {
	src := []byte("apiVersion: v1\nkind: Service\nmetadata:\n  name: svc\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cm\n")
	ws, err := ParseWorkloads(src)
	if err != nil {
		t.Fatalf("ParseWorkloads: %v", err)
	}
	if len(ws) != 0 {
		t.Errorf("expected no workloads, got %v", ws)
	}
}
