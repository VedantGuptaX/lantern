# Getting started with Lantern

This walks from an empty machine to a cluster where every service is
instrumented, scraped, and alerting on its error budget.

Two paths, depending on what you already run:

- **[Path A — empty cluster](#path-a--empty-cluster)**: Lantern installs a
  complete stack. Best for evaluating the project.
- **[Path B — you already run Prometheus and Grafana](#path-b--bring-your-own-backend)**:
  Lantern installs nothing and generates config for your existing backends.
  This is the intended production posture.

---

## 0. How Lantern thinks

Understanding this makes everything else obvious.

Lantern is a **compiler**. It takes two inputs and produces Kubernetes
manifests:

```
  ObservabilityStack          ServiceObservability
  "where telemetry goes,      "what this service is,
   and what the rules are"     and what it promises"
  written once by you          written per service
  (or by the Helm chart)       (or drafted by discover)
            │                           │
            └───────────┬───────────────┘
                        ▼
                  lantern synth
                        │
                        ▼
     Instrumentation · ServiceMonitor · PrometheusRule
                        │
                        ▼
                   kubectl apply
```

Lantern never talks to your cluster. It reads YAML, writes YAML. `kubectl`,
Argo CD, or Flux does the applying. That means you can review every byte
before anything changes, and the same command works against a live cluster or
a GitOps repository.

**Two objects, two owners:**

| Object | Scope | Who writes it | Answers |
|---|---|---|---|
| `ObservabilityStack` | cluster | platform team, once | Where does telemetry go? What are the guardrails? |
| `ServiceObservability` | namespace | each service owner | What is this service? What does it promise? |

---

## 1. Install the CLI

```bash
git clone https://github.com/VedantGuptaX/lantern.git
cd lantern
make build
```

That produces `bin/lantern`. Put it on your `PATH` if you like:

```bash
sudo install bin/lantern /usr/local/bin/
lantern version
```

Requires Go 1.24+. There are no other dependencies — Lantern builds entirely
against the Go standard library.

### Try it with no cluster at all

Everything below works offline. This is the fastest way to see what Lantern
does before touching infrastructure:

```bash
make demo
```

That runs `lantern discover` over a sample eight-workload cluster in
`examples/cluster/`, then compiles the result. Read the output — it is the
whole product in one command.

---

## Path A — empty cluster

You have a cluster (kind, minikube, a fresh EKS/GKE) with nothing installed.

> **Try it on a throwaway cluster first.** `./scripts/verify-kind.sh` creates a
> kind cluster, runs every step below, applies the result, checks what landed,
> and writes `verify-report.txt`. The chart has never actually been installed
> by anyone, so expect to fix something — the report tells you what.
>
> Needs `docker` (or Colima), `kind`, `helm`, `kubectl` and `go` on your
> machine, and about 8GB available to containers.

### A1. Install the stack

Lantern generates references to CRDs owned by the OpenTelemetry Operator and
the Prometheus Operator. Without those operators, applying its output fails
with `no matches for kind "ServiceMonitor"`. The quickstart chart installs
everything:

```bash
helm dependency update charts/lantern-stack

helm install lantern charts/lantern-stack \
  -n observability --create-namespace \
  -f charts/lantern-stack/values-quickstart.yaml
```

This installs:

| Component | Why |
|---|---|
| OpenTelemetry Operator | The `Instrumentation` CRD — injects agents into your pods |
| Prometheus Operator | The `ServiceMonitor` and `PrometheusRule` CRDs |
| Prometheus | Metrics storage |
| **node-exporter** | **Node system metrics** (CPU, memory, disk, network) |
| **kube-state-metrics** | **Cluster object metrics** (deployments, pods, restarts) |
| Grafana | Dashboards — with kube-prometheus-stack's cluster views preloaded |
| Tempo | Trace storage |
| Loki | Log storage |
| OTel Collector | Receives from every injected agent, fans out to the three backends |

> **This is demo-scale.** Ephemeral storage, 24-hour retention, single
> replicas, no HA. It exists so the project is evaluable in five minutes. Do
> not run it as production without sizing the subcharts yourself.

Wait for everything to come up:

```bash
kubectl get pods -n observability -w
```

### A2. Get the stack description

The chart writes an `ObservabilityStack` describing exactly what it just
installed. Read it back:

```bash
kubectl get cm -n observability lantern-stack \
  -o jsonpath='{.data.stack\.yaml}' > stack.yaml

cat stack.yaml
```

This is the neat part: the chart tells the compiler what exists, so the two
can never disagree about endpoint addresses.

Now skip to **[Step 2 — discover your services](#2-discover-your-services)**.

---

## Path B — bring your own backend

You already run Prometheus (or Mimir), Grafana, and maybe Tempo and Loki.
Lantern installs nothing and just generates config pointing at them.

### B1. Check your prerequisites

Lantern needs two operators present. Check:

```bash
kubectl get crd instrumentations.opentelemetry.io   # OpenTelemetry Operator
kubectl get crd servicemonitors.monitoring.coreos.com   # Prometheus Operator
```

If either is missing, install just that piece — you do not need the whole
quickstart:

```bash
helm install lantern charts/lantern-stack \
  -n observability --create-namespace \
  --set opentelemetry-operator.enabled=true \
  --set backends.metrics.remoteWrite=http://YOUR-PROMETHEUS:9090/api/v1/write \
  --set backends.traces.otlpEndpoint=YOUR-TEMPO:4317 \
  --set backends.logs.endpoint=http://YOUR-LOKI:3100
```

### B2. Write your stack file

Or write `stack.yaml` by hand. Start from `examples/stack.yaml`:

```yaml
apiVersion: lantern.dev/v1alpha1
kind: ObservabilityStack
metadata:
  name: default
spec:
  backends:
    metrics:
      type: prometheus
      remoteWrite: http://mimir.observability:9009/api/v1/push
      queryURL: http://mimir.observability:9009/prometheus
      operator: prometheus-operator
    traces:
      type: tempo
      otlpEndpoint: tempo.observability:4317
    logs:
      type: loki
      endpoint: http://loki.observability:3100

  instrumentation:
    provider: otel-operator
    collector: http://otel-collector.observability:4318
    ebpf:
      enabled: false        # requires privileged access — opt in deliberately

  # Guardrails. Service owners cannot exceed these.
  policy:
    requireTeamLabel: true
    maxRouteCardinality: 200
    denyLabels: [user_id, email, session_id, request_id]

  # Applied to any service that declares no SLOs of its own.
  defaults:
    environment: production
    slo:
      availability: 99.9
      latency: { objective: 99.0, threshold: 500ms }
```

> **One thing to get right.** Your collector must have
> `resource_to_telemetry_conversion: enabled` on its Prometheus exporter.
> Without it, the `service_name` label that Lantern's generated SLI queries
> select on is never produced, and **every alert silently never fires**. The
> bundled collector sets this; if you run your own, check it.

---

## 2. Discover your services

You could write a `ServiceObservability` per service by hand. On a cluster
with forty microservices, you won't. So don't:

```bash
kubectl get deploy,statefulset,daemonset,cronjob -A -o yaml \
  | lantern discover - > services.yaml
```

Or against a GitOps repo, with no cluster access at all:

```bash
lantern discover ./k8s/manifests > services.yaml
```

### Read the output before applying it

Everything `discover` produces is **inference**. It shows its reasoning as
comments and marks uncertain guesses `REVIEW`:

```yaml
# instrumentation.runtime: java (container image eclipse-temurin:21-jre)
# serviceKind: http (container port "http")
# team: payments (label team=payments)
# target.metricsPort: metrics (container port named "metrics")
apiVersion: lantern.dev/v1alpha1
kind: ServiceObservability
metadata:
  name: checkout-api
  namespace: shop
spec:
  target:
    kind: Deployment
    name: checkout-api
    selector: { app: checkout-api }
    metricsPort: metrics
  serviceKind: http
  team: payments
  instrumentation:
    runtime: java
```

and summarises what needs attention on stderr:

```
discovered 7 service(s) from 8 workload(s)

review before applying:
  instrumentation.runtime    needs review on 3 service(s): nightly-reconcile, pricing-svc, session-store
  serviceKind                needs review on 2 service(s): session-store, settlement-worker
  target.metricsPort         needs review on 4 service(s): ...
  team                       needs review on 1 service(s): storefront
```

### What it infers, and from what

| Field | Signals, in priority order |
|---|---|
| `serviceKind` | CronJob → `cron` · off-the-shelf image (redis, postgres, kafka…) → `database`/`worker` · port named `grpc` → `grpc` · port named `http`/`web`/`api` → `http` · name containing worker/consumer → `worker` · no serving port → `worker` |
| `instrumentation.runtime` | Existing `inject-*` annotation → `OTEL_EXPORTER_OTLP_ENDPOINT` in env → container image → command |
| `team` | `team` · `squad` · `owner` · `app.kubernetes.io/part-of` · `backstage.io/owner` · then the `-team` flag |
| `target.metricsPort` | Port named `metrics`/`prometheus` → `prometheus.io/port` annotation → a single HTTP-like port |

Platform namespaces (`kube-system`, `cert-manager`, `argocd`, …) are skipped
by default. Use `-all` to include them, `-ns a,b` to restrict.

### Two things it refuses to guess

- **SLOs.** An objective is a promise your team makes. Inferring 99.9% from a
  manifest would present a guess as a commitment. Discovered services inherit
  the stack defaults, or get none.
- **Teams.** With no ownership label, `team` stays empty. If your stack policy
  sets `requireTeamLabel: true`, compilation *fails* for that service. That
  failure is the system working — an alert nobody owns is an alert nobody
  answers.

Use `-team unassigned` to get moving, then fix them properly.

---

## 3. Compile and apply

```bash
lantern synth -stack stack.yaml services.yaml > manifests.yaml
```

Diagnostics go to **stderr**, manifests to **stdout**, so piping is safe:

```bash
lantern synth -stack stack.yaml services.yaml | kubectl apply -f -
```

Read the diagnostics — they explain every decision:

```
info  checkout-api: instrumentation mode auto resolved to "agent" (runtime "java"
      from container image eclipse-temurin:21-jre): java has a mature agent
      injector with deeper span coverage than eBPF
info  checkout-api: dropping 4 policy-denied label(s) at scrape time: email,
      request_id, session_id, user_id
warn  pricing-svc: slo "availability" builds on experimental OpenTelemetry
      semantic conventions for serviceKind "grpc"
```

Useful flags:

| Flag | Effect |
|---|---|
| `-o <dir>` | One file per object instead of a stream — good for GitOps repos |
| `-quiet` | Suppress diagnostics |
| `-strict` | Fail the build on warnings — use this in CI |

For GitOps, commit the output rather than piping to `kubectl`:

```bash
lantern synth -stack stack.yaml services.yaml -o ./k8s/observability/
git add k8s/observability && git commit -m "regenerate observability config"
```

---

## 4. What you actually get

### Instrumentation, without touching application code

Lantern annotates your workload and the OpenTelemetry Operator injects an
agent at pod start. Your developers change nothing.

Which mechanism gets used is resolved automatically per service:

| Runtime | Mode | Why |
|---|---|---|
| Java, Node.js, Python | agent | Mature injectors, deeper framework spans than eBPF |
| .NET | agent | eBPF instrumentation has no .NET support yet |
| Go | eBPF | The Go agent is uprobe-based anyway and needs `OTEL_GO_AUTO_TARGET_EXE` |
| Rust, C++, unknown | eBPF | No agent exists; eBPF still yields RED metrics for HTTP/gRPC/SQL |
| Already instrumented | sdk-only | Configure the exporter, inject nothing |

Override per service when the guess is wrong:

```yaml
spec:
  instrumentation:
    mode: agent          # auto | agent | ebpf | sdk-only | none
    runtime: java
```

> **Why an eBPF service gets no annotation.** If an agent *and* an eBPF probe
> both instrument the same handler, every request produces two spans and
> doubled RED metrics — silently. Lantern treats the two as mutually exclusive
> and leaves eBPF workloads deliberately un-annotated so the operator cannot
> also inject an agent.

### SLOs that actually page correctly

Declare an objective:

```yaml
slos:
  - name: availability
    type: availability      # availability | latency | custom
    objective: 99.9
    window: 30d
  - name: latency
    type: latency
    objective: 99.0
    threshold: 300ms
    window: 30d
```

Lantern generates the SLI error-ratio recording rules at seven windows, five
metadata rules, and multiwindow multi-burn-rate alerts using the windows and
factors from the Google SRE Workbook:

| Severity | Short | Long | Factor | Fires at 99.9% when error rate exceeds |
|---|---|---|---|---|
| critical | 5m | 1h | 14.4 | 1.44% |
| critical | 30m | 6h | 6 | 0.6% |
| warning | 2h | 1d | 3 | 0.3% |
| warning | 6h | 3d | 1 | 0.1% |

The short window suppresses alerts for spikes that already stopped; the long
window establishes that budget is genuinely being consumed. Alerts carry a
`team` label, which is what Alertmanager routes on.

For anything without a built-in template, bring your own query:

```yaml
slos:
  - name: freshness
    type: custom
    objective: 99.0
    window: 7d
    errorQuery: sum(rate(settlement_batches_stale_total[{{.window}}]))
    totalQuery: sum(rate(settlement_batches_total[{{.window}}]))
```

`{{.window}}` is substituted at each burn-rate window.

### Semantic convention stability

Lantern generates queries against OpenTelemetry semantic conventions. Their
stability varies, and it warns you:

| serviceKind | Metric family | Status |
|---|---|---|
| `http` | `http_server_request_duration_seconds` | **stable** |
| `grpc` | `rpc_server_duration_seconds` | experimental |
| `worker` | `messaging_process_duration_seconds` | experimental |
| `database` | `db_client_operation_duration_seconds` | experimental |

HTTP is the only one whose underlying convention cannot move under you. For
the others, expect to pin your semconv version or use `type: custom`.

---

## 5. Verify it worked

```bash
# the annotation landed
kubectl get deploy checkout-api -n shop \
  -o jsonpath='{.spec.template.metadata.annotations}'

# Prometheus picked up the scrape config
kubectl get servicemonitor -n shop

# the rules loaded (an invalid rule file makes Prometheus reject ALL of them)
kubectl get prometheusrule -n shop -o yaml | head -40
```

Then port-forward Grafana:

```bash
kubectl port-forward -n observability svc/lantern-grafana 3000:80
# default quickstart login: admin / lantern
```

You will find kube-prometheus-stack's cluster and node dashboards, your traces
in Tempo, your logs in Loki, and Lantern's burn-rate alerts under **Alerting →
Alert rules**.

> **You will not find a per-service Lantern dashboard.** Dashboard generation
> is the next milestone and is not built. See [Project status](README.md#project-status).

---

## Troubleshooting

**`no matches for kind "ServiceMonitor"` / `"Instrumentation"`**
The corresponding operator is not installed. See [B1](#b1-check-your-prerequisites).

**Alerts exist but never fire, even during an outage**
Almost always the label mismatch. Lantern's SLI queries select on
`service_name` and `deployment_environment`, which only exist if your
collector's Prometheus exporter has `resource_to_telemetry_conversion:
enabled`. Check with:
```
sum(rate(http_server_request_duration_seconds_count{service_name="checkout-api"}[5m]))
```
If that returns nothing, the labels aren't there.

**Prometheus ignores everything Lantern generated**
kube-prometheus-stack defaults to only picking up resources with its own Helm
release labels. The quickstart values set
`serviceMonitorSelectorNilUsesHelmValues: false` — if you installed it
yourself, set that too.

**Duplicated spans, doubled request counts**
Something is instrumenting twice. Check whether the workload carries both a
Lantern-managed `inject-*` annotation and a separate eBPF probe. Lantern
enforces mutual exclusion for services it manages; a manually-added annotation
can defeat that.

**`spec.team is required by the ObservabilityStack policy`**
Working as intended. Either label the workload with its owning team, or pass
`-team <name>` to `discover`, or set `requireTeamLabel: false` in your stack
while you migrate.

**`serviceKind "cron" has no built-in SLI template`**
Stack default SLOs are skipped for service kinds with no template — you get a
warning, not a failure. Add a `type: custom` SLO to get burn-rate alerts for
those.

---

## Where to go next

- **[DESIGN.md](DESIGN.md)** — the architecture, why the compiler is a pure
  function, the competitive landscape, and the full roadmap.
- **[CONTRIBUTING.md](CONTRIBUTING.md)** — how to work on it. The P1 dashboard
  generator is the most valuable thing anyone could pick up.
- **[charts/lantern-stack/values.yaml](charts/lantern-stack/values.yaml)** —
  every knob on the stack, commented.
