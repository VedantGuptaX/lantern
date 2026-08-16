<div align="center">

# Lantern

**Observability as code for Kubernetes.**

One typed service definition compiles to OpenTelemetry instrumentation,
Prometheus scrape config, and SLO burn-rate alerts.

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24+-00ADD8.svg)](https://go.dev)
[![Status](https://img.shields.io/badge/status-alpha%20(P0)-orange.svg)](#project-status)

[Getting Started](GETTING_STARTED.md) · [Design](DESIGN.md) · [Contributing](CONTRIBUTING.md) · [Adopting on a live cluster](docs/adopting-an-existing-cluster.md) · [GPU & inference observability](docs/gpu-and-inference-observability.md) · [Setting up with an AI agent](agent_handoff.md) · [Roadmap](plan.md)

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
also covers GPU-backed inference servers (vLLM, Triton, NIM, TGI, ...). There
is no OTel semantic convention for time-to-first-token or inter-token
latency the way there is for `http.server.request.duration`, so instead of
guessing wrong and shipping an alert that never fires, `serviceKind:
inference` makes you name the metric once, then gets the same burn-rate
machinery every other SLO type gets — for free:

```yaml
apiVersion: lantern.dev/v1alpha1
kind: ServiceObservability
metadata:
  name: llama-70b-server
  namespace: ml
spec:
  target: { kind: Deployment, name: llama-70b-server, metricsPort: metrics }
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

GPU **node** health (utilization, temperature, power, ECC/XID errors) is a
separate, cluster-level concern from a specific inference server's SLOs —
it goes through the chart's `gpuMonitoring` toggle instead of a spec, wired
to `dcgm-exporter` you already run. See
[GPU & inference observability](docs/gpu-and-inference-observability.md) for
the full picture, including saturation SLOs that combine both (e.g. "page me
if GPU memory utilization stays above 90% for the request queue depth this
service is actually seeing").

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

**Measured for real** — a 2-node AKS cluster (`Standard_D2s_v3`, 2 vCPU/8Gi
each), `kubectl describe nodes` before and after each component, not
estimated from chart defaults. Every subchart this project depends on
(kube-prometheus-stack, Loki, Tempo, the OTel Operator) ships with **zero**
default resource requests — "size it yourself" is the norm across this whole
ecosystem, not a Lantern-specific gap. The numbers below are what this
project's own values files now set explicitly, so `make install-quickstart`
doesn't hand you an unbounded footprint.

| Component | CPU request | Mem request | Notes |
|---|---|---|---|
| kube-prometheus-stack core (Prometheus, Alertmanager, Grafana, kube-state-metrics, the operator itself) | ~245m | ~592Mi | Single-scheduled, not per-node |
| OTel Operator | ~30m | ~64Mi | Single-scheduled |
| Lantern's own OTel Collector | ~100m | ~256Mi | Single-scheduled |
| Loki (`SingleBinary`, gateway/caches off) | ~30m | ~128Mi | Single-scheduled |
| Tempo | ~30m | ~128Mi | Single-scheduled |
| node-exporter | ~10m | ~24Mi | DaemonSet — **per node** |
| `logsCollector` (pod-log shipping) | ~50m | ~256Mi | DaemonSet — **per node** |
| `loki-canary` | ~10m | ~24Mi | DaemonSet — **per node**, required by Loki's own `helm test` hook |
| OBI (eBPF trace probe, bring-your-own — see [docs](docs/getting-signals-into-grafana.md)) | ~10m | ~256Mi | DaemonSet — **per node**; memory, not CPU, is what it actually needs |

Add up the "single-scheduled" rows once, and the "per node" rows once per
node in your pool. On the 2-node cluster this was measured on: **~595m CPU /
~2.3Gi RAM combined** for the complete stack — metrics, logs, and traces
together, dashboards included.

| Mode | Free cluster capacity needed |
|---|---|
| **Bring your own backend** | ~0.3 vCPU / 0.5Gi RAM — just the OTel Operator and collector. Whatever your existing Prometheus/Grafana already needs is separate. Not yet measured as precisely as the quickstart path above. |
| **Quickstart, metrics + dashboards only** (no logs, no traces) | ~435m vCPU / ~1.2Gi RAM combined, from the table above minus the logs/traces/OBI rows. |
| **Quickstart, full stack** (metrics + logs + traces) | ~595m vCPU / ~2.3Gi RAM combined, measured as above. This is tighter than the old "~1.5 vCPU / 2.5Gi" estimate suggested — that number was never wrong, it was just for a much smaller slice of what the quickstart now includes. |
| **Storage** | ~10–20Gi of PVC if you enable persistent storage for Loki/Tempo. The quickstart defaults to ephemeral filesystem storage — no PVC, but log/trace data is lost on pod restart. Fine for evaluation, not for anything you'd want to keep. |
| **GPU / DCGM monitoring (Lantern's own footprint)** | ~0 additional — `gpuMonitoring.enabled` only adds a `ServiceMonitor` and a `PrometheusRule` (two Kubernetes objects, not a workload). |

`./scripts/preflight-check.sh` computes your cluster's actual free capacity
against these numbers before you install anything. On a genuinely tight
cluster (this one had well under 200m CPU free across both nodes combined
by the time the full stack went in), expect to actually hit that math, not
just clear it comfortably — plan accordingly rather than assuming "quickstart"
means "always fits."

### GPU node prerequisites (NVIDIA GPU Operator / DCGM — not installed by Lantern)

`gpuMonitoring.enabled` assumes the NVIDIA GPU Operator (which includes
`dcgm-exporter`) is already running on your GPU nodes — that's separate
infrastructure this project doesn't install or manage, so budget for it
before turning the toggle on. Real numbers where NVIDIA publishes them,
honest gaps where they don't — this project's own rule ("never guess at a
number it can't verify") applies here too, not just to PromQL:

| Component | CPU request | Mem request | Source |
|---|---|---|---|
| `dcgm-exporter` alone | 10–100m | 128Mi–512Mi (limit up to 1Gi) | [NVIDIA GPU Operator `ClusterPolicy` docs](https://docs.nvidia.com/datacenter/cloud-native/gpu-telemetry/latest/dcgm-exporter.html) — varies with how many DCGM fields you collect; the default field list is the main lever if you need to trim it |
| `nvidia-driver-daemonset` | ~100m | ~128Mi (limit ~1 CPU / 2Gi) | [NVIDIA GPU Operator docs](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/getting-started.html) — this is the installer process, not the driver itself (the driver is just files on the host once installed); expect a real, temporary CPU spike during the initial driver load on each node, not just this steady-state number |
| Device plugin, container toolkit, GPU/node-feature-discovery, the operator itself (5 more components) | **Not officially documented** | **Not officially documented** | NVIDIA doesn't publish per-component sizing for the rest of the 8-component stack. Don't trust a number here from anywhere that isn't NVIDIA's own docs or your own `kubectl top` after a real install — this table won't invent one either. |

Practical takeaway: `dcgm-exporter` itself is cheap and predictable enough to
plan around. The other 7 components of the GPU Operator stack are not —
measure them for real on your own GPU nodes before assuming a number, the
same discipline this project asks of you for PromQL metric names.

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

### Not on Kubernetes?

The compiler itself — `discover`/`synth`, the object model, everything
above — is Kubernetes-only by design (see [DESIGN.md](DESIGN.md)); that's
not changing. But if you just want to see plain Docker/Docker Compose
container logs and host metrics in Grafana without any of that,
**[docker/](docker/)** is a separate, standalone Loki + Prometheus + Grafana
stack — no traces, no per-container metrics (see the docker README for why),
zero relationship to the compiler or `charts/lantern-stack`. See
[docker/README.md](docker/README.md).

## Setting this up with an AI coding agent

If you're using Cursor, Claude Code, or another AI coding agent to help set
Lantern up — either for yourself, or you *are* that agent reading this on
someone else's behalf — start with **[agent_handoff.md](agent_handoff.md)**
instead of jumping straight to `helm install`. It's short, and it's the
difference between an agent that follows this project's safety discipline
(preflight before install, diff before apply, one service at a time for
instrumentation injection) and one that finds out why those rules exist by
breaking something first.

It exists because this project's own setup was verified end-to-end by an AI
agent against a real, populated cluster, and every real problem hit along
the way — a chart bug, a capacity miscalculation, an unreviewed guess
applied too broadly — came from skipping a step this project already tells
you not to skip. `agent_handoff.md` is that experience written down so the
next agent doesn't have to rediscover it.

## Project status

Alpha, but **verified end-to-end on a real, populated AKS cluster** — not
just synthetic `kind` fixtures. Metrics, logs, traces, and generated
dashboards all confirmed working with real data, from a real multi-service
deployment, real capacity constraints included. This table is deliberately
narrow — it only lists what's actually built and tested in the code today.
Everything still ahead (per-team dashboard folders, the operator, SDKs, and
why they're prioritized that way) is tracked separately in
**[plan.md](plan.md)**, so this README doesn't drift into a wishlist.

| | Status |
|---|---|
| Compiler core and CLI | ✅ Done |
| OpenTelemetry agent injection | ✅ Done |
| eBPF / agent auto-selection | ✅ Done — selects the mode; does **not** install the eBPF probe itself. Bring-your-own path (OBI) verified end-to-end on a real cluster, real gotchas documented — see [docs](docs/getting-signals-into-grafana.md) |
| Prometheus scrape config | ✅ Done — real bug found and fixed installing on a live cluster: a `ServiceMonitor` could reference a named port that doesn't exist on the target `Service` with no warning; now caught at `-strict` time |
| SLO burn-rate alerts | ✅ Done — real bug found and fixed on a live cluster: numeric SLO constants (objective, error budget) could serialize as bare YAML numbers instead of quoted strings, which the live `PrometheusRule` CRD schema rejects outright; now emits correctly and verified loading with `health: ok` against a real Prometheus |
| Workload discovery (`lantern discover`) | ✅ Done — real bug found and fixed against real `kubectl get -A -o yaml` output: the YAML parser rejected valid line-folded scalars that only show up in aged, real-cluster workloads, never in fixtures |
| GPU node health (DCGM) | ✅ Done — cluster-level, via the `gpuMonitoring` chart toggle |
| GPU inference-server SLOs (ttft, inter-token latency, queue depth) | ✅ Done — `serviceKind: inference`, see [docs](docs/gpu-and-inference-observability.md) |
| Preflight safety gate | ✅ Done — `make preflight`, gates `make install-*`, plus an in-cluster hook. Two real bugs found and fixed, both on the same underlying ambiguity: the CRD-ownership check couldn't tell "this release's own brand-new/own-CRD" from "a foreign one" on a fresh install (fixed by counting actual custom-resource instances instead of trusting the CRD-level Helm annotation, which `crds/`-folder CRDs never get regardless of owner) — and then, found on a real second `helm upgrade`, the *same* ambiguity one level down: a nonzero instance count alone still isn't proof of a foreign install, since this release's own already-existing ServiceMonitors/PrometheusRules/Alertmanager/Prometheus CR instances don't carry that CRD-level annotation either. Now checks per-instance ownership (Helm annotation or `lantern.dev/managed=true`) before blocking |
| System metrics | ✅ Via kube-prometheus-stack, not generated by Lantern |
| Traces | ✅ Verified end-to-end on a real cluster — Tempo + OBI (bring-your-own eBPF probe) → collector → Tempo → Grafana, confirmed with real trace data from a real service. See [docs](docs/getting-signals-into-grafana.md) for the exact gotchas (memory sizing, the `bpffs` hostPath mount, `privileged` vs. narrowed capabilities). Tempo now defaults to persistent storage (`persistence.enabled: true`, matching Loki) — trace data survives a pod restart instead of living only on ephemeral storage |
| Kubernetes Events (queryable history beyond etcd's ~1h TTL) | ✅ Verified end-to-end on a real cluster — not generated by Lantern's chart, same bring-your-own pattern as OBI: a standalone `kubernetes-events-exporter` release watches the Events API continuously and pushes to Loki with a hand-scoped `ClusterRole` (the upstream chart's own default RBAC is a cluster-wide wildcard read on every resource including Secrets — scoped down instead). See [docs](docs/getting-signals-into-grafana.md) |
| Log collection (pod logs → Loki) | ✅ Done, verified end-to-end on a real cluster — `logsCollector.enabled`, a DaemonSet collector; off by default, on in the quickstart. Two real bugs found and fixed: undersized memory limit silently dropped log batches for any service verbose enough to log full SQL query text, and `start_at: beginning` meant every restart of the collector replayed the entire node's log history (including every system pod), overwhelming Loki's own ingestion limits and starving out real, current traffic behind the backlog. See [docs](docs/getting-signals-into-grafana.md) |
| Per-service Grafana dashboards | ✅ Done, verified rendering real data on a real cluster — generated from the same recording rules that drive alerts; no per-team folders yet, see [docs](docs/getting-signals-into-grafana.md) |
| Quickstart Helm chart | ✅ `helm install`/`helm upgrade` now run and verified repeatedly against a real AKS cluster, not just `helm template`/`helm lint`. Several real install-time bugs found and fixed along the way: Loki's chart needing an explicit `deploymentMode`, an assumed cert-manager dependency that doesn't hold on a cluster without it, a CRD/cert install-ordering deadlock between the OTel Operator's self-signed cert and Helm's own apply ordering, and a Tempo receiver/datasource-port mismatch. `verify-kind.sh` (the local `kind`-based rehearsal) still hasn't been run in any environment that had `kind`/Docker available — real-cluster verification happened instead, which is a stronger check but not a substitute if you specifically want the throwaway-cluster rehearsal documented in [CONTRIBUTING.md](CONTRIBUTING.md) |

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md). The
highest-value place to help right now is per-team dashboard folders
(grafana-operator's `GrafanaDashboard`/`GrafanaFolder` CRDs) and actually
running `helm install` for the first time — see [plan.md](plan.md) for the
current roadmap and why it's next.

```bash
make all      # fmt, vet, test, build
make demo     # discover → synth, end to end (no cluster needed)
make golden   # re-baseline golden files after an intended change

./scripts/verify-kind.sh   # full end-to-end on a throwaway kind cluster
```

`verify-kind.sh` exercises the same ground the project now has real-cluster
evidence for — chart dependency resolution, `helm lint`, a real install,
discovery against a live cluster, and applying generated manifests — but on
a disposable local `kind` cluster instead of a shared one. It writes
`verify-report.txt`. It still hasn't been run in any environment that had
`kind`/Docker available; real-cluster verification against a live AKS
cluster happened instead (see [Project status](#project-status) — every row
marked verified-on-a-real-cluster came from that, not from `kind`). That's
a stronger check in the ways that matter (real traffic, real capacity
pressure, real multi-namespace RBAC) but not a substitute for the fast,
disposable, no-blast-radius loop `verify-kind.sh` is meant to give
contributors. If you have `kind`/Docker locally and run it, the report is
still one of the most useful things you could open an issue with.

## License

[Apache License 2.0](LICENSE). Free for anyone to use, modify and distribute,
commercially or otherwise.

Apache 2.0 rather than MIT because it carries an explicit patent grant, which
matters for a tool companies run in production infrastructure, and because it
matches the license of every project Lantern interoperates with —
OpenTelemetry, Prometheus, and the Kubernetes ecosystem. See [NOTICE](NOTICE)
for attributions.
