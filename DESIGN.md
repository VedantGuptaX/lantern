# Lantern — Observability as Code

**An architecture & design document**

| | |
|---|---|
| **Status** | Draft for review |
| **Author** | Vedant |
| **Date** | 15 August 2026 |
| **Working name** | Lantern (placeholder) |
| **API group** | `lantern.dev/v1alpha1` |

---

## 1. Problem statement

A developer who wants their service monitored today has to produce, or ask a platform
engineer to produce, six unrelated artifacts:

| Artifact | Owner project | Format |
|---|---|---|
| Agent injection config | OpenTelemetry Operator | `Instrumentation` CR |
| Collector pipeline | OpenTelemetry Operator | `OpenTelemetryCollector` CR |
| Scrape config | Prometheus Operator | `ServiceMonitor` / `PodMonitor` |
| Alert rules | Prometheus Operator | `PrometheusRule` (PromQL) |
| Dashboard | Grafana | 4,000-line JSON |
| Dashboard access | Grafana | Folder + permission + team mapping |

Every one of these has a mature "as code" story. **None of them share a schema.**
The platform engineer is the compiler — manually translating "team X runs an HTTP
service and wants 99.9% availability" into six dialects, then maintaining the
translation forever as six upstreams evolve independently.

That manual translation is the product opportunity. It is deterministic,
repetitive, and currently performed by a human.

### What this is not

This is **not** another OpenTelemetry. OTel is the data-plane standard and it is
correct. This is not a storage backend, an agent, or a query engine.

This is a **compiler**: one typed input, many well-known outputs.

---

## 2. Design decisions taken

Four decisions were made before this document and constrain everything below.

| Decision | Choice | Consequence |
|---|---|---|
| **Developer interface** | Both — typed SDK compiles down to a CRD | The CRD is the contract; SDKs are ergonomic front-ends. Neither audience is second class. |
| **Backend** | Bring your own | We generate config for existing Prometheus / Grafana / Tempo / Loki. We own no storage, no scaling, no retention. |
| **Instrumentation** | Both agent and eBPF, auto-selected by runtime | Best coverage; requires a detection layer and a hard guard against double-instrumentation. |
| **First deliverable** | This document | Circulate before writing code. |

---

## 3. Competitive landscape — and why we compose rather than compete

| Project | What it does well | Where it stops |
|---|---|---|
| **OpenTelemetry Operator** | Agent injection for Java/Node/Python/.NET/Go; collector lifecycle | No dashboards, no alerts, no SLOs, no access control |
| **OBI** (OTel eBPF Instrumentation) | Zero-touch RED metrics + traces for HTTP/gRPC/SQL, any language | Pre-1.0; no .NET; shallow spans; not a config system |
| **Odigos** | Best-in-class instrumentation orchestration — discovers workloads, picks eBPF vs agent, builds pipelines from a `Destination` CR | Ends at "telemetry reaches your backend". No dashboard/alert/SLO/RBAC generation |
| **Grafana Foundation SDK** | Dashboards and alerts as typed code in Go/TS/Python/Java/PHP | Only the visualisation layer. Public preview. You still hand-write every panel |
| **Perses** | Declarative dashboards, Go/CUE SDK, operator with native CRDs | Same scope limit as above; smaller ecosystem |
| **Prometheus Operator** | Scrape and rule config as CRs | Requires you to write the PromQL |
| **Sloth / Pyrra / OpenSLO** | SLO spec → multiwindow-multiburn Prometheus rules | Isolated from instrumentation and dashboards |
| **OTel Weaver** | Telemetry schema as code; generates typed instrumentation libs; CI drift detection | Schema layer only; nothing downstream consumes it |

**Positioning:** Lantern is the layer *above* all of these. The most important
consequence is about Odigos specifically — Odigos is not a competitor, it is a
candidate **backend for our instrumentation emitter**. If a cluster already runs
Odigos, Lantern should emit Odigos `Source`/`Destination` CRs instead of OTel
Operator CRs and skip its own detection entirely.

**The unique claim:** nobody generates dashboards, burn-rate alerts, and folder
permissions from a single typed service definition. That is the whole product.

