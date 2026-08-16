#!/usr/bin/env bash
#
# Read-only safety gate for installing the Lantern quickstart stack onto a
# cluster that might already have monitoring on it.
#
# Why this exists: Helm's actual behavior on a pre-existing CRD is to SKIP
# installing it and print a warning — it does not error out, and it does not
# overwrite. That sounds safe, but it means `helm install` can succeed while
# quietly leaving the OpenTelemetry Operator or Prometheus Operator running
# against whatever CRD schema was already on the cluster — possibly owned by
# a different release, possibly an older version. The failure shows up later,
# confusingly, as a controller that won't reconcile — not as an install-time
# error. This script surfaces that risk BEFORE `helm install` runs, while
# everything is still just a read.
#
# It touches nothing. Every command below is `get`/`describe`/`auth can-i`.
#
# An absent Helm ownership annotation on a CRD is NOT proof of a foreign
# install by itself — Helm's crds/ mechanism never annotates CRDs, including
# this release's own leftover residue from an earlier aborted attempt. When
# the annotation is missing, this script additionally counts existing custom
# resource instances under that CRD: zero means nothing to conflict with
# regardless of who owns the definition; a real count means something is
# actually in use and blocks as before. Found and fixed after this exact
# ambiguity produced a false BLOCK on a genuinely clean, correct install.
#
# Exit codes:
#   0  clean, or warnings only
#   1  missing tools / cannot reach the cluster
#   2  a BLOCKing conflict was found — do not run the quickstart subcharts
#      as-is; see the recommendation printed for each one
#
# Usage:
#   ./scripts/preflight-check.sh                  # check the default namespace
#   ./scripts/preflight-check.sh -n observability  # explicit namespace
#   ./scripts/preflight-check.sh -r lantern        # explicit intended release name
#   ./scripts/preflight-check.sh --strict          # treat warnings as blocking too

set -uo pipefail

NAMESPACE=observability
RELEASE=lantern
STRICT=0

while [ $# -gt 0 ]; do
  case "$1" in
    -n) NAMESPACE="$2"; shift 2 ;;
    -r) RELEASE="$2"; shift 2 ;;
    --strict) STRICT=1; shift ;;
    *) echo "unknown flag: $1" >&2; exit 1 ;;
  esac
done

PASS=0; WARN=0; BLOCK=0
say()   { printf '%s\n' "$*"; }
pass()  { PASS=$((PASS+1));  printf '  ✅ %s\n' "$*"; }
warn()  { WARN=$((WARN+1));  printf '  ⚠️  %s\n' "$*"; }
block() { BLOCK=$((BLOCK+1)); printf '  ❌ %s\n' "$*"; }

say "════════════════════════════════════════════════════════════════"
say "Lantern preflight check — $(date)"
say "target namespace: $NAMESPACE   intended release name: $RELEASE"
say "════════════════════════════════════════════════════════════════"

# ---------------------------------------------------------------------------
# 0. tools and cluster reachability
# ---------------------------------------------------------------------------
say ""
say "0. tools"
for t in kubectl jq; do
  if command -v "$t" >/dev/null 2>&1; then
    pass "$t present"
  else
    printf '  ❌ %s NOT FOUND — install it and re-run (brew install %s)\n' "$t" "$t"
    exit 1
  fi
done

if ! kubectl cluster-info >/dev/null 2>&1; then
  say "  ❌ cannot reach a cluster with the current kubeconfig context."
  kubectl config current-context 2>&1 | sed 's/^/     current context: /'
  exit 1
fi
CTX=$(kubectl config current-context 2>/dev/null)
pass "cluster reachable (context: $CTX)"
say ""
say "  You are about to check, and possibly install into, this exact context."
say "  If that is not the cluster you meant, stop and switch context now."

