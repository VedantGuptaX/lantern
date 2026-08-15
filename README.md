<div align="center">

# Lantern

**Observability as code for Kubernetes.**

One typed service definition compiles to OpenTelemetry instrumentation,
Prometheus scrape config, and SLO burn-rate alerts.

[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.24+-00ADD8.svg)](https://go.dev)
[![Status](https://img.shields.io/badge/status-alpha%20(P0)-orange.svg)](#project-status)

[Getting Started](GETTING_STARTED.md) · [Design](DESIGN.md) · [Contributing](CONTRIBUTING.md)

</div>

---

## The problem

Getting one service monitored on Kubernetes means producing six unrelated
artifacts, in six different formats, owned by six different projects:

| Artifact | Owned by | Format |
|---|---|---|
| Agent injection | OpenTelemetry Operator | `Instrumentation` CR |
| Collector pipeline | OpenTelemetry Operator | `OpenTelemetryCollector` CR |
| Scrape config | Prometheus Operator | `ServiceMonitor` |
| Alert rules | Prometheus Operator | `PrometheusRule` — hand-written PromQL |
| Dashboard | Grafana | 4,000 lines of JSON |
| Dashboard access | Grafana | Folder + permissions + team mapping |

Every one of these has a mature "as code" story. **None of them share a
schema.** So a platform engineer becomes the compiler — manually translating
"team X runs an HTTP service and wants 99.9% availability" into six dialects,
then maintaining that translation forever as six upstreams drift apart.

That translation is deterministic, repetitive, and currently done by a human.

## What Lantern does

Lantern is a **compiler**, not a platform. It owns no storage, no agent, and no
query engine. You describe a service once; it generates the rest.

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

**Nobody writes a line of PromQL.** The burn-rate arithmetic is the single
easiest thing to get subtly wrong by hand, and a subtly wrong burn-rate alert
is worse than no alert at all.

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

**Compose, don't compete.** OpenTelemetry is the correct data-plane standard.
Prometheus Operator is the correct scrape-config API. Lantern sits above them
and generates their config; it never tries to replace them. If you already run
Odigos or Grafana Alloy, Lantern is designed to emit into those instead.

**Bring your own backend.** Lantern points at the Prometheus, Grafana, Tempo
and Loki you already run. A quickstart chart can install a full stack for an
empty cluster, but that path is opt-in and demo-scale — the project has no
ambition to become a storage vendor.

**The compiler is a pure function.** `Compile(spec, stack, facts) → objects`
performs no I/O — no cluster access, no network, no clock, no randomness.
Everything impure is resolved by the caller and passed in. This is what makes
output byte-reproducible, golden-file tests a real contract, and the CLI and
future operator share one implementation. A test parses the package's imports
and fails the build if anything impure sneaks in.

**Guardrails belong in the compiler.** High-cardinality labels are dropped at
scrape time and in the collector, because a well-meaning team shipping a
`user_id` label is the most common way to melt a Prometheus instance.

**Escape hatches are mandatory.** Generated output is opinionated, and somebody
will disagree with it. Anything generated can be overridden.

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

Alpha. **P0 of the [design](DESIGN.md) is complete and tested**; the rest is
not built yet. Being precise about this matters more than looking finished:

| | Status |
|---|---|
| Compiler core and CLI | ✅ Done |
| OpenTelemetry agent injection | ✅ Done |
| eBPF / agent auto-selection | ✅ Done |
| Prometheus scrape config | ✅ Done |
| SLO burn-rate alerts | ✅ Done |
| Workload discovery | ✅ Done |
| Quickstart Helm chart | ✅ Done (never `helm lint`ed — see chart README) |
| System metrics | ✅ Via kube-prometheus-stack, not generated by Lantern |
| Traces | ✅ Collector → Tempo |
| **Generated dashboards** | ❌ **P1 — not built** |
| Grafana folders and team RBAC | ❌ P1 |
| Log ↔ trace correlation | ❌ Logs ship to Loki; `correlateTraceID` is parsed and unused |
| Kubernetes operator | ❌ P2 |
| `lantern.dev/profile` label shortcut | ❌ P2 |
| TypeScript / Python SDKs | ❌ P3 |

**There is no Lantern dashboard yet.** Today you get instrumentation, scrape
config and alerts; the dashboards you see are the ones kube-prometheus-stack
ships. Dashboard generation via the Grafana Foundation SDK is the next
milestone and the one that carries the most risk.

## Contributing

Contributions are welcome — see [CONTRIBUTING.md](CONTRIBUTING.md). The
highest-value place to help right now is the P1 dashboard generator.

```bash
make all      # fmt, vet, test, build
make demo     # discover → synth, end to end
make golden   # re-baseline golden files after an intended change
```

## License

[Apache License 2.0](LICENSE). Free for anyone to use, modify and distribute,
commercially or otherwise.

Apache 2.0 rather than MIT because it carries an explicit patent grant, which
matters for a tool companies run in production infrastructure, and because it
matches the license of every project Lantern interoperates with —
OpenTelemetry, Prometheus, and the Kubernetes ecosystem. See [NOTICE](NOTICE)
for attributions.
