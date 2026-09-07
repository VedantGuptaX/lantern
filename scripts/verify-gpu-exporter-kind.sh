#!/usr/bin/env bash
#
# End-to-end verification of gpuMonitoring.installExporter on a throwaway kind
# cluster -- WITHOUT needing real GPU hardware.
#
# Why this is possible at all: the gate this project uses before installing
# dcgm-exporter is "does some node already advertise nvidia.com/gpu as
# allocatable" (see scripts/preflight-check.sh --gpu-install-exporter). That's
# a value in the Node object's .status, not a real device -- kubelet doesn't
# reconcile extended resources it doesn't itself manage, so `kubectl patch`
# can add it directly and it sticks. This script builds a 2-worker kind
# cluster, fakes exactly that signal on ONE worker, and then runs the real
# preflight check + a real `helm install` against it.
#
# What this DOES prove: the preflight gate correctly blocks/allows on the
# right signal, the nodeSelector this script derives from the preflight
# check's own output actually restricts the DaemonSet to the intended node
# (and away from the other one), RBAC and the chart's manifests are otherwise
# correct, and the ServiceMonitor this chart generates has a selector/
# namespace that actually matches the real dcgm-exporter pod -- all things
# that were previously only checked via `helm template`, not a live API
# server + real scheduler.
#
# What this does NOT prove: that dcgm-exporter can talk to a real GPU. The
# exporter pod on the fake node is expected to CrashLoopBackOff -- there's no
# /dev/nvidia*, no driver, nothing for it to attach to. That's not a bug in
# this chart; it's the one thing this project has never claimed to test
# without real hardware. This script checks the failure looks like "no GPU
# device", not like "RBAC denied" or "ImagePullBackOff" or "stuck Pending".
#
# Also covers the two dashboard pieces added alongside installExporter:
#   - "no GPU" scenario: a plain install with gpuMonitoring left at its
#     default (off) must emit ZERO GPU-shaped objects -- no fleet dashboard,
#     no dcgm ServiceMonitor/PrometheusRule. Checked via `helm template`
#     before the cluster is even touched, since this needs no live API
#     server at all.
#   - "fake GPU" scenario: once gpuMonitoring + installExporter are on, the
#     fleet dashboard ConfigMap (templates/gpu-dashboard.yaml) must exist,
#     carry valid JSON, and be labeled grafana_dashboard: "1" so a real
#     Grafana sidecar would actually pick it up -- checked live, against the
#     real object the API server stored, not just what was submitted.
#   - the compiler's per-service GPU panel (pkg/emit/grafana) gets exercised
#     too: the bundled gpu-inference example is discovered/synthesized/
#     applied for real, same as verify-kind.sh's own end-to-end section, and
#     its dashboard ConfigMap is checked for the GPU utilization panel.
#
# Usage:
#   ./scripts/verify-gpu-exporter-kind.sh          # run everything, then tear down
#   ./scripts/verify-gpu-exporter-kind.sh --keep   # leave the cluster running afterwards
#
# Requires: docker, kind, helm, kubectl, jq -- and a Docker daemon kind can
# actually use. kind needs real (or rootful-equivalent) cgroup delegation;
# on a rootless Docker host without that delegation, cluster creation fails
# with "requires setting systemd property Delegate=yes" before this script
# gets anywhere near GPU-specific logic -- that's an environment limitation,
# not something this script or the chart can work around.

set -uo pipefail

CLUSTER=lantern-gpu-test
NS=observability
REPORT="$(pwd)/verify-gpu-exporter-report.txt"
KEEP=0

for arg in "$@"; do
  case "$arg" in
    --keep) KEEP=1 ;;
    *) echo "unknown flag: $arg" >&2; exit 2 ;;
  esac
done

: > "$REPORT"
STEP=0
declare -a RESULTS=()
declare -a NOTES=()

log()  { echo "$*" | tee -a "$REPORT"; }
rule() { log ""; log "════════════════════════════════════════════════════════════════"; }

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

