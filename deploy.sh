#!/usr/bin/env bash
# deploy.sh — World 2 deployment CLI
#
# Usage:
#   ./deploy.sh hub    [hub-kubeconfig] [cluster-name]   deploy central stack
#   ./deploy.sh agent  <agent-kubeconfig>...             deploy agent on one or more clusters
#   ./deploy.sh undeploy hub   [hub-kubeconfig]          tear down hub stack
#   ./deploy.sh undeploy agent <agent-kubeconfig>...     remove agent from clusters
#
# hub-kubeconfig is optional if $KUBECONFIG is already exported.
# For 'agent', the hub cluster is resolved interactively (prompts if unclear).
#
# Examples:
#   ./deploy.sh hub   ~/Downloads/sgn3/kubeconfig.yaml
#   ./deploy.sh agent ~/Downloads/sql97/kubeconfig.yaml \
#                     ~/Downloads/sql108/kubeconfig.yaml
#
#   ./deploy.sh undeploy hub
#   ./deploy.sh undeploy agent ~/Downloads/sql97/kubeconfig.yaml
#
# Cluster name is derived from the kubeconfig filename (sgn3.yaml → sgn3)
# or parent directory (sgn3/kubeconfig.yaml → sgn3).
#
# Requirements: kubectl

set -euo pipefail

REGISTRY="${REGISTRY:-ghcr.io/siqiliu18}"
NAMESPACE="monitoring"
KAFKA_EXTERNAL_PORT="30094"

# ─── helpers ──────────────────────────────────────────────────────────────────

usage() {
  grep '^#' "$0" | grep -v '#!/' | sed 's/^# \?//'
  exit 1
}

log() { echo "[deploy] $*"; }
die() { echo "[error]  $*" >&2; exit 1; }

confirm_hub() {
  local action="$1"   # "deploy" or "undeploy"
  local node_ip="$2"
  echo ""
  echo "  action    : $action hub"
  echo "  kubeconfig: $KUBECONFIG"
  echo "  node IP   : $node_ip"
  echo ""
  printf "Proceed? [y/N] "
  read -r answer
  [[ "$answer" =~ ^[Yy]$ ]] || die "aborted"
}

require() {
  for cmd in "$@"; do
    command -v "$cmd" &>/dev/null || die "'$cmd' is required but not installed"
  done
}

resolve_path() { eval echo "$1"; }

# Set KUBECONFIG to a single file and verify the cluster is reachable.
# If no path is given, falls back to the already-exported $KUBECONFIG.
use_kubeconfig() {
  if [[ -z "${1:-}" ]]; then
    [[ -n "${KUBECONFIG:-}" ]] || die "no kubeconfig provided and \$KUBECONFIG is not set"
    kubectl cluster-info --request-timeout=5s >/dev/null \
      || die "cannot reach cluster at $KUBECONFIG"
    log "connected to cluster via \$KUBECONFIG ($KUBECONFIG)"
    return
  fi
  local kubeconfig
  kubeconfig=$(resolve_path "$1")
  [[ -f "$kubeconfig" ]] || die "kubeconfig not found: $kubeconfig"
  export KUBECONFIG="$kubeconfig"
  kubectl cluster-info --request-timeout=5s >/dev/null \
    || die "cannot reach cluster at $kubeconfig"
  log "connected to cluster via $kubeconfig"
}

# Derive cluster name from kubeconfig path.
# 1. Use filename if it's not a generic name (e.g. sgn3.yaml → sgn3, sql108.yaml → sql108)
# 2. Fall back to parent directory name (e.g. Downloads/sql108/kubeconfig.yaml → sql108)
derive_cluster_name() {
  local kubeconfig
  kubeconfig=$(resolve_path "$1")
  local filename
  filename=$(basename "$kubeconfig" .yaml)
  # Generic filenames that don't identify a cluster
  case "$filename" in
    kubeconfig|config|kube-config|kubeconfig_*)
      basename "$(dirname "$kubeconfig")"
      ;;
    *)
      echo "$filename"
      ;;
  esac
}

