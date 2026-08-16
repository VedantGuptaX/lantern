# Docker log stack (no Kubernetes required)

A standalone Loki + Grafana stack for anyone running plain Docker or Docker
Compose, not Kubernetes. Same "bring your own backend, this is an opt-in
convenience" philosophy as `charts/lantern-stack`'s Kubernetes quickstart —
just for logs, on Docker instead. **This directory has no relationship to
Lantern's Go compiler, `lantern discover`/`synth`, or `charts/lantern-stack`
whatsoever.** It doesn't touch, depend on, or get invoked by any of that —
zero risk to the Kubernetes path if something here is wrong.

Metrics and traces are intentionally out of scope. Logs only, for now.

**Verified end-to-end for real**, not just sourced from docs: rootless
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

**4. Open Grafana** at <http://localhost:3000> (`admin` / `lantern` —
change this before leaving it reachable by anyone else). The Loki datasource
and a **"Docker: Container Logs"** dashboard are both pre-provisioned — pick
your container from the dropdown, logs appear if step 3 was done correctly
for that container.

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

## Known limitations

- **The plugin has to be installed before any container using it starts.**
  If you add the `logging:` block to a service that's already running,
  `docker compose up` needs to recreate that container for the change to
  take effect (normal Compose behavior, not specific to this).
- **No metrics, no traces.** If you need those on Docker later, that's a
  separate, larger piece of work — see the discussion in the project's
  history for why this stayed logs-only for now (mainly: Loki's Docker
  driver has no metrics/traces equivalent, so that would mean a real
  OTel-Collector-based pipeline instead, a different design from this one).
- **Ephemeral by default.** `loki-data`/`grafana-data` are named Docker
  volumes, not bind mounts — `docker compose down -v` deletes everything.
  Fine for local dev, not for anything you want to keep.
- **Default Grafana admin password is `lantern`** (matches the Kubernetes
  quickstart's convention) — change it if this is reachable by anyone but
  you.

## Tearing down

```bash
docker compose down          # stop, keep the volumes (logs survive)
docker compose down -v       # stop and delete all stored logs/dashboards state
```
