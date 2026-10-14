#!/usr/bin/env bash
# GD-04: lose a Karpenter node (or drain a whole zone) and let PDBs, topology spread and Karpenter respond.
# shellcheck source-path=SCRIPTDIR
set -euo pipefail
# shellcheck source=lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

usage() {
  cat <<EOF
Usage: $GD_NAME --context CTX --namespace NS (--node NAME | --zone ZONE) --mode MODE [--timeout SECONDS]
       $GD_NAME --context CTX --namespace NS --rollback [--node NAME | --zone ZONE]

Modes:
  drain               cordon, then kubectl drain each node in turn (respects PodDisruptionBudgets)
  delete-nodeclaim    delete the Karpenter NodeClaim(s): Karpenter cordons, drains and terminates
  terminate-instance  terminate the EC2 instance(s) at once, without draining (needs AWS_PROFILE and
                      TASUKI_ACCOUNT_ID, checked like 'make aws-*')

Only Karpenter nodes (label karpenter.sh/nodepool) are ever targeted; system nodes are refused. Nodes are
shared by tasuki-dev, tasuki-prod and monitoring: if a target node runs tasuki-prod pods, the script refuses
unless --namespace tasuki-prod and ALLOW_PROD=yes (a declared prod drill). It warns when Prometheus,
Alertmanager or Grafana run on a target. --timeout bounds each drain (default 600).

Undo: --rollback uncordons the nodes this script cordoned (saved in its state file), or the --node/--zone
given. Terminated or deleted nodes need no undo: Karpenter replaces the capacity.

$GD_COMMON_USAGE
EOF
}

node=""
zone=""
mode=""
rollback=0
timeout=600
while [ $# -gt 0 ]; do
  gd_common_flag "$@"
  if [ "$GD_SHIFT" -gt 0 ]; then
    shift "$GD_SHIFT"
    continue
  fi
  case "$1" in
    --node)
      gd_need_value "$1" "${2:-}"
      node="$2"
      shift 2
      ;;
    --zone)
      gd_need_value "$1" "${2:-}"
      zone="$2"
      shift 2
      ;;
    --mode)
      gd_need_value "$1" "${2:-}"
      mode="$2"
      shift 2
      ;;
    --timeout)
      gd_need_value "$1" "${2:-}"
      timeout="$2"
      shift 2
      ;;
    --rollback)
      rollback=1
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *) gd_usage_error "unknown argument: $1" ;;
  esac
done
if [ -n "$node" ] && [ -n "$zone" ]; then gd_usage_error "give --node or --zone, not both"; fi
if [ "$rollback" = 0 ]; then
  [ -n "$node$zone" ] || gd_usage_error "--node or --zone is required"
  case "$mode" in drain | delete-nodeclaim | terminate-instance) ;; *) gd_usage_error "--mode is required: drain, delete-nodeclaim or terminate-instance" ;; esac
fi
case "$timeout" in '' | *[!0-9]*) gd_usage_error "--timeout must be a whole number of seconds" ;; esac

gd_require_target
gd_require_tools jq
state_file="$(gd_state_dir)/node-loss-cordoned"

if [ "$rollback" = 1 ]; then
  if [ -n "$node" ]; then
    nodes="$node"
  elif [ -n "$zone" ]; then
    nodes="$(kc get nodes -l "topology.kubernetes.io/zone=$zone,karpenter.sh/nodepool" -o name | sed 's|^node/||' | tr '\n' ' ')"
  else
    nodes="$(tr '\n' ' ' <"$state_file" 2>/dev/null || true)"
  fi
  [ -n "${nodes// /}" ] || gd_die "nothing to uncordon (no --node/--zone, and no saved list in $state_file)"
  gd_plan "uncordon: $nodes"
  gd_confirm
  for n in $nodes; do
    if kc get node "$n" >/dev/null 2>&1; then kc uncordon "$n"; else gd_say "$n no longer exists (replaced); skipping"; fi
  done
  rm -f "$state_file"
  gd_record rollback "uncordoned $nodes"
  exit 0
fi

# Targets: Karpenter nodes only.
if [ -n "$node" ]; then
  labels="$(kc get node "$node" -o json | jq -r '.metadata.labels')" || gd_die "node $node not found"
  printf '%s' "$labels" | jq -e 'has("karpenter.sh/nodepool") and .["node-pool"] != "system"' >/dev/null ||
    gd_die "$node is not a Karpenter worker node (system nodes are never targeted)"
  nodes="$node"