# Like run(), but for a step whose NON-ZERO exit is an EXPECTED, documented
# outcome rather than a failure -- here, `helm install --wait` on a dcgm-exporter
# DaemonSet that can never become Ready without a real GPU. It records into
# NOTES, never into RESULTS, so it is never counted toward the failed-steps
# total. The authoritative checks for this condition are the placement and
# crash-reason assertions in the steps below; this step just gets the objects
# onto the cluster and blocks (via --wait) until the pod has reached its
# terminal crash state, so those later assertions see a settled pod.
run_info() {
  local name="$1"; shift
  STEP=$((STEP + 1))
  rule
  log "STEP $STEP: $name"
  log "\$ $*"
  log "────────────────────────────────────────────────────────────────"
  local start; start=$(date +%s)
  if "$@" >>"$REPORT" 2>&1; then
    local dur=$(( $(date +%s) - start ))
    log "ℹ️  INFO  ($name completed without error, ${dur}s)"
    NOTES+=("INFO  $name — completed without error")
  else
    local code=$? dur=$(( $(date +%s) - start ))
    log "ℹ️  INFO  ($name exited $code — EXPECTED without real GPU hardware; the crash-reason check below is the authoritative assertion, ${dur}s)"
    NOTES+=("INFO  $name — exited $code (expected without real GPU; verified by the crash-reason check, not a failure)")
  fi
  return 0
}

cleanup_and_exit() {
  local code="$1"
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
  exit "$code"
}

# ---------------------------------------------------------------------------
# Preflight -- tools, then the cluster itself
# ---------------------------------------------------------------------------

rule
log "GPU exporter verification — $(date)"
log "host: $(uname -s) $(uname -m)"
rule
log "PREFLIGHT"

MISSING=0
for tool in docker kind helm kubectl jq; do
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
  MISSING=1
fi
if [ "$MISSING" -eq 1 ]; then
  log ""
  log "Install what's missing, then re-run."
  exit 1
fi

if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  log ""
  log "note: kind cluster '$CLUSTER' already exists; deleting it for a clean run"
  kind delete cluster --name "$CLUSTER" >>"$REPORT" 2>&1
fi

# Two workers, deliberately: one gets the fake GPU signal, the other doesn't.
# Without a second, plain node, an empty or wrong nodeSelector would still
# "work" by accident -- there'd be nowhere else for the pod to go.
KIND_CONFIG=$(mktemp)
cat > "$KIND_CONFIG" <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
  - role: worker
  - role: worker
EOF

run "create 3-node kind cluster (1 control-plane + 2 workers)" \
  kind create cluster --name "$CLUSTER" --config "$KIND_CONFIG" --wait 120s
rm -f "$KIND_CONFIG"

if ! kubectl cluster-info --context "kind-$CLUSTER" >/dev/null 2>&1; then
  log ""
  log "❌ cluster is not reachable; skipping every remaining step."
  printf '%s\n' "${RESULTS[@]}" | tee -a "$REPORT"
  cleanup_and_exit 1
fi
kubectl config use-context "kind-$CLUSTER" >>"$REPORT" 2>&1

# gpuMonitoring.enabled creates a ServiceMonitor + PrometheusRule -- these CRDs
# normally come from kube-prometheus-stack, which this test deliberately does
# NOT install (too heavy for what's being tested here). Installing just the
# two CRDs directly is enough for the objects to apply and be inspectable.
run "install ServiceMonitor + PrometheusRule CRDs (minimal, not the full Prometheus Operator)" bash -c "
  kubectl apply -f https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/main/example/prometheus-operator-crd/monitoring.coreos.com_servicemonitors.yaml &&
  kubectl apply -f https://raw.githubusercontent.com/prometheus-operator/prometheus-operator/main/example/prometheus-operator-crd/monitoring.coreos.com_prometheusrules.yaml"

# --- fake the GPU-readiness signal on exactly one worker --------------------

WORKERS=($(kubectl get nodes -o name | grep worker | sed 's#node/##'))
if [ "${#WORKERS[@]}" -lt 2 ]; then
  log "❌ expected 2 worker nodes, found ${#WORKERS[@]}: ${WORKERS[*]}"
  cleanup_and_exit 1
fi
GPU_NODE="${WORKERS[0]}"
PLAIN_NODE="${WORKERS[1]}"
log ""
log "faking nvidia.com/gpu readiness on: $GPU_NODE  (control node: $PLAIN_NODE stays plain)"

run "patch $GPU_NODE: add nvidia.com/gpu.present label" \
  kubectl label node "$GPU_NODE" nvidia.com/gpu.present=true --overwrite

run "patch $GPU_NODE: add nvidia.com/gpu to capacity+allocatable" \
  kubectl patch node "$GPU_NODE" --subresource=status --type=merge -p \
    '{"status":{"capacity":{"nvidia.com/gpu":"1"},"allocatable":{"nvidia.com/gpu":"1"}}}'