# ---------------------------------------------------------------------------
# 1. RBAC — can we actually do the install steps this implies?
# ---------------------------------------------------------------------------
say ""
say "1. permissions"
for verb_resource in "create customresourcedefinitions" "create clusterrolebindings" "create namespaces"; do
  # shellcheck disable=SC2086
  if kubectl auth can-i $verb_resource >/dev/null 2>&1; then
    pass "can $verb_resource"
  else
    warn "cannot $verb_resource — the quickstart install will fail here; bring-your-own may still work if these already exist"
  fi
done

# ---------------------------------------------------------------------------
# 2. CRD ownership — the check that actually matters
# ---------------------------------------------------------------------------
say ""
say "2. CRDs the quickstart subcharts would install"
say "   (a CRD Helm doesn't own it will SKIP silently, not error — that's the risk)"

# CRDs owned by kube-prometheus-stack (via the Prometheus Operator) and by the
# OpenTelemetry Operator. Not every version of every operator ships every one
# of these; a CRD this script doesn't know about simply isn't checked, which
# is a gap, not a false alarm.
PROM_CRDS="alertmanagers.monitoring.coreos.com alertmanagerconfigs.monitoring.coreos.com podmonitors.monitoring.coreos.com probes.monitoring.coreos.com prometheuses.monitoring.coreos.com prometheusrules.monitoring.coreos.com scrapeconfigs.monitoring.coreos.com servicemonitors.monitoring.coreos.com thanosrulers.monitoring.coreos.com"
OTEL_CRDS="instrumentations.opentelemetry.io opentelemetrycollectors.opentelemetry.io opampbridges.opentelemetry.io targetallocators.opentelemetry.io"

check_crd_group() {
  local group_name="$1"; shift
  local any_found=0
  for crd in "$@"; do
    if ! kubectl get crd "$crd" >/dev/null 2>&1; then
      continue
    fi
    any_found=1
    local owner_name owner_ns managed_by versions
    owner_name=$(kubectl get crd "$crd" -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null)
    owner_ns=$(kubectl get crd "$crd" -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-namespace}' 2>/dev/null)
    managed_by=$(kubectl get crd "$crd" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}' 2>/dev/null)
    versions=$(kubectl get crd "$crd" -o jsonpath='{.spec.versions[*].name}' 2>/dev/null)

    if [ "$owner_name" = "$RELEASE" ] && [ "$owner_ns" = "$NAMESPACE" ]; then
      pass "$crd already owned by this release ($RELEASE/$NAMESPACE) — this is just an upgrade"
      continue
    fi

    if [ -z "$owner_name" ] && [ -z "$managed_by" ]; then
      # No ownership annotation/label is NOT proof of a foreign install on
      # its own: Helm's crds/ mechanism (how kube-prometheus-stack and the
      # OTel Operator ship these) never stamps one, even for a chart's own
      # CRDs — including leftover residue from THIS release's own earlier,
      # aborted attempt. The only reliable signal is whether the CRD already
      # has real custom resources under it: nothing to lose, or something
      # real that could get clobbered.
      local resource group instance_output instance_count
      resource="${crd%%.*}"
      group="${crd#*.}"
      instance_output=$(kubectl get "$resource.$group" --all-namespaces -o name 2>&1)
      if [ $? -ne 0 ]; then
        instance_count=-1
      elif [ -z "$instance_output" ]; then
        instance_count=0
      else
        instance_count=$(printf '%s\n' "$instance_output" | wc -l | tr -d ' ')
      fi

      if [ "$instance_count" -eq 0 ] 2>/dev/null; then
        pass "$crd exists with no Helm ownership annotation, but zero custom resources under it — nothing to conflict with"
      elif [ "$instance_count" -lt 0 ] 2>/dev/null; then
        block "$crd exists and its custom resources could not be listed (RBAC or API error) — cannot confirm it's safe. served versions: [$versions]"
        say "     -> re-run with a kubeconfig that can list $group resources, or investigate manually."
      else
        block "$crd exists, installed by something other than Helm (raw manifest, an operator lifecycle manager, etc), and already has $instance_count custom resource(s) under it. served versions: [$versions]"
        say "     -> Helm will SKIP this CRD rather than manage it. The controller Lantern installs"
        say "        will run against whatever schema is already there. Confirm compatibility before"
        say "        proceeding, or point Lantern at the existing controller instead of installing a new one."
      fi
    else
      block "$crd exists, owned by Helm release '${owner_name:-unknown}' in namespace '${owner_ns:-unknown}' — NOT this install"
      say "     -> installing $group_name here will not touch this CRD (Helm skips it), but the new"
      say "        controller and the existing one will both reconcile against a SHARED, single"
      say "        cluster-wide CRD. Recommended: disable this subchart and point"
      say "        backends.* in stack.yaml at the existing installation instead."
    fi
  done
  if [ "$any_found" -eq 0 ]; then
    pass "$group_name: no matching CRDs found on the cluster — clean to install"
  fi
}

