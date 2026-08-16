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

Off by default. When turned on, adds a `ServiceMonitor` pointed at whatever
dcgm-exporter you already run (installed by the NVIDIA GPU Operator or its own
chart — this chart does not install dcgm-exporter itself, the same way it
doesn't install node-exporter) plus a `PrometheusRule` of GPU health alerts:
thermal throttle risk, uncorrectable ECC errors, XID errors, and power
headroom. Neither restarts pods or touches running services.

```yaml
gpuMonitoring:
  enabled: true
  dcgmExporter:
    namespace: gpu-operator          # where dcgm-exporter actually runs
    selector: { app: nvidia-dcgm-exporter }
    port: metrics
```

This is cluster-level infrastructure monitoring, not per-service SLOs — same
split as node-exporter vs. an OpenTelemetry HTTP latency SLO. For a specific
`ServiceObservability` to build its own SLO against GPU saturation (or against
an inference server's queue depth, time-to-first-token, or inter-token
latency), use `serviceKind: inference` and `type: saturation` /
`type: latency` with an explicit `metric:` in the compiler — see
[docs/gpu-and-inference-observability.md](../../docs/gpu-and-inference-observability.md).
Per-pod GPU attribution additionally requires dcgm-exporter's own
`DCGM_EXPORTER_KUBERNETES=true` setting (configured on dcgm-exporter, not
here) so its series carry `pod`/`namespace` labels a selector can match.

## Status: written, never installed

This chart has not been resolved by `helm dependency update`, linted, rendered,
or installed. Treat it as a first draft.

Subchart versions were checked against upstream release pages in August 2026,
which caught two real problems worth knowing about:

- **Grafana's Loki and Tempo charts moved repositories** on 30 January 2026,
  from `grafana/helm-charts` to `grafana-community/helm-charts`. The old URL
  still serves archived versions but gets no new releases.
- **Loki jumped from 6.55.0 to the 17/18 series** — twelve majors of breaking
  changes. The default deployment mode changed from SimpleScalable to
  Monolithic, Enterprise support was removed, and the bundled MinIO is
  deprecated. The Loki block in `values-quickstart.yaml` is the least
  trustworthy part of this chart; if the install fails there, use
  `--set loki.enabled=false` and carry on. Metrics, traces, instrumentation
  and alerts do not depend on Loki.

To verify:

```bash
../../scripts/verify-kind.sh          # everything, on a throwaway kind cluster
```

or by hand:

```bash
helm dependency update charts/lantern-stack
helm lint charts/lantern-stack
helm template lantern charts/lantern-stack -f charts/lantern-stack/values-quickstart.yaml
```
