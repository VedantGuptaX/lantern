# lantern-stack

The backends Lantern compiles against.

Two modes:

| Mode | Command | What it does |
|---|---|---|
| **Bring your own** (default) | `helm install lantern charts/lantern-stack` | Installs nothing. Renders an `ObservabilityStack` describing the Prometheus/Grafana/Tempo/Loki you already run, plus an OTel Collector that forwards to them. |
| **Quickstart** | `helm install lantern charts/lantern-stack -f values-quickstart.yaml` | Installs a complete stack on an empty cluster. Demo-scale only. |

See [GETTING_STARTED.md](../../GETTING_STARTED.md) for the full walkthrough.

## The ConfigMap loop

The chart writes the `ObservabilityStack` it generated into a ConfigMap, so the
compiler reads its configuration from the thing that installed the backends —
the two can never disagree about endpoint addresses.

```bash
kubectl get cm -n observability lantern-stack \
  -o jsonpath='{.data.stack\.yaml}' > stack.yaml
```

It is a ConfigMap rather than a custom resource because P0 ships no operator
and therefore no CRDs; a CR would be rejected by the API server.

## Two settings that matter more than they look

**`resource_to_telemetry_conversion: enabled`** on the collector's Prometheus
exporter. Without it the `service_name` label that Lantern's generated SLI
queries select on is never produced, and every alert silently never fires.

**`serviceMonitorSelectorNilUsesHelmValues: false`** on Prometheus. Without it,
kube-prometheus-stack only picks up resources carrying its own Helm release
labels, and ignores everything Lantern emits.

Both are set in `values-quickstart.yaml`. If you run these components yourself,
check them.

## GPU / DCGM monitoring — `gpuMonitoring.enabled`

Off by default. When turned on, adds a `ServiceMonitor` pointed at
dcgm-exporter's Service, a `PrometheusRule` of GPU health alerts (thermal
throttle risk, uncorrectable ECC errors, XID errors, power headroom), and
(`gpuMonitoring.dashboard.enabled`, on by default alongside it) a fleet-wide
Grafana dashboard — GPU/node count, per-GPU utilization and memory-copy,
temperature with the same `thermalCriticalC` threshold line the alert above
uses, power draw vs. cap, and 24h ECC/XID error tables. See
`templates/gpu-dashboard.yaml`. None of this restarts pods or touches running
services.

Default assumption is bring-your-own — dcgm-exporter is ALREADY installed and
running, typically by the NVIDIA GPU Operator, sometimes as a standalone
chart:

```yaml
gpuMonitoring:
  enabled: true
  dcgmExporter:
    namespace: gpu-operator          # where dcgm-exporter actually runs
    selector: { app: nvidia-dcgm-exporter }
    port: metrics
```

### `gpuMonitoring.installExporter` — the one case this chart *will* install

There's a narrower, safe self-install path for GPU nodes that already run
real GPU workloads (driver + device plugin proven working — something has
already been scheduled against the `nvidia.com/gpu` extended resource) but
have no dcgm-exporter on them yet. This does **not** install the NVIDIA GPU
Operator, a driver, or the container toolkit — only the `dcgm-exporter`
chart itself (a new dependency, condition `dcgm-exporter.enabled`), same
class of change as node-exporter.

```yaml
gpuMonitoring:
  enabled: true
  installExporter: true
dcgm-exporter:
  enabled: true
  nodeSelector:
    nvidia.com/gpu.present: "true"   # REQUIRED -- see below, don't guess this
```

Before turning this on:

1. Run `./scripts/preflight-check.sh --gpu-install-exporter` against the real
   cluster. It's read-only and BLOCKs unless at least one node already
   advertises `nvidia.com/gpu` as allocatable — the only cluster-visible
   proof the driver/device plugin actually work — and it prints the real
   label(s) present on those specific nodes.
2. Set `dcgm-exporter.nodeSelector` to one of the labels the preflight check
   printed. It defaults to `{}` (empty) deliberately: there's no universal
   label for "this is a GPU node" across clusters, and an empty selector
   schedules the DaemonSet onto every node, including non-GPU ones, where it
   CrashLoopBackOffs.

