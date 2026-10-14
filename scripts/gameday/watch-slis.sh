#!/usr/bin/env bash
# Prints Tasuki's key SLIs and signals every N seconds, from the Prometheus HTTP API (read-only).
# shellcheck source-path=SCRIPTDIR
set -euo pipefail
# shellcheck source=lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

usage() {
  cat <<EOF
Usage: $GD_NAME --context CTX --namespace NS [--interval SECONDS] [--count N | --once] [--extra 'LABEL=PROMQL']...

Read-only. Port-forwards to monitoring/prometheus-operated (unless --prom-url) and prints, every interval:
orders availability and latency (server side), quote/order failures as the simulator saw them (client side),
location, tracking and dispatch freshness, outbox and consumer lag, fleet-ingest accepted and shed points,
tracking drops, ETA fallback share, pods not ready or pending, the error budget left (tasuki-prod only), and
the firing alerts. All queries use the namespace you give, so dev drills see dev numbers. In tasuki-dev there
are no SLO budgets and most alerts do not fire (their rules select tasuki-prod).

Options:
  --interval SECONDS     time between samples (default 15)
  --count N              stop after N samples (default: until Ctrl-C)
  --once                 one sample, then exit (what record-timeline.sh snapshot uses)
  --extra 'LABEL=PROMQL' an extra query for this drill; {ns} is replaced by the namespace. Repeatable.

$GD_COMMON_USAGE
EOF
}

interval=15
count=0
extras=""
while [ $# -gt 0 ]; do
  gd_common_flag "$@"
  if [ "$GD_SHIFT" -gt 0 ]; then
    shift "$GD_SHIFT"
    continue
  fi
  case "$1" in
    --interval)
      gd_need_value "$1" "${2:-}"
      interval="$2"
      shift 2
      ;;
    --count)
      gd_need_value "$1" "${2:-}"
      count="$2"
      shift 2
      ;;
    --once)
      count=1
      shift
      ;;
    --extra)
      gd_need_value "$1" "${2:-}"
      case "$2" in *=*) ;; *) gd_usage_error "--extra needs LABEL=PROMQL" ;; esac
      extras="${extras}${2}"$'\n'
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *) gd_usage_error "unknown argument: $1" ;;
  esac
done
case "$interval" in '' | *[!0-9]*) gd_usage_error "--interval must be a whole number of seconds" ;; esac
case "$count" in '' | *[!0-9]*) gd_usage_error "--count must be a whole number" ;; esac

gd_require_target
gd_require_tools curl jq
gd_plan "read the SLIs of $GD_NAMESPACE from Prometheus every ${interval}s (read-only)"
gd_confirm
gd_prom_start

ns="$GD_NAMESPACE"
orders_sel="namespace=\"$ns\",service=\"orders\",method=\"POST\",route=~\"/v1/quotes|/v1/orders\""
create_sel="namespace=\"$ns\",service=\"orders\",method=\"POST\",route=\"/v1/orders\""
sim_sel="namespace=\"$ns\",call=~\"quote|order\""

# label <TAB> PromQL. Ratios are "good share": 1 is perfect.
queries="$(
  cat <<EOF