# Get node IP for NodePort access (ExternalIP preferred, falls back to InternalIP)
get_node_ip() {
  local ip
  ip=$(kubectl get nodes \
    -o jsonpath='{.items[0].status.addresses[?(@.type=="ExternalIP")].address}' 2>/dev/null || true)
  if [[ -z "$ip" ]]; then
    ip=$(kubectl get nodes \
      -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' 2>/dev/null || true)
  fi
  echo "$ip"
}

# ─── hub ──────────────────────────────────────────────────────────────────────

deploy_hub() {
  local kubeconfig="${1:-}"
  local cluster_name="${2:-$(derive_cluster_name "${kubeconfig:-$KUBECONFIG}")}"

  use_kubeconfig "$kubeconfig"

  local node_ip
  node_ip=$(get_node_ip)
  [[ -z "$node_ip" ]] && die "could not determine node IP"

  confirm_hub "deploy" "$node_ip"
  log "=== deploying hub (cluster-name=$cluster_name) ==="

  kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $NAMESPACE
EOF

  log "node IP: $node_ip"

  log "deploying Kafka..."
  kubectl apply -f k8s/hub/kafka/service.yaml
  sed "s/REPLACE_KAFKA_LB_IP/$node_ip/g" k8s/hub/kafka/statefulset.yaml \
    | kubectl apply -f -

  log "deploying InfluxDB..."
  kubectl apply -f k8s/hub/influxdb/

  log "deploying consumer..."
  sed "s|REGISTRY/|${REGISTRY}/|g" k8s/hub/consumer/deployment.yaml \
    | kubectl apply -f -

  log "deploying API..."
  sed "s|REGISTRY/|${REGISTRY}/|g" k8s/hub/api/deployment.yaml \
    | kubectl apply -f -

  log "deploying Grafana..."
  kubectl apply -f k8s/hub/grafana/

  log "deploying agent on hub cluster..."
  # Hub agent is co-located with Kafka — use internal PLAINTEXT listener (port 9092).
  # External NodePort (9094→30094) requires hairpin NAT which bare-metal clusters often lack.
  _apply_agent "$cluster_name" "kafka-0.kafka-headless.monitoring.svc.cluster.local" "9092"

  log ""
  log "=== hub ready ==="
  log "Kafka (for agents): $node_ip:$KAFKA_EXTERNAL_PORT"
  log "API:                http://$node_ip:30080"
  log "Grafana:            http://$node_ip:30300  (admin / admin)"
  log ""
  log "Next: ./deploy.sh agent <agent-kubeconfig>..."
}

# ─── agent ────────────────────────────────────────────────────────────────────

# Interactively resolve which kubeconfig points at the hub cluster.
# - If $KUBECONFIG is set, ask whether it is the hub; let user override if not.
# - If $KUBECONFIG is unset, prompt for a path.
# Prints "<confirmed|unconfirmed>:<path>" to stdout.
# confirmed  = user said "y" to the hub question — no second prompt needed
# unconfirmed = user typed a path manually — caller should show Proceed? prompt
resolve_hub_kubeconfig() {
  local hub_kc=""

  if [[ -n "${KUBECONFIG:-}" ]]; then
    echo "" >&2
    echo "Hub cluster (where Kafka runs):" >&2
    echo "  current \$KUBECONFIG: $KUBECONFIG" >&2
    echo "" >&2
    printf "Is this where the hub is running? [y/N] " >&2
    read -r answer
    if [[ "$answer" =~ ^[Yy]$ ]]; then
      echo "confirmed:$KUBECONFIG"
      return
    fi
  fi

  printf "Hub kubeconfig path: " >&2
  read -r hub_kc
  hub_kc=$(eval echo "$hub_kc")   # expand ~ in the path
  echo "unconfirmed:$hub_kc"
}

deploy_agents() {
  local resolved
  resolved=$(resolve_hub_kubeconfig)
  local confirmed="${resolved%%:*}"       # "confirmed" or "unconfirmed"
  local hub_kubeconfig="${resolved#*:}"   # everything after the first ":"

  use_kubeconfig "$hub_kubeconfig"
  local kafka_ip
  kafka_ip=$(get_node_ip)
  [[ -z "$kafka_ip" ]] && die "could not determine hub node IP from $hub_kubeconfig"

  # Build display list of agent target names
  local targets=()
  for kc in "$@"; do
    targets+=("$(derive_cluster_name "$kc")")
  done
  local targets_str
  targets_str=$(IFS=", "; echo "${targets[*]}")

  echo ""
  echo "  hub kubeconfig: $hub_kubeconfig"
  echo "  kafka IP      : $kafka_ip:$KAFKA_EXTERNAL_PORT"
  echo "  agent targets : $targets_str"
  echo ""
  if [[ "$confirmed" == "unconfirmed" ]]; then
    printf "Proceed? [y/N] "
    read -r answer
    [[ "$answer" =~ ^[Yy]$ ]] || die "aborted"
  fi

  for kubeconfig in "$@"; do
    local cluster_name
    cluster_name=$(derive_cluster_name "$kubeconfig")
    use_kubeconfig "$kubeconfig"
    log "=== deploying agent (cluster-name=$cluster_name) ==="
    _apply_agent "$cluster_name" "$kafka_ip"
    log "agent deployed on $cluster_name"
  done
}

_apply_agent() {
  local cluster_name="$1"
  local kafka_ip="$2"
  local kafka_port="${3:-$KAFKA_EXTERNAL_PORT}"   # default 30094 (NodePort); hub agent passes 9092 (PLAINTEXT)

  kubectl apply -f k8s/agent/serviceaccount.yaml

  sed \
    -e "s/REPLACE_CLUSTER_NAME/$cluster_name/g" \
    -e "s/REPLACE_KAFKA_IP:9094/$kafka_ip:$kafka_port/g" \
    -e "s|REGISTRY/|${REGISTRY}/|g" \
    k8s/agent/deployment.yaml \
    | kubectl apply -f -
}

# ─── undeploy ─────────────────────────────────────────────────────────────────

undeploy_hub() {
  local kubeconfig="${1:-}"
  use_kubeconfig "$kubeconfig"

  local node_ip
  node_ip=$(get_node_ip)

  confirm_hub "undeploy" "$node_ip"
  log "=== removing hub stack ==="

  # Deleting the namespace removes all namespaced resources in one shot
  # (Kafka, InfluxDB, consumer, API, Grafana, agent)
  kubectl delete namespace "$NAMESPACE" --ignore-not-found

  # ClusterRole and ClusterRoleBinding are cluster-scoped — not deleted with namespace
  kubectl delete clusterrole    vm-metrics-agent --ignore-not-found
  kubectl delete clusterrolebinding vm-metrics-agent --ignore-not-found

  log "hub stack removed"
}

undeploy_agents() {
  for kubeconfig in "$@"; do
    use_kubeconfig "$kubeconfig"
    local cluster_name
    cluster_name=$(derive_cluster_name "$kubeconfig")
    log "=== removing agent from $cluster_name ==="
    kubectl delete namespace "$NAMESPACE" --ignore-not-found
    kubectl delete clusterrole    vm-metrics-agent --ignore-not-found
    kubectl delete clusterrolebinding vm-metrics-agent --ignore-not-found
    log "agent removed from $cluster_name"
  done
}

# ─── entrypoint ───────────────────────────────────────────────────────────────

require kubectl

case "${1:-}" in
  hub)
    deploy_hub "${2:-}" "${3:-}"   # kubeconfig optional — falls back to $KUBECONFIG
    ;;
  agent)
    [[ $# -ge 2 ]] || usage
    shift
    deploy_agents "$@"
    ;;
  undeploy)
    case "${2:-}" in
      hub)
        undeploy_hub "${3:-}"   # kubeconfig optional — falls back to $KUBECONFIG
        ;;
      agent)
        [[ $# -ge 3 ]] || usage
        shift 2
        undeploy_agents "$@"
        ;;
      *)
        usage
        ;;
    esac
    ;;
  *)
    usage
    ;;
esac