When `installExporter: true`, `templates/gpu-monitoring.yaml` computes the
ServiceMonitor's namespace/selector/port from the `dcgm-exporter` subchart's
own deterministic values (`.Release.Namespace`,
`app.kubernetes.io/name=dcgm-exporter` + `app.kubernetes.io/instance=<release
name>`, port `metrics`) rather than trusting `gpuMonitoring.dcgmExporter.*`,
which still describes the bring-your-own default and would point at the
wrong thing here. The subchart's own `serviceMonitor.enabled` is forced off
in `values.yaml` to avoid creating two ServiceMonitors for the same pods.
`kubernetes.enablePodLabels: true` is turned on by default too, so DCGM
series carry pod/namespace labels immediately — the automated equivalent of
the `DCGM_EXPORTER_KUBERNETES=true` step described below for the
bring-your-own path.

`lantern init` offers this as a sub-question when you answer "yes" to GPU
monitoring and "no" to "is dcgm-exporter already running" — but since `init`
never touches a live cluster (see CLAUDE.md), it can't verify GPU-readiness
itself; the preflight check above is the actual gate.

This is cluster-level infrastructure monitoring, not per-service SLOs — same
split as node-exporter vs. an OpenTelemetry HTTP latency SLO. For a specific
`ServiceObservability` to build its own SLO against GPU saturation (or against
an inference server's queue depth, time-to-first-token, or inter-token
latency), use `serviceKind: inference`. Set `inferenceServer: vllm|triton|nim|
tgi` and the compiler generates the standard SLOs (ttft / inter-token / queue-
depth) against that server's known metric names automatically — no `metric:`
to hand-write (`lantern discover` even sets `inferenceServer` for you from the
image); or declare your own `type: saturation` / `type: latency` SLOs with an
explicit `metric:` to override. See
[docs/gpu-and-inference-observability.md](../../docs/gpu-and-inference-observability.md).
Per-pod GPU attribution on a bring-your-own install additionally requires
dcgm-exporter's own `DCGM_EXPORTER_KUBERNETES=true` setting (configured on
dcgm-exporter, not here) so its series carry `pod`/`namespace` labels a
selector can match — `installExporter: true` gets this via
`kubernetes.enablePodLabels` instead, as above.

## Log collection — `logsCollector.enabled`

Off by default in `values.yaml`, on in `values-quickstart.yaml`. Without it,
`loki.enabled: true` deploys an empty log *store* with a Grafana datasource
pointed at it — nothing actually tails pod logs into it. `collector.yaml`'s
Deployment-mode collector doesn't do this either: its `logs` pipeline only
accepts telemetry actively pushed to it via OTLP, and nothing in Lantern
pushes application logs that way unless a service's own SDK does.

`logsCollector` is a separate DaemonSet-mode `OpenTelemetryCollector` (see
`templates/logs-collector.yaml`) that tails `/var/log/pods` on every node,
extracts `k8s.namespace.name`/`k8s.pod.name`/`k8s.deployment.name` from the
log file path itself, resolves the owning Deployment via the Kubernetes API
(hence the `ClusterRole` this template also creates), and ships to
`backends.logs`. Loki's own OTLP ingestion treats those three plus
`service.name` as index labels by default — checked against Loki's docs
directly — so namespace/pod/deployment/service filtering in Grafana needs no
extra Loki-side config. See
[docs/getting-signals-into-grafana.md](../../docs/getting-signals-into-grafana.md)
for the full picture, including the equivalent gap for traces (eBPF mode
needs a probe Lantern does not install) and how the compiler's generated
dashboards use all of this.

## Dashboards — generated automatically once datasources are configured

The compiler emits a per-service Grafana dashboard (`pkg/emit/grafana`) for
every `ServiceObservability`, gated on `backends.dashboards.type: grafana`
(the default) and picked up by the Grafana sidecar this chart already
enables (`ConfigMap` labeled `grafana_dashboard: "1"`). Panels reuse the
exact recording rules that drive the burn-rate alerts, so the dashboard and
the alert can never disagree.

