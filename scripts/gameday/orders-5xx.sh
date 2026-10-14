#!/usr/bin/env bash
# GD-03: make order creation fail (5xx) for a fixed time by revoking INSERT on orders_order from the app role.
# Stands in for "a bad deploy that returns 5xx", to spend the orders availability budget on purpose.
# shellcheck source-path=SCRIPTDIR
set -euo pipefail
# shellcheck source=lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

usage() {
  cat <<EOF
Usage: $GD_NAME --context CTX --namespace NS [--duration SECONDS] [--cluster NAME]
       $GD_NAME --context CTX --namespace NS --rollback [--cluster NAME]

Runs, as the postgres superuser on the primary of CloudNativePG cluster --cluster (default orders-db):
  REVOKE INSERT ON TABLE orders_order FROM orders
so every POST /v1/orders fails inside its transaction (nothing half-written; Django answers 500). Quotes,
pickups and deliveries keep working. Holds the fault for --duration seconds (default 300, at most 1800), then
re-grants. It also re-grants on Ctrl-C or any exit.

Undo: automatic. If this process died (laptop closed), --rollback runs
  GRANT INSERT ON TABLE orders_order TO orders
and checks has_table_privilege().

$GD_COMMON_USAGE
EOF
}

cluster="orders-db"
duration=300
rollback=0
while [ $# -gt 0 ]; do
  gd_common_flag "$@"
  if [ "$GD_SHIFT" -gt 0 ]; then
    shift "$GD_SHIFT"
    continue
  fi
  case "$1" in
    --duration)
      gd_need_value "$1" "${2:-}"
      duration="$2"
      shift 2
      ;;
    --cluster)
      gd_need_value "$1" "${2:-}"
      cluster="$2"
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
case "$duration" in '' | *[!0-9]*) gd_usage_error "--duration must be a whole number of seconds" ;; esac
if [ "$duration" -lt 30 ] || [ "$duration" -gt 1800 ]; then gd_usage_error "--duration must be between 30 and 1800 seconds"; fi

gd_require_target

primary="$(k get clusters.postgresql.cnpg.io "$cluster" -o jsonpath='{.status.currentPrimary}' 2>/dev/null || true)"
[ -n "$primary" ] || gd_die "cannot find the primary of $cluster in $GD_NAMESPACE"
psql_orders() { k exec "$primary" -c postgres -- psql -X -q -v ON_ERROR_STOP=1 -d orders -Atc "$1"; }
can_insert() { psql_orders "select has_table_privilege('orders', 'orders_order', 'INSERT')"; }

grant() {
  psql_orders "GRANT INSERT ON TABLE orders_order TO orders" &&
    gd_record rollback "GRANT INSERT on orders_order to orders (insert allowed: $(can_insert))"
}

if [ "$rollback" = 1 ]; then
  gd_plan "on $primary: GRANT INSERT ON TABLE orders_order TO orders"
  gd_confirm
  grant || gd_die "GRANT failed: run it by hand with kubectl exec -it $primary -c postgres -- psql -d orders"
  exit 0
fi

[ "$(can_insert)" = "t" ] || gd_die "the orders role already cannot insert into orders_order; run --rollback first"
gd_plan "on $primary: REVOKE INSERT ON TABLE orders_order FROM orders, for ${duration}s" \
  "impact: every order creation in $GD_NAMESPACE fails (500); quotes and courier calls keep working" \
  "re-grant automatically when the time is up, on Ctrl-C, or on any exit"
gd_confirm

restore() {
  gd_say "restoring INSERT on orders_order"
  grant || gd_say "RE-GRANT FAILED: run $GD_NAME --rollback now"
}
gd_on_exit restore
psql_orders "REVOKE INSERT ON TABLE orders_order FROM orders"
gd_record inject "REVOKE INSERT on orders_order from orders on $primary for ${duration}s (insert allowed: $(can_insert))"

end=$(($(date +%s) + duration))
while [ "$(date +%s)" -lt "$end" ]; do
  left=$((end - $(date +%s)))
  gd_say "fault active, ${left}s left (Ctrl-C ends it early and re-grants)"
  sleep $((left < 30 ? left : 30))
done
gd_record observe "fault window of ${duration}s over"
