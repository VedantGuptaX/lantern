# agent_handoff.md — for AI coding agents helping someone set up Lantern

This file is for you if you're Cursor, Claude Code, or any other AI coding
agent, and a human has just asked you to help them set up Lantern in their
own repo or against their own cluster. It's not a walkthrough — that's
[GETTING_STARTED.md](GETTING_STARTED.md) — it's the judgment calls and sharp
edges that a walkthrough can't encode, written by an agent that ran this
project's full flow (`discover` → `synth` → install → canary → dashboards →
logs → traces) against a real, populated, capacity-constrained cluster and
hit real problems doing it.

Read this whole file before running anything. It's short on purpose.

---

## Orient yourself in one paragraph

Lantern is a **compiler, not a platform**: one typed `ServiceObservability`
spec compiles to OpenTelemetry instrumentation, Prometheus scrape config, and
SLO burn-rate alerts. It owns no storage, runs no agent, runs no query
engine — it reads a spec and writes YAML for tools the user already runs
(OpenTelemetry Operator, Prometheus Operator, Grafana) or, via the optional
quickstart chart, can install for them. `kubectl`/`helm` do the actual
applying — Lantern itself never touches a live cluster. If you remember
nothing else from this file, remember that distinction: every step below is
either "generate/inspect a file" (always safe, no side effects) or "change
the cluster" (needs the discipline in this file).

## First question to ask, before running anything

**Is the target cluster empty/disposable, or does it already run real
services?** This determines which doc you follow, and the two are not
interchangeable:

- **Empty, disposable, or `kind`/demo** → [GETTING_STARTED.md](GETTING_STARTED.md).
  Quickstart install is low-risk here; nothing to conflict with, nothing to
  disrupt.
- **Already running real workloads, anyone depends on it, or you're not
  certain** → [docs/adopting-an-existing-cluster.md](docs/adopting-an-existing-cluster.md).
  Follow it staged, in order. Do not shortcut it because it looks like a lot
  of steps — the two things it protects against (cluster-scoped CRD/webhook
  conflicts, and pod restarts from instrumentation injection landing on
  every service at once) are the two realistic ways this goes wrong, and
  both are silent failures, not loud ones, if you skip the checks.

If you genuinely don't know which situation you're in, **ask the human** —
don't guess from context clues like namespace names or how quiet the cluster
looks. A "UAT" or "staging" cluster with synthetic data is still real
infrastructure other people may depend on.

## Non-negotiable rules, regardless of which cluster

1. **Run the preflight check before any `helm install` of the quickstart
   subcharts, every time, even if you ran it five minutes ago.**
   `make preflight` (or `./scripts/preflight-check.sh -n <namespace>`) is
   100% read-only and computes real conflict/capacity state. `make
   install-quickstart` / `make install-byo` already gate on it — don't call
   `helm install` directly to route around that gate. If it exits non-zero,
   stop and surface the finding to the human; don't retry with
   `--set preflight.hook.enabled=false` on your own judgment call.
2. **Never apply an instrumentation-injection annotation
   (`instrumentation.opentelemetry.io/inject-*`) to more than one service in
   the same apply.** This is the one step that restarts real pods and
   changes running process behavior. One service, watched through a full
   rollout, soaked, before the next.
3. **`kubectl diff` before every `kubectl apply`, no exceptions** — including
   for "just the scrape config." Diff is how you catch a `stack.yaml`
   pointing at the wrong endpoint, or a resource that already exists with
   different intent, before it's live.
4. **Never delete a CRD** without first listing every custom resource of
   that kind, cluster-wide, and confirming none belong to something you
   didn't create. CRD deletion cascades to everything of that kind, not just
   what you meant to remove.
5. **Never handle kubeconfig contents, cloud credentials, or service
   principal secrets in chat.** If you need cluster access, it should
   already be ambient (an authenticated `kubectl` context) — never ask the
   human to paste one in, and never echo one back.
6. **Escalate rather than guess** on anything cluster-scoped (CRDs,
   webhooks, `ClusterRoleBinding`s), anything outside the current
   canary/wave, or any request to skip a safety step "to save time." A human
   can decide to accept a risk explicitly; you deciding on their behalf
   without saying so is the failure mode this rule exists to prevent.

## The correct order of operations

```
lantern discover  →  human reviews every REVIEW marker  →  lantern synth -strict
        ↓                                                          ↓
  never trust blindly                                    kubectl diff -f manifests.yaml
                                                                     ↓
                                          apply ServiceMonitor/PrometheusRule broadly
                                          (additive, no pod restarts — safe once diffed)
                                                                     ↓
                                    stage instrumentation injection ONE service at a time
```

