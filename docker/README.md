# Docker logs + metrics stack (no Kubernetes required)

A standalone Loki + Prometheus + Grafana stack for anyone running plain
Docker or Docker Compose, not Kubernetes. Same "bring your own backend, this
is an opt-in convenience" philosophy as `charts/lantern-stack`'s Kubernetes
quickstart — just for Docker instead. **This directory has no relationship
to Lantern's Go compiler, `lantern discover`/`synth`, or
`charts/lantern-stack` whatsoever.** It doesn't touch, depend on, or get
invoked by any of that — zero risk to the Kubernetes path if something here
is wrong.

Traces are still out of scope. Metrics here means Prometheus + host-level
metrics via node-exporter, **not** per-container resource metrics — see
[Why no per-container metrics](#why-no-per-container-metrics) for the real,
tested reason cAdvisor doesn't work here.

**Verified end-to-end for real**, not just sourced from docs. Logs: rootless
Docker installed on a host that had none, a plain `docker run nginx`
container and a separate `docker compose`-started one both wired to this
stack's Loki via the driver, and real log lines (actual nginx access log
entries, not synthetic test data) confirmed reaching Grafana through its
own datasource proxy — the same query path the bundled dashboard uses, not
just Loki's API directly. One real bug was found and fixed this way: the
dashboard's dropdown originally filtered on `compose_service`, which is
**only** present for `docker compose`-started containers — a plain `docker
run` container (exactly the first thing tested) was invisible in the
dropdown despite its logs genuinely reaching Loki correctly. Fixed to
filter on `container_name` instead, which is present in both cases; see
[What labels you get, for free](#what-labels-you-get-for-free) for why.
Metrics: Prometheus confirmed actually scraping both its own and
node-exporter's `/metrics` (targets `up`, not just "container running"),
and real host CPU/memory numbers confirmed flowing through Grafana's
datasource proxy into the bundled dashboard. Also found and fixed this way:
cAdvisor, the obvious first choice for per-container metrics, does not work
at all on this host's rootless Docker install — see below.

## Setup

**1. Install Loki's Docker logging driver plugin** — a one-time, per-host
step. This is a Docker daemon plugin, not something `docker compose up` can
install for you:

```bash
docker plugin install grafana/loki-docker-driver:3.7.0-amd64 --alias loki --grant-all-permissions
# ARM64 host: use the -arm64 tag suffix instead of -amd64
```

**2. Start Loki + Grafana:**

```bash
cd docker
docker compose up -d
```

**3. Point your own services at Loki.** The stack above only runs Loki and
Grafana — it doesn't ship anyone's logs by itself. Add this `logging:` block
to every service in your own `docker-compose.yml` whose logs you want to
see:

```yaml
services:
  your-service:
    # ... your existing config ...
    logging:
      driver: loki
      options:
        loki-url: "http://localhost:3100/loki/api/v1/push"
        # Defaults are 10 retries / 10s timeout / 5m max backoff -- fine
        # when Loki is healthy, but there's a real, documented failure mode
        # where an unreachable Loki endpoint blocks the Docker daemon from
        # stopping/removing the container at all (see the GitHub issues
        # linked above). These four options avoid that:
        mode: non-blocking
        max-buffer-size: "4m"
        loki-retries: "2"
        loki-timeout: "1s"
        loki-max-backoff: "800ms"
```

If your services run in a *different* `docker-compose.yml`/project than this
stack, `loki-url` needs Loki's real reachable address from that other
project's network — `http://localhost:3100/...` only works if Loki's `3100`
port is published to the host and the other project can reach the host
network. Same-host, different-compose-project is the common case this
covers; cross-host needs a real reachable IP/hostname instead of
`localhost`.

**4. Point your own services at Prometheus, if they expose metrics.** The
stack runs Prometheus and node-exporter (host CPU/memory/disk/network) out
of the box — no extra step needed for host metrics. If your own service
exposes a Prometheus-format `/metrics` endpoint, add a scrape job for it in
`prometheus.yml`:

```yaml
scrape_configs:
  # ... existing prometheus/node jobs ...
  - job_name: my-service
    static_configs:
      - targets: ['my-service:9000']   # container name : metrics port
```

Compose puts every service on the same default network, so the container
name resolves as a hostname — no published port or extra networking needed
unless you also want to query it from the host directly. Re-run
`docker compose up -d` after editing (Prometheus doesn't auto-reload this
file).

**5. Open Grafana** at <http://localhost:3000> (`admin` / `lantern` —
change this before leaving it reachable by anyone else). The Loki and
Prometheus datasources and two dashboards — **"Docker: Container Logs"** and
**"Docker: Host Metrics"** — are all pre-provisioned. Pick your container
from the logs dropdown; the metrics dashboard needs no selection, it's
single-host by design.

