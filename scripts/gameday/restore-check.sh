#!/usr/bin/env bash
# GD-08: row counts and checksums for every table of a CloudNativePG database, and a comparison of two runs.
# shellcheck source-path=SCRIPTDIR
set -euo pipefail
# shellcheck source=lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

usage() {
  cat <<EOF
Usage: $GD_NAME --context CTX --namespace NS --cluster NAME [--database orders] [--out FILE]
       $GD_NAME --context CTX --namespace NS --compare FILE_A FILE_B

Read-only. On the primary of --cluster, for every table in schema public, prints
  <table> TAB <row count> TAB <md5 of all rows' text, ordered>
plus the newest orders_order.created_at (for RPO). Encrypted columns are hashed as stored; no value is printed.
Reads every row: fine at dev size. --out writes the table to FILE as well.
--compare diffs two such files: exit 0 when identical, 1 when not.

$GD_COMMON_USAGE
EOF
}

cluster=""
database="orders"
out=""
compare_a=""
compare_b=""
while [ $# -gt 0 ]; do
  gd_common_flag "$@"
  if [ "$GD_SHIFT" -gt 0 ]; then
    shift "$GD_SHIFT"
    continue
  fi
  case "$1" in
    --cluster)
      gd_need_value "$1" "${2:-}"
      cluster="$2"
      shift 2
      ;;
    --database)
      gd_need_value "$1" "${2:-}"
      database="$2"
      shift 2
      ;;
    --out)
      gd_need_value "$1" "${2:-}"
      out="$2"
      shift 2
      ;;
    --compare)
      gd_need_value "$1" "${2:-}"
      gd_need_value "$1" "${3:-}"
      compare_a="$2"
      compare_b="$3"
      shift 3
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *) gd_usage_error "unknown argument: $1" ;;
  esac
done
if [ -z "$compare_a" ]; then
  [ -n "$cluster" ] || gd_usage_error "--cluster or --compare is required"
  case "$cluster$database" in *[!a-z0-9_-]*) gd_usage_error "--cluster/--database: lowercase letters, digits, '-' and '_' only" ;; esac
fi

gd_require_target

if [ -n "$compare_a" ]; then
  if [ ! -f "$compare_a" ] || [ ! -f "$compare_b" ]; then gd_die "both files must exist"; fi
  gd_plan "compare $compare_a with $compare_b (local files only)"
  gd_confirm
  if diff -u "$compare_a" "$compare_b"; then
    gd_record observe "restore-check: $compare_a and $compare_b are identical"
  else
    gd_record observe "restore-check: $compare_a and $compare_b DIFFER (see the diff above)"
    exit 1
  fi
  exit 0
fi

primary="$(k get clusters.postgresql.cnpg.io "$cluster" -o jsonpath='{.status.currentPrimary}' 2>/dev/null || true)"
[ -n "$primary" ] || gd_die "cannot find the primary of $cluster in $GD_NAMESPACE"
gd_plan "on $primary, database $database: count and checksum every table in schema public (read-only)"
gd_confirm

psql_db() { k exec "$primary" -c postgres -- psql -X -q -v ON_ERROR_STOP=1 -d "$database" -Atc "$1"; }
# One query that builds the per-table SQL, so table names come from the catalogue, quoted by format('%I').
sql="$(psql_db "select string_agg(format(
  'select %L, count(*), coalesce(md5(string_agg(t::text, %L order by t::text)), %L) from public.%I t',
  tablename, '|', 'empty', tablename), ' union all ' order by tablename)
  from pg_tables where schemaname = 'public'")"
[ -n "$sql" ] || gd_die "no tables in schema public of $database"
result="$(psql_db "$sql" | tr '|' '\t' | sort)"
newest="$(psql_db "select coalesce(max(created_at)::text, 'none') from orders_order" 2>/dev/null || echo "n/a")"
printf '%s\n' "$result"
if [ -n "$out" ]; then printf '%s\n' "$result" >"$out"; fi
gd_record observe "restore-check $cluster: $(printf '%s\n' "$result" | grep -c .) tables; newest orders_order.created_at=$newest"
