# GPU monitoring guide

A practical, end-to-end walkthrough for getting GPU and LLM-inference
observability out of Lantern. GPU observability comes in two independent
halves, and they go through different parts of the tool:

| Half | What it covers | How Lantern does it |
|---|---|---|
| **GPU node health** | utilization, temperature, power, clocks, ECC/XID errors | the `gpuMonitoring` Helm toggle — a `ServiceMonitor` for `dcgm-exporter` plus a `PrometheusRule` of health alerts |
| **Inference SLOs** | time-to-first-token, inter-token latency, request queue depth | `serviceKind: inference` + `spec.inferenceServer` — the compiler generates the SLOs, burn-rate alerts, and dashboard for you |

You can use either half on its own. Most GPU fleets want both: node health tells
you a card is throttling or degraded; inference SLOs tell you users are getting
slow tokens.

For the underlying SLI/metric reference (exact query shapes, the per-exporter
metric cheat sheet, per-pod attribution), see
[gpu-and-inference-observability.md](gpu-and-inference-observability.md). This
guide is the "how to set it up and what you get" companion to it.

---

## Fastest path: `lantern init`

`lantern init` asks what you run and writes a matching Helm values overlay. When
you answer yes to GPU monitoring, it also asks which inference servers you run:

```
GPU node monitoring (DCGM)? [y/N] y
  Is dcgm-exporter already running on those GPU nodes ...? [Y/n] y
  Which inference servers run on those GPU nodes? Lantern will auto-build
  their SLOs + dashboard panels ...
    1) vllm   2) triton   3) nim   4) tgi
  (comma/space-separated numbers or names, or enter to skip) vllm
```

It turns `gpuMonitoring` on in the values overlay and prints the next steps for
wiring up the inference SLOs (below). Naming your inference servers here is
optional — `lantern discover` detects them from the container image anyway — but
it makes the guidance concrete.

---

## Half 1 — GPU node health (`gpuMonitoring`)

### Enabling it

`gpuMonitoring.enabled: true` (set it via `lantern init`, or directly in your
Helm values) emits two additive objects — no pod restarts, nothing that changes
a running service:

- a **`ServiceMonitor`** pointed at your `dcgm-exporter` Service, so Prometheus
  scrapes GPU metrics, and
- a **`PrometheusRule`** with four GPU health alerts.

### The alerts you get

| Alert | Fires when | Severity |
|---|---|---|
| `LanternGPUThermalThrottleImminent` | `DCGM_FI_DEV_GPU_TEMP` above `thermalCriticalC` (default 85 °C) for 5m | warning |
| `LanternGPUUncorrectableECCError` | any double-bit ECC error in the last 15m | critical |
| `LanternGPUXIDError` | any XID error in the last 15m | critical |
| `LanternGPUPowerNearLimit` | power draw within `powerHeadroomPercent` (default 5%) of the cap for 10m | warning |

Tune the thresholds under `gpuMonitoring.alerts` in your values.

### Where dcgm-exporter comes from

`dcgm-exporter` is the GPU metrics source. Lantern does **not** install the
NVIDIA driver, device plugin, or full GPU Operator. Two supported setups:

- **Bring your own (default, `installExporter: false`).** You already run
  `dcgm-exporter` (via the NVIDIA GPU Operator or its own chart). Point Lantern
  at it with `gpuMonitoring.dcgmExporter.{namespace,selector,port}`.
