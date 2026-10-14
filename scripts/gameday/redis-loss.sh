#!/usr/bin/env bash
# GD-01: lose the live fleet cache (Valkey, StatefulSet "redis") and time fleet-state's rebuild from Kafka.
# shellcheck source-path=SCRIPTDIR
set -euo pipefail
# shellcheck source=lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

usage() {
  cat <<EOF
Usage: $GD_NAME --context CTX --namespace NS --mode delete-pod|flushall [--timeout SECONDS]
       $GD_NAME --context CTX --namespace NS --rollback

  delete-pod   delete redis-0. It has no persistence, so every key is lost; the StatefulSet recreates it.
  flushall     FLUSHALL on the running redis-0: the data goes, the process and connections stay.
Then waits for redis-0 to be Ready and for fleet-state to write fleet:state:rebuilt_at again, and prints how
long each took (also into GAMEDAY_LOG). --timeout bounds each wait (default 300).

Undo: nothing to restore, the cache is derived from courier.locations. --rollback restarts fleet-state, which
always rebuilds on start, and waits for it to be Ready (use it if the marker does not come back).

$GD_COMMON_USAGE
EOF
}

mode=""
rollback=0
timeout=300
while [ $# -gt 0 ]; do
  gd_common_flag "$@"
  if [ "$GD_SHIFT" -gt 0 ]; then
    shift "$GD_SHIFT"
    continue
  fi
  case "$1" in
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
if [ "$rollback" = 0 ]; then
  case "$mode" in delete-pod | flushall) ;; *) gd_usage_error "--mode is required: delete-pod or flushall" ;; esac
fi
case "$timeout" in '' | *[!0-9]*) gd_usage_error "--timeout must be a whole number of seconds" ;; esac

gd_require_target

valkey() { k exec redis-0 -c valkey -- valkey-cli "$@" 2>/dev/null; }
marker_present() { [ "$(valkey EXISTS fleet:state:rebuilt_at || echo 0)" = "1" ]; }
couriers() { valkey ZCARD geo:couriers:all || echo "?"; }

if [ "$rollback" = 1 ]; then
  gd_plan "restart deployment/fleet-state (it rebuilds Valkey from Kafka on start) and wait for it"
  gd_confirm
  gd_record rollback "restarting fleet-state to force a rebuild"
  k rollout restart deployment/fleet-state
  k rollout status deployment/fleet-state --timeout="${timeout}s"
  gd_record rollback "fleet-state ready; marker present: $(marker_present && echo yes || echo no); couriers: $(couriers)"
  exit 0
fi

k get pod redis-0 >/dev/null || gd_die "pod redis-0 not found in $GD_NAMESPACE"
before_uid="$(k get pod redis-0 -o jsonpath='{.metadata.uid}')"
before_couriers="$(couriers)"
before_marker="$(marker_present && echo yes || echo no)"

case "$mode" in
  delete-pod) action="delete pod redis-0 (all cached keys are lost; the StatefulSet recreates it)" ;;
  flushall) action="run FLUSHALL on redis-0 (all cached keys are lost; the process stays up)" ;;
esac
gd_plan "$action" "wait for Valkey and for fleet-state's rebuild marker, and time both" \
  "affects: fleet-state (rebuilds), dispatch (pauses until the marker is back); not tracking or orders"
gd_confirm

gd_record observe "before: couriers in geo:couriers:all=$before_couriers, marker present=$before_marker"
start="$(date +%s)"
gd_record inject "$action"
case "$mode" in
  delete-pod) k delete pod redis-0 --wait=false ;;
  flushall) valkey FLUSHALL >/dev/null || gd_die "FLUSHALL failed" ;;
esac

new_ready() {
  local uid ready
  uid="$(k get pod redis-0 -o jsonpath='{.metadata.uid}' 2>/dev/null || true)"
  ready="$(k get pod redis-0 -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
  [ -n "$uid" ] && [ "$uid" != "$before_uid" ] && [ "$ready" = "True" ]
}
if [ "$mode" = "delete-pod" ]; then
  gd_wait_until "$timeout" 2 new_ready || gd_die "redis-0 not Ready after ${timeout}s"
  gd_record observe "redis-0 Ready again after $(($(date +%s) - start))s"
fi
valkey_back="$(date +%s)"

gd_wait_until "$timeout" 2 marker_present || gd_die "fleet:state:rebuilt_at still missing after ${timeout}s: consider --rollback"
now="$(date +%s)"
gd_record observe "rebuild marker back after $((now - start))s ($((now - valkey_back))s after Valkey was back); couriers: $(couriers) (before: $before_couriers)"
