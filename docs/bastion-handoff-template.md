# Bastion handoff template

`CLAUDE.md` is the file a Claude Code session reads first when it starts cold
on a bastion host — it's what makes that session useful and safe immediately,
without re-deriving context or re-litigating decisions already made. It is
deliberately **gitignored**, because it carries per-engagement specifics
(cluster names, resource groups, who owns what) that don't belong in a public
or shared repo history.

That's the right call for what goes *in* the file, but it has one real cost:
the first time someone sets up a new bastion, there's nothing to start from —
the previous `CLAUDE.md` is gone the moment the bastion is torn down, and
reconstructing it means digging through commit history for whichever commit
happened to add it before it was gitignored (`git log --diff-filter=A --
CLAUDE.md`, then `git show <sha>:CLAUDE.md`). That's exactly the kind of
archaeology this file exists to prevent.

**This file is the fix: a tracked, generic starting point.** It is *not*
gitignored — copy it to `CLAUDE.md` at the start of a new engagement, fill in
the blanks, and let the copy (not this template) carry the real cluster
specifics from then on.

```bash
cp docs/bastion-handoff-template.md CLAUDE.md
# then fill in every _fill in_ below and delete this instructional block
```

---

## What this is

**Lantern** is an observability-as-code compiler: one typed
`ServiceObservability` spec compiles to OpenTelemetry instrumentation,
Prometheus scrape config, and SLO burn-rate alerts. It is a compiler, not a
platform — it generates YAML, it never talks to a cluster itself. `kubectl`
does the applying.