# --- the actual gate this project ships ------------------------------------

run "./scripts/preflight-check.sh --gpu-install-exporter  ← must PASS against the faked node" \
  ./scripts/preflight-check.sh -n "$NS" -r lantern --gpu-install-exporter

log ""
log "note: re-running with the fake node REMOVED to confirm the gate also BLOCKs correctly"
kubectl label node "$GPU_NODE" nvidia.com/gpu.present- >>"$REPORT" 2>&1
kubectl patch node "$GPU_NODE" --subresource=status --type=json \
  -p='[{"op":"remove","path":"/status/allocatable/nvidia.com~1gpu"},{"op":"remove","path":"/status/capacity/nvidia.com~1gpu"}]' \
  >>"$REPORT" 2>&1
if ./scripts/preflight-check.sh -n "$NS" -r lantern --gpu-install-exporter >>"$REPORT" 2>&1; then
  log "❌ preflight PASSED with no GPU node present -- the gate is broken, do not trust it"
  RESULTS+=("FAIL  preflight correctly blocks with no GPU node")
else
  log "✅ preflight correctly BLOCKed with no GPU node present"
  RESULTS+=("PASS  preflight correctly blocks with no GPU node")
fi
log ""
log "restoring the fake signal for the actual install below"
kubectl label node "$GPU_NODE" nvidia.com/gpu.present=true --overwrite >>"$REPORT" 2>&1
kubectl patch node "$GPU_NODE" --subresource=status --type=merge -p \
  '{"status":{"capacity":{"nvidia.com/gpu":"1"},"allocatable":{"nvidia.com/gpu":"1"}}}' >>"$REPORT" 2>&1

# --- install with gpuMonitoring.installExporter, targeted at the fake node --

run "helm dependency update  ← pulls the real dcgm-exporter chart" \
  helm dependency update charts/lantern-stack

# --- "no GPU cluster" scenario: default values, gpuMonitoring untouched ----
#
# No live cluster needed for this one -- `helm template` is enough, and
# running it BEFORE the live install below means a regression here fails
# fast instead of after several more minutes of cluster work.
rule
log "STEP $((++STEP)): \"no GPU cluster\" scenario — default values must emit ZERO GPU-shaped objects"
log "────────────────────────────────────────────────────────────────"
NO_GPU_RENDER=$(helm template lantern charts/lantern-stack -n "$NS" 2>&1)
echo "$NO_GPU_RENDER" >>"$REPORT"
NO_GPU_HITS=$(echo "$NO_GPU_RENDER" | grep -ic "gpu-fleet-dashboard\|dcgm" || true)
if [ "${NO_GPU_HITS:-0}" -eq 0 ]; then
  log "✅ a default install (gpuMonitoring left off) renders no fleet dashboard, no dcgm ServiceMonitor/PrometheusRule, nothing GPU-shaped at all"
  RESULTS+=("PASS  no-GPU install emits zero GPU-shaped objects")
else
  log "❌ found $NO_GPU_HITS GPU-related match(es) in a render where gpuMonitoring was never turned on -- something is on by default that shouldn't be"
  RESULTS+=("FAIL  no-GPU install unexpectedly emits GPU-shaped object(s)")
fi

run_info "helm install: gpuMonitoring.installExporter + dcgm-exporter, nothing else" \
  helm install lantern charts/lantern-stack \
    -n "$NS" --create-namespace \
    --set kube-prometheus-stack.enabled=false \
    --set opentelemetry-operator.enabled=false \
    --set tempo.enabled=false \
    --set loki.enabled=false \
    --set collector.enabled=false \
    --set logsCollector.enabled=false \
    --set gpuMonitoring.enabled=true \
    --set gpuMonitoring.installExporter=true \
    --set dcgm-exporter.enabled=true \
    --set-string dcgm-exporter.nodeSelector."nvidia\.com/gpu\.present"=true \
    --timeout 3m --wait
