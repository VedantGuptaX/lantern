# Getting metrics, logs, and traces into Grafana

This exists because "I ran `lantern synth` and installed the chart, why is
Grafana empty?" has three different real answers depending on which signal
you mean. They don't share a root cause, so they don't share a fix.

## The short version

| Signal | Works out of the box? | What's actually needed |
|---|---|---|
| Cluster/node health (CPU, memory, disk, load) | **Yes** | Nothing — `kube-prometheus-stack`'s Grafana ships default cluster/node dashboards automatically (`defaultDashboardsEnabled: true` upstream). |
| Per-service metrics | **Mostly** — needs `target.metricsPort` set correctly per service | `ServiceMonitor`s scrape once the port is right; per-service *dashboards* need `backends.dashboards.type: grafana` (on by default) and a Prometheus datasource UID set (`backends.metrics.datasource`). |
| Logs | **No, not without `logsCollector.enabled`** | Nothing tails pod logs by default anywhere in this project. See below. |
| Traces | **No, not without a separately-installed eBPF probe (or SDK agent)** | `mode: ebpf` is a compiler decision, not a deployed probe. See below. |

## Logs: nothing ships them by default

This is the one worth understanding, because it's easy to assume it already
works. `values-quickstart.yaml` turns on Loki (`loki.enabled: true`) — a log
*store* — and wires a Grafana datasource to query it. Neither of those puts a
single log line into Loki. The OTel Collector `collector.yaml` deploys
(Deployment-mode, always on) has a `logs` pipeline, but its only receiver is
`otlp` — it accepts logs actively pushed to it via OTLP, which nothing does
unless a service's own SDK is configured to export logs that way. Nothing in
this project reads `/var/log/pods` by default.

The fix: `logsCollector.enabled` (on in `values-quickstart.yaml`, off in the
bring-your-own `values.yaml`) deploys a second, DaemonSet-mode
`OpenTelemetryCollector` — a pod on every node, reading container log files
directly, because that's the only way to reach node-local files. See
`charts/lantern-stack/templates/logs-collector.yaml` and the chart README's
"Log collection" section for the full pipeline. The short version: it tails
`/var/log/pods/*/*/*.log`, uses the `container` operator to extract
`k8s.namespace.name`/`k8s.pod.name`/`k8s.pod.uid`/`k8s.container.name`
straight from the file path (no application changes needed), then the
`k8sattributes` processor uses `k8s.pod.uid` to look up the owning Deployment
via the Kubernetes API and adds `k8s.deployment.name`.

**Why filtering by namespace/deployment/pod/service just works in Grafana
with no extra config:** Loki's own OTLP ingestion treats
`k8s.namespace.name`, `k8s.pod.name`, `k8s.deployment.name`, and
`service.name` as default index labels — checked directly against Loki's
OTLP ingestion docs, not assumed. Every log line lands with `namespace`,
`pod`, and `deployment` as real Loki labels. A `service.name` (→
`service_name` in Loki) is additionally derived from the Deployment name via
a `resource` processor, so it lines up with the same label convention
per-service SLI queries already use where SDK/agent instrumentation is
active. If your Deployment naming doesn't match how you think of "the
service," adjust that processor or the generated dashboard's query directly.

To find a specific service's logs by hand, once this is running:

```logql
{namespace="shop", deployment="checkout-api"}
```

## Traces: `mode: ebpf` doesn't deploy anything

This one is subtler. `lantern discover`/`synth` will happily resolve a
service to `instrumentation.mode: ebpf` — and that's a correct compiler
decision, not a bug — but check `pkg/emit/otel/otel.go`'s own comment on the
`ModeEBPF` case: *"eBPF is configured at the collector/OBI level, not by
annotating the workload."* Lantern's job for eBPF mode is to deliberately
**not** inject a per-pod agent annotation, on the assumption that something
else is doing eBPF-based auto-instrumentation and pushing the result to the
collector's OTLP receiver (which is already listening, always — no change
needed there). Nothing in this chart deploys that something else. There's no
OBI/Beyla DaemonSet template anywhere in `charts/lantern-stack/templates/`.