orders availability 5m (server)	1 - ((sum(rate(http_requests_total{$orders_sel,status=~"5.."}[5m])) or vector(0)) / sum(rate(http_requests_total{$orders_sel}[5m])))
orders POST req/s	sum(rate(http_requests_total{$orders_sel}[1m]))
orders 5xx/s	sum(rate(http_requests_total{$orders_sel,status=~"5.."}[1m])) or vector(0)
orders 503 database_unavailable/s by reason	sum by (reason) (rate(orders_database_unavailable_total{namespace="$ns"}[1m]))
orders creates <300ms 5m	sum(rate(http_request_duration_seconds_bucket{$create_sel,le="0.3"}[5m])) / sum(rate(http_request_duration_seconds_count{$create_sel}[5m]))
client view: quote+order failures 1m	(sum(rate(simulator_client_request_duration_seconds_count{$sim_sel,outcome!~"2..|4.."}[1m])) or vector(0)) / sum(rate(simulator_client_request_duration_seconds_count{$sim_sel}[1m]))
simulator orders created/min	sum(rate(simulator_orders_created_total{namespace="$ns"}[5m])) * 60
location freshness 5m (<=2s)	sum(rate(fleet_state_freshness_seconds_bucket{namespace="$ns",le="2.0"}[5m])) / sum(rate(fleet_state_freshness_seconds_count{namespace="$ns"}[5m]))
tracking freshness 5m (<=10s)	sum(rate(tracking_event_age_seconds_bucket{namespace="$ns",le="10.0"}[5m])) / sum(rate(tracking_event_age_seconds_count{namespace="$ns"}[5m]))
dispatch assigned <=60s 30m	sum(rate(dispatch_assignment_duration_seconds_bucket{namespace="$ns",outcome="assigned",le="60.0"}[30m])) / sum(rate(dispatch_assignment_duration_seconds_count{namespace="$ns"}[30m]))
outbox lag (s)	max(orders_outbox_lag_seconds{namespace="$ns"})
fleet-state kafka lag (msgs)	sum(kafka_consumergroup_lag{namespace="$ns",consumergroup="fleet-state",topic="courier.locations"})
ingest accepted points/s	sum(rate(fleet_ingest_points_total{namespace="$ns",result="accepted"}[1m]))
ingest shed requests/s	sum(rate(fleet_ingest_shed_requests_total{namespace="$ns"}[1m])) or vector(0)
tracking dropped share 5m	(sum(rate(tracking_sse_dropped_events_total{namespace="$ns"}[5m])) or vector(0)) / (sum(rate(tracking_event_age_seconds_count{namespace="$ns"}[5m])) + (sum(rate(tracking_sse_dropped_events_total{namespace="$ns"}[5m])) or vector(0)))
quotes on ETA fallback 5m	sum(rate(orders_quotes_created_total{namespace="$ns",eta_source="fallback"}[5m])) / sum(rate(orders_quotes_created_total{namespace="$ns"}[5m]))
pods running but not ready	sum(kube_pod_status_ready{namespace="$ns",condition="false"} * on (namespace, pod) group_left() (kube_pod_status_phase{namespace="$ns",phase="Running"} == 1)) or vector(0)
pods pending	sum(kube_pod_status_phase{namespace="$ns",phase="Pending"}) or vector(0)
EOF
)"
if [ "$ns" = "tasuki-prod" ]; then
  queries="${queries}"$'\n'"error budget left (per service/SLO)"$'\t'"slo:period_error_budget_remaining:ratio"
fi
queries="${queries}"$'\n'"firing alerts (alertname/severity=count)"$'\t'"count by (alertname, severity) (ALERTS{alertstate=\"firing\",alertname!~\"Watchdog|InfoInhibitor\"})"

while IFS= read -r line; do
  [ -n "$line" ] || continue
  label="${line%%=*}"
  query="${line#*=}"
  queries="${queries}"$'\n'"${label}"$'\t'"${query//\{ns\}/$ns}"
done <<<"$extras"

samples=0
while :; do
  printf -- '--- %s  %s ---\n' "$(gd_now)" "$ns"
  while IFS=$'\t' read -r label query; do
    [ -n "$label" ] || continue
    printf '%-40s %s\n' "$label" "$(gd_prom_value "$query")"
  done <<<"$queries"
  [ "$ns" = "tasuki-prod" ] || echo "(tasuki-dev has no SLO budgets; SLO and most cause alerts select tasuki-prod)"
  samples=$((samples + 1))
  if [ "$count" -gt 0 ] && [ "$samples" -ge "$count" ]; then break; fi
  sleep "$interval"
done
