#!/usr/bin/env bash
# GD-01 on the local Compose stack: lose the live fleet cache (Valkey) and time the recovery.
#
# The cluster version is scripts/gameday/redis-loss.sh (kubectl). This one needs only Docker Compose and the
# local stack with the simulator running (make up, make sim-up or scripts/loadtest/run.sh --keep).
set -euo pipefail

usage() {
  cat <<EOF
Usage: $(basename "$0") --mode restart|flushall [--outage SECONDS] [--timeout SECONDS] [--log FILE]

  restart    stop the redis container, wait --outage seconds (default 20), start it again. It has no
             persistence, so it comes back empty: the local twin of deleting redis-0.
  flushall   FLUSHALL on the running Valkey: the data goes, the process and connections stay.

Then it samples once a second until fleet-state has rebuilt the cache, both fleet-state and dispatch are ready
and dispatch has resumed (or --timeout, default 300 s), printing a timeline: the cache marker, couriers in the
geo index, readiness, dispatch paused, and what users saw meanwhile (tracking events sent, orders created,
orders 5xx). Also written to --log (default: docs/gameday/logs/<date>-gd-01-local-<mode>.log).

Undo: nothing to restore; the cache is derived from Kafka. If it never recovers: docker compose restart fleet-state dispatch
EOF
}

mode="" outage=20 timeout=300 log=""
while [ $# -gt 0 ]; do
  case "$1" in
    --mode) mode="${2:?}"; shift 2 ;;
    --outage) outage="${2:?}"; shift 2 ;;
    --timeout) timeout="${2:?}"; shift 2 ;;
    --log) log="${2:?}"; shift 2 ;;
    -h | --help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done
case "$mode" in restart | flushall) ;; *) usage >&2; exit 2 ;; esac
for n in "$outage" "$timeout"; do
  case "$n" in '' | *[!0-9]*) echo "--outage and --timeout take whole seconds" >&2; exit 2 ;; esac
done

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$repo"
log="${log:-docs/gameday/logs/$(date +%Y-%m-%d)-gd-01-local-$mode.log}"
mkdir -p "$(dirname "$log")"

