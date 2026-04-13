#!/usr/bin/env bash
# deploy.sh — World 2 deployment CLI
#
# Usage:
#   ./deploy.sh hub   <kubeconfig> [cluster-name]
#   ./deploy.sh agent <kubeconfig>... [--name name1,name2,...]
#
# Examples:
#   ./deploy.sh hub ~/Downloads/sgn3/kubeconfig.yaml
#   ./deploy.sh hub ~/Downloads/sgn3/kubeconfig.yaml sgn3
#
#   ./deploy.sh agent ~/Downloads/sql97/kubeconfig.yaml
#   ./deploy.sh agent ~/Downloads/sql97/kubeconfig.yaml ~/Downloads/sql108/kubeconfig.yaml
#
# cluster-name: used as vm_id tag in InfluxDB.
#   Defaults to the parent directory name of the kubeconfig file.
#   e.g. ~/Downloads/sql108/kubeconfig.yaml → sql108
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

require() {
  for cmd in "$@"; do
    command -v "$cmd" &>/dev/null || die "'$cmd' is required but not installed"
  done
}

resolve_path() { eval echo "$1"; }

# Set KUBECONFIG to a single file and verify the cluster is reachable
use_kubeconfig() {
  local kubeconfig
  kubeconfig=$(resolve_path "$1")
  [[ -f "$kubeconfig" ]] || die "kubeconfig not found: $kubeconfig"
  export KUBECONFIG="$kubeconfig"
  kubectl cluster-info --request-timeout=5s >/dev/null \
    || die "cannot reach cluster at $kubeconfig"
  log "connected to cluster via $kubeconfig"
}

# Derive cluster name from kubeconfig path: ~/Downloads/sql108/kubeconfig.yaml → sql108
derive_cluster_name() {
  local kubeconfig
  kubeconfig=$(resolve_path "$1")
  basename "$(dirname "$kubeconfig")"
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
  local kubeconfig="$1"
  local cluster_name="${2:-$(derive_cluster_name "$kubeconfig")}"

  use_kubeconfig "$kubeconfig"
  log "=== deploying hub (cluster-name=$cluster_name) ==="

  kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $NAMESPACE
EOF

  local node_ip
  node_ip=$(get_node_ip)
  [[ -z "$node_ip" ]] && die "could not determine node IP"
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
  _apply_agent "$cluster_name" "$node_ip"

  # Save node IP so subsequent 'deploy.sh agent' calls can find it
  echo "$node_ip" > .kafka-node-ip

  log ""
  log "=== hub ready ==="
  log "Kafka (for agents): $node_ip:$KAFKA_EXTERNAL_PORT"
  log "API:                http://$node_ip:30080"
  log "Grafana:            http://$node_ip:30300  (admin / admin)"
  log ""
  log "Next: ./deploy.sh agent <kubeconfig>..."
}

# ─── agent ────────────────────────────────────────────────────────────────────

deploy_agents() {
  [[ -f .kafka-node-ip ]] \
    || die ".kafka-node-ip not found — run './deploy.sh hub' first"
  local kafka_ip
  kafka_ip=$(cat .kafka-node-ip)

  for kubeconfig in "$@"; do
    local cluster_name
    cluster_name=$(derive_cluster_name "$kubeconfig")
    use_kubeconfig "$kubeconfig"
    log "=== deploying agent (cluster-name=$cluster_name, kafka=$kafka_ip:$KAFKA_EXTERNAL_PORT) ==="
    _apply_agent "$cluster_name" "$kafka_ip"
    log "agent deployed on $cluster_name"
  done
}

_apply_agent() {
  local cluster_name="$1"
  local kafka_ip="$2"

  kubectl apply -f k8s/agent/serviceaccount.yaml

  sed \
    -e "s/REPLACE_CLUSTER_NAME/$cluster_name/g" \
    -e "s/REPLACE_KAFKA_IP/$kafka_ip/g" \
    -e "s|REGISTRY/|${REGISTRY}/|g" \
    k8s/agent/deployment.yaml \
    | kubectl apply -f -
}

# ─── entrypoint ───────────────────────────────────────────────────────────────

require kubectl

case "${1:-}" in
  hub)
    [[ $# -ge 2 ]] || usage
    deploy_hub "$2" "${3:-}"
    ;;
  agent)
    [[ $# -ge 2 ]] || usage
    shift
    deploy_agents "$@"
    ;;
  *)
    usage
    ;;
esac