else
  nodes="$(kc get nodes -l "topology.kubernetes.io/zone=$zone,karpenter.sh/nodepool" -o name | sed 's|^node/||' | tr '\n' ' ')"
  [ -n "${nodes// /}" ] || gd_die "no Karpenter nodes in zone $zone"
  zones_left="$(kc get nodes -l karpenter.sh/nodepool -o json |
    jq -r --arg z "$zone" '[.items[].metadata.labels["topology.kubernetes.io/zone"]] | unique | map(select(. != $z)) | length')"
  [ "$zones_left" -ge 1 ] || gd_die "zone $zone holds every Karpenter node; the drill needs another zone to move to"
fi

# Who is on those nodes: the environments and monitoring share them.
pods_on() { kc get pods -A --field-selector "spec.nodeName=$1" -o json | jq -r '.items[] | "\(.metadata.namespace)/\(.metadata.name)"'; }
prod_pods=""
monitoring_pods=""
summary=""
for n in $nodes; do
  list="$(pods_on "$n")"
  found="$(printf '%s\n' "$list" | grep '^tasuki-prod/' || true)"
  if [ -n "$found" ]; then prod_pods="$prod_pods$found"$'\n'; fi
  found="$(printf '%s\n' "$list" | grep -E '^monitoring/(prometheus|alertmanager|kube-prometheus-stack-grafana)' || true)"
  if [ -n "$found" ]; then monitoring_pods="$monitoring_pods$found"$'\n'; fi
  summary="$summary $n: $(printf '%s\n' "$list" | grep -c . || true) pods;"
done
if [ -n "$prod_pods" ] && [ "$GD_NAMESPACE" != "tasuki-prod" ]; then
  gd_die "these nodes run tasuki-prod pods, so this drill would touch prod:
$prod_pods
Re-run with --namespace tasuki-prod and ALLOW_PROD=yes inside a scheduled window, or choose another node."
fi
if [ -n "$monitoring_pods" ]; then
  gd_say "WARNING: observability runs on a target node, expect a gap in metrics or alerts:"
  printf '%s\n' "$monitoring_pods" >&2
fi

case "$mode" in
  drain) action="cordon, then drain one by one (PDBs respected, timeout ${timeout}s each)" ;;
  delete-nodeclaim) action="delete their Karpenter NodeClaims (Karpenter drains and terminates)" ;;
  terminate-instance) action="terminate their EC2 instances at once, with no drain" ;;
esac
gd_plan "target node(s): $nodes" "$action" "pods:$summary"
gd_confirm

case "$mode" in
  drain)
    for n in $nodes; do
      kc cordon "$n"
      echo "$n" >>"$state_file"
    done
    gd_record inject "cordoned $nodes"
    for n in $nodes; do
      start="$(date +%s)"
      gd_record inject "draining $n"
      if kc drain "$n" --ignore-daemonsets --delete-emptydir-data --timeout="${timeout}s"; then
        gd_record observe "$n drained in $(($(date +%s) - start))s"
      else
        gd_record observe "$n drain did not finish in ${timeout}s (a PodDisruptionBudget blocking?); node stays cordoned"
      fi
    done
    ;;
  delete-nodeclaim)
    claims=""
    for n in $nodes; do
      c="$(kc get nodeclaims.karpenter.sh -o json | jq -r --arg n "$n" '.items[] | select(.status.nodeName == $n) | .metadata.name')"
      [ -n "$c" ] || gd_die "no NodeClaim for node $n"
      claims="$claims $c"
    done
    gd_record inject "deleting NodeClaim(s)$claims for $nodes"
    # shellcheck disable=SC2086 # $claims is a space-separated list of names
    kc delete nodeclaims.karpenter.sh $claims --wait=false
    ;;
  terminate-instance)
    gd_require_tools aws
    # shellcheck source=../../infra/scripts/common.sh
    source "$GD_REPO_ROOT/infra/scripts/common.sh"
    require_aws_profile
    require_tasuki_account
    ids=""
    for n in $nodes; do
      provider="$(kc get node "$n" -o jsonpath='{.spec.providerID}')"
      id="${provider##*/}"
      case "$id" in i-[0-9a-f]*) ;; *) gd_die "cannot read an instance ID from $n's providerID ($provider)" ;; esac
      ids="$ids $id"
    done
    gd_record inject "terminating EC2 instance(s)$ids (nodes $nodes) without a drain"
    # shellcheck disable=SC2086 # $ids is a space-separated list of instance IDs
    aws ec2 terminate-instances --region ap-northeast-1 --instance-ids $ids --query 'TerminatingInstances[].InstanceId' --output text
    ;;
esac
gd_say "now watch: kubectl get nodes -w; kubectl get nodeclaims -w; kubectl -n $GD_NAMESPACE get pods -o wide -w"