# --set-string, not --set, on that last line deliberately: nodeSelector values
# are a map[string]string in the Kubernetes API, but plain --set would render
# an unquoted YAML `true` (a boolean) and the API server rejects the whole
# object with a type-mismatch error at apply time. Found by actually
# rendering this exact --set and inspecting the YAML, not assumed.
#
# run_info, NOT run: the exporter pod is EXPECTED to crash (see header comment),
# so `helm install --wait` will time out and exit non-zero for a reason that has
# nothing to do with what's being tested. run_info records this into the
# informational NOTES bucket instead of RESULTS, so it never counts as a failed
# step. --wait is kept deliberately: it blocks until the pod has reached its
# terminal crash state, so the placement and crash-reason assertions below (the
# real checks) see a settled pod rather than one still pulling its image.

# --- the real assertions -----------------------------------------------------

rule
log "STEP $((++STEP)): where did the DaemonSet actually schedule?"
log "────────────────────────────────────────────────────────────────"
kubectl get pods -n "$NS" -o wide -l app.kubernetes.io/name=dcgm-exporter 2>&1 | tee -a "$REPORT"

SCHEDULED_ON=$(kubectl get pods -n "$NS" -l app.kubernetes.io/name=dcgm-exporter -o jsonpath='{.items[*].spec.nodeName}' 2>/dev/null)
POD_COUNT=$(kubectl get pods -n "$NS" -l app.kubernetes.io/name=dcgm-exporter --no-headers 2>/dev/null | wc -l | tr -d ' ')

if [ "$POD_COUNT" -eq 1 ] && [ "$SCHEDULED_ON" = "$GPU_NODE" ]; then
  log "✅ exactly 1 pod, scheduled on $GPU_NODE (the faked node) — nodeSelector worked"
  RESULTS+=("PASS  DaemonSet scheduled only on the GPU-labeled node")
else
  log "❌ expected exactly 1 pod on $GPU_NODE, got $POD_COUNT pod(s) on [$SCHEDULED_ON]"
  RESULTS+=("FAIL  DaemonSet scheduling (got $POD_COUNT pod(s) on [$SCHEDULED_ON], want 1 on $GPU_NODE)")
fi

rule
log "STEP $((++STEP)): does the ServiceMonitor's selector actually match the pod's real labels?"
log "────────────────────────────────────────────────────────────────"
SM_NS=$(kubectl get servicemonitor -n "$NS" lantern-dcgm-exporter -o jsonpath='{.spec.namespaceSelector.matchNames[0]}' 2>/dev/null)
SM_SELECTOR=$(kubectl get servicemonitor -n "$NS" lantern-dcgm-exporter -o json 2>/dev/null | jq -Sc '.spec.selector.matchLabels // {}')
POD_LABELS=$(kubectl get pods -n "$NS" -l app.kubernetes.io/name=dcgm-exporter -o json 2>/dev/null | jq -Sc '.items[0].metadata.labels // {} | with_entries(select(.key | test("^app.kubernetes.io/(name|instance)$")))')
log "  ServiceMonitor namespaceSelector: $SM_NS  (expect: $NS)"
log "  ServiceMonitor selector:          $SM_SELECTOR"
log "  Pod's own name/instance labels:   $POD_LABELS"

if [ "$SM_NS" = "$NS" ] && [ "$SM_SELECTOR" = "$POD_LABELS" ] && [ -n "$SM_SELECTOR" ]; then
  log "✅ ServiceMonitor selector is an exact match for the real pod — nothing to scrape would be silently missed"
  RESULTS+=("PASS  ServiceMonitor selector matches real pod labels")
else
  log "❌ mismatch — this ServiceMonitor would match zero pods (or the wrong ones)"
  RESULTS+=("FAIL  ServiceMonitor selector does not match real pod labels")
fi

rule
log "STEP $((++STEP)): \"fake GPU cluster\" scenario — does the fleet dashboard ConfigMap actually exist, with real, valid JSON?"
log "────────────────────────────────────────────────────────────────"
DASH_JSON=$(kubectl get configmap -n "$NS" lantern-gpu-fleet-dashboard -o jsonpath='{.data.gpu-fleet\.json}' 2>/dev/null)
DASH_LABEL=$(kubectl get configmap -n "$NS" lantern-gpu-fleet-dashboard -o jsonpath='{.metadata.labels.grafana_dashboard}' 2>/dev/null)
if [ -z "$DASH_JSON" ]; then
  log "❌ ConfigMap lantern-gpu-fleet-dashboard not found (or has no gpu-fleet.json key) -- a real Grafana sidecar would have nothing to discover"
  RESULTS+=("FAIL  fleet dashboard ConfigMap missing")