# ---- Probes (read-only) ------------------------------------------------------------------------------------
metric() { # metric URL NAME_REGEX: the sum of every matching sample, 0 if none
  curl -fsS --max-time 2 "$1" 2>/dev/null | awk -v re="^($2)([{ ])" '$0 ~ re { s += $NF } END { printf "%.0f", s + 0 }'
}
ready() { [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "$1")" = 200 ] && echo yes || echo no; }
valkey() { docker compose exec -T redis valkey-cli "$@" 2>/dev/null || echo "-"; }

sample() {
  local up marker couriers
  if [ "$(valkey ping)" = PONG ]; then
    up=up
    marker="$(valkey exists fleet:state:rebuilt_at)"
    couriers="$(valkey zcard geo:couriers:all)"
  else
    up=down marker="-" couriers="-"
  fi
  printf '%s %s %s %s %s %s %s %s %s %s\n' "$up" "$marker" "$couriers" \
    "$(ready http://127.0.0.1:8084/readyz)" "$(ready http://127.0.0.1:8082/readyz)" \
    "$(metric http://127.0.0.1:8082/metrics dispatch_paused)" \
    "$(metric http://127.0.0.1:8083/metrics 'tracking_event_age_seconds_count')" \
    "$(metric http://127.0.0.1:8090/metrics simulator_orders_created_total)" \
    "$(metric http://127.0.0.1:8000/metrics 'http_requests_total\{[^}]*status="5[0-9][0-9]"[^}]*\}')" \
    "$(metric http://127.0.0.1:8084/metrics fleet_state_rebuilds_total)"
}

say() { printf '%s\n' "$*" | tee -a "$log"; }

# ---- Preconditions -----------------------------------------------------------------------------------------
read -r up marker couriers fs_ready d_ready paused events0 orders0 errors0 rebuilds0 <<<"$(sample)"
if [ "$up" != up ] || [ "$marker" != 1 ] || [ "$fs_ready" != yes ] || [ "$d_ready" != yes ]; then
  echo "Not at steady state (valkey $up, marker $marker, fleet-state ready $fs_ready, dispatch ready $d_ready)." >&2
  echo "Start the stack and the simulator first, and let them run for at least 5 minutes." >&2
  exit 1
fi
if [ "$(ready http://127.0.0.1:8090/readyz)" != yes ]; then
  echo "The simulator is not running: start it first (make sim-up), so the drill has traffic and viewers." >&2
  exit 1
fi

say "# GD-01 local, mode=$mode, started $(date -u +%Y-%m-%dT%H:%M:%SZ)"
say "steady state: couriers in geo index $couriers, dispatch paused $paused, fleet-state rebuilds so far $rebuilds0"
say ""
say "   t  valkey  marker  couriers  fs-ready  dispatch-ready  paused  tracking-events  orders-created  orders-5xx"

start=$(date +%s)
t() { echo $(($(date +%s) - start)); }
row() {
  local s
  read -r up marker couriers fs_ready d_ready paused events orders errors rebuilds <<<"$(sample)"
  s="$(printf '%4s  %-6s  %-6s  %-8s  %-8s  %-14s  %-6s  %+15s  %+14s  %+10s' "$(t)" "$up" "$marker" "$couriers" \
    "$fs_ready" "$d_ready" "$paused" "+$((events - events0))" "+$((orders - orders0))" "+$((errors - errors0))")"
  say "$s"
}

# ---- Inject ------------------------------------------------------------------------------------------------
case "$mode" in
  restart)
    say "T+$(t)s inject: docker compose stop redis (down for ${outage}s)"
    docker compose stop --timeout 1 redis >/dev/null 2>&1
    end_outage=$((start + outage))
    while [ "$(date +%s)" -lt "$end_outage" ]; do
      row
      sleep 1
    done
    say "T+$(t)s docker compose start redis (empty: no persistence)"
    docker compose start redis >/dev/null 2>&1
    ;;
  flushall)
    say "T+$(t)s inject: FLUSHALL"
    valkey flushall >/dev/null
    ;;
esac

# ---- Watch the recovery ------------------------------------------------------------------------------------
back="" marker_at="" dispatch_paused_seen=no resumed_at=""
deadline=$((start + outage + timeout))
while [ "$(date +%s)" -lt "$deadline" ]; do
  row
  [ -z "$back" ] && [ "$up" = up ] && back=$(t)
  [ "$paused" = 1 ] && dispatch_paused_seen=yes
  [ -z "$marker_at" ] && [ "$marker" = 1 ] && marker_at=$(t)
  if [ -n "$marker_at" ] && [ "$fs_ready" = yes ] && [ "$d_ready" = yes ] && [ "$paused" = 0 ]; then
    resumed_at=$(t)
    break
  fi
  sleep 1
done

say ""
say "## Result"
say "valkey answering again:            ${back:-never} s after the injection"
say "fleet:state:rebuilt_at back:       ${marker_at:-never} s after the injection"
say "fleet-state and dispatch ready:    ${resumed_at:-not within the timeout} s after the injection"
say "dispatch paused at some point:     $dispatch_paused_seen"
say "fleet-state rebuilds:              +$((rebuilds - rebuilds0))"
say "couriers in the geo index now:     $couriers"
say "tracking events sent meanwhile:    +$((events - events0))   (hypothesis: keeps rising, tracking never reads Valkey)"
say "orders created meanwhile:          +$((orders - orders0))   (hypothesis: keeps rising, orders never reads Valkey)"
say "orders API 5xx meanwhile:          +$((errors - errors0))   (hypothesis: 0)"
say "log: $log"
[ -n "$resumed_at" ]