---

## 4. Architecture

### 4.1 The layering

```
┌───────────────────────────────────────────────────────────────┐
│  L3  SDKs        Go · TypeScript · Python                     │
│      typed builders, presets, IDE completion                  │
└──────────────────────────┬────────────────────────────────────┘
                           │ synthesizes
                           ▼
┌───────────────────────────────────────────────────────────────┐
│  L2  THE CONTRACT      ServiceObservability CR (YAML)         │
│      + ObservabilityStack CR (platform-owned)                 │
│      This is the stable, versioned API. Everything else       │
│      is an implementation detail.                             │
└──────────────────────────┬────────────────────────────────────┘
                           │ Compile(spec, facts) → []Object
                           ▼
┌───────────────────────────────────────────────────────────────┐
│  L1  COMPILER CORE     pure function, no I/O, no cluster      │
│      deterministic · golden-file tested · reusable            │
└──────────────────────────┬────────────────────────────────────┘
                           │
        ┌──────────┬───────┴────┬───────────┬──────────┐
        ▼          ▼            ▼           ▼          ▼
┌───────────┐┌──────────┐┌───────────┐┌─────────┐┌──────────┐
│  L0 otel  ││ L0 prom  ││L0 grafana ││ L0 slo  ││L0 compose│
│Instrument.││ServiceMon││Foundation ││ Sloth   ││ docker-  │
│Collector  ││PromRule  ││SDK → JSON ││ rules   ││ compose  │
│Odigos CRs ││          ││folder+RBAC││         ││          │
└───────────┘└──────────┘└───────────┘└─────────┘└──────────┘
                           │
              ┌────────────┴────────────┐
              ▼                         ▼
      ┌───────────────┐        ┌────────────────┐
      │  lantern CLI  │        │   Operator     │
      │  synth / diff │        │  reconciles    │
      │  → GitOps     │        │  → live apply  │
      └───────────────┘        └────────────────┘
```

### 4.2 The single most important decision

**The compiler core is a pure function.**

```go
package compile

// Compile is deterministic. Same inputs, byte-identical outputs.
// No network calls. No clock reads. No random. No cluster access.
func Compile(
    svc   v1alpha1.ServiceObservability,
    stack v1alpha1.ObservabilityStack,
    facts Facts,          // pre-resolved: detected runtime, k8s version, installed CRDs
) (Output, error)

type Output struct {
    Objects []client.Object   // typed k8s objects, ordered deterministically
    Files   map[string][]byte // compose target: yaml, collector config, dashboards
    Diags   []Diagnostic      // warnings that don't fail compilation
}
```

Everything I/O-shaped — cluster reads, runtime detection, Grafana API calls —
happens *outside* this function and arrives as `Facts`.

This buys four things that are hard to retrofit:

1. **The CLI and the operator share one implementation.** No drift between what
   `lantern synth` renders and what the operator applies.
2. **Golden-file testing.** Every feature is a `testdata/` directory: input spec,
   expected output tree. Regressions become impossible to miss.
3. **`lantern diff` is free.** Compile, compare to live, print. No separate code path.
4. **Upstream schema churn is contained.** Six upstream projects will break their
   CRDs. Each break is one emitter and one golden-file update.

If you build nothing else from this document, build `Compile` as a pure function.

---

## 5. The contract: CRD schemas

Two resources with a deliberate ownership split.

### 5.1 `ObservabilityStack` — cluster-scoped, platform team owns

Written once per cluster by you. Developers never see it. It answers "where does
telemetry go and what are the rules."

