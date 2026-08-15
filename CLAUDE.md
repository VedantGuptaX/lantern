# CLAUDE.md — session handoff for the Lantern bastion

This file exists so a Claude Code session starting cold on the bastion host
has enough context to be useful and safe immediately, without re-deriving
anything or re-litigating decisions already made. Read this whole file before
running anything against a real cluster.

---

## What this is

**Lantern** is an observability-as-code compiler: one typed
`ServiceObservability` spec compiles to OpenTelemetry instrumentation,
Prometheus scrape config, and SLO burn-rate alerts. It is a compiler, not a
platform — it generates YAML, it never talks to a cluster itself. `kubectl`
does the applying.

Full picture: [`README.md`](README.md) (what it does, project status),
[`DESIGN.md`](DESIGN.md) (architecture and why), [`GETTING_STARTED.md`](GETTING_STARTED.md)
(the standard walkthrough).

## Project status — don't relitigate this

P0 is done and tested: the compiler, the CLI, `lantern discover`, OTel
injection, Prometheus scrape/alert generation, and the quickstart Helm chart.

**Not built:** dashboard generation (no per-service Lantern dashboard exists
yet — this is the biggest visible gap and the next milestone, P1), Grafana
folder/RBAC emission, the Kubernetes operator (P2), log↔trace correlation.

The chart's Helm dependency versions were corrected against real upstream
release pages as of August 2026 (Grafana moved Loki/Tempo to
`grafana-community/helm-charts` on 30 Jan 2026 — the chart points at the new
location). The chart has been checked but **never actually installed by
anyone**. Treat every `helm install` as a first real-world run, not a known-good
path.

---

## THE ENGAGEMENT THIS BASTION EXISTS FOR

Onboarding Lantern onto an **existing AKS UAT cluster that already runs real
microservices.** This is not a greenfield demo. The cluster has traffic on it,
even if it's UAT-grade traffic, and the goal is to add observability without
destabilizing anything already running.

The staged runbook for exactly this situation is
[`docs/adopting-an-existing-cluster.md`](docs/adopting-an-existing-cluster.md).
**Read it before running any `helm install` or `kubectl apply` against the
cluster.** Do not shortcut it because it looks like a lot of steps — the two
things it protects against (cluster-scoped CRD/webhook conflicts, and pod
restarts from instrumentation injection landing on every service at once) are
the two realistic ways this goes wrong.

### Cluster facts — fill in before starting

| | |
|---|---|
| AKS cluster name | _fill in_ |
| Resource group / subscription | _fill in_ |
| kubectl context name | _fill in_ |
| Existing monitoring already on this cluster? | _fill in — check via Step 1 of the runbook, don't assume_ |
| `observability` namespace already in use for something else? | _fill in_ |
| Who owns this cluster / who to loop in before cluster-scoped changes | _fill in_ |
| Node pool size / headroom | _fill in — `kubectl describe nodes`_ |

If these aren't filled in, fill them in first. Don't guess at cluster state —
the preflight commands in the runbook are cheap and non-destructive; run them.

---

## Non-negotiable rules for this engagement

These aren't style preferences. Each one maps to a specific way this cluster
gets broken.

1. **Never run `helm install` of the quickstart subcharts
   (kube-prometheus-stack, tempo, loki, opentelemetry-operator) without first
   completing Step 1 (preflight) of the adoption runbook.** If any of those
   CRDs or a Grafana/Prometheus already exist, installing again risks CRD
   ownership conflicts and webhook collisions — a broken mutating webhook can
   block pod creation *cluster-wide*, not just for the namespace you're
   touching.

2. **Never apply an instrumentation-injection annotation to more than one
   service at a time on the first pass.** `instrumentation.opentelemetry.io/inject-*`
   restarts every pod of that Deployment and injects a real agent into the
   running process. This is the step that actually changes running service
   behavior. Canary one low-risk service, soak it, then expand — see runbook
   Step 4.

3. **Always `kubectl diff` before `kubectl apply`.** No exceptions, including
   for "just the scrape config" — diff first is how you catch a stack.yaml
   pointing at the wrong endpoint before it's live.

4. **Never delete a CRD** (`kubectl delete crd ...`) without first listing
   every custom resource of that kind, cluster-wide, and confirming none of
   them belong to something you didn't create. CRD deletion cascades.

5. **Never handle or request AKS credentials, kubeconfig contents, or Azure
   service principal secrets in chat.** Run `az login` / `kubelogin` locally
   on the bastion, outside the conversation. If a session needs to run
   `kubectl`/`helm` against the cluster, it should already be authenticated
   via the ambient kubeconfig — never paste tokens or keys into a prompt.

6. **Escalate rather than guess** on: anything cluster-scoped (CRDs, webhooks,
   ClusterRoleBindings), any change to a service outside the current
   canary/wave, any policy change that widens `denyLabels` or raises sampling
   above what the runbook suggests, and any request to skip the diff step "to
   save time."

## What's safe to do without asking

- Everything in `pkg/`, `cmd/`, `charts/` — editing Lantern's own source,
  running `make all`, `make demo`, `./scripts/verify-kind.sh` against a local
  `kind` cluster.
- `lantern discover`, `lantern synth`, `lantern validate` — these only read
  files and write files. They never touch a live cluster.
- Any `kubectl get`, `kubectl describe`, `kubectl diff`, `kubectl top` against
  the AKS cluster — read-only, always fine.
- Applying `ServiceMonitor`/`PrometheusRule` objects broadly, once Step 1 of
  the runbook has confirmed no conflicting stack exists — these are additive
  and don't restart pods.

---

## Commands cheat sheet

```bash
# build and test — no cluster needed
make all
make demo                          # discover -> synth on the bundled example

# rehearse against a throwaway cluster before touching AKS
./scripts/verify-kind.sh
./scripts/verify-kind.sh --no-loki # skip the least-verified chart component

# generate only — safe against any cluster, including AKS, read-only
lantern discover - < workloads.yaml > services.yaml
lantern synth -stack stack.yaml services.yaml -strict > manifests.yaml
kubectl diff -f manifests.yaml     # ALWAYS before apply
```

Full staged AKS runbook: [`docs/adopting-an-existing-cluster.md`](docs/adopting-an-existing-cluster.md).

## Repo map

```
README.md, GETTING_STARTED.md, DESIGN.md, CONTRIBUTING.md   docs
docs/adopting-an-existing-cluster.md    staged runbook for a live, populated cluster
api/v1alpha1/            the two CRD-shaped types: ObservabilityStack, ServiceObservability
pkg/compile/              pure compiler core — no I/O, enforced by boundary_test.go
pkg/discover/             workload -> draft spec inference
pkg/emit/{otel,prom}/     the actual generators
cmd/lantern/              the CLI (discover, synth, validate)
charts/lantern-stack/     the Helm chart — bring-your-own by default, quickstart opt-in
scripts/verify-kind.sh    end-to-end check on a throwaway kind cluster
examples/, testdata/golden/   fixtures and the golden-file test contract
```

## If you're picking this up mid-rollout

Check what's already been applied before doing anything else:

```bash
kubectl get servicemonitor,prometheusrule -A -l lantern.dev/managed=true
kubectl get deploy -A -o json | jq -r '.items[] | select(.spec.template.metadata.annotations | keys[]? | test("instrumentation.opentelemetry.io/inject-")) | .metadata.namespace + "/" + .metadata.name'
```

The second command lists every service that already has instrumentation
injected — that's your "already in a wave" list. Don't re-annotate them, and
don't assume the person before you finished a wave's soak period — check
timestamps and whether the service has been stable since.
