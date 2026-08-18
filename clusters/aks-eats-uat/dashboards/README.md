# Grafana dashboards for `aks-eats-uat`

Four hand-built Grafana dashboards, deployed directly to the `aks-eats-uat`
AKS cluster during onboarding. **These are cluster-specific artifacts, not
part of `charts/lantern-stack`** — they hardcode this cluster's real
namespaces (`eats-core`, `eats-support`, `cde`) and real service names
(`tfs-booking-service`, etc.), so they're not reusable as-is on another
cluster. That's a different thing from the chart's own automatically
generated per-service dashboards (`spec.dashboard` on each
`ServiceObservability` — see
[docs/getting-signals-into-grafana.md](../../../docs/getting-signals-into-grafana.md)),
which *are* generic and ship with the compiler.

Each file here is a plain `ConfigMap` (label `grafana_dashboard: "1"`),
picked up automatically by the Grafana sidecar already enabled in
`values-quickstart.yaml`. Applying one of these is additive — it doesn't
touch any other dashboard, doesn't restart any pod, and needs no CRDs.

| File | Dashboard | What it shows |
|---|---|---|
| `cluster-uptime.yaml` | Cluster & Service Uptime | Node/deployment health at a glance — CPU/memory/disk by node, deployment availability, container restarts |
| `service-logs.yaml` | Service Logs | Namespace/pod dropdown → log volume + raw log lines |
| `service-traces.yaml` | Service Traces | Per-service trace count, p95 duration, trace list, duration over time |
| `investigate.yaml` | Investigate a Point in Time | The main correlation dashboard — set a time window, see Monitors → Kubernetes Events → Logs → Traces for that exact window, all live-queried, nothing static |

## Deploying / updating

```bash
kubectl diff -f clusters/aks-eats-uat/dashboards/investigate.yaml   # ALWAYS diff first
kubectl apply -f clusters/aks-eats-uat/dashboards/investigate.yaml
```

Grafana's sidecar polls every 30s (`updateIntervalSeconds`) and picks up the
change automatically — no restart needed.

## Keeping this in sync with the live cluster

These files are snapshots. If you edit a dashboard live in Grafana's UI (or
apply a change directly with `kubectl` without updating the file here first),
re-pull it before committing so git stays the source of truth, not a stale
copy:

```bash
kubectl get configmap lantern-dashboard-investigate -n observability -o yaml \
  | python3 -c "
import sys, yaml, json
d = yaml.safe_load(sys.stdin)
d['metadata'] = {k: d['metadata'][k] for k in ('name', 'namespace', 'labels')}
for k, v in d['data'].items():
    d['data'][k] = json.dumps(json.loads(v), indent=2) + '\n'
yaml.safe_dump(d, sys.stdout, default_flow_style=False, sort_keys=False, width=100)
" > clusters/aks-eats-uat/dashboards/investigate.yaml
```

The embedded dashboard JSON is pretty-printed (not minified) specifically so
`git diff` on these files is actually readable panel-by-panel.