```yaml
apiVersion: lantern.dev/v1alpha1
kind: ObservabilityStack
metadata:
  name: default
spec:
  backends:
    metrics:
      type: prometheus                     # prometheus | mimir | victoriametrics
      remoteWrite: http://mimir.o11y:9009/api/v1/push
      queryURL:    http://mimir.o11y:9009/prometheus
      operator:    prometheus-operator     # emit ServiceMonitor/PrometheusRule
    traces:
      type: tempo
      otlpEndpoint: tempo.o11y:4317
    logs:
      type: loki
      endpoint: http://loki.o11y:3100
    dashboards:
      type: grafana                        # grafana | perses
      instanceRef: { name: main, namespace: o11y }   # grafana-operator Grafana CR
      folderStrategy: per-team             # per-team | per-namespace | flat

  instrumentation:
    provider: otel-operator                # otel-operator | odigos | none
    ebpf:
      enabled: true
      provider: obi                        # obi | odigos
      privileged: true                     # required; surfaced explicitly

  # Guardrails. Developers cannot exceed these.
  policy:
    maxSeriesPerService: 25000
    allowedSamplingRates: { min: 0.01, max: 1.0 }
    requireTeamLabel: true
    maxRouteCardinality: 200               # http.route values before collapse to "other"
    denyLabels: [user_id, email, session_id, request_id]   # cardinality bombs

  defaults:
    slo:
      availability: 99.9
      latency: { objective: 99.0, threshold: 500ms }
    retention: { traces: 7d, metrics: 30d }
```

### 5.2 `ServiceObservability` — namespaced, developer owns

This is the developer-facing object. **Target: under 20 lines for the common case.**

```yaml
apiVersion: lantern.dev/v1alpha1
kind: ServiceObservability
metadata:
  name: checkout-api
  namespace: shop
spec:
  target:
    kind: Deployment
    name: checkout-api
    # or: selector: { matchLabels: { app: checkout-api } }

  # Drives dashboard template + which semconv metrics we expect.
  serviceKind: http                # http | grpc | worker | cron | database | batch | custom

  team: payments                   # → Grafana folder + RBAC + alert routing

  signals:
    metrics: true
    traces:  { enabled: true, samplingRate: 0.1 }
    logs:    { enabled: true, correlateTraceID: true }
    profiles: false

  slos:
    - name: availability
      type: availability           # availability | latency | throughput | freshness | custom
      objective: 99.9
      window: 30d
    - name: latency
      type: latency
      objective: 99.0
      threshold: 300ms
      window: 30d

  # Optional. Everything above is enough for a working dashboard.
  dashboard:
    extraPanels:
      - title: Payment provider errors
        query: sum by (provider) (rate(payment_provider_errors_total[5m]))
        viz: timeseries
    patches:                       # RFC-6902 escape hatch — never lock the dashboard
      - op: replace
        path: /panels/0/fieldConfig/defaults/unit
        value: reqps

  # Escape hatches
  instrumentation:
    mode: auto                     # auto | agent | ebpf | sdk-only | none
    runtime: ""                    # override detection: java|nodejs|python|dotnet|go|other
  attributes:                      # extra resource attributes
    deployment.environment: production
    service.version: "{{ .Image.Tag }}"
```

### 5.3 The zero-config path

Most developers will not write even 20 lines. A label on the Deployment must be
enough:

```yaml
metadata:
  labels:
    lantern.dev/profile: http-service
    lantern.dev/team: payments
```

The operator synthesizes a default `ServiceObservability` from the label plus
`ObservabilityStack.spec.defaults`, and marks it `ownerReferences`-linked to the
Deployment so it is garbage collected. **If the zero-config path does not produce
a dashboard a developer is happy with, the product has failed.** Everything else
is refinement.

---

## 6. The SDK layer

### 6.1 Generation strategy

Do not hand-write three SDKs — that is three drifting implementations.

```
api/v1alpha1/*.go  (Go structs, kubebuilder markers — source of truth)
        │
        │ controller-gen → OpenAPI v3 schema
        ▼
    schema.json
        │
        ├─→ codegen → sdk/go/types.go       ┐
        ├─→ codegen → sdk/ts/types.ts       ├─ generated, never edited
        └─→ codegen → sdk/python/types.py   ┘
                │
                └─ thin hand-written ergonomic layer per language
                   (builders, presets, validation) — ~300 LOC each
```

The generated types are mechanical. The hand-written layer is small and is where
the developer experience lives. This mirrors how Grafana's Foundation SDK is
built (codegen from schema + thin builders), which is a good sign the approach
scales.

### 6.2 Go

