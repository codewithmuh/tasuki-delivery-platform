#!/usr/bin/env bash
# Read-only go/no-go before a game day (docs/gameday/README.md, safety rule 3).
# shellcheck source-path=SCRIPTDIR
set -euo pipefail
# shellcheck source=lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

usage() {
  cat <<EOF
Usage: $GD_NAME --context CTX --namespace NS [--service NAME]... [--no-prometheus]

Read-only. Checks, and prints OK / WARN / NO-GO for each:
  - Argo CD: the environment's Applications are Synced and Healthy
  - pods in the namespace: none crash-looping, failing or stuck Pending
  - data stores: Kafka 'tasuki' Ready, CloudNativePG 'orders-db' healthy, Valkey 'redis-0' Ready
  - the simulator is running, and which scenario
  - Argo CD: no Application of the environment left paused (auto-sync off) by an earlier drill
  - Prometheus: no page alert firing (severity page, or upstream critical); in tasuki-prod, more than 50 % of the error budget left for every SLO of
    each --service (the error-budget policy: game days only above 50 %)
Exit 0 = GO, 1 = NO-GO.

Options:
  --service NAME     Sloth service the drill targets (orders, fleet-state, tracking, dispatch). Repeatable.
  --no-prometheus    skip the Prometheus checks

$GD_COMMON_USAGE
EOF
}

services=""
use_prom=1
while [ $# -gt 0 ]; do
  gd_common_flag "$@"
  if [ "$GD_SHIFT" -gt 0 ]; then
    shift "$GD_SHIFT"
    continue
  fi
  case "$1" in
    --service)
      gd_need_value "$1" "${2:-}"
      case "$2" in *[!a-z0-9-]*) gd_usage_error "--service: lowercase letters, digits and '-' only" ;; esac
      services="${services:+$services }$2"
      shift 2
      ;;
    --no-prometheus)
      use_prom=0
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *) gd_usage_error "unknown argument: $1" ;;
  esac
done

gd_require_target
gd_require_tools jq
gd_plan "read Argo CD, pod, data-store and simulator state in $GD_NAMESPACE (read-only)" \
  "read firing alerts and error budgets from Prometheus (read-only)"
gd_confirm

nogo=0
ok() { echo "OK     $*"; }
warn() { echo "WARN   $*"; }
bad() {
  echo "NO-GO  $*"
  nogo=1
}

kc get namespace "$GD_NAMESPACE" >/dev/null 2>&1 || gd_die "cannot read namespace $GD_NAMESPACE (wrong context, or the cluster is down?)"

# Argo CD: tasuki-<env> and every <service>-<env>.
apps="$(kc -n argocd get applications.argoproj.io -o json 2>/dev/null |
  jq -r --arg env "$GD_ENV" '.items[] | select(.metadata.name == ("tasuki-" + $env) or (.metadata.name | endswith("-" + $env)))
    | "\(.metadata.name) \(.status.sync.status // "?") \(.status.health.status // "?") \(if .spec.syncPolicy.automated == null then "paused" else "auto" end)"' || true)"
if [ -z "$apps" ]; then
  warn "no Argo CD Applications found for $GD_ENV (cannot read the argocd namespace?)"
else
  while read -r name sync health policy; do
    if [ "$sync" = "Synced" ] && [ "$health" = "Healthy" ]; then ok "argocd $name"; else bad "argocd $name is $sync/$health"; fi
    if [ "$policy" = "paused" ]; then bad "argocd $name has auto-sync paused (resume it: deploy/README.md, Game days)"; fi
  done <<<"$apps"
fi

# Pods: anything not Running/Succeeded, any container not ready, any restarts in a crash loop.
pods="$(k get pods -o json | jq -r '.items[]
  | select(.status.phase != "Succeeded")
  | . as $p
  | ([.status.containerStatuses[]? | select(.ready | not) | (.state.waiting.reason // "not ready")] | join(",")) as $why
  | select($p.status.phase != "Running" or $why != "")
  | "\($p.metadata.name) \($p.status.phase) \($why)"')"
if [ -z "$pods" ]; then ok "every pod in $GD_NAMESPACE is running and ready"; else
  while read -r line; do bad "pod $line"; done <<<"$pods"
fi

# Data stores (names from deploy/README.md).
kafka_ready="$(k get kafkas.kafka.strimzi.io tasuki -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
if [ "$kafka_ready" = "True" ]; then ok "kafka tasuki Ready"; else bad "kafka tasuki Ready=${kafka_ready:-unknown}"; fi
pg="$(k get clusters.postgresql.cnpg.io orders-db -o json 2>/dev/null |
  jq -r '"\(.status.phase // "?")|\(.status.readyInstances // 0)/\(.status.instances // 0)|\(.status.currentPrimary // "?")"' || true)"
case "$pg" in
  "Cluster in healthy state|"*) ok "orders-db ${pg#*|} (instances ready|primary)" ;;
  *) bad "orders-db: ${pg:-not found}" ;;
esac
valkey="$(k get pod redis-0 -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null || true)"
if [ "$valkey" = "True" ]; then ok "valkey redis-0 Ready"; else bad "valkey redis-0 Ready=${valkey:-not found}"; fi

# The simulator is the traffic; without it a drill shows nothing.
sim="$(k get deployment simulator -o json 2>/dev/null |
  jq -r '"\(.status.readyReplicas // 0)/\(.spec.replicas // 0)|\([.spec.template.spec.containers[0].env[]? | select(.name == "SCENARIO") | .value][0] // "?")"' || true)"
case "$sim" in
  "" | "0/"*) bad "simulator not running (${sim:-not found}); set replicas: 1 in deploy/apps/overlays/$GD_ENV/values/simulator.yaml" ;;
  *) ok "simulator ${sim%%|*} ready, scenario: ${sim#*|}" ;;
esac

if [ "$use_prom" = 1 ]; then
  gd_prom_start
  pages="$(gd_prom_value 'count by (alertname) (ALERTS{alertstate="firing",severity=~"page|critical"})')"
  if [ "$pages" = "-" ]; then ok "no page alert firing"; else bad "page alerts firing: $pages"; fi
  if [ "$GD_NAMESPACE" = "tasuki-prod" ]; then
    for svc in $services; do
      left="$(gd_prom_value "min(slo:period_error_budget_remaining:ratio{sloth_service=\"$svc\"})")"
      case "$left" in
        - | error) bad "$svc: no error budget series (the gate would say no_data)" ;;
        *)
          if awk -v v="$left" 'BEGIN { exit !(v > 0.5) }'; then ok "$svc: $left of the error budget left (> 0.5)"; else
            bad "$svc: only $left of the error budget left; the policy allows game days above 0.5"
          fi
          ;;
      esac
    done
  elif [ -n "$services" ]; then
    warn "tasuki-dev has no SLO budgets; --service is only checked in tasuki-prod"
  fi
fi

if [ "$nogo" = 0 ]; then
  echo "GO"
  gd_record observe "preflight GO"
else
  echo "NO-GO"
  gd_record observe "preflight NO-GO"
  exit 1
fi
