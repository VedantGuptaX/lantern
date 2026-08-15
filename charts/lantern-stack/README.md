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

## Before publishing

Subchart versions in `Chart.yaml` are version *patterns*, written without
network access to the chart repositories. Run these on a machine with Helm
before tagging a release:

```bash
helm dependency update charts/lantern-stack
helm lint charts/lantern-stack
helm template lantern charts/lantern-stack -f charts/lantern-stack/values-quickstart.yaml
```