Full picture: [`README.md`](../README.md) (what it does, project status),
[`DESIGN.md`](../DESIGN.md) (architecture and why), [`GETTING_STARTED.md`](../GETTING_STARTED.md)
(the standard walkthrough), [`plan.md`](../plan.md) if it exists locally (full
roadmap and a decisions log — check there for anything not yet in
`README.md`'s project status).

## Project status — don't relitigate this

Check [`README.md`'s Project status section](../README.md#project-status) for
the current, exact "shipped" list — it changes over time and this template
should not try to duplicate it. As of when this template was last touched:
the compiler, the CLI, `lantern discover`, OTel injection, Prometheus
scrape/alert generation, the quickstart Helm chart, and GPU/DCGM node health +
GPU inference-server SLOs are done and tested. Dashboard generation, Grafana
folder/RBAC emission, the Kubernetes operator, and log↔trace correlation are
not built yet.

The Helm chart's real-world verification status matters more than what's
"done": check [`plan.md`'s "Known verification debt" table](../plan.md) (or
`CLAUDE.md`'s own prior notes, if this isn't the first engagement) before
assuming `helm install` is a known-good path. Treat every install as a first
real-world run unless someone specifically confirms otherwise.

---

## THE ENGAGEMENT THIS BASTION EXISTS FOR

_fill in: what cluster, what's already running on it, what the goal of this
engagement is. Is this greenfield or does the cluster already carry real
traffic? That distinction changes everything below it._

If the cluster already has real traffic on it — even UAT-grade — read
[`docs/adopting-an-existing-cluster.md`](adopting-an-existing-cluster.md)
before running any `helm install` or `kubectl apply`. Do not shortcut it
because it looks like a lot of steps — the two things it protects against
(cluster-scoped CRD/webhook conflicts, and pod restarts from instrumentation
injection landing on every service at once) are the two realistic ways this
goes wrong.

### Prerequisites — check these before anything else

The bastion needs, at minimum: `kubectl` authenticated against the target
cluster (via `az login` / `kubelogin`, run locally, never in chat — see the
non-negotiable rules below), `go` (matching `go.mod`'s version), `make`,
`jq`, and ideally `yq` (used in the runbook's canary-extraction step). Check
each with `which <tool>` before assuming it's there; a bastion image is not
guaranteed to match a dev laptop.

**`docker`/`kind` may not be available on a locked-down bastion.** Step 0 of
the runbook (`./scripts/verify-kind.sh`) needs one of them, and if neither is
installed, that rehearsal step cannot run as written — see the runbook's
"no docker/kind available" fallback for the partial substitute (`go test` +
`helm lint`/`helm template`), and treat the rehearsal as skipped, not passed,
when relying on it.

### Cluster facts — fill in before starting

| | |
|---|---|
| Cluster name | _fill in_ |
| Cloud / resource group / subscription (or equivalent) | _fill in_ |
| kubectl context name | _fill in_ |
| Existing monitoring already on this cluster? | _fill in — check via Step 1 of the runbook, don't assume_ |
| `observability` namespace already in use for something else? | _fill in_ |
| Who owns this cluster / who to loop in before cluster-scoped changes | _fill in_ |
| Node pool size / headroom | _fill in — `kubectl describe nodes`_ |
| GPU nodes present? (changes whether `gpuMonitoring` applies) | _fill in_ |

If these aren't filled in, fill them in first. Don't guess at cluster state —
the preflight commands in the runbook are cheap and non-destructive; run them.

---

## Resource requirements

See [`README.md`'s Resource requirements table](../README.md#resource-requirements)
for the current numbers (including the GPU monitoring row, if relevant to this
engagement) — kept there rather than duplicated here so it can't drift out of
sync. `make preflight` computes the cluster's actual free capacity against
those numbers before anything is installed — see the rule below, it's not
optional.

## Non-negotiable rules for this engagement

These aren't style preferences. Each one maps to a specific way a cluster
gets broken.

1. **Never run `helm install` directly. Use `make preflight` first, always —
   or `make install-quickstart` / `make install-byo`, which run it for you and
   abort before `helm install` if it fails.** This is automated, not just
   documented: `scripts/preflight-check.sh` is a real prerequisite in the
   Makefile, and a defense-in-depth copy runs as a Helm pre-install hook for
   anyone who bypasses `make` anyway. The specific risk it's checking for:
   Helm's actual behavior on a CRD that already exists is to **skip it and
   warn, not error** — so a plain `helm install` can succeed while the
   controller it just installed quietly reconciles against a CRD owned by
   something else entirely. That's how this breaks quietly instead of loudly.

2. **Never apply an instrumentation-injection annotation to more than one
   service at a time on the first pass.** `instrumentation.opentelemetry.io/inject-*`
   restarts every pod of that Deployment and injects a real agent into the
   running process. This is the step that actually changes running service
   behavior. Canary one low-risk service, soak it, then expand — see runbook
   Step 4. If every discovered service resolves to eBPF mode rather than agent
   injection, the per-Deployment-annotation canary procedure has nothing to
   operate on — see the runbook's eBPF-specific staging note for the
   alternative.

3. **Always `kubectl diff` before `kubectl apply`.** No exceptions, including
   for "just the scrape config" — diff first is how you catch a `stack.yaml`
   pointing at the wrong endpoint before it's live.

4. **Never delete a CRD** (`kubectl delete crd ...`) without first listing
   every custom resource of that kind, cluster-wide, and confirming none of
   them belong to something you didn't create. CRD deletion cascades.

5. **Never handle or request cluster credentials, kubeconfig contents, or
   cloud service-principal secrets in chat.** Authenticate locally on the
   bastion, outside the conversation. If a session needs to run
   `kubectl`/`helm` against the cluster, it should already be authenticated
   via the ambient kubeconfig — never paste tokens or keys into a prompt.

6. **Escalate rather than guess** on: anything cluster-scoped (CRDs, webhooks,
   ClusterRoleBindings), any change to a service outside the current
   canary/wave, any policy change that widens `denyLabels` or raises sampling
   above what the runbook suggests, and any request to skip the diff step "to
   save time." Also escalate — don't paper over — a `-team` value from
   `discover` that's actually a placeholder (e.g. `unassigned`): it satisfies
   `policy.requireTeamLabel` mechanically without satisfying the point of the
   check, which is real alert ownership. Route every discovered service to a
   real team before its alerts go live.

## What's safe to do without asking

- Everything in `pkg/`, `cmd/`, `charts/` — editing Lantern's own source,
  running `make all`, `make demo`, `./scripts/verify-kind.sh` against a local
  `kind` cluster.
- `lantern discover`, `lantern synth`, `lantern validate` — these only read
  files and write files. They never touch a live cluster.
- Any `kubectl get`, `kubectl describe`, `kubectl diff`, `kubectl top` against
  the cluster — read-only, always fine.
- Applying `ServiceMonitor`/`PrometheusRule` objects broadly, once Step 1 of
  the runbook has confirmed no conflicting stack exists — these are additive
  and don't restart pods.

---

## Commands cheat sheet

```bash
# build and test — no cluster needed
make all
make demo                          # discover -> synth on the bundled example

# rehearse against a throwaway cluster before touching the real one
./scripts/verify-kind.sh
./scripts/verify-kind.sh --no-loki # skip the least-verified chart component

# generate only — safe against any cluster, read-only
kubectl get deploy,statefulset,daemonset,cronjob -A -o yaml \
  | lantern discover -team unassigned - > services.yaml
lantern synth -stack stack.yaml services.yaml -strict > manifests.yaml
kubectl diff -f manifests.yaml     # ALWAYS before apply
```

Flags can go before or after positional arguments either way — both forms
above are equivalent.

Full staged runbook: [`docs/adopting-an-existing-cluster.md`](adopting-an-existing-cluster.md).

## Repo map

```
README.md, GETTING_STARTED.md, DESIGN.md, CONTRIBUTING.md   docs
docs/adopting-an-existing-cluster.md    staged runbook for a live, populated cluster
docs/gpu-and-inference-observability.md GPU/DCGM + inference-server SLO guide
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
