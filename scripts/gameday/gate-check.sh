#!/usr/bin/env bash
# GD-03: ask the rollout gate (tools/rollout-gate) whether a canary may start, against the live error budget.
# shellcheck source-path=SCRIPTDIR
set -euo pipefail
# shellcheck source=lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

usage() {
  cat <<EOF
Usage: $GD_NAME --context CTX --namespace NS --service NAME [--slo NAME] [--min-budget RATIO]

Read-only. Port-forwards to Prometheus and runs 'rollout-gate check' (the binary on PATH, or 'go run' from
tools/rollout-gate), the same decision Argo Rollouts' error-budget-gate analysis gets. Prints the remaining
budget per SLO and the gate's JSON decision. Exit code is the gate's: 0 allowed, 1 blocked, 2 bad input.
SLO budgets exist only in tasuki-prod; in tasuki-dev the gate answers no_data and, failing closed, blocks.

Options:
  --service NAME       Sloth service: orders, fleet-state, tracking, dispatch
  --slo NAME           one SLO (default: every SLO of the service, as the policy freezes on any)
  --min-budget RATIO   MIN_BUDGET_REMAINING for the gate (default 0)

$GD_COMMON_USAGE
EOF
}

service=""
slo=""
min_budget="0"
while [ $# -gt 0 ]; do
  gd_common_flag "$@"
  if [ "$GD_SHIFT" -gt 0 ]; then
    shift "$GD_SHIFT"
    continue
  fi
  case "$1" in
    --service)
      gd_need_value "$1" "${2:-}"
      service="$2"
      shift 2
      ;;
    --slo)
      gd_need_value "$1" "${2:-}"
      slo="$2"
      shift 2
      ;;
    --min-budget)
      gd_need_value "$1" "${2:-}"
      min_budget="$2"
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *) gd_usage_error "unknown argument: $1" ;;
  esac
done
[ -n "$service" ] || gd_usage_error "--service is required"
case "$service$slo" in *[!a-z0-9-]*) gd_usage_error "--service and --slo: lowercase letters, digits and '-' only" ;; esac
case "$min_budget" in 0 | 0.[0-9]*) ;; *) gd_usage_error "--min-budget must be in [0, 1)" ;; esac

gd_require_target
if command -v rollout-gate >/dev/null 2>&1; then
  gate=(rollout-gate)
else
  gd_require_tools go
  gate=(go -C "$GD_REPO_ROOT/tools/rollout-gate" run ./cmd/rollout-gate)
fi
gd_plan "read slo:period_error_budget_remaining:ratio for $service (read-only)" \
  "run: ${gate[*]} check --service $service${slo:+ --slo $slo} (MIN_BUDGET_REMAINING=$min_budget)"
gd_confirm
gd_prom_start

[ "$GD_NAMESPACE" = "tasuki-prod" ] || gd_say "note: $GD_NAMESPACE has no SLO budgets; expect cause no_data (blocked: the gate fails closed)"
echo "budget left: $(gd_prom_value "slo:period_error_budget_remaining:ratio{sloth_service=\"$service\"}")"

rc=0
MIN_BUDGET_REMAINING="$min_budget" "${gate[@]}" check --service "$service" ${slo:+--slo "$slo"} \
  --prometheus-url "$GD_PROM_URL" || rc=$?
case "$rc" in
  0) gd_record observe "gate: $service ALLOWED (exit 0)" ;;
  1) gd_record observe "gate: $service BLOCKED (exit 1)" ;;
  *) gd_record observe "gate: error (exit $rc)" ;;
esac
exit "$rc"