```go
package main

import "github.com/you/lantern/sdk/go/lantern"

func main() {
    svc := lantern.NewHTTPService("checkout-api").
        InNamespace("shop").
        OwnedBy("payments").
        Targeting(lantern.Deployment("checkout-api")).
        WithSLO(lantern.Availability(99.9)).
        WithSLO(lantern.Latency(99.0, "300ms")).
        WithTraceSampling(0.1).
        WithPanel(lantern.TimeSeries("Payment provider errors").
            Query(`sum by (provider) (rate(payment_provider_errors_total[5m]))`))

    lantern.Emit(svc)   // → ServiceObservability YAML on stdout
}
```

### 6.3 TypeScript

```typescript
import { HTTPService, Availability, Latency, Deployment } from '@lantern/sdk';

export default new HTTPService('checkout-api')
  .inNamespace('shop')
  .ownedBy('payments')
  .targeting(Deployment('checkout-api'))
  .withSLO(Availability(99.9))
  .withSLO(Latency(99.0, '300ms'))
  .withTraceSampling(0.1);
```

### 6.4 Python

```python
from lantern import HTTPService, Availability, Latency, Deployment

service = (
    HTTPService("checkout-api")
    .in_namespace("shop")
    .owned_by("payments")
    .targeting(Deployment("checkout-api"))
    .with_slo(Availability(99.9))
    .with_slo(Latency(99.0, "300ms"))
    .with_trace_sampling(0.1)
)
```

**SDKs emit the CR. They do not talk to the cluster.** `lantern synth main.go`
runs the program, captures the CR, compiles it. This keeps the trust boundary at
the CR and means an SDK bug can never produce output the CRD schema would reject.

---

## 7. Compilation targets

What a single `ServiceObservability` for an HTTP service produces.

### 7.1 Instrumentation

**When `provider: otel-operator`** — patch the target workload with the language
annotation and ensure a namespace-scoped `Instrumentation` CR exists:

| Detected runtime | Annotation emitted |
|---|---|
| Java | `instrumentation.opentelemetry.io/inject-java: "true"` |
| Node.js | `instrumentation.opentelemetry.io/inject-nodejs: "true"` |
| Python | `instrumentation.opentelemetry.io/inject-python: "true"` |
| .NET | `instrumentation.opentelemetry.io/inject-dotnet: "true"` |
| Go | `instrumentation.opentelemetry.io/inject-go: "true"` (requires `OTEL_GO_AUTO_TARGET_EXE`) |
| already-instrumented | `instrumentation.opentelemetry.io/inject-sdk: "true"` (config only, no agent) |

```yaml
apiVersion: opentelemetry.io/v1alpha1
kind: Instrumentation
metadata: { name: lantern-default, namespace: shop }
spec:
  exporter: { endpoint: http://lantern-collector.o11y:4318 }
  propagators: [tracecontext, baggage]
  sampler: { type: parentbased_traceidratio, argument: "0.1" }
  resource:
    resourceAttributes:
      deployment.environment: production
      service.namespace: shop
      team: payments
```

**When `provider: odigos`** — emit Odigos `Source` and `Destination` CRs instead
and skip detection entirely; Odigos already solves this better than we would.

### 7.2 Runtime detection and the eBPF/agent selection matrix

Detection lives in `pkg/detect`, runs **outside** the compiler, and yields a
`Facts.Runtime` value. Signals, in priority order:

1. `spec.instrumentation.runtime` explicit override — always wins
2. Existing OTel annotations on the workload — respect them, do not fight
3. Presence of `OTEL_EXPORTER_OTLP_ENDPOINT` env → treat as already-instrumented
4. Container image name / tag heuristics (`openjdk`, `node:`, `python:`, `golang:`)
5. Command and args (`java -jar`, `node`, `python`, `dotnet`)
6. Fallback: unknown