For `serviceKind: inference` specifically, the compiler adds one more panel:
GPU utilization scoped to that service's own pods (`DCGM_FI_DEV_GPU_UTIL{namespace=...,
pod=~"<service>-.*"}`). This needs dcgm-exporter's Kubernetes pod-mapping
turned on (see [docs/gpu-and-inference-observability.md](../../docs/gpu-and-inference-observability.md))
and is a best-effort pod-name-prefix match, not a guaranteed-unique join —
the panel's own description says so, don't trust it blind against a real SLO
without checking. This is separate from the `gpuMonitoring` fleet dashboard
above: that one is cluster-wide and chart-shipped, this one is per-service
and compiler-generated.

**`backends.{metrics,traces,logs}.datasource` have to actually match what's
provisioned, or panels render skipped, not broken-looking.**
`values-quickstart.yaml` sets these to `prometheus` / `tempo-main` /
`loki-main` to match kube-prometheus-stack's own Grafana datasource UID and
the `additionalDataSources` block further down in the same file — checked
against the chart source directly, not assumed (a real bug here — Tempo's
URL pointed at Loki's port, copy-pasted from the entry next to it — cost a
silent `502` on every trace query until it was found and fixed). If you're
on the bring-your-own path, these default to empty; set them to your own
Grafana's real datasource UIDs.

## Traces — bring your own eBPF probe (or SDK agent)

`instrumentation.ebpf.enabled` and `mode: ebpf` are compiler-side decisions;
neither this chart nor the compiler deploys the actual eBPF probe that
generates spans. Nothing in `charts/lantern-stack/templates/` runs
OBI/Beyla. Once you install one yourself pointed at
`http://<release>-collector.<namespace>.svc:4317`, traces work end-to-end —
verified for real, not just wired up: real spans confirmed reaching Tempo
and rendering in Grafana through its actual datasource proxy. See
[docs/getting-signals-into-grafana.md](../../docs/getting-signals-into-grafana.md)
for a working values example and four real gotchas (memory sizing for the
eBPF maps, the `/sys/fs/bpf` hostPath mount HTTP/gRPC tracing actually needs,
why the narrowed-capability path may not fully work, and the
`hostNetwork` tradeoff `contextPropagation.enabled` pulls in).

## Kubernetes Events — bring your own exporter, same pattern as traces

Same shape of gap: `kubectl get events` expires from etcd after ~1h, and
nothing in this chart exports them anywhere longer-lived. Verified end-to-end
on a real cluster using a standalone `kubernetes-events-exporter` release
(not part of this chart) pushing to Loki, queryable with a `job="k8s-events"`
label — see
[docs/getting-signals-into-grafana.md](../../docs/getting-signals-into-grafana.md#kubernetes-events-not-shipped-by-default-same-pattern-as-obi)
for the working values file and, importantly, why its own default RBAC
(cluster-wide read on every resource including Secrets) needs scoping down
before you apply it.

## Status: verified end-to-end against a real, populated AKS cluster

`helm install`/`helm upgrade` have run repeatedly against a real AKS cluster
running real multi-namespace workloads — not just `helm template`/`helm
lint`, and not just `kind` fixtures. Metrics, logs, traces, and generated
dashboards all confirmed working with real data by the end of that
verification: real Prometheus queries returning real values, real log lines
from real services in Loki, real spans in Tempo rendered through Grafana's
own proxy. `scripts/verify-kind.sh` — the disposable local-cluster rehearsal
— still hasn't run in an environment with `kind`/Docker available; real
cluster verification happened instead, which exercises real capacity
pressure and real RBAC in a way `kind` fixtures don't, but isn't a
substitute for the fast, no-blast-radius loop `verify-kind.sh` is meant to
give contributors.

Real problems found this way, all fixed and re-verified against the same
cluster they were found on:

- **Grafana's Loki and Tempo charts moved repositories** on 30 January 2026,
  from `grafana/helm-charts` to `grafana-community/helm-charts`. The old URL
  still serves archived versions but gets no new releases.
- **Loki's actual default deployment mode is `SimpleScalable`**, which needs
  object storage this chart doesn't configure. Pinning
  `deploymentMode: SingleBinary` alone isn't enough either — the
  `write`/`read`/`backend` replica counts default to 3 each regardless of
  `deploymentMode`, so those need zeroing too. Both are done in
  `values-quickstart.yaml`; confirmed the rendered output is a single
  StatefulSet, not three read/write/backend workloads.
- **Tempo's default receiver list includes `opencensus`**, a receiver type
  the pinned Tempo image dropped years ago — crashed on every start. Helm
  values merging can't delete an inherited map key, so the fix overrides the
  whole templated `config` string with `opencensus` excluded, not just the
  `receivers` map.
- **The OTel Operator subchart assumes cert-manager**
  (`admissionWebhooks.certManager.enabled: true` upstream), which most
  clusters adopting Lantern for the first time don't have. Its own
  self-signed-cert fallback also regenerates on every `helm upgrade` by
  default, with nothing restarting the pod serving the old cert — a real
  `helm upgrade` immediately after a clean install failed with "certificate
  signed by unknown authority" every time after that, forever, once it
  happened once. Both defaulted off/false in `values.yaml`.
- **The `OpenTelemetryCollector` CRD ships as a regular template**, not in
  Helm's special `crds/` directory, so on a genuinely fresh cluster it lands
  in the same apply batch as this chart's own `OpenTelemetryCollector`
  custom resources — which fails the whole install outright. Both
  `collector.yaml` and `logs-collector.yaml` now gate their CR on
  `.Capabilities.APIVersions.Has` (see `lantern.otelCollectorCRDReady` in
  `_helpers.tpl`), skipping gracefully on first install instead of failing;
  `make install-quickstart`/`install-byo` run the necessary follow-up
  `helm upgrade` automatically.
- **The in-cluster CRD-ownership preflight hook blocked every genuinely
  fresh install**, unconditionally. Helm's `crds/` mechanism never stamps an
  ownership annotation — not on a foreign install, and not on this
  release's own brand-new CRDs either — so "no annotation" was never
  reliable evidence of a conflict on its own. The hook (and
  `scripts/preflight-check.sh`) now additionally check whether the CRD
  actually has real custom-resource instances under it; an empty CRD has
  nothing to conflict with regardless of who "owns" the definition.
- **...and then blocked every `helm upgrade` after the first install too**,
  found on a real second upgrade of an already-populated cluster: a nonzero
  instance count alone still isn't proof of a foreign install — this
  release's own already-existing ServiceMonitors/PrometheusRules (applied
  via `lantern synth | kubectl apply`, not Helm) and its own
  Alertmanager/Prometheus CRs (Helm-owned, but the *CRD* itself still
  carries no annotation) were both being counted as foreign and blocking
  every upgrade. Fixed to check per-instance ownership — Helm annotation
  matching this release, or `lantern.dev/managed=true` — before blocking on
  a nonzero count.
