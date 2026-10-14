#!/usr/bin/env bash
# GD-05: delete Strimzi Kafka broker pods and time their recovery.
# shellcheck source-path=SCRIPTDIR
set -euo pipefail
# shellcheck source=lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

usage() {
  cat <<EOF
Usage: $GD_NAME --context CTX --namespace NS [--brokers N|all | --pod NAME] [--timeout SECONDS]
       $GD_NAME --context CTX --namespace NS --rollback

Deletes broker pods of Kafka 'tasuki' (node pool 'dual-role', pods tasuki-dual-role-<n>), then waits until
Strimzi has recreated them and they are Ready, printing the times (also into GAMEDAY_LOG).
  --brokers N|all  how many brokers, lowest ordinal first (default 1)
  --pod NAME       one specific broker pod instead
  --timeout S      how long to wait for recovery (default 600)
In tasuki-dev the single broker IS the cluster: deleting it is a full Kafka outage. In tasuki-prod (3 brokers,
min.insync.replicas 2) one broker should be invisible; two break writes and the KRaft quorum.

Undo: Strimzi recreates deleted pods by itself. --rollback only waits until every broker pod and the Kafka
resource are Ready, and prints their state.

$GD_COMMON_USAGE
EOF
}

SELECTOR="strimzi.io/cluster=tasuki,strimzi.io/pool-name=dual-role"
brokers="1"
pod=""
rollback=0
timeout=600
while [ $# -gt 0 ]; do
  gd_common_flag "$@"
  if [ "$GD_SHIFT" -gt 0 ]; then
    shift "$GD_SHIFT"
    continue
  fi
  case "$1" in
    --brokers)
      gd_need_value "$1" "${2:-}"
      brokers="$2"
      shift 2
      ;;
    --pod)
      gd_need_value "$1" "${2:-}"
      pod="$2"
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
case "$brokers" in all | [1-9]) ;; *) gd_usage_error "--brokers must be 1-9 or all" ;; esac
case "$timeout" in '' | *[!0-9]*) gd_usage_error "--timeout must be a whole number of seconds" ;; esac

gd_require_target
gd_require_tools jq

pods_json() { k get pods -l "$SELECTOR" -o json; }
all_ready() {
  local want="$1" ready
  ready="$(pods_json | jq '[.items[] | select(.metadata.deletionTimestamp == null)
    | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))] | length')"
  [ "$ready" -ge "$want" ]
}
kafka_ready() {
  [ "$(k get kafkas.kafka.strimzi.io tasuki -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null)" = "True" ]
}

total="$(pods_json | jq '.items | length')"
[ "$total" -gt 0 ] || gd_die "no broker pods match $SELECTOR in $GD_NAMESPACE"

if [ "$rollback" = 1 ]; then
  gd_plan "wait until all $total broker pods and Kafka 'tasuki' are Ready (nothing is changed)"
  gd_confirm
  gd_wait_until "$timeout" 5 all_ready "$total" || gd_die "brokers not all Ready after ${timeout}s: check PVC zones vs nodes (kubectl get pvc -o wide)"
  gd_wait_until "$timeout" 5 kafka_ready || gd_die "Kafka 'tasuki' not Ready after ${timeout}s"
  k get kafkas.kafka.strimzi.io tasuki
  k get pods -l "$SELECTOR" -o wide
  gd_record rollback "all $total brokers and Kafka 'tasuki' Ready"
  exit 0
fi

if [ -n "$pod" ]; then
  pods_json | jq -e --arg p "$pod" 'any(.items[]; .metadata.name == $p)' >/dev/null || gd_die "$pod is not a broker pod ($SELECTOR)"
  targets="$pod"
else
  [ "$brokers" = "all" ] && brokers="$total"
  [ "$brokers" -le "$total" ] || gd_die "--brokers $brokers but only $total broker pods exist"
  targets="$(pods_json | jq -r '.items[].metadata.name' | sort | head -n "$brokers" | tr '\n' ' ')"
fi
n_targets="$(echo "$targets" | wc -w | tr -d ' ')"

impact="one of $total brokers: Kafka should tolerate it"
if [ "$n_targets" -ge "$total" ]; then
  impact="EVERY broker: a full Kafka outage (outbox waits, fleet-ingest sheds with 503, tracking goes quiet)"
elif [ "$((total - n_targets))" -lt 2 ]; then
  impact="$n_targets of $total brokers: below min.insync.replicas=2 and the KRaft quorum, so writes stop"
fi
gd_plan "delete broker pod(s): $targets" "impact: $impact" "wait up to ${timeout}s for Strimzi to bring them back"
gd_confirm

start="$(date +%s)"
gd_record inject "deleting broker pod(s) $targets ($impact)"
# shellcheck disable=SC2086 # $targets is a space-separated list of pod names
k delete pod $targets --wait=false

# Wait until the deleted pods are actually gone from the Ready set, then until all are Ready again.
sleep 5
gd_wait_until "$timeout" 3 all_ready "$total" || gd_die "brokers not all Ready after ${timeout}s; run --rollback to keep waiting"
gd_record observe "all $total broker pods Ready again $(($(date +%s) - start))s after the delete"
if gd_wait_until "$timeout" 5 kafka_ready; then
  gd_record observe "Kafka 'tasuki' Ready $(($(date +%s) - start))s after the delete"
else
  gd_die "Kafka 'tasuki' not Ready after ${timeout}s"
fi