- **Let Lantern install just the exporter (`installExporter: true`).** For GPU
  nodes that already run real GPU workloads successfully but have no
  `dcgm-exporter` yet. This installs *only* `dcgm-exporter` — no driver, no
  toolkit. It is gated by `./scripts/preflight-check.sh --gpu-install-exporter`,
  which blocks unless a node already advertises `nvidia.com/gpu` as allocatable.
  See the [chart README](../charts/lantern-stack/README.md#gpumonitoringinstallexporter--the-one-case-this-chart-will-install).

### Tensor-core and other profiling metrics

`dcgm-exporter` does **not** emit the `DCGM_FI_PROF_*` profiling family (tensor
pipe activity, FP16/32/64, DRAM/interconnect) by default. To populate those
panels, run `dcgm-exporter` with a metrics config that enables the DCP (Data
Center Profiling) field group. Caveats: it needs a recent driver, only one DCGM
profiling client can run at a time, and it is limited under MIG.

---

## Half 2 — inference-server SLOs (`serviceKind: inference`)

This is the part that turns a model server into burn-rate alerts and a dashboard
with **no metric names hand-written.**

### The one-line setup

On the `ServiceObservability` for your model server, set `inferenceServer`:

```yaml
apiVersion: lantern.dev/v1alpha1
kind: ServiceObservability
metadata:
  name: llama-70b-server
  namespace: ml
spec:
  serviceKind: inference
  inferenceServer: vllm      # <- the only line you add
  target:
    kind: Deployment
    name: llama-70b-server
    metricsPort: metrics
  team: ml-platform
  instrumentation:
    mode: none               # the server exports its own Prometheus metrics
  # no slos: block needed
```

`lantern synth` on this produces a `ServiceMonitor`, a `PrometheusRule` (SLI
recording rules + multiwindow burn-rate alerts), and a Grafana dashboard — all
against the server's real metric names.

`lantern discover` sets `inferenceServer` for you automatically from the
container image: `vllm/vllm-openai` → `vllm`, `tritonserver` → `triton`,
`nvcr.io/nim/…` → `nim`, `text-generation-inference` → `tgi`.

### What each preset generates

| Server | Built-in SLOs |
|---|---|
| **vllm** | ttft, inter-token latency, queue depth |
| **nim** | same as vLLM (verify metric names if your NIM uses the TensorRT-LLM backend) |
| **triton** | queue depth (Triton exposes no token-latency metrics by default) |
| **tgi** | inter-token latency, queue depth |

### Review the thresholds

The metric **names** in each preset are curated; the SLO **thresholds and
objectives are defaults** (objective 99%, ttft 500ms, inter-token 50ms, queue
depth 10). Lantern cannot verify a histogram's bucket boundaries, so it flags
every preset SLO for review — `lantern synth` prints a `warn`, and
`lantern discover` marks the field `REVIEW`. Check each threshold against your
server's actual metrics, or override the whole preset by declaring your own
`slos:` block (an explicit `slos:` always wins).

A latency threshold that doesn't line up with a real histogram bucket boundary
matches nothing and reads as "always fast" rather than failing loudly — so this
review step matters. See
[gpu-and-inference-observability.md](gpu-and-inference-observability.md#type-latency-with-metric--ttft-inter-token-latency-anything-durational)
for the bucket-boundary details.

### Per-pod GPU attribution

To scope a saturation SLO (e.g. GPU utilization) to one service's pods, enable
`dcgm-exporter`'s Kubernetes pod-mapping (`DCGM_EXPORTER_KUBERNETES=true`) so its
series carry `pod`/`namespace` labels. See
[gpu-and-inference-observability.md](gpu-and-inference-observability.md#joining-per-service-slos-to-dcgm-per-pod-gpu-attribution).

---

## End-to-end

```bash
# 1. Draft specs from what's running; inference servers get serviceKind:
#    inference + inferenceServer set automatically (flagged REVIEW).
lantern discover - < workloads.yaml > services.yaml

# 2. Read services.yaml — set target.metricsPort, review the SLO thresholds.

# 3. Compile and inspect before applying.
lantern synth -stack stack.yaml services.yaml | kubectl diff -f -

# 4. Apply.
lantern synth -stack stack.yaml services.yaml | kubectl apply -f -
```

Each inference service gets its `Lantern: <service>` dashboard (burn-rate +
error-budget panels per SLO). GPU node health shows up through the community
DCGM dashboard and the `gpuMonitoring` alerts.