This mirrors the `gpuMonitoring` pattern exactly — Lantern assumes an
externally-managed dcgm-exporter and just wires a `ServiceMonitor` to it —
except dcgm-exporter is a mature, commonly-pre-installed piece of
infrastructure on GPU clusters, whereas an eBPF instrumentation probe
generally isn't something already sitting on a typical cluster. If a service
resolved to `mode: ebpf` and you expect to see its traces, one of these needs
to happen:

1. Install [OpenTelemetry eBPF Instrumentation (OBI)](https://github.com/open-telemetry/opentelemetry-ebpf-instrumentation)
   (formerly Grafana Beyla) yourself, pointed at the collector's OTLP
   endpoint — typically `http://<release>-collector.<namespace>.svc:4317`
   (confirm the exact service name with `kubectl get svc -n <namespace>`).
   This needs privileged/kernel access; treat it with the same caution
   `instrumentation.ebpf.privileged: true` already signals in this chart's
   values.

   **Verified end-to-end on a real cluster** (not a hypothetical — traces
   confirmed reaching Tempo and rendering in Grafana). Four real gotchas hit
   along the way, none obvious from OBI's own default values:

   ```bash
   helm repo add open-telemetry https://open-telemetry.github.io/opentelemetry-helm-charts
   helm install obi open-telemetry/opentelemetry-ebpf-instrumentation \
     -n observability \
     --set privileged=true \
     --set resources.requests.cpu=10m \
     --set resources.requests.memory=256Mi \
     --set resources.limits.memory=512Mi \
     --set config.data.otel_traces_export.endpoint="http://<release>-collector.<namespace>.svc:4317" \
     --set-json 'config.data.discovery.instrument=[{"k8s_namespace":"your-app-namespace"}]' \
     --set-json 'volumes=[{"name":"bpffs","hostPath":{"path":"/sys/fs/bpf","type":"DirectoryOrCreate"}}]' \
     --set-json 'volumeMounts=[{"name":"bpffs","mountPath":"/sys/fs/bpf"}]'
   ```

   - **Memory, not CPU, is what OBI actually needs.** The chart ships with
     zero default resources (same "you decide" pattern as every subchart
     this project depends on). A too-small memory limit doesn't fail
     loudly — the pod stays `Running`, but the eBPF tracer silently gives
     up per-process: `"couldn't load tracer ... map create: cannot
     allocate memory"`, and that process's traces just never appear
     anywhere. 256Mi/512Mi was enough in practice; size up if you're
     tracing many processes per node.
   - **`/sys/fs/bpf` needs a real hostPath mount.** Without it, OBI starts
     clean and even traces *some* things (DNS-level probes worked), but
     HTTP/gRPC tracing — which needs pinned eBPF maps to correlate
     multi-packet request/response state — silently produces nothing.
     The warning in the logs is easy to miss: `"creating OTEL namespace in
     bpffs failed (is bpffs mounted?)"`, followed by `"OBI will still
     work, but features depending on pinned maps... will be disabled"` —
     which undersells it; basic tracing depends on this too, not just the
     optional log-enricher/profile-correlation features the message names.
   - **`privileged: true` was needed in practice.** The chart's narrower
     mode (`privileged: false` + `extraCapabilities`) is real and worth
     trying first on a cluster where minimizing privilege matters more —
     but even after adding every capability the values.yaml comments
     suggested (`BPF`, `SYS_PTRACE`, `NET_RAW`, `SYS_ADMIN`), HTTP tracing
     still failed with `"error running iterator in netns: join target ns:
     operation not permitted"`. Full `privileged: true` (the chart's own
     default) resolved it immediately. If your cluster already runs a CNI
     with comparably privileged node agents (Calico, Cilium, Azure CNI),
     this isn't a new category of exposure — just know it's not the
     minimal-privilege path.
   - **`contextPropagation.enabled` (default `true`) pulls in
     `hostNetwork: true`.** That's a real, separate blast-radius increase
     from `privileged: true` — sharing the node's network namespace, not
     just kernel capabilities — and it's only needed to correlate spans
     *across* service-to-service calls into one connected trace. Setting
     `contextPropagation.enabled: false` avoids `hostNetwork` entirely and
     still produces real, useful per-service traces; you lose cross-service
     correlation, not tracing itself. Worth the tradeoff on most clusters.
   - Scope `config.data.discovery.instrument` to your actual app
     namespace(s) (`k8s_namespace: your-app-namespace`, repeatable). OBI's
     default tries to instrument every process on the node — system pods
     included — which burns CPU/memory budget on tracing calico, coredns,
     etc. that you almost certainly don't want traces for anyway.

2. Or: force `mode: agent`/`mode: sdk` for that service instead
   (`spec.instrumentation.mode` in its `ServiceObservability`), if an SDK
   agent exists for its runtime and eBPF's shallower span coverage isn't
   good enough.
3. Or: accept that this service currently has metrics and (once
   `logsCollector` is on) logs, but no traces, until one of the above
   happens. That's an honest, working state — not a broken one.

## Dashboards: generated automatically once datasources are configured

`ServiceObservability`'s compiler now emits a per-service Grafana dashboard
(`pkg/emit/grafana`), gated on `backends.dashboards.type: grafana` (already
the default in `values.yaml`) and `spec.dashboard.disabled` (default false).
It's a `ConfigMap` labeled `grafana_dashboard: "1"`, picked up automatically
by the Grafana sidecar that's already enabled
(`sidecar.dashboards.enabled: true`, `searchNamespace: ALL` in
`values-quickstart.yaml`) — no extra install needed for the bundled
kube-prometheus-stack Grafana this chart already deploys.

What's on it: a burn-rate timeseries panel and an error-budget-remaining stat
panel per SLO — built from the exact same recording rules
(`lantern:sli_error:ratio_rate*`, `lantern:current_burn_rate:ratio`,
`lantern:period_error_budget_remaining:ratio`) that drive the burn-rate
alerts, so the dashboard and the alert can never disagree — plus a Logs panel
(Loki, filtered by namespace/deployment) and a Traces panel (Tempo, TraceQL
by `resource.service.name`), each skipped with an explanatory diagnostic if
its datasource UID isn't configured. `spec.dashboard.extraPanels` appends
custom PromQL panels; `spec.dashboard.patches` applies RFC 6902 JSON Patch
operations to the generated dashboard JSON as an escape hatch for anything
the generator doesn't cover.

**Datasource UIDs have to actually be set for panels to render.**
`values-quickstart.yaml` sets `backends.metrics.datasource: prometheus`,
`backends.traces.datasource: tempo-main`, and
`backends.logs.datasource: loki-main` to match what's actually provisioned
(kube-prometheus-stack's own Grafana datasource defaults to UID
`"prometheus"` — checked against the chart source, not assumed; the other
two match `additionalDataSources` in the same values file). If you're on the
bring-your-own path (`values.yaml`), these default to empty and need to be
set to your own Grafana's actual datasource UIDs, or the dashboard renders
with those panel groups skipped and a diagnostic explaining why.

**What this deliberately does not do yet:** per-team folders. `Backends.
Dashboards.FolderStrategy`/`InstanceRef`/`InstanceLabels` in the
`ObservabilityStack` type, and the `GrafanaFolder`/`GrafanaDashboard` ranks
already reserved in `pkg/kube/object.go`'s sort order, point at a future
upgrade to grafana-operator's CRD-based provisioning model (a `Grafana` CR
plus `GrafanaDashboard`/`GrafanaFolder` CRs reconciled into it) — that gets
real per-team folders, but needs grafana-operator installed, which this chart
doesn't do. Every generated dashboard lands in Grafana's default "General"
folder for now. See `plan.md`'s roadmap (P1: "Grafana folders and team
RBAC").