say ""
say "  Prometheus Operator / kube-prometheus-stack:"
check_crd_group "kube-prometheus-stack" $PROM_CRDS
say ""
say "  OpenTelemetry Operator:"
check_crd_group "opentelemetry-operator" $OTEL_CRDS

# ---------------------------------------------------------------------------
# 3. webhooks — informational. Kubernetes itself rejects an exact name clash
#    on create, so this is a heads-up, not a blocking check.
# ---------------------------------------------------------------------------
say ""
say "3. admission webhooks (informational — Kubernetes rejects exact-name clashes on its own)"
WH=$(kubectl get validatingwebhookconfigurations,mutatingwebhookconfigurations -o name 2>/dev/null \
  | grep -Ei 'opentelemetry|prometheus' || true)
if [ -z "$WH" ]; then
  pass "no existing opentelemetry/prometheus webhook configurations found"
else
  warn "existing webhook configuration(s) found — review before installing:"
  echo "$WH" | sed 's/^/     /'
fi

# ---------------------------------------------------------------------------
# 4. workload scan — is there already something that looks like a monitoring
#    stack running, even without owning any of the CRDs above?
# ---------------------------------------------------------------------------
say ""
say "4. existing monitoring-shaped workloads (informational — human judgment call)"
FOUND=$(kubectl get deploy,statefulset -A -o json 2>/dev/null \
  | jq -r '.items[] | select(.spec.template.spec.containers[].image
      | test("grafana/grafana|prom/prometheus|prom/alertmanager|grafana/tempo|grafana/loki"; "i"))
      | "\(.metadata.namespace)/\(.metadata.name)"' 2>/dev/null | sort -u)
if [ -z "$FOUND" ]; then
  pass "nothing matching grafana/prometheus/alertmanager/tempo/loki images found in any namespace"
else
  warn "workload(s) that look like an existing monitoring stack:"
  echo "$FOUND" | sed 's/^/     /'
  say "     -> if these are real, prefer bring-your-own (values.yaml) over the quickstart"
  say "        subcharts, and point stack.yaml at them instead of installing new ones."
fi

# ---------------------------------------------------------------------------
# 5. node capacity
# ---------------------------------------------------------------------------
say ""
say "5. node capacity"
# Recommended minimums for the quickstart stack — see README.md "Resource
# requirements". These are requests-based estimates, not yet measured.
MIN_CPU_MILLI=1500
MIN_MEM_MI=2500
REC_CPU_MILLI=4000
REC_MEM_MI=8000

