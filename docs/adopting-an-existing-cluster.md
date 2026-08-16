# Adopting Lantern on a cluster that already has real traffic

This is the runbook for the case the top-level docs don't cover: a cluster
that already runs real microservices, where "let's just try it" carries actual
risk. Written for UAT, but nothing here is UAT-specific — the same steps apply
to any cluster you can't afford to disrupt.

**The core rule: scrape config and alerts are additive and safe to roll out
broadly. Instrumentation injection restarts pods and changes runtime
behaviour, and must be staged one service at a time.** Everything below
follows from that split.

Cloud-agnostic — AKS, EKS, GKE, on-prem. Swap the placeholder values for yours.

---

## Step 0 — rehearse locally, not on the shared cluster

### Bastion prerequisites

Check these before anything else — a bastion image is not guaranteed to match
a dev laptop, and finding a missing tool mid-runbook is worse than finding it
now:

```bash
kubectl version --client   # authenticated against the target cluster already?
go version                 # matches go.mod's version?
which make jq
which yq                   # used by the canary-extraction command in Step 4b
which docker || which kind # see below if neither is present
sudo -n true 2>/dev/null && echo "passwordless sudo available"
```

`kubectl` should already be authenticated via `az login`/`kubelogin` (or your
cloud's equivalent) run locally on the bastion — never hand credentials or
kubeconfig contents to a chat session; see `CLAUDE.md`'s non-negotiable rules.

Before touching the shared cluster, run the whole flow against a throwaway
`kind` cluster so the first time you see a failure isn't on infrastructure
other people depend on:

```bash
./scripts/verify-kind.sh
```

If that hasn't been run successfully at least once, don't proceed to Step 1.

**No `docker`/`kind` on this bastion?** Some locked-down bastions genuinely
don't have either, and installing one may not be an option you control. This
is a real gap — `verify-kind.sh` is the only check that exercises a real
install end to end — but there's a partial substitute that still catches most
of what matters:

```bash
go build ./... && go test ./...      # the compiler itself, fully covered
helm lint charts/lantern-stack --values charts/lantern-stack/values-quickstart.yaml
helm template charts/lantern-stack --values charts/lantern-stack/values-quickstart.yaml \
  | kubectl apply --dry-run=client -f -   # server-side-free manifest validation
```

This proves the Go code is correct and the Helm chart renders valid,
schema-conformant Kubernetes objects — it does **not** prove the chart
actually installs (dependency resolution, CRD ordering, and runtime behavior
are exactly what `kind` would catch and this can't). Treat this as "rehearsal
skipped, partial substitute run," not as equivalent to Step 0 passing, and say
so explicitly in any handoff.

---

## Step 1 — preflight: find out what's already there

Run every check before installing anything. Nothing here is destructive.

**Use the automated gate — don't do this by hand.**

```bash
make preflight
```

This runs `scripts/preflight-check.sh`, which does everything below in one
pass: checks RBAC, checks every Prometheus-Operator/OTel-Operator CRD for an
existing (and possibly foreign) owner, scans for name-colliding webhooks,
scans running workloads for existing Grafana/Prometheus/Tempo/Loki images, and
computes real node headroom against the numbers in the
[resource requirements table](../README.md#resource-requirements). It exits
non-zero on anything it classifies as a BLOCK, and `make install-quickstart` /
`make install-byo` both depend on it — Make will refuse to run `helm install`
if preflight fails. Run it with `--strict` (`./scripts/preflight-check.sh
--strict`) to also fail on WARN-level findings if you want zero ambiguity
before touching a shared cluster.

Treat this as the primary method: run it before Step 2, always. The commands
below are what it's actually checking under the hood — read them if you want
to understand a specific finding, run a check manually while debugging, or
verify something `preflight-check.sh` doesn't cover for your environment:

```bash
# Is there already a Prometheus Operator or OTel Operator on this cluster?
kubectl get crd | grep -E 'monitoring.coreos.com|opentelemetry.io'

# Is there already a Grafana, Prometheus, Tempo, or Loki running anywhere?
kubectl get pods -A | grep -Ei 'grafana|prometheus|tempo|loki|otel-collector'

# What webhooks already exist? A broken webhook can block pod creation
# cluster-wide — know what's there before adding to it.
kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations

# Node headroom. kube-prometheus-stack + Tempo + Loki + a collector is heavy;
# on a cluster with existing workloads you're competing for capacity, not
# starting from empty.
kubectl top nodes
kubectl describe nodes | grep -A5 "Allocated resources"

# Do you actually have the RBAC to install CRDs and operators here?
kubectl auth can-i create customresourcedefinitions
kubectl auth can-i create clusterrolebindings
```

**Decision point:**

| Finding | What it means |
|---|---|
| No existing `monitoring.coreos.com` or `opentelemetry.io` CRDs, no Grafana/Prometheus running | Quickstart path is viable — see [GETTING_STARTED.md](../GETTING_STARTED.md) Path A. Still stage instrumentation per Step 4 below; don't skip that. |
| Existing Prometheus Operator, Grafana, or OTel Operator already present | **Do not install the quickstart chart's subcharts.** Use `values.yaml` (bring-your-own) and point Lantern at what's already running. Only install a *missing* piece — e.g. OTel Operator alone, if that's the only gap. |
| Insufficient node headroom | Size down before adding anything heavy: skip Loki and/or Tempo initially (`--set loki.enabled=false --set tempo.enabled=false`), or negotiate more capacity first. |
| Insufficient RBAC | Stop. You need a cluster-admin (or someone with that access) for the CRD/operator install step specifically, even if you don't need it day to day. |

---

## Step 2 — install only what's missing

If Step 1 found an existing stack, do **not** run the quickstart values file.
Write a `stack.yaml` pointing at what's already there (see
[GETTING_STARTED.md](../GETTING_STARTED.md) Path B), and if OTel Operator is
the only missing piece:

This custom partial install doesn't go through the `make install-*` targets
above (those are wired to the two standard values files), so re-run
`make preflight` (or `./scripts/preflight-check.sh -n observability -r
lantern`) yourself immediately before this `helm install` — don't assume the
Step 1 run is still fresh if any time has passed.

```bash
helm install lantern charts/lantern-stack \
  -n observability --create-namespace \
  --set kube-prometheus-stack.enabled=false \
  --set tempo.enabled=false \
  --set loki.enabled=false \
  --set opentelemetry-operator.enabled=true \
  --set backends.metrics.remoteWrite=http://YOUR-EXISTING-PROMETHEUS:9090/api/v1/write \
  --set backends.metrics.queryURL=http://YOUR-EXISTING-PROMETHEUS:9090 \
  --set backends.dashboards.instanceRef.name=YOUR-EXISTING-GRAFANA
```

If nothing exists and Step 1 gave you the all-clear, the quickstart path from
[GETTING_STARTED.md](../GETTING_STARTED.md) applies as written.

Either way: **`helm install` here only installs operators and (optionally)
storage backends. No microservice is touched yet.** Nothing in this step
restarts a single pod outside the `observability` namespace.

---

## Step 3 — generate, don't apply

```bash
kubectl get cm -n observability lantern-stack \
  -o jsonpath='{.data.stack\.yaml}' > stack.yaml

kubectl get deploy,statefulset,daemonset,cronjob -A -o yaml \
  | lantern discover - -team unassigned > services.yaml
```

**Read `services.yaml` in full.** Every `REVIEW` marker is a guess about a
real production-adjacent service. Fix the teams — an unrouted alert is worse
than no alert. Remove entries for anything you don't want touched yet; you can
run `discover` again later for the rest.

```bash
lantern synth -stack stack.yaml services.yaml -strict > manifests.yaml
```

`-strict` fails the build on warnings instead of printing them — you want to
see every one on a shared cluster.

**Diff before applying, every time:**

```bash
kubectl diff -f manifests.yaml
```

---

## Step 4 — split the rollout: scrape/alerts first, instrumentation staged

This is the step that actually determines whether anything breaks.

### 4a. Scrape config and alerts — apply broadly

`ServiceMonitor` and `PrometheusRule` don't touch pod specs. Applying these
for every discovered service is safe to do in one pass:

```bash
grep -B2 '^kind: \(ServiceMonitor\|PrometheusRule\)' manifests.yaml  # sanity check what's in scope
kubectl apply -f manifests.yaml --prune -l lantern.dev/managed=true \
  --dry-run=client  # review, then drop --dry-run=client
```

You now have metrics and burn-rate alerts on every service, with zero pod
restarts.

### 4b. Instrumentation — one service, then wait, then expand

Every `Deployment` patch in `manifests.yaml` that adds an
`instrumentation.opentelemetry.io/inject-*` annotation **will restart that
workload's pods on apply.** Do not apply all of them at once.

```bash
# extract just the annotation patch for ONE low-risk, non-critical service
lantern synth -stack stack.yaml services.yaml -strict \
  | yq 'select(.kind == "Deployment" and .metadata.name == "SOME-LOW-RISK-SERVICE")' \
  > canary-patch.yaml

kubectl diff -f canary-patch.yaml
kubectl apply -f canary-patch.yaml

# watch it. really watch it.
kubectl rollout status deploy/SOME-LOW-RISK-SERVICE -n TARGET-NS
kubectl top pod -n TARGET-NS -l app=SOME-LOW-RISK-SERVICE
kubectl logs -n TARGET-NS -l app=SOME-LOW-RISK-SERVICE --tail=100
```

Soak that for a day before touching the next one. Watch for: restart loops,
memory growth (agent overhead is usually 50–150MB per pod, more for JVMs),
and startup time changes if anything has a tight liveness-probe timeout.

Once the canary is stable, expand in waves — by team, or by criticality, never
"everything" in one apply. A reasonable cadence: canary → same team's other
services → one more team → the rest, each wave separated by at least a day.

**All services resolved to eBPF mode, not agent injection?** The staging
procedure above has nothing to operate on in that case — eBPF instrumentation
attaches at the node level via the OTel Operator's eBPF profiling support, not
through a per-Deployment annotation, so there's no `inject-*` annotation to
extract and no single Deployment patch to canary. Stage it at the node level
instead:

1. Confirm `instrumentation.ebpf.enabled` is actually on for only as many
   nodes as you intend to canary — if the cluster uses node pools, target the
   eBPF DaemonSet at one pool first (a nodeSelector/affinity on the generated
   DaemonSet, not a Lantern-generated object) rather than letting it schedule
   cluster-wide on first apply.
2. Watch node-level signals, not pod-level ones: `kubectl top nodes` for CPU
   overhead from the eBPF collector, and `dmesg`/kernel logs on the target
   nodes for anything unexpected — eBPF programs run in kernel space, so a bad
   interaction shows up there before it shows up in a workload's own logs.
3. Once that node pool is stable, expand pool by pool the same way you'd
   expand team by team above — never flip it cluster-wide in one apply.

If the cluster only has one node pool, there's no way to stage this
sub-cluster — treat the whole eBPF rollout as a single higher-risk canary
step, soak it longer than a day, and make sure whoever owns the cluster knows
before you flip it on.

---

## Step 5 — rollback

Everything Lantern generates carries `lantern.dev/managed: "true"`. To remove
everything it added, without guessing:

```bash
kubectl delete servicemonitor,prometheusrule -A -l lantern.dev/managed=true
```

To revert an instrumentation patch on one service, remove the annotation and
let it roll:

```bash
kubectl annotate deploy SOME-SERVICE -n TARGET-NS \
  instrumentation.opentelemetry.io/inject-java- 
```

(Trailing `-` removes the key.) This restarts the pod again, without the
agent.

To remove the operators/CRDs themselves — only do this if nothing depends on
them, since CRD deletion cascades to every custom resource of that type,
cluster-wide, including ones you didn't create:

```bash
kubectl get servicemonitor,prometheusrule -A   # confirm nothing else's is in here first
helm uninstall lantern -n observability
```

---

## Guardrails to tighten for a shared cluster

In `stack.yaml`, before the first apply:

```yaml
spec:
  policy:
    requireTeamLabel: true       # force ownership before instrumentation goes out
    maxSeriesPerService: 10000   # tighter than the 25000 default on a shared cluster
    minSamplingRate: 0.01
    maxSamplingRate: 0.2         # cap trace volume until you've seen real cardinality
```

Start every service's trace sampling low (`samplingRate: 0.05`–`0.1`) and turn
it up once you've confirmed the collector and backend can absorb the volume.

**`requireTeamLabel: true` checks that a team is *present*, not that it's
real.** `lantern discover -team unassigned` (the example command in Step 3)
satisfies the check mechanically — every service gets `team: unassigned`,
which is a non-empty string — without satisfying the point of the check,
which is that alerts route to someone who'll act on them. Reading every
`REVIEW` marker in `services.yaml` per Step 3 is what actually closes this
gap; the policy flag only stops you from forgetting to look, it doesn't do
the looking for you. Before the first real apply, grep for the placeholder
team and route each one:

```bash
grep -B5 'team: unassigned' services.yaml   # every hit needs a real owner
```

---

## What this runbook deliberately does not cover

- Multi-cluster / fleet rollout — this is single-cluster.
- Production (as opposed to UAT) — the same staging discipline applies, with
  more caution and probably a longer soak per wave.
- Rolling back a partially-applied instrumentation wave that already caused an
  incident — that's an incident-response runbook, not this one. If a canary
  causes problems, stop expanding and revert that one service first; don't
  troubleshoot mid-rollout.