## What labels you get, for free

Loki's Docker driver automatically discovers and attaches, per the
[driver's own docs](https://grafana.com/docs/loki/latest/send-data/docker-driver/configuration/)
and confirmed for real against both a plain `docker run` container and a
`docker compose` one:

- `container_name` — defaults to the container's own name, present for
  **both** plain `docker run` and Compose (this is what the dashboard's
  dropdown actually filters on, precisely because it's universal —
  `compose_service` alone would leave every plain `docker run` container
  invisible in the dropdown, which is exactly what happened testing this
  against a real `docker run nginx` before this was caught and fixed)
- `compose_project` and `compose_service` — **only** for services started
  via `docker compose`, absent entirely for plain `docker run`. Useful for
  your own filtered queries in Explore (e.g. `{compose_project="my-app"}`
  to see every service in one stack at once) but not what the bundled
  dashboard uses as its primary filter.

No log-parsing config to write, no `filelog` receiver, no bind-mounting
`/var/lib/docker/containers` — the driver pushes straight to Loki's HTTP
push API as log lines are written. This is the main reason this stack uses
Loki's own driver instead of an OTel Collector `filelog` receiver the way
`charts/lantern-stack`'s Kubernetes `logsCollector` does: Kubernetes has a
consistent `/var/log/pods/<namespace>_<pod>_<uid>/...` path Lantern can
parse for free namespace/pod/deployment identity; Docker doesn't have an
equivalent, so hand-rolling that parsing would be strictly worse than the
label attachment Loki's driver already does natively.

## Why no per-container metrics

cAdvisor is the standard way to get per-container CPU/memory/network/disk
metrics in Prometheus format, and it was the first thing tried here. **It
does not work on this host's rootless Docker install, confirmed empirically,
not assumed:**

- With `--docker_only=true` (the normal mode, gives containers their real
  names): cAdvisor's Docker factory registers, but every container fails
  with `failed to identify the read-write layer ID for container ... open
  .../image/overlayfs/layerdb/mounts/<id>/mount-id: no such file or
  directory`. Root cause: this host's rootless Docker (installed via
  `get.docker.com/rootless`) uses the containerd image store/snapshotter,
  not the legacy overlayfs graphdriver cAdvisor's Docker factory expects.
  Tested against both cAdvisor v0.49.1 and v0.52.1 — same failure on both.
- Without `--docker_only`, cAdvisor falls back to walking raw cgroups
  directly, which does produce metrics — but zero of them are scoped to an
  actual `docker-<id>.scope`, only parent systemd slices
  (`/user.slice/user-1000.slice/...`). No per-container breakdown at all,
  just confirms the same underlying gap.

This is a known, documented class of problem
([google/cadvisor#3037](https://github.com/google/cadvisor/issues/3037),
[#3245](https://github.com/google/cadvisor/issues/3245),
[#3728](https://github.com/google/cadvisor/issues/3728)), not something
specific to this repo. If you're running Docker in **rootful** mode instead
of rootless, cAdvisor's standard setup (`-v /var/run/docker.sock:...`,
`-v /var/lib/docker:/var/lib/docker:ro`) is likely to just work — this gap
is specifically a rootless + containerd-image-store combination. Worth
retrying if your setup differs from this one.

What you get instead: host-level CPU/memory/disk/network from node-exporter
(no docker socket dependency at all, verified working cleanly), and
per-service metrics for anything that exposes its own `/metrics` endpoint
(step 4 above) — which is arguably the better signal for services you
control anyway, versus inferring resource use from the outside.

## Known limitations

- **The Loki plugin has to be installed before any container using it
  starts.** If you add the `logging:` block to a service that's already
  running, `docker compose up` needs to recreate that container for the
  change to take effect (normal Compose behavior, not specific to this).
- **No per-container metrics, no traces.** See above for metrics; traces
  would need a real OTel-Collector-based pipeline, a separate, larger piece
  of work not attempted here.
- **Ephemeral by default.** `loki-data`/`grafana-data`/`prometheus-data` are
  named Docker volumes, not bind mounts — `docker compose down -v` deletes
  everything. Fine for local dev, not for anything you want to keep.
- **Default Grafana admin password is `lantern`** (matches the Kubernetes
  quickstart's convention) — change it if this is reachable by anyone but
  you.

## Tearing down

```bash
docker compose down          # stop, keep the volumes (logs/metrics survive)
docker compose down -v       # stop and delete all stored state
```
