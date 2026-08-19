# GPU and inference-server observability

Two separate things get asked for together often enough that they're worth
naming up front, because they go through different halves of Lantern:

1. **GPU node health** — utilization, temperature, power, ECC errors, XID
   errors — from `dcgm-exporter`, normally installed by the NVIDIA GPU
   Operator or its own chart (bring-your-own; `gpuMonitoring.installExporter`
   is the one narrow exception where Lantern installs just the exporter
   itself — see the chart README section linked below). This is
   cluster/node-level infrastructure monitoring, the same shape as
   node-exporter. It goes through `charts/lantern-stack`'s `gpuMonitoring`
   block, not the compiler — see the [chart
   README](../charts/lantern-stack/README.md#gpu--dcgm-monitoring--gpumonitoringenabled).
2. **Inference-server SLOs** — time-to-first-token (TTFT), inter-token
   latency, request queue depth — from the model-serving container itself
   (vLLM, Triton, NIM, TGI, ...). This is a per-service concern and goes
   through a normal `ServiceObservability` spec with `serviceKind: inference`.

This doc covers #2, plus how to join the two together for per-service GPU
saturation SLOs.

## Why there's no built-in metric family for this

Every other `serviceKind` (`http`, `grpc`, `worker`, `database`) maps to an
OpenTelemetry semantic-convention metric name — `http_server_request_duration`
and friends — that's the same everywhere, so the compiler can build the SLI
without you naming a metric. There's no equivalent for inference: vLLM, Triton
and NIM each name their histograms and gauges differently, and none of them
are an OTel semantic convention. So `serviceKind: inference` has **no
built-in SLI template** — every SLO on it is built from a `metric:` field you
supply, pointed at whatever your exporter actually emits. This is deliberate:
Lantern would rather make you name the metric than guess wrong and generate an
alert that silently never fires.

## `type: latency` with `metric:` — TTFT, inter-token latency, anything durational

```yaml
apiVersion: lantern.dev/v1alpha1
kind: ServiceObservability
metadata:
  name: llama-70b-server
  namespace: ml
spec:
  target:
    kind: Deployment
    name: llama-70b-server
    metricsPort: metrics
  serviceKind: inference
  team: ml-platform
  slos:
    - name: ttft
      type: latency
      metric: vllm:time_to_first_token_seconds   # histogram, no _bucket/_count suffix
      objective: 99.0
      threshold: 500ms
      window: 7d
    - name: inter-token-latency
      type: latency
      metric: vllm:time_per_output_token_seconds
      objective: 99.0
      threshold: 50ms
      window: 7d
  instrumentation:
    mode: none   # the app exports its own Prometheus metrics; no agent needed
```

This reuses the exact same burn-rate machinery every other `type: latency` SLO
gets — multiwindow-multiburn alerts, recording rules, the works — it just
points at your metric instead of an OTel semconv one. Two things to get right,
since Lantern can't check either for you:

- **The threshold must line up with an actual histogram bucket boundary.** If
  your exporter's buckets don't include the exact value your `threshold`
  converts to (in seconds), the query silently matches nothing and the SLO
  reads as "perfectly fast" instead of failing loudly. Check your exporter's
  bucket boundaries before picking a threshold.
- **`metric:` is the histogram base name**, not the full series name — Lantern
  appends `_count` and `_bucket` itself, matching how OpenTelemetry (and every
  Prometheus histogram client) exports one.

Every SLO built this way is flagged `warn` in `lantern synth` output, same as
any other unverified metric — read the diagnostic, it names the exact metric
you're trusting.

## `type: saturation` — queue depth, GPU utilization, anything gauge-based

Queue depth isn't a duration, so it doesn't fit the histogram model above.
`type: saturation` is a threshold-breach ratio over a gauge: the fraction of
scrapes where the gauge was above `threshold`. It reduces to the same
bad-events/total-events shape as every other SLO type, so it gets the same
burn-rate alerts:

```yaml
    - name: queue-depth
      type: saturation
      metric: vllm:num_requests_waiting
      objective: 99.0
      threshold: "10"   # a plain number — this isn't a duration
      window: 7d
```

Use this for anything reported as a point-in-time level: inference queue
depth, GPU utilization percent, GPU memory-copy utilization, or a DCGM metric
once you've wired up per-pod attribution (below).

## Discovery

`lantern discover` recognizes GPU inference workloads two ways: known image
names (`vllm/vllm-openai`, `tritonserver`, `nvcr.io/nim/...`,
`text-generation-inference`, and a few others) and, as a fallback, any
container requesting the `nvidia.com/gpu` extended resource. Either signal
gets `serviceKind: inference` with `instrumentation.mode: none` set
automatically — GPU model servers already export their own Prometheus
metrics, and injecting an OTel agent into a GPU-resident serving process is
rarely what you want. Both are flagged `REVIEW` like any other inferred field:
a GPU request alone doesn't distinguish an inference server from GPU batch or
training work, so check it.

Discovery never invents the `metric:` field on an SLO — it doesn't invent SLOs
at all for any service kind. You write those once you know what your exporter
calls things.

## Joining per-service SLOs to DCGM (per-pod GPU attribution)

By default, `dcgm-exporter`'s series are labeled by GPU and node (`gpu`,
`UUID`, `Hostname`, `pci_bus_id`) but not by pod — which is fine for the
cluster-level health alerts in `gpuMonitoring`, but not enough to build a
saturation SLO scoped to one `ServiceObservability`. To get pod/namespace
labels onto DCGM series, enable dcgm-exporter's own Kubernetes pod-mapping
feature (this is a dcgm-exporter setting, not a Lantern one):