## Kubernetes Events: not shipped by default, same pattern as OBI

Same shape of gap as traces: `kubectl get events` has almost no memory —
events expire from etcd roughly an hour after they happen, by design, and
there's no built-in way to query "what events fired at 7:03pm three days
ago." Nothing in this chart exports them anywhere longer-lived. If you want
Warning-type events (OOMKilled, Evicted, BackOff, FailedScheduling,
Unhealthy, etc.) queryable alongside your logs/metrics/traces on a real
lookback window, something needs to watch the Events API continuously and
push to Loki as events happen — this chart doesn't do that for you, same as
it doesn't deploy an eBPF probe for you.

**Verified end-to-end on a real cluster** using
[kubernetes-events-exporter](https://github.com/ownkube/kubernetes-events-exporter)
— an actively-maintained fork of the original `resmoio` project, which has
been dormant since 2023 and has documented event-loss bugs; Bitnami's chart
was deprecated in August 2025. Deployed as a **standalone Helm release, not
part of this chart** (same reasoning as OBI: it's a cluster add-on orthogonal
to what Lantern's compiler emits, not something the `ServiceObservability`
object model has any business modeling).

```bash
helm upgrade --install lantern-events-exporter \
  oci://ghcr.io/ownkube/charts/kubernetes-events-exporter \
  --version 0.1.2 \
  -n observability \
  -f events-exporter-values.yaml
```

`events-exporter-values.yaml`:

```yaml
fullnameOverride: lantern-events-exporter

replicaCount: 1

resources:
  requests:
    cpu: 20m
    memory: 32Mi
  limits:
    memory: 64Mi

# See the RBAC note below -- the chart's own ClusterRole is intentionally
# not used here.
rbac:
  create: false

serviceAccount:
  create: true

config:
  logLevel: info
  logFormat: json
  # Only forward events newer than this. Without a cap, the exporter's
  # first sync after a restart would replay whatever's still sitting in
  # etcd's ~1h window as a burst -- small compared to the logsCollector
  # backlog-replay storm documented above (this is capped at etcd's own
  # short retention, not a node's entire log history), but the same
  # underlying shape of bug, worth capping deliberately rather than
  # relying on it being small enough to not matter.
  maxEventAgeSeconds: 60
  route:
    routes:
      - match:
          - receiver: "loki"
  receivers:
    - name: "loki"
      loki:
        url: http://lantern-loki.observability.svc.cluster.local:3100/loki/api/v1/push
        streamLabels:
          job: k8s-events
          namespace: "{{ .InvolvedObject.Namespace }}"
          kind: "{{ .InvolvedObject.Kind }}"
          reason: "{{ .Reason }}"
          type: "{{ .Type }}"
```

- **The chart's default RBAC is far broader than this needs, and it's worth
  scoping down.** `rbac.create: true` grants cluster-wide `get/watch/list` on
  `apiGroups: ["*"], resources: ["*"]` — every object in every namespace,
  including Secrets — plus full CRUD on `coordination.k8s.io/leases`. That's
  enough to watch Events, but it's also enough to read every Secret on the
  cluster if this pod's identity were ever compromised, and the chart
  exposes no values field to narrow it — the ClusterRole is hardcoded in its
  template. Set `rbac.create: false` and apply a hand-scoped ClusterRole
  instead, covering only Events plus the object kinds the exporter actually
  looks up for `InvolvedObject` enrichment (it caches Pod/Deployment/etc.
  metadata by default — `config.omitLookup: true` skips this if you'd rather
  not grant even read access to workload objects):

  ```yaml
  apiVersion: rbac.authorization.k8s.io/v1
  kind: ClusterRole
  metadata:
    name: lantern-events-exporter
  rules:
    - apiGroups: [""]
      resources: ["events", "pods", "nodes", "namespaces", "replicationcontrollers"]
      verbs: ["get", "list", "watch"]
    - apiGroups: ["events.k8s.io"]
      resources: ["events"]
      verbs: ["get", "list", "watch"]
    - apiGroups: ["apps"]
      resources: ["deployments", "replicasets", "daemonsets", "statefulsets"]
      verbs: ["get", "list", "watch"]
    - apiGroups: ["batch"]
      resources: ["jobs", "cronjobs"]
      verbs: ["get", "list", "watch"]
  ---
  apiVersion: rbac.authorization.k8s.io/v1
  kind: ClusterRoleBinding
  metadata:
    name: lantern-events-exporter
  roleRef:
    apiGroup: rbac.authorization.k8s.io
    kind: ClusterRole
    name: lantern-events-exporter
  subjects:
    - kind: ServiceAccount
      name: lantern-events-exporter
      namespace: observability
  ```

  This is a **cluster-scoped RBAC change** (`ClusterRole`/`ClusterRoleBinding`)
  — treat it with the same care as any other CRD/webhook/ClusterRoleBinding
  decision: read it, understand exactly what it grants, don't apply it on
  autopilot.

- **Query it like any other Loki stream.** The `job="k8s-events"` label
  distinguishes it from pod logs; `namespace`, `kind`, `reason`, and `type`
  (`Normal`/`Warning`) come from the event itself via `streamLabels`, no
  extra parsing needed for basic filtering. For a readable message instead
  of the raw JSON payload the Loki sink sends, use a LogQL `| json` +
  `line_format` pipeline, e.g.:

  ```
  {job="k8s-events", namespace=~"my-namespace"}
    | json
    | line_format "[{{.type}}] {{.reason}} — {{.involvedObject_kind}}/{{.involvedObject_name}}: {{.message}}"
  ```

- **This closes the historical-lookback gap, but only from the moment it's
  deployed onward** — it cannot retroactively recover events that already
  expired from etcd before it started running. Deploy it early, not
  reactively after an incident you wish you could look back on.

## Durable storage for logs and traces (production clusters)

The quickstart's own defaults are demo-scale: Loki's chart defaults
`singleBinary.persistence.enabled: true` (a real PVC, 10Gi, out of the box —
no override needed), but Tempo's chart defaults `persistence.enabled:
false` — trace data lives on the pod's ephemeral writable layer and is gone
on every restart or reschedule. Fine for a five-minute demo, silently weaker
than log retention for anything you'd actually want to query after an
incident.

`values-quickstart.yaml` now sets `tempo.persistence.enabled: true` (10Gi)
to match Loki. **Verified on a real cluster**: a StatefulSet's
`volumeClaimTemplates` cannot be added in-place — Kubernetes rejects the
`helm upgrade` outright (`Forbidden: updates to statefulset spec for fields
other than 'replicas', ... are forbidden`). Enabling persistence on an
already-running Tempo needs `kubectl delete statefulset <release>-tempo -n
<namespace>` (safe here specifically because it had no PVC yet, so there was
no trace data to lose) followed by `helm upgrade` to recreate it with the
new spec. If you're enabling this on a Tempo that's already had persistence
on and is just changing size/storageClassName, that same deletion still
applies — plan for the pod restart, not just the config change.

If your organization already has a central log/trace store (an existing
Loki/Tempo cluster, an ELK stack, a vendor SaaS), pointing Lantern's
collector at that instead of the quickstart's own Loki/Tempo (the
bring-your-own path, `values.yaml`) avoids the PV question entirely — see
`docs/adopting-an-existing-cluster.md`. PV-backed persistence and "point at
an existing central store" are the two real options; an ephemeral quickstart
install is not a production posture for either signal.