| Runtime | Default mode | Rationale |
|---|---|---|
| Java | **agent** | Deepest framework coverage; most mature injector |
| Node.js | **agent** | Mature; eBPF misses async context |
| Python | **agent** | Mature |
| .NET | **agent** | OBI has no .NET support yet (2026 roadmap item) |
| Go | **ebpf** | Go agent is uprobe-based anyway and needs `OTEL_GO_AUTO_TARGET_EXE`; OBI is simpler to operate |
| Rust / C++ / other | **ebpf** | No agent exists |
| Unknown | **ebpf** | Safe: produces RED metrics for HTTP/gRPC/SQL regardless of language |
| Already instrumented | **sdk-only** | Configure the exporter, inject nothing |

#### The double-instrumentation guard

This is the sharpest footgun in the whole design. An agent and OBI both
instrumenting the same HTTP handler produce **two spans per request** and
**double-counted RED metrics**. Silent, expensive, and it destroys trust in the
data on day one.

Mitigations, all three:

1. Compiler-level: `agent` and `ebpf` are mutually exclusive per workload. Not a
   warning — a compile error.
2. OBI discovery config excludes any pod carrying an
   `instrumentation.opentelemetry.io/inject-*` annotation.
3. A validating admission webhook rejects manual annotation of a Lantern-managed
   workload, with a message pointing at `spec.instrumentation.mode`.

OBI's 2026 roadmap includes hybrid eBPF + SDK instrumentation. When that lands,
revisit — but until it is stable, hard exclusion is the correct posture.

### 7.3 Metrics collection

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: checkout-api
  namespace: shop
  labels: { lantern.dev/managed: "true", team: payments }
spec:
  selector: { matchLabels: { app: checkout-api } }
  endpoints:
    - port: metrics
      interval: 30s
      metricRelabelings:
        # policy.denyLabels enforcement, compiled in
        - action: labeldrop
          regex: "user_id|email|session_id|request_id"
```

Plus a collector `transform` processor that collapses `http.route` beyond
`policy.maxRouteCardinality` into `"other"` — cardinality control is a compiler
responsibility, not a developer one.

### 7.4 SLOs → burn-rate alerts

Do not write burn-rate PromQL by hand. Sloth already generates SLI recording
rules, metadata recording rules, and **multiwindow-multiburn alerts** from a
declarative spec, and its generator is importable as a Go library.

Two modes:

- **CLI / GitOps:** call Sloth's generator at compile time, emit a plain
  `PrometheusRule`. No runtime dependency on Sloth in the cluster.
- **Operator (optional):** emit a Sloth `PrometheusServiceLevel` CR and let its
  controller do the work.

Prefer the first. Fewer moving parts in the cluster; the output is inspectable in
git.

Generated from `type: availability, objective: 99.9`:

```
SLI = sum(rate(http_server_request_duration_seconds_count{status=~"5.."}[{{window}}]))
    / sum(rate(http_server_request_duration_seconds_count[{{window}}]))