- **Tempo has no persistent storage by default**; trace data lived on the
  pod's ephemeral writable layer and was gone on every restart. Loki's own
  chart already defaults to a real PVC (`singleBinary.persistence.enabled:
  true`); `values-quickstart.yaml` now sets `tempo.persistence.enabled: true`
  to match. Enabling this on an already-running Tempo needs a StatefulSet
  delete-and-recreate first — Kubernetes rejects adding
  `volumeClaimTemplates` to an existing StatefulSet in-place — see
  [docs/getting-signals-into-grafana.md](../../docs/getting-signals-into-grafana.md#durable-storage-for-logs-and-traces-production-clusters).
- **`logsCollector` shipped with `start_at: beginning`**, replaying a node's
  entire log history — every system pod included, not just app workloads —
  on every restart of the collector itself, not just its first start. On a
  real cluster this blew through Loki's own ingestion rate limit and
  out-of-order rejection window, and because the backlog and live traffic
  share the same send queue, real current log lines got delayed/dropped
  behind the backlog storm too. Now `start_at: end`, the correct setting for
  a persistent tailing daemon. Its default resources (100Mi/200Mi) were also
  too small for real application logs — the `memory_limiter` processor
  silently rejected log batches for any service verbose enough to log full
  SQL query text, with no error visible anywhere except the collector's own
  pod logs. Raised to 256Mi/512Mi in `values.yaml`.

If Loki (or Tempo) still gives you trouble on a capacity-constrained
cluster, `--set loki.enabled=false` / `--set tempo.enabled=false` and carry
on — metrics, instrumentation, and alerts don't depend on either (logs need
Loki, traces need Tempo, obviously).

To verify:

```bash
../../scripts/verify-kind.sh          # everything, on a throwaway kind cluster
```

or by hand:

```bash
helm dependency update charts/lantern-stack
helm lint charts/lantern-stack
helm template lantern charts/lantern-stack -f charts/lantern-stack/values-quickstart.yaml --validate
```

`make preflight` (100% read-only) computes real free capacity against the
[resource requirements table in the top-level README](../../README.md#resource-requirements)
before `make install-quickstart`/`install-byo` will call `helm install` at
all — every subchart here ships with zero default resource requests unless
this chart's own values files set one explicitly, so don't treat a
successful `helm install` as proof of a correctly-sized deployment.
