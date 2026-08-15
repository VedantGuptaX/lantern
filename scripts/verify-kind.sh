#!/usr/bin/env bash
#
# End-to-end verification of Lantern on a throwaway kind cluster.
#
# Everything this project claims but has never actually executed gets executed
# here: chart dependency resolution, helm lint, a real install, discovery
# against a live cluster, and applying generated manifests.
#
# It deliberately does NOT stop at the first failure. A report that says
# "step 4 failed and steps 5-9 were never attempted" is far less useful than
# one showing every step's outcome, so each step records its status and the
# script continues wherever continuing is meaningful.
#
# Usage:
#   ./scripts/verify-kind.sh              # run everything
#   ./scripts/verify-kind.sh --keep       # leave the cluster running afterwards
#   ./scripts/verify-kind.sh --no-loki    # skip Loki (the least-verified part)
#
# Output: verify-report.txt in the repo root. Send that file back for debugging.

set -uo pipefail

CLUSTER=lantern-test
NS=observability
REPORT="$(pwd)/verify-report.txt"
KEEP=0
LOKI=true

for arg in "$@"; do
  case "$arg" in
    --keep)    KEEP=1 ;;
    --no-loki) LOKI=false ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

: > "$REPORT"
STEP=0
declare -a RESULTS=()

log()  { echo "$*" | tee -a "$REPORT"; }
rule() { log ""; log "════════════════════════════════════════════════════════════════"; }

# run <name> <command...> — records status, never aborts the script.
run() {
  local name="$1"; shift
  STEP=$((STEP + 1))
  rule
  log "STEP $STEP: $name"
  log "\$ $*"
  log "────────────────────────────────────────────────────────────────"
  local start; start=$(date +%s)
  if "$@" >>"$REPORT" 2>&1; then
    local dur=$(( $(date +%s) - start ))
    log "✅ PASS  ($name, ${dur}s)"
    RESULTS+=("PASS  $name")
    return 0
  else
    local code=$? dur=$(( $(date +%s) - start ))
    log "❌ FAIL  ($name, exit $code, ${dur}s)"
    RESULTS+=("FAIL  $name (exit $code)")
    return $code
  fi
}

# ---------------------------------------------------------------------------
# Preflight — check tools before spending ten minutes discovering one is absent
# ---------------------------------------------------------------------------

rule
log "Lantern verification — $(date)"
log "host: $(uname -s) $(uname -m)"
rule
log "PREFLIGHT"

MISSING=0
for tool in docker kind helm kubectl go; do
  if command -v "$tool" >/dev/null 2>&1; then
    ver=$("$tool" version 2>/dev/null | head -1 | cut -c1-70)
    log "  ✅ $tool  ${ver:-present}"
  else
    log "  ❌ $tool  NOT FOUND"
    MISSING=1
  fi
done

if ! docker info >/dev/null 2>&1; then
  log "  ❌ docker daemon is not responding."
  log "     On macOS with Colima:  colima start --cpu 4 --memory 8"
  log "     With Docker Desktop:   start the app, and raise its memory to 8GB"
  MISSING=1
else
  # kube-prometheus-stack + Tempo + Loki + a collector will not fit in 4GB.
  MEM=$(docker info --format '{{.MemTotal}}' 2>/dev/null || echo 0)
  MEM_GB=$(( MEM / 1024 / 1024 / 1024 ))
  log "  ✅ docker daemon up, ${MEM_GB}GB available to containers"
  if [ "$MEM_GB" -lt 6 ]; then
    log "  ⚠️  ${MEM_GB}GB is likely too little. Expect pods stuck Pending or OOMKilled."
    log "     Colima:  colima stop && colima start --cpu 4 --memory 8"
  fi
fi

if [ "$MISSING" -eq 1 ]; then
  log ""
  log "Install what's missing, then re-run. On macOS:"
  log "  brew install kind helm kubectl go"
  log "  brew install colima docker && colima start --cpu 4 --memory 8"
  exit 1
fi

# ---------------------------------------------------------------------------
# The actual verification
# ---------------------------------------------------------------------------

run "build the lantern CLI" make build
LANTERN="$(pwd)/bin/lantern"

run "compile the bundled example (no cluster needed)" \
  "$LANTERN" validate -stack examples/stack.yaml examples/checkout-api.yaml

run "discover → synth round trip (no cluster needed)" make demo

# --- cluster ---------------------------------------------------------------

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log ""
  log "note: kind cluster '$CLUSTER' already exists; deleting it for a clean run"
  kind delete cluster --name "$CLUSTER" >>"$REPORT" 2>&1
fi

run "create kind cluster" kind create cluster --name "$CLUSTER" --wait 120s

if ! kubectl cluster-info --context "kind-$CLUSTER" >/dev/null 2>&1; then
  log ""
  log "❌ cluster is not reachable; skipping every remaining step."
  printf '%s\n' "${RESULTS[@]}" | tee -a "$REPORT"
  exit 1
fi
kubectl config use-context "kind-$CLUSTER" >>"$REPORT" 2>&1

# --- chart -----------------------------------------------------------------

# This is the step most likely to fail: chart versions and repository URLs were
# written from release pages, never resolved by helm.
run "helm dependency update  ← most likely failure point" \
  helm dependency update charts/lantern-stack