```yaml
# on dcgm-exporter itself (e.g. via the NVIDIA GPU Operator's values)
env:
  - name: DCGM_EXPORTER_KUBERNETES
    value: "true"
```

If instead you're on the `gpuMonitoring.installExporter: true` self-install
path (see the chart README), this is already on by default via
`dcgm-exporter.kubernetes.enablePodLabels: true` in `values.yaml` — nothing
extra to configure. Either way, verify the resulting label names against a
live cluster's `/metrics` output before trusting an SLO selector on them, per
the warning below; chart defaults and NVIDIA's own env var both changed shape
across versions in the past.

Once that's live, DCGM series carry `pod`, `namespace`, and `container`
labels, and a saturation SLO can select on them the same way any other SLO
selects on its service:

```yaml
    - name: gpu-utilization
      type: saturation
      metric: DCGM_FI_DEV_GPU_UTIL
      objective: 99.0
      threshold: "95"
      window: 7d
```

Verify the labels are actually there before trusting the SLO — `kubectl
port-forward` to dcgm-exporter and check `/metrics` for `pod=` on the series,
or query it directly once Prometheus is scraping it. If the labels aren't
present, the SLO's selector (built from `service_name`/`pod`/`namespace`
depending on instrumentation mode — see `prom.SeriesSelector`) won't match
anything, and the SLO will silently read as always-passing rather than error.

## Cheat sheet: common metric names by exporter

Lantern doesn't verify any of these — check them against your actual
exporter's `/metrics` output before shipping an SLO. Names, buckets, and
availability change between versions.

| Signal | vLLM (OpenAI-compatible server) | Triton Inference Server | dcgm-exporter |
|---|---|---|---|
| Time to first token | `vllm:time_to_first_token_seconds` | not exposed by default | — |
| Inter-token / per-output-token latency | `vllm:time_per_output_token_seconds` | not exposed by default | — |
| End-to-end request latency | `vllm:e2e_request_latency_seconds` | `nv_inference_request_duration_us` (microseconds, not seconds) | — |
| Queue depth | `vllm:num_requests_waiting` | `nv_inference_pending_request_count` | — |
| GPU utilization | — | — | `DCGM_FI_DEV_GPU_UTIL` |
| GPU memory-copy utilization | — | — | `DCGM_FI_DEV_MEM_COPY_UTIL` |
| GPU temperature | — | — | `DCGM_FI_DEV_GPU_TEMP` |
| GPU power draw / cap | — | — | `DCGM_FI_DEV_POWER_USAGE` / `DCGM_FI_DEV_POWER_MGMT_LIMIT` |
| Uncorrectable ECC errors | — | — | `DCGM_FI_DEV_ECC_DBE_VOL_TOTAL` |
| XID errors | — | — | `DCGM_FI_DEV_XID_ERRORS` |

NVIDIA NIM wraps vLLM or TensorRT-LLM depending on configuration; check which
backend your NIM deployment uses before assuming the vLLM column applies.
Triton has no built-in notion of tokens — TTFT/inter-token latency only exist
if your backend (e.g. a TensorRT-LLM Triton backend) adds them itself, under
its own metric names.
