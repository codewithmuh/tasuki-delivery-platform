#!/usr/bin/env bash
# GD-06: CloudNativePG switchover, or loss of the primary, for the orders database; times the failover.
# shellcheck source-path=SCRIPTDIR
set -euo pipefail
# shellcheck source=lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

usage() {
  cat <<EOF
Usage: $GD_NAME --context CTX --namespace NS --mode switchover|kill-primary [--cluster NAME] [--timeout SECONDS]
       $GD_NAME --context CTX --namespace NS --rollback [--cluster NAME]

  switchover    planned: promote a healthy standby with 'kubectl cnpg promote' (needs the cnpg kubectl plugin
                and at least 2 instances: tasuki-prod)
  kill-primary  unplanned: delete the primary pod with a 1 s grace period. With one instance (tasuki-dev) this
                is a restart of the only database, not a failover: orders is down until it is back.
Then prints every change of the cluster's primary and phase with the elapsed seconds, until the cluster is
healthy again (also into GAMEDAY_LOG). --cluster defaults to orders-db; --timeout to 600.

Undo: CloudNativePG recovers by itself. --rollback switches back to the primary saved before the drill (needs
the plugin and 2 instances) and waits for a healthy cluster.

$GD_COMMON_USAGE
EOF
}

mode=""
cluster="orders-db"
rollback=0
timeout=600
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
    --cluster)
      gd_need_value "$1" "${2:-}"
      cluster="$2"
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
  case "$mode" in switchover | kill-primary) ;; *) gd_usage_error "--mode is required: switchover or kill-primary" ;; esac
fi
case "$timeout" in '' | *[!0-9]*) gd_usage_error "--timeout must be a whole number of seconds" ;; esac

gd_require_target
gd_require_tools jq

HEALTHY="Cluster in healthy state"
state_file="$(gd_state_dir)/pg-failover-$cluster"
status() { k get clusters.postgresql.cnpg.io "$cluster" -o json; }
field() { status | jq -r "$1"; }
have_plugin() { kubectl cnpg version >/dev/null 2>&1; }

# Prints primary/phase changes until the cluster is healthy with every instance ready.
follow() {
  local start="$1" last="" now cur
  while :; do
    cur="$(status | jq -r '"\(.status.currentPrimary // "-")|\(.status.phase // "-")|\(.status.readyInstances // 0)/\(.status.instances // 0)"' || echo "?")"
    if [ "$cur" != "$last" ]; then
      gd_record observe "t+$(($(date +%s) - start))s primary=${cur%%|*} phase=$(echo "$cur" | cut -d'|' -f2) ready=${cur##*|}"
      last="$cur"
    fi
    case "$cur" in
      *"|$HEALTHY|"*)
        now="${cur##*|}"
        if [ "${now%/*}" = "${now#*/}" ]; then return 0; fi
        ;;
    esac
    if [ $(($(date +%s) - start)) -ge "$timeout" ]; then return 1; fi
    sleep 2
  done
}

status >/dev/null || gd_die "cluster $cluster not found in $GD_NAMESPACE"
primary="$(field '.status.currentPrimary // ""')"
instances="$(field '.status.instances // 0')"
[ -n "$primary" ] || gd_die "$cluster has no current primary; not a good moment for a drill"

if [ "$rollback" = 1 ]; then
  original="$(cat "$state_file" 2>/dev/null || true)"
  if [ -z "$original" ] || [ "$original" = "$primary" ] || [ "$instances" -lt 2 ]; then
    gd_plan "wait for $cluster to be healthy (primary is $primary; nothing to switch back)"
    gd_confirm
  else
    have_plugin || gd_die "the cnpg kubectl plugin is needed to switch back (kubectl cnpg promote)"
    gd_plan "switch $cluster back to its original primary $original (now $primary)" "wait until healthy"
    gd_confirm
    gd_record rollback "switchover back to $original"
    kc cnpg promote "$cluster" "$original" --namespace "$GD_NAMESPACE"
  fi
  follow "$(date +%s)" || gd_die "$cluster not healthy after ${timeout}s"
  rm -f "$state_file"
  gd_record rollback "$cluster healthy, primary $(field '.status.currentPrimary')"
  exit 0
fi

case "$mode" in
  switchover)
    [ "$instances" -ge 2 ] || gd_die "$cluster has $instances instance(s); a switchover needs a standby (tasuki-prod has 2)"
    have_plugin || gd_die "the cnpg kubectl plugin is needed for a switchover (or use --mode kill-primary)"
    target="$(status | jq -r --arg p "$primary" '[.status.instancesStatus.healthy[]? | select(. != $p)][0] // ""')"
    [ -n "$target" ] || gd_die "no healthy standby to promote"
    gd_plan "planned switchover of $cluster: promote $target, demote $primary" \
      "orders writes pause for the switchover (seconds expected)"
    ;;
  kill-primary)
    impact="unplanned failover to the standby"
    [ "$instances" -ge 2 ] || impact="ONLY instance: orders is down until it restarts (not a failover)"
    gd_plan "delete the primary pod $primary with a 1 s grace period" "impact: $impact"
    ;;
esac
gd_confirm

echo "$primary" >"$state_file"
start="$(date +%s)"
case "$mode" in
  switchover)
    gd_record inject "switchover: promoting $target (primary was $primary)"
    kc cnpg promote "$cluster" "$target" --namespace "$GD_NAMESPACE"
    ;;
  kill-primary)
    gd_record inject "deleting primary pod $primary (grace period 1 s)"
    k delete pod "$primary" --grace-period=1 --wait=false
    ;;
esac

# The status can still say "healthy" for a moment after the inject: first wait for the operator to react.
changed() {
  local cur
  cur="$(status | jq -r '"\(.status.currentPrimary // "")|\(.status.phase // "")"' || echo "|")"
  [ "${cur%%|*}" != "$primary" ] || [ "${cur#*|}" != "$HEALTHY" ]
}
gd_wait_until 60 1 changed || gd_record observe "the operator reported no change within 60s of the inject"

if follow "$start"; then
  gd_record observe "$cluster healthy $(($(date +%s) - start))s after the inject; primary now $(field '.status.currentPrimary')"
else
  gd_die "$cluster not healthy after ${timeout}s (see kubectl cnpg status $cluster)"
fi