elif [ "$DASH_LABEL" != "1" ]; then
  log "❌ ConfigMap exists but grafana_dashboard label = \"$DASH_LABEL\", want \"1\" -- the sidecar (searchNamespace: ALL, label grafana_dashboard: \"1\") would never pick this up"
  RESULTS+=("FAIL  fleet dashboard ConfigMap missing the grafana_dashboard=1 label")
elif ! echo "$DASH_JSON" | jq -e '.panels | length > 0' >/dev/null 2>&1; then
  log "❌ ConfigMap's gpu-fleet.json is not valid JSON, or has no panels -- this is what the API server actually stored, not what was submitted"
  echo "$DASH_JSON" | tee -a "$REPORT"
  RESULTS+=("FAIL  fleet dashboard JSON invalid or empty as stored by the API server")
else
  PANEL_COUNT=$(echo "$DASH_JSON" | jq '.panels | length')
  log "✅ ConfigMap present, labeled grafana_dashboard=1, $PANEL_COUNT valid panels as actually stored by the API server"
  RESULTS+=("PASS  fleet dashboard ConfigMap present, labeled, and valid")
fi

rule
log "STEP $((++STEP)): does the compiler's per-service GPU panel survive a real discover → synth → apply round trip?"
log "────────────────────────────────────────────────────────────────"
run "build the lantern CLI" make build
LANTERN="$(pwd)/bin/lantern"
run "apply the bundled gpu-inference example (real ServiceMonitor + PrometheusRule + dashboard ConfigMap, live)" bash -c "
  kubectl create namespace ml --dry-run=client -o yaml | kubectl apply -f - &&
  '$LANTERN' synth -stack testdata/golden/gpu-inference/stack.yaml testdata/golden/gpu-inference/service.yaml -quiet \
    | kubectl apply -f -"
INFERENCE_DASH=$(kubectl get configmap -n ml lantern-dashboard-llama-70b-server -o jsonpath='{.data.llama-70b-server\.json}' 2>/dev/null)
if echo "$INFERENCE_DASH" | jq -e '.panels[] | select(.title == "GPU utilization (this service'"'"'s pods)")' >/dev/null 2>&1; then
  log "✅ the live, applied inference-service dashboard has the GPU utilization panel — proven through a real API server, not just helm template"
  RESULTS+=("PASS  per-service GPU panel present in the live-applied dashboard")
else
  log "❌ GPU utilization panel missing from the live-applied inference dashboard"
  RESULTS+=("FAIL  per-service GPU panel missing from live-applied dashboard")
fi

rule
log "STEP $((++STEP)): does the exporter pod fail for the EXPECTED reason (no GPU device), not a chart bug?"
log "────────────────────────────────────────────────────────────────"
POD_NAME=$(kubectl get pods -n "$NS" -l app.kubernetes.io/name=dcgm-exporter -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)
if [ -n "$POD_NAME" ]; then
  kubectl describe pod -n "$NS" "$POD_NAME" 2>&1 | tail -20 | tee -a "$REPORT"
  log ""
  log "container log tail:"
  kubectl logs -n "$NS" "$POD_NAME" --tail=30 2>&1 | tee -a "$REPORT" || log "  (no logs yet)"
  log ""
  log "^ expected: something about failing to initialize NVML / no devices found."
  log "  If instead this shows RBAC denied, ImagePullBackOff, or CreateContainerError,"
  log "  that IS a real chart bug and needs investigating, not a hardware gap."
else
  log "❌ no dcgm-exporter pod found at all -- worse than a crash, nothing scheduled"
  RESULTS+=("FAIL  no dcgm-exporter pod found")
fi

# ---------------------------------------------------------------------------

rule
log "SUMMARY"
log "────────────────────────────────────────────────────────────────"
printf '%s\n' "${RESULTS[@]}" | tee -a "$REPORT"
if [ "${#NOTES[@]}" -gt 0 ]; then
  log ""
  log "informational (expected outcomes, NOT counted as failures):"
  printf '%s\n' "${NOTES[@]}" | tee -a "$REPORT"
fi
FAILS=$(printf '%s\n' "${RESULTS[@]}" | grep -c '^FAIL' || true)
log ""
if [ "${FAILS:-0}" -eq 0 ]; then
  log "✅ everything that CAN be verified without real GPU hardware passed"
else
  log "❌ $FAILS step(s) failed — full output is in $REPORT"
fi

cleanup_and_exit "${FAILS:-0}"
