<div align="center">

# Lantern

**Observability as code for Kubernetes.**

One typed service definition compiles to OpenTelemetry instrumentation,
Prometheus scrape config, and SLO burn-rate alerts.

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24+-00ADD8.svg)](https://go.dev)
[![Status](https://img.shields.io/badge/status-alpha%20(P0)-orange.svg)](#project-status)

[Getting Started](GETTING_STARTED.md) · [Design](DESIGN.md) · [Contributing](CONTRIBUTING.md) · [Adopting on a live cluster](docs/adopting-an-existing-cluster.md) · [GPU & inference observability](docs/gpu-and-inference-observability.md) · [Roadmap](plan.md)

</div>

---

## The problem

If you've ever actually had to wire up logging, monitoring, and tracing for a
new service on Kubernetes, you already know the problem isn't writing *one*
config file — it's writing **six of them**, in six different formats, owned
by six different projects that have never agreed on anything with each other.

All you wanted was: "page me if `checkout-api`'s error rate or latency gets
bad." To get that, you end up hand-writing:

| What you're really configuring | Who owns the format | What you have to write |
|---|---|---|
| Getting telemetry out of the app | OpenTelemetry Operator | An `Instrumentation` CR |
| Getting that telemetry somewhere | OpenTelemetry Operator | An `OpenTelemetryCollector` CR |
| Telling Prometheus what to scrape | Prometheus Operator | A `ServiceMonitor` |
| Telling Prometheus when to page you | Prometheus Operator | A `PrometheusRule` — hand-written PromQL |
| Showing the data | Grafana | ~4,000 lines of dashboard JSON |
| Letting your team actually see it | Grafana | Folder + permissions + team mapping |

Every one of these tools is genuinely good at its own job, and each has its
own solid "as code" story. The problem is that **none of them share a
schema.** You become the human compiler — manually translating "team X runs
an HTTP service and wants 99.9% availability" into six dialects, then keeping
that translation correct forever as all six upstreams keep shipping breaking
changes on their own schedules, independently of each other.

**Then GPUs show up, and it gets worse.** HTTP services at least have a
shared answer: OpenTelemetry's semantic conventions give everyone
`http.server.request.duration`, so a compiler can build an alert without you
naming a metric. There is no such agreement for GPUs or LLM inference. If
you've ever tried to monitor a GPU fleet — say, GPU health via the DCGM
operator, alongside an inference server's own serving-quality metrics (time
to first token, inter-token latency, request queue depth) — you've hit this
first-hand: every exporter names the same kind of signal differently, and
nothing standardizes it the way OTel did for HTTP:

| Signal | vLLM | Triton | dcgm-exporter |
|---|---|---|---|
| Time to first token | `vllm:time_to_first_token_seconds` | not exposed by default | — |
| Inter-token latency | `vllm:time_per_output_token_seconds` | not exposed by default | — |
| Request queue depth | `vllm:num_requests_waiting` | `nv_inference_pending_request_count` | — |
| GPU utilization | — | — | `DCGM_FI_DEV_GPU_UTIL` |
| GPU temperature | — | — | `DCGM_FI_DEV_GPU_TEMP` |

There is no metric family a compiler can quietly assume for you here — the
honest move is to make you name the metric once, instead of guessing wrong
and shipping an alert that silently never fires. That's exactly what Lantern
does for GPU and inference workloads; more on that below.

That whole translation — for HTTP, gRPC, workers, or GPUs — is deterministic,
repetitive, and today it's done by a human, over and over, per service.

## What Lantern does

Think of Lantern the way you'd think of a regular compiler, just for
observability config instead of machine code: you write one small,
human-readable description of a service, and Lantern generates the six
artifacts above from it — correctly, the same way, every time.

Lantern itself is a **compiler, not a platform.** It owns no storage, runs no
agent, and runs no query engine — it just reads your one spec and writes
config for the tools you already run (OpenTelemetry Operator, Prometheus
Operator, Grafana). You describe a service once; it generates the rest.

```yaml
apiVersion: lantern.dev/v1alpha1
kind: ServiceObservability
metadata:
  name: checkout-api
  namespace: shop
spec:
  target: { kind: Deployment, name: checkout-api, metricsPort: metrics }
  serviceKind: http
  team: payments
  slos:
    - { name: availability, type: availability, objective: 99.9, window: 30d }
    - { name: latency, type: latency, objective: 99.0, threshold: 300ms, window: 30d }
```

```console
$ lantern synth -stack stack.yaml checkout-api.yaml | kubectl apply -f -
```

Twenty lines in. Out comes:

```
Instrumentation      OTLP exporter, sampler, resource attributes
Deployment (patch)   instrumentation.opentelemetry.io/inject-java
ServiceMonitor       scrape config, with policy-denied labels dropped
PrometheusRule       per SLO:
                       7  SLI error-ratio recording rules
                       5  metadata rules (objective, budget, burn rate)
                       2  multiwindow multi-burn-rate alerts
```

**You never write a line of PromQL by hand.** The burn-rate math is the
single easiest thing to get subtly wrong, and a subtly wrong alert is worse
than no alert at all — it gives you false confidence instead of a page.

The same compiler, same burn-rate math, and same "no PromQL by hand" promise
also covers GPU-backed inference servers (vLLM, Triton, NIM, ...) — see
[GPU & inference observability](docs/gpu-and-inference-observability.md).

## Already running services? Don't write specs by hand

`lantern discover` reads your existing workloads and drafts a spec for each
one, showing its reasoning and flagging every guess:

```console
$ kubectl get deploy,statefulset,cronjob -A -o yaml | lantern discover -

# instrumentation.runtime: java (container image eclipse-temurin:21-jre)
# serviceKind: http (container port "http")
# team: payments (label team=payments)
# REVIEW target.metricsPort: (unset) (no port named metrics and no prometheus.io/port annotation)
apiVersion: lantern.dev/v1alpha1
kind: ServiceObservability
...

discovered 7 service(s) from 8 workload(s)

review before applying:
  target.metricsPort   needs review on 4 service(s): ...
  team                 needs review on 1 service(s): storefront
```

It will **not** invent SLOs or teams. An objective is a commitment, not a
guess, and an alert with no owner cannot be routed.

## Design principles

These hold up whether you're already fluent in OpenTelemetry and PromQL, or
you're just trying to get one service monitored without reading five specs
first. Each one is a short plain-English rule, with the reasoning underneath
for when you want it.

**Compose, don't compete.** Lantern generates config for tools you already
run — it doesn't try to replace Prometheus, Grafana, or OpenTelemetry.
_Why:_ OpenTelemetry is the correct data-plane standard and Prometheus
Operator is the correct scrape-config API; Lantern sits above them. If you
already run Odigos or Grafana Alloy instead, Lantern is designed to emit into
those too.

**Bring your own backend.** By default, Lantern assumes you already have
somewhere to send this data, and just points at it.
_Why:_ Lantern targets the Prometheus, Grafana, Tempo and Loki you already
run. A quickstart chart can install a full stack on an empty cluster for
evaluation, but that path is opt-in and demo-scale — this project has no
ambition to become a storage vendor.

**Never guess at a metric name it can't verify.** If there's no standard
name for a signal — which is the normal case for GPU and inference metrics,
and the exception for HTTP — Lantern makes you name the metric once, instead
of silently assuming one and generating an alert that never fires.
_Why:_ this is also what makes GPU health and inference-server SLOs (ttft,
inter-token latency, queue depth) possible at all without Lantern hardcoding
vLLM's, Triton's, or NIM's naming conventions — the same escape hatch that
handles "a metric OTel hasn't standardized yet" today handles "a metric OTel
will never standardize" tomorrow.

**The compiler is a pure function.** Same input, same output, every time —
no surprises between two runs of the same command.
_Why:_ `Compile(spec, stack, facts) → objects` performs no I/O: no cluster
access, no network, no clock, no randomness. Everything impure is resolved
by the caller and passed in. This is what makes output byte-reproducible,
golden-file tests a real contract, and lets the CLI and a future operator
share one implementation. A test parses the package's own imports and fails
the build if anything impure sneaks in.

**Guardrails live in the compiler, not in a wiki page.** A well-meaning
team shipping a `user_id` label shouldn't be able to melt your Prometheus.
_Why:_ high-cardinality labels are dropped at scrape time and in the
collector automatically, enforced by the compiler rather than documented as
a rule someone has to remember.

**Escape hatches are mandatory, not a nice-to-have.** Generated output is
opinionated, and somebody will always disagree with a specific default.
_Why:_ anything Lantern generates can be overridden. A tool that can't be
overridden gets abandoned the first time someone needs one exception —
that's also true of the GPU/inference metric-name override above.

## Resource requirements

Estimated from the chart's own component defaults, not yet measured against a
real install — see [scripts/verify-kind.sh](scripts/verify-kind.sh) and update
this table with real `kubectl top` numbers once someone runs it.

| Mode | Free cluster capacity needed |
|---|---|
| **Bring your own backend** | ~0.3 vCPU / 0.5Gi RAM — just the OTel Operator and collector. Whatever your existing Prometheus/Grafana already needs is separate. |
| **Quickstart, minimum** | ~1.5 vCPU / 2.5Gi RAM free, **plus ~50m CPU / 50Mi RAM per node** for node-exporter (it's a DaemonSet). Expect tight — pods may throttle under load. |
| **Quickstart, recommended** | ~4 vCPU / 8Gi RAM free. This is what [GETTING_STARTED.md](GETTING_STARTED.md) and `verify-kind.sh` check for. |
| **Storage** | ~10–20Gi of PVC if you enable persistent storage for Loki/Tempo. The quickstart defaults to ephemeral filesystem storage — no PVC, but log/trace data is lost on pod restart. Fine for evaluation, not for anything you'd want to keep. |
| **GPU / DCGM monitoring** | ~0 additional — `gpuMonitoring.enabled` only adds a `ServiceMonitor` and a `PrometheusRule` (two Kubernetes objects, not a workload). It assumes `dcgm-exporter` is already running via the NVIDIA GPU Operator; that DaemonSet's own resource footprint is separate infrastructure Lantern doesn't install or size for you. |

`./scripts/preflight-check.sh` computes your cluster's actual free capacity
against these numbers before you install anything.

## Not installing this blind — the preflight check

Here's the specific failure mode this exists to catch: `helm install` on a
CRD that already exists **doesn't fail.** Helm just skips it and prints a
warning. Which means an install can look 100% successful while a
brand-new OpenTelemetry Operator or Prometheus Operator quietly starts
reconciling against a CRD schema — and an owning release — that belongs to
something else entirely. The install looks fine. The controller doesn't
work. Nothing tells you why.

Before the quickstart subcharts touch your cluster:

```bash
make preflight          # or: ./scripts/preflight-check.sh -n observability
```

100% read-only — every single check is a `get`, `describe`, or `auth can-i`,
nothing that could change cluster state:

| Check | Catches |
|---|---|
| CRD ownership | The exact silent-failure mode described above |
| Admission webhook name collisions | A colliding webhook name breaking admission for both installs |
| Existing Grafana / Prometheus / Tempo / Loki-shaped workloads | Installing a second copy of something already running |
| RBAC | Confirming the identity running this actually has permission to do it |
| Free node capacity | Checked against the [resource table above](#resource-requirements) |

If it finds a real conflict, it exits non-zero — and `make install-quickstart`
/ `make install-byo` refuse to call `helm install` when that happens. **This
gate is load-bearing, not a doc you're free to skip.**

Belt-and-braces: a copy of the same CRD-ownership check also runs as a Helm
pre-install hook inside the cluster, for anyone who runs `helm install`
directly instead of through `make`. Helm doesn't guarantee hook-vs-CRD
ordering precisely enough for that hook to count as a hard guarantee on its
own, so treat `make preflight` as the real check and the in-cluster hook —
[templates/preflight-hook.yaml](charts/lantern-stack/templates/preflight-hook.yaml)
— as a backstop, not the other way around.

## Install

```bash
git clone https://github.com/VedantGuptaX/lantern.git
cd lantern
make build          # produces bin/lantern
```

No external Go modules — the whole thing builds with the standard library. Go
1.24 or later.

Full walkthrough: **[GETTING_STARTED.md](GETTING_STARTED.md)**

## Project status

Alpha, actively developed. This table is deliberately narrow — it only lists
what's actually built and tested in the code today. Everything still ahead
(dashboards, the operator, SDKs, and why they're prioritized that way) is
tracked separately in **[plan.md](plan.md)**, so this README doesn't drift
into a wishlist.

| | Status |
|---|---|
| Compiler core and CLI | ✅ Done |
| OpenTelemetry agent injection | ✅ Done |
| eBPF / agent auto-selection | ✅ Done |
| Prometheus scrape config | ✅ Done |
| SLO burn-rate alerts | ✅ Done |
| Workload discovery (`lantern discover`) | ✅ Done |
| GPU node health (DCGM) | ✅ Done — cluster-level, via the `gpuMonitoring` chart toggle |
| GPU inference-server SLOs (ttft, inter-token latency, queue depth) | ✅ Done — `serviceKind: inference`, see [docs](docs/gpu-and-inference-observability.md) |
| Preflight safety gate | ✅ Done — `make preflight`, gates `make install-*`, plus an in-cluster hook |
| System metrics | ✅ Via kube-prometheus-stack, not generated by Lantern |
| Traces | ✅ Collector → Tempo |
| Quickstart Helm chart | ⚠️ Written, **not yet installed or linted on a real cluster** — see [verify-kind.sh](scripts/verify-kind.sh) |

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md). The
highest-value place to help right now is dashboard generation — see
[plan.md](plan.md) for the current roadmap and why it's next.

```bash
make all      # fmt, vet, test, build
make demo     # discover → synth, end to end (no cluster needed)
make golden   # re-baseline golden files after an intended change

./scripts/verify-kind.sh   # full end-to-end on a throwaway kind cluster
```

`verify-kind.sh` is the one that matters: it exercises everything the project
claims but has never executed — chart dependency resolution, `helm lint`, a
real install, discovery against a live cluster, and applying generated
manifests. It writes `verify-report.txt`. **Nobody has run it yet.** If you do,
the report is the most useful thing you could open an issue with.

## License

[Apache License 2.0](LICENSE). Free for anyone to use, modify and distribute,
commercially or otherwise.

Apache 2.0 rather than MIT because it carries an explicit patent grant, which
matters for a tool companies run in production infrastructure, and because it
matches the license of every project Lantern interoperates with —
OpenTelemetry, Prometheus, and the Kubernetes ecosystem. See [NOTICE](NOTICE)
for attributions.