```

with the Google SRE workbook window pairs:

| Severity | Short window | Long window | Burn rate factor |
|---|---|---|---|
| page | 5m | 1h | 14.4 |
| page | 30m | 6h | 6 |
| ticket | 2h | 1d | 3 |
| ticket | 6h | 3d | 1 |

Alert labels carry `team: payments`, which is what Alertmanager routes on.

### 7.5 Dashboards — the differentiator

Generated with the Grafana Foundation SDK (Go), emitted as a grafana-operator
`GrafanaDashboard` CR so the reconciler never needs Grafana admin credentials.

**Why generation works:** OpenTelemetry semantic conventions are machine-readable
and, for HTTP, **stable**. `serviceKind: http` implies `http.server.request.duration`
exists as a histogram in seconds carrying `http.request.method`, `url.scheme`,
`http.route`, `http.response.status_code`, and `error.type`. From that, RED panels
are a *derivation*, not a design decision.

Stability varies by domain and the compiler must respect that. HTTP metrics are
stable and safe to generate against today. RPC/gRPC and messaging conventions are
still experimental — pin the semconv version in `pkg/semconv`, generate the
registry with Weaver, and fail the build loudly when an upstream rename lands
rather than silently emitting dashboards that query metrics nobody produces.

Template selection by `serviceKind`:

| serviceKind | Primary metric family | Generated sections |
|---|---|---|
| `http` | `http.server.request.duration` *(stable)* | Rate/Errors/Duration by route · status code breakdown · top slow routes · in-flight requests |
| `grpc` | `rpc.server.duration` *(experimental)* | Same, keyed on `rpc.method` · gRPC status codes |
| `worker` | `messaging.*` *(experimental)* | Consumer lag · queue depth · processing duration · DLQ rate · retry rate |
| `cron` | custom + k8s Job | Last success · duration trend · failure streak · schedule drift |
| `database` | `db.client.operation.duration` *(experimental)* | Query latency by operation · connection pool · slow query count |

Build `http` first. It is the only one whose underlying convention cannot move
under you, and it covers the majority of services.

Every dashboard also gets, unconditionally:

- **Runtime section** — selected by detected runtime: JVM heap/GC/threads, Node
  event-loop lag, Go goroutines/GC pause, .NET GC
- **Kubernetes section** — restarts, OOMKills, CPU throttling, memory working set,
  replica availability
- **SLO section** — error budget remaining, burn rate, 30d compliance
- **Correlation** — trace exemplars on latency panels; a log panel filtered to the
  service with `trace_id` linking into Tempo

```go
// pkg/emit/grafana/http.go — sketch
func httpDashboard(svc v1alpha1.ServiceObservability, ds dashboard.DataSourceRef) (dashboard.Dashboard, error) {
    b := dashboard.NewDashboardBuilder(svc.Name).
        Tags([]string{"lantern", "generated", svc.Spec.Team}).
        Refresh("30s").
        Time("now-6h", "now").
        WithVariable(routeVariable(svc, ds)).
        WithRow(dashboard.NewRowBuilder("Golden signals")).
        WithPanel(ratePanel(svc, ds)).
        WithPanel(errorPanel(svc, ds)).
        WithPanel(durationPanel(svc, ds)).      // p50/p90/p99 + exemplars
        WithRow(dashboard.NewRowBuilder("SLO")).
        WithPanel(errorBudgetPanel(svc, ds)).
        WithPanel(burnRatePanel(svc, ds))

    b = appendRuntimeSection(b, svc, ds)
    b = appendKubernetesSection(b, svc, ds)
    b = appendCustomPanels(b, svc.Spec.Dashboard.ExtraPanels, ds)

    return b.Build()
}
```

**The escape hatch is mandatory.** Generated dashboards are opinionated and
somebody will hate panel 3. `spec.dashboard.patches` applies RFC-6902 JSON Patch
to the built dashboard before emission. A developer who cannot override the
generator will abandon the tool and hand-write JSON, and you are back where you
started.

### 7.6 Access — the part the request actually turns on

"Give access to the developers" was the original hard problem. It compiles to
three grafana-operator CRs derived from `spec.team`:

```yaml
---
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaFolder
metadata: { name: team-payments }
spec:
  instanceSelector: { matchLabels: { dashboards: main } }
  title: "Team Payments"
  permissions: |
    { "items": [ { "teamId": 4, "permission": 2 } ] }   # Edit
---
apiVersion: grafana.integreatly.org/v1beta1
kind: GrafanaDashboard
metadata: { name: checkout-api }
spec:
  folderRef: team-payments
  instanceSelector: { matchLabels: { dashboards: main } }
  json: |
    { ...generated by Foundation SDK... }