run "helm lint" helm lint charts/lantern-stack

run "helm template (render without installing)" \
  helm template lantern charts/lantern-stack \
    -n "$NS" -f charts/lantern-stack/values-quickstart.yaml

HELM_ARGS=(-n "$NS" --create-namespace -f charts/lantern-stack/values-quickstart.yaml --timeout 15m --wait)
if [ "$LOKI" = false ]; then
  HELM_ARGS+=(--set loki.enabled=false)
  log ""
  log "note: --no-loki given, installing without Loki"
fi

run "helm install (this pulls a lot of images; be patient)" \
  helm install lantern charts/lantern-stack "${HELM_ARGS[@]}"

# --- what actually came up -------------------------------------------------

rule
log "STEP $((++STEP)): cluster state after install"
log "────────────────────────────────────────────────────────────────"
kubectl get pods -n "$NS" -o wide 2>&1 | tee -a "$REPORT"
log ""
log "CRDs Lantern emits into:"
for crd in instrumentations.opentelemetry.io servicemonitors.monitoring.coreos.com prometheusrules.monitoring.coreos.com; do
  if kubectl get crd "$crd" >/dev/null 2>&1; then
    log "  ✅ $crd"
  else
    log "  ❌ $crd  MISSING — generated manifests will not apply"
  fi
done

NOTREADY=$(kubectl get pods -n "$NS" --no-headers 2>/dev/null | grep -vcE 'Running|Completed' || true)
if [ "${NOTREADY:-0}" -gt 0 ]; then
  log ""
  log "⚠️  $NOTREADY pod(s) not Running — details:"
  kubectl get pods -n "$NS" --no-headers 2>/dev/null | grep -vE 'Running|Completed' \
    | awk '{print $1}' | while read -r p; do
      log ""
      log "--- $p ---"
      kubectl describe pod -n "$NS" "$p" 2>&1 | tail -25 | tee -a "$REPORT"
    done
fi

# --- the real end-to-end test ----------------------------------------------

run "extract the ObservabilityStack the chart generated" \
  bash -c "kubectl get cm -n $NS lantern-stack -o jsonpath='{.data.stack\.yaml}' > /tmp/lantern-stack.yaml && cat /tmp/lantern-stack.yaml"

# Deploy something real to discover. A stock nginx gives us a workload with a
# named http port and no instrumentation, which is the common case.
run "deploy a sample workload to discover" bash -c "
  kubectl create namespace demo --dry-run=client -o yaml | kubectl apply -f - &&
  kubectl create deployment web --image=nginx:alpine -n demo --dry-run=client -o yaml | kubectl apply -f - &&
  kubectl label deployment web -n demo team=platform --overwrite &&
  kubectl patch deployment web -n demo --type=json \
    -p='[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/ports\",\"value\":[{\"name\":\"http\",\"containerPort\":80}]}]' &&
  kubectl rollout status deployment/web -n demo --timeout=120s"

run "lantern discover against the live cluster" bash -c "
  kubectl get deploy,statefulset,daemonset,cronjob -A -o yaml \
    | '$LANTERN' discover - -team unassigned > /tmp/lantern-services.yaml &&
  cat /tmp/lantern-services.yaml"

run "lantern synth against the real stack config" \
  "$LANTERN" synth -stack /tmp/lantern-stack.yaml /tmp/lantern-services.yaml

run "apply generated manifests to the cluster" bash -c "
  '$LANTERN' synth -stack /tmp/lantern-stack.yaml /tmp/lantern-services.yaml -quiet \
    | kubectl apply -f -"

rule
log "STEP $((++STEP)): verify the generated objects landed"
log "────────────────────────────────────────────────────────────────"
kubectl get servicemonitor,prometheusrule -A -l lantern.dev/managed=true 2>&1 | tee -a "$REPORT"
log ""
log "Prometheus rule load errors (empty is good):"
kubectl logs -n "$NS" -l app.kubernetes.io/name=prometheus --tail=200 2>/dev/null \
  | grep -iE 'error|invalid' | head -20 | tee -a "$REPORT" || log "  (none found)"

# ---------------------------------------------------------------------------

rule
log "SUMMARY"
log "────────────────────────────────────────────────────────────────"
printf '%s\n' "${RESULTS[@]}" | tee -a "$REPORT"
FAILS=$(printf '%s\n' "${RESULTS[@]}" | grep -c '^FAIL' || true)
log ""
if [ "${FAILS:-0}" -eq 0 ]; then
  log "✅ everything passed"
else
  log "❌ $FAILS step(s) failed — full output is in $REPORT"
fi

log ""
log "Grafana:  kubectl port-forward -n $NS svc/lantern-grafana 3000:80"
log "          admin / lantern"
if [ "$KEEP" -eq 1 ]; then
  log ""
  log "cluster kept. delete it with: kind delete cluster --name $CLUSTER"
else
  log ""
  log "deleting the cluster (pass --keep to preserve it)"
  kind delete cluster --name "$CLUSTER" >>"$REPORT" 2>&1
fi

log ""
log "report written to $REPORT"
exit "${FAILS:-0}"
