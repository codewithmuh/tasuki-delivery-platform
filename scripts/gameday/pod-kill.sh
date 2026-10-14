#!/usr/bin/env bash
# GD-09 / GD-10: delete pods of one Tasuki service (routing, dispatch, tracking, ...); its Deployment recreates them.
# shellcheck source-path=SCRIPTDIR
set -euo pipefail
# shellcheck source=lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

usage() {
  cat <<EOF
Usage: $GD_NAME --context CTX --namespace NS --app NAME [--count N|all] [--timeout SECONDS]
       $GD_NAME --context CTX --namespace NS --app NAME --rollback

Deletes N (default 1) server pods of the service NAME (labels app.kubernetes.io/name=NAME,
app.kubernetes.io/component=server), then waits until its Deployment (or, in tasuki-prod, its Argo Rollout) has every
replica available again and prints the time (also into GAMEDAY_LOG). Data stores have their own scripts: redis-loss.sh, kafka-broker-loss.sh,
pg-failover.sh.

Undo: the controller recreates the pods. --rollback only waits for every replica to be available.

$GD_COMMON_USAGE
EOF
}

app=""
count="1"
rollback=0
timeout=300
while [ $# -gt 0 ]; do
  gd_common_flag "$@"
  if [ "$GD_SHIFT" -gt 0 ]; then
    shift "$GD_SHIFT"
    continue
  fi
  case "$1" in
    --app)
      gd_need_value "$1" "${2:-}"
      app="$2"
      shift 2
      ;;
    --count)
      gd_need_value "$1" "${2:-}"
      count="$2"
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
[ -n "$app" ] || gd_usage_error "--app is required"
case "$app" in
  redis | valkey) gd_usage_error "use redis-loss.sh for the cache" ;;
  *[!a-z0-9-]*) gd_usage_error "--app: lowercase letters, digits and '-' only" ;;
esac
case "$count" in all | [1-9]) ;; *) gd_usage_error "--count must be 1-9 or all" ;; esac
case "$timeout" in '' | *[!0-9]*) gd_usage_error "--timeout must be a whole number of seconds" ;; esac

gd_require_target
gd_require_tools jq
if k get deployment "$app" >/dev/null 2>&1; then
  kind="deployment"
elif k get rollouts.argoproj.io "$app" >/dev/null 2>&1; then
  kind="rollouts.argoproj.io"
else
  gd_die "no Deployment or Rollout called $app in $GD_NAMESPACE"
fi
# Every desired replica available (the same fields on Deployments and Rollouts).
all_available() {
  k get "$kind" "$app" -o json | jq -e '(.status.availableReplicas // 0) >= (.spec.replicas // 1)' >/dev/null
}

if [ "$rollback" = 1 ]; then
  gd_plan "wait for $kind/$app to have every replica available (nothing is changed)"
  gd_confirm
  gd_wait_until "$timeout" 3 all_available || gd_die "$kind/$app not fully available after ${timeout}s"
  gd_record rollback "$kind/$app fully available"
  exit 0
fi

pods="$(k get pods -l "app.kubernetes.io/name=$app,app.kubernetes.io/component=server" -o name | sort)"
total="$(printf '%s\n' "$pods" | grep -c . || true)"
[ "$total" -gt 0 ] || gd_die "no running pods for $app"
[ "$count" = "all" ] && count="$total"
[ "$count" -le "$total" ] || gd_die "--count $count but $app has only $total pod(s)"
targets="$(printf '%s\n' "$pods" | head -n "$count" | tr '\n' ' ')"
impact="$count of $total replicas"
[ "$count" -lt "$total" ] || impact="EVERY replica: $app is unavailable until a new pod is Ready"
gd_plan "delete $targets" "impact: $impact"
gd_confirm

start="$(date +%s)"
gd_record inject "deleting $targets ($impact)"
# shellcheck disable=SC2086 # $targets is a space-separated list of pod names
k delete $targets --wait=false
sleep 5
if gd_wait_until "$timeout" 3 all_available; then
  gd_record observe "$kind/$app fully available again $(($(date +%s) - start))s after the delete"
else
  gd_die "$kind/$app not fully available after ${timeout}s"
fi