```

Team membership syncs from the OIDC group claim, so it is never managed by hand.
Folder strategy is a stack-level policy (`per-team` / `per-namespace` / `flat`)
rather than a per-service decision — otherwise you get 400 folders.

The developer-visible outcome: `kubectl get serviceobservability checkout-api -o
jsonpath='{.status.dashboardURL}'` returns a link they can open, and they already
have permission.

### 7.7 The Compose target

`lantern synth --target compose` emits a directory:

```
docker-compose.yaml          prom + grafana + tempo + loki + otel collector
otel-collector-config.yaml
grafana/provisioning/dashboards/checkout-api.json
prometheus/rules/checkout-api.yaml
```

Same compiler, same dashboards, different emitter. This gives local dev parity —
a developer sees the *same* dashboard on their laptop as in production — and it
opens the tool to teams not on Kubernetes at all.

---

## 8. Repository layout

```
lantern/
├── api/v1alpha1/                 # CRD types — the source of truth
│   ├── observabilitystack_types.go
│   ├── serviceobservability_types.go
│   └── zz_generated.deepcopy.go
├── pkg/
│   ├── compile/                  # THE PURE FUNCTION. No I/O. Ever.
│   │   ├── compile.go
│   │   └── compile_test.go       # golden-file driven
│   ├── detect/                   # runtime detection (impure, produces Facts)
│   ├── semconv/                  # Weaver-generated attribute registry
│   └── emit/
│       ├── otel/                 # Instrumentation, Collector, Odigos
│       ├── prom/                 # ServiceMonitor, PrometheusRule
│       ├── grafana/              # Foundation SDK → GrafanaDashboard/Folder
│       ├── perses/               # alternative dashboard emitter
│       ├── slo/                  # Sloth generator wrapper
│       └── compose/              # docker-compose target
├── cmd/
│   ├── lantern/                  # CLI: synth, diff, validate, docs, lint
│   └── manager/                  # operator (controller-runtime)
├── internal/controller/          # reconcilers — thin, delegate to pkg/compile
├── sdk/
│   ├── go/  ts/  python/         # generated types + thin builder layer
├── charts/lantern/               # Helm chart
├── testdata/golden/              # input spec → expected output tree
└── docs/
```

**Rule:** `pkg/compile` may not import `k8s.io/client-go`. Enforce it in CI with
an import-boundary lint. This one rule keeps the architecture honest.

---

## 9. Phased roadmap

Deliberately **CLI-first**. The compiler is the product; the operator is a
delivery mechanism. Building the operator first means four weeks of
controller-runtime plumbing before anyone has seen a single generated dashboard —
and dashboard quality is the entire risk.

### P0 — Compiler and CLI (weeks 1–4)

- `api/v1alpha1` CRD types, `controller-gen` wired up
- `pkg/compile` as a pure function with the golden-file harness
- Emitters: OTel `Instrumentation` + `ServiceMonitor` + `PrometheusRule`
- `lantern synth` / `validate`
- **Exit criterion:** `lantern synth checkout.yaml | kubectl apply -f -` produces
  a service emitting traces and metrics into an existing stack.

### P1 — Dashboards and SLOs (weeks 5–8) — *the value*

- Foundation SDK integration; `http` and `grpc` templates
- Runtime + Kubernetes sections
- Sloth-based burn-rate alert generation
- `GrafanaFolder` / `GrafanaDashboard` / team permission emission
- `spec.dashboard.patches` escape hatch
- **Exit criterion:** three real internal services adopt it and their teams stop
  maintaining hand-written dashboards.

### P2 — Operator (weeks 9–12)

- controller-runtime manager, both CRDs, status subresource with `dashboardURL`
- Runtime detection + auto mode selection
- `lantern.dev/profile` label shortcut and default synthesis
- Validating webhook (double-instrumentation guard, policy enforcement)
- Helm chart
- **Exit criterion:** a developer adds two labels and gets a working dashboard.

### P3 — Reach (weeks 13+)

- TypeScript and Python SDKs
- eBPF/OBI path and the selection matrix
- Compose target
- Odigos emitter
- Weaver schema registry integration + CI drift detection
- `worker` / `cron` / `database` dashboard templates
- Perses emitter

---

## 10. Risks

| # | Risk | Severity | Mitigation |
|---|---|---|---|
| 1 | **Generated dashboards are not good enough.** Dashboard design is taste-driven; a mediocre generated dashboard is worse than none. | **Highest** | Ship P1 to three real teams before building anything else. Mandatory patch escape hatch. Steal panel design from `kube-prometheus-stack` and the OTel demo rather than inventing. |
| 2 | **Double instrumentation** — duplicated spans and doubled RED metrics destroy trust silently. | High | Three-layer guard (§7.2). Compile error, not warning. |
| 3 | **Cardinality explosion** from auto-generated `http.route` labels blowing up Prometheus. | High | `policy.maxRouteCardinality` collapse to `"other"`; `denyLabels` labeldrop; a `lantern lint` that estimates series count before apply. |
| 4 | **Foundation SDK is public preview**; dashboard schema v2 is still moving. | Medium | Isolate behind a `DashboardEmitter` interface. Perses as a second implementation proves the abstraction. Pin versions. |
| 5 | **Six upstream CRD schemas to track.** Each will break. | Medium | Version compatibility matrix in CI; integration tests per supported upstream version; golden files make breakage loud. |
| 6 | **Adoption** — developers will not write even 20 lines of YAML. | Medium | The label shortcut must produce a genuinely good dashboard with zero configuration. Measure adoption by zero-config usage, not CR count. |
| 7 | **Scope creep into being a platform.** The temptation to bundle storage. | Medium | "Bring your own backend" is a decision, not a phase. Write it down and defend it. |
| 8 | eBPF requires privileged access; some clusters forbid it. | Low | eBPF is opt-in at stack level; agent path is a complete fallback. |

---

## 11. Open questions

1. **Multi-cluster.** One Grafana fronting many clusters requires a consistent
   `cluster` label injected at the collector. Does the `ObservabilityStack` become
   a fleet-level object, or do you run one per cluster and reconcile centrally?
2. **Alert routing ownership.** Lantern emits `team` labels. Who owns the
   Alertmanager receiver config — is that in scope, or does it stop at the label?
3. **Log correlation depth.** Injecting `trace_id` into logs requires either an
   SDK log appender or a collector-side join. The agent path gets it free; the
   eBPF path does not. Is partial correlation acceptable?
4. **Cost attribution.** `team` labels make per-team telemetry spend computable.
   Is a cost dashboard in scope for v1 or a follow-on?
5. **Brownfield migration.** Teams with existing hand-written dashboards need an
   import path (`lantern import <dashboard-uid>` → best-effort spec + leftover
   patches). Worth building, or explicitly out of scope?
6. **Do you build on Odigos rather than beside it?** If Odigos handles
   instrumentation well, P3's Odigos emitter arguably becomes P0's *only*
   instrumentation path, and Lantern narrows to "the dashboard, SLO, and access
   compiler." That is a smaller, sharper, more defensible product. Worth deciding
   before P0.

---

## 12. Appendix — end to end

**Input** (20 lines, written by a developer):

```yaml
apiVersion: lantern.dev/v1alpha1
kind: ServiceObservability
metadata: { name: checkout-api, namespace: shop }
spec:
  target: { kind: Deployment, name: checkout-api }
  serviceKind: http
  team: payments
  signals:
    traces: { enabled: true, samplingRate: 0.1 }
  slos:
    - { name: availability, type: availability, objective: 99.9, window: 30d }
    - { name: latency, type: latency, objective: 99.0, threshold: 300ms, window: 30d }
```

**Output** (`lantern synth`, ~1,400 lines, written by nobody):

```
opentelemetry.io/v1alpha1/Instrumentation           lantern-default        (shop)
apps/v1/Deployment                                  checkout-api  [patched annotations]
monitoring.coreos.com/v1/ServiceMonitor             checkout-api           (shop)
monitoring.coreos.com/v1/PrometheusRule             checkout-api-slo       (shop)
    ├── 6  SLI recording rules       (5m/30m/1h/2h/6h/1d/3d windows)
    ├── 3  metadata recording rules
    └── 8  multiwindow-multiburn alerts, labelled team=payments
grafana.integreatly.org/v1beta1/GrafanaFolder       team-payments
grafana.integreatly.org/v1beta1/GrafanaDashboard    checkout-api
    ├── Golden signals   — rate, errors, p50/p90/p99 with exemplars, by route
    ├── SLO              — error budget remaining, burn rate, 30d compliance
    ├── Runtime          — auto-selected from detected runtime
    ├── Kubernetes       — restarts, OOMKills, throttling, replicas
    └── Logs             — service-filtered, trace_id linked to Tempo
```

That ratio — 20 lines in, 1,400 lines out, zero hand-written PromQL, zero
hand-written dashboard JSON — is the product.