ALLOC_CPU=$(kubectl get nodes -o json 2>/dev/null | jq -r '[.items[].status.allocatable.cpu] | map(if test("m$") then (.[:-1] | tonumber) else (tonumber * 1000) end) | add')
ALLOC_MEM_KI=$(kubectl get nodes -o json 2>/dev/null | jq -r '[.items[].status.allocatable.memory] | map(if test("Ki$") then (.[:-2] | tonumber) elif test("Mi$") then (.[:-2] | tonumber * 1024) elif test("Gi$") then (.[:-2] | tonumber * 1024 * 1024) else (tonumber / 1024) end) | add')
REQ_CPU=$(kubectl get pods -A -o json 2>/dev/null | jq -r '[.items[].spec.containers[].resources.requests.cpu? // "0"] | map(if test("m$") then (.[:-1] | tonumber) else (tonumber * 1000) end) | add')
REQ_MEM_KI=$(kubectl get pods -A -o json 2>/dev/null | jq -r '[.items[].spec.containers[].resources.requests.memory? // "0"] | map(if test("Ki$") then (.[:-2] | tonumber) elif test("Mi$") then (.[:-2] | tonumber * 1024) elif test("Gi$") then (.[:-2] | tonumber * 1024 * 1024) else 0 end) | add')

if [ -n "$ALLOC_CPU" ] && [ -n "$ALLOC_MEM_KI" ] && [ "$ALLOC_CPU" != "null" ]; then
  FREE_CPU=$((ALLOC_CPU - ${REQ_CPU:-0}))
  FREE_MEM_MI=$(( (ALLOC_MEM_KI - ${REQ_MEM_KI:-0}) / 1024 ))
  say "  allocatable: ${ALLOC_CPU}m CPU, $((ALLOC_MEM_KI / 1024))Mi RAM across the cluster"
  say "  already requested: ${REQ_CPU:-0}m CPU, $(( ${REQ_MEM_KI:-0} / 1024 ))Mi RAM"
  say "  free (by requests, not actual usage): ${FREE_CPU}m CPU, ${FREE_MEM_MI}Mi RAM"
  say "  (node-exporter adds ~50m CPU / 50Mi RAM PER NODE on top of this, not counted above)"

  if [ "$FREE_CPU" -lt "$MIN_CPU_MILLI" ] || [ "$FREE_MEM_MI" -lt "$MIN_MEM_MI" ]; then
    block "free capacity is below the quickstart minimum (${MIN_CPU_MILLI}m CPU / ${MIN_MEM_MI}Mi RAM)"
    say "     -> installing the full quickstart stack here risks Pending/OOMKilled pods."
    say "        Start with --set loki.enabled=false --set tempo.enabled=false, or free up capacity first."
  elif [ "$FREE_CPU" -lt "$REC_CPU_MILLI" ] || [ "$FREE_MEM_MI" -lt "$REC_MEM_MI" ]; then
    warn "free capacity is below the recommended level (${REC_CPU_MILLI}m CPU / ${REC_MEM_MI}Mi RAM) — should run, may be tight under load"
  else
    pass "free capacity comfortably covers the quickstart stack"
  fi
else
  warn "could not compute node capacity (jq parse issue or empty cluster) — check manually with 'kubectl describe nodes'"
fi

# ---------------------------------------------------------------------------
# summary
# ---------------------------------------------------------------------------
say ""
say "════════════════════════════════════════════════════════════════"
say "SUMMARY: $PASS pass, $WARN warn, $BLOCK block"
say "════════════════════════════════════════════════════════════════"

if [ "$BLOCK" -gt 0 ]; then
  say ""
  say "❌ Do not run the quickstart install as-is. Fix the blocking items above —"
  say "   most commonly this means disabling one subchart and pointing stack.yaml"
  say "   at what already exists (see docs/adopting-an-existing-cluster.md)."
  exit 2
fi

if [ "$WARN" -gt 0 ] && [ "$STRICT" -eq 1 ]; then
  say ""
  say "❌ warnings present and --strict was given."
  exit 2
fi

if [ "$WARN" -gt 0 ]; then
  say ""
  say "⚠️  clean enough to proceed, but read the warnings above first."
  exit 0
fi

say ""
say "✅ clean. Safe to proceed with the quickstart install."
exit 0