`discover`, `synth`, and `validate` only read and write files — run them as
freely as you need to iterate. The line you don't cross without the human's
explicit go-ahead is anything that touches the live cluster: `helm install`/
`upgrade`, `kubectl apply`, deleting or restarting anything.

## Sharp edges — read these before you rediscover them the hard way

Every one of these cost real debugging time finding out the hard way against
a real cluster. Save yourself the loop:

- **Every subchart this project depends on ships with zero default CPU/
  memory requests** (kube-prometheus-stack, Loki, Tempo, the OTel Operator —
  this project's own values files set explicit defaults for the pieces it
  controls directly, but a lot of the subchart internals are still
  unbounded). `helm install` succeeding is not proof the deployment is
  correctly sized. Check the [resource requirements table](README.md#resource-requirements)
  and confirm with `kubectl top` after install, especially on a cluster that
  isn't obviously oversized for this.
- **`target.metricsPort` unset means the generated `ServiceMonitor` might
  scrape nothing, silently.** `-strict` now warns about this (a real bug
  found and fixed against a live cluster), but a warning still requires a
  human to actually fix it — verify the named port exists on the real
  `Service` (`kubectl get svc -o yaml`), don't assume `discover`'s guess is
  right just because it didn't error.
- **`team: unassigned` (the `-team` flag's fallback) satisfies
  `requireTeamLabel: true` mechanically, not in spirit.** The policy only
  checks the field is non-empty — it can't tell a real owner from a
  placeholder. Read every `REVIEW` marker `discover` gives you; don't apply
  anything with a placeholder team to a shared cluster.
- **`instrumentation.mode: ebpf` is a correct compiler decision, but Lantern
  does not deploy the eBPF probe itself.** If a service resolves to `mode:
  ebpf` and the human expects to see its traces, nothing produces them until
  you (or they) separately install an eBPF instrumentation agent (OBI/Beyla)
  — see [docs/getting-signals-into-grafana.md](docs/getting-signals-into-grafana.md)
  for a verified-working config and the four real gotchas in it (undersized
  memory silently drops traces per-process with no loud error; the
  `/sys/fs/bpf` hostPath mount is required for HTTP/gRPC tracing
  specifically, not optional; the narrowed-capability path may not fully
  work even after adding every capability the values.yaml comments suggest;
  `contextPropagation.enabled` pulls in `hostNetwork` as a real, separate
  privilege increase worth knowing about before you accept it by default).
- **CLI flags can go before, after, or between positional arguments** —
  `lantern discover -team unassigned -` and `lantern discover - -team
  unassigned` behave identically. If you see a `stat <flagname>: no such
  file or directory` error from an older checkout, that's this bug,
  long since fixed; not a sign you're holding the tool wrong today.

## When to stop and ask the human

- Before the first `helm install`/`upgrade` against any cluster that isn't
  explicitly disposable.
- Before expanding instrumentation injection beyond the current
  canary/wave.
- Before deleting anything, especially a CRD or an existing Helm release.
- Whenever `discover` marks something `REVIEW` and you don't have a
  confident, verifiable answer — a guess dressed up as confidence is worse
  than surfacing the uncertainty.
- Whenever preflight or `kubectl diff` shows something you didn't expect —
  investigate before proceeding, don't rationalize it away.

## Where to go for more detail

| If the human wants to... | Read |
|---|---|
| See the full setup walkthrough on an empty/demo cluster | [GETTING_STARTED.md](GETTING_STARTED.md) |
| Set this up on a cluster with real, running services | [docs/adopting-an-existing-cluster.md](docs/adopting-an-existing-cluster.md) |
| Understand *why* the compiler is designed the way it is | [DESIGN.md](DESIGN.md) |
| Add GPU node health or inference-server SLOs | [docs/gpu-and-inference-observability.md](docs/gpu-and-inference-observability.md) |
| Get logs, traces, or generated dashboards actually showing data | [docs/getting-signals-into-grafana.md](docs/getting-signals-into-grafana.md) |
| Contribute code back, understand the compiler purity boundary | [CONTRIBUTING.md](CONTRIBUTING.md) |
| See what's built vs. still ahead | [README.md § Project status](README.md#project-status) |

Every one of the docs above stays current with what's actually shipped —
if something in this file or one of those ever looks stale against what
you're seeing in the code or a real cluster, trust the code and the real
cluster, and consider it worth a PR to fix the doc.
