# Shared by scripts/gameday/*.sh. Source it; don't run it.
# The GD_* variables are read by the scripts that source this file, hence SC2034.
# shellcheck shell=bash disable=SC2034
#
# Every game-day script:
#   - needs --context and --namespace, with no defaults, and accepts only tasuki-dev or tasuki-prod;
#   - refuses tasuki-prod unless ALLOW_PROD=yes is set in the environment;
#   - prints what it will do and asks before doing it (--yes skips the prompt for automation);
#   - appends what it did to the drill log when GAMEDAY_LOG is set (see record-timeline.sh).
# Exit codes: 0 done, 1 refused or failed, 2 bad usage.
# Works with the bash 3.2 that macOS ships: no associative arrays, no mapfile, no ${var,,}.

GD_NAME="$(basename "$0")"
GD_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GD_REPO_ROOT="$(cd "$GD_LIB_DIR/../../.." && pwd)"
GD_CONTEXT=""
GD_NAMESPACE=""
GD_ENV=""
GD_YES=0
GD_PROM_URL="${PROM_URL:-}"
GD_PF_PID=""
GD_SHIFT=0
GD_CLEANUP_HOOKS=""

# --- output ------------------------------------------------------------------------------------------

# Timestamps are JST, with the offset, so drill logs read the same wherever they were written.
gd_now() { TZ=Asia/Tokyo date '+%Y-%m-%dT%H:%M:%S%z'; }

gd_say() { printf '%s %s\n' "$(gd_now)" "$*" >&2; }

gd_die() {
  echo "$GD_NAME: $*" >&2
  exit 1
}

gd_usage_error() {
  echo "$GD_NAME: $*" >&2
  echo "Run '$GD_NAME --help' for usage." >&2
  exit 2
}

# Prints a line and, when GAMEDAY_LOG is set, appends it to the drill log with a timestamp and a kind
# (inject, observe, rollback, ...). Never pass secret values to this function.
gd_record() {
  local kind="$1"
  shift
  gd_say "[$kind] $*"
  if [ -n "${GAMEDAY_LOG:-}" ]; then
    [ -f "$GAMEDAY_LOG" ] || gd_die "GAMEDAY_LOG=$GAMEDAY_LOG does not exist (create it with record-timeline.sh start)"
    printf -- '- %s **%s** (%s, %s) %s\n' "$(gd_now)" "$kind" "$GD_NAME" "${GD_NAMESPACE:-?}" "$*" >>"$GAMEDAY_LOG"
  fi
}

# --- arguments ---------------------------------------------------------------------------------------

gd_need_value() {
  case "${2:-}" in
    "" | --*) gd_usage_error "$1 needs a value" ;;
  esac
}

# Handles the flags every script shares. Sets GD_SHIFT to the number of arguments it used (0: not a common flag).
gd_common_flag() {
  GD_SHIFT=0
  case "$1" in
    --context)
      gd_need_value "$1" "${2:-}"
      GD_CONTEXT="$2"
      GD_SHIFT=2
      ;;
    --context=*)
      GD_CONTEXT="${1#*=}"
      GD_SHIFT=1
      ;;
    --namespace | -n)
      gd_need_value "$1" "${2:-}"
      GD_NAMESPACE="$2"
      GD_SHIFT=2
      ;;
    --namespace=*)
      GD_NAMESPACE="${1#*=}"
      GD_SHIFT=1
      ;;
    --yes | -y)
      GD_YES=1
      GD_SHIFT=1
      ;;
    --prom-url)
      gd_need_value "$1" "${2:-}"
      GD_PROM_URL="$2"
      GD_SHIFT=2
      ;;
  esac
  return 0
}

GD_COMMON_USAGE="Common options:
  --context NAME     kube context to use (required; no default, on purpose)
  --namespace NS     tasuki-dev or tasuki-prod (required). tasuki-prod also needs ALLOW_PROD=yes
  --yes              do not ask for confirmation (automation)
  --prom-url URL     use this Prometheus instead of a port-forward to monitoring/prometheus-operated
  -h, --help         show this help

Environment: ALLOW_PROD=yes (required for tasuki-prod), GAMEDAY_LOG (drill log to append to),
PROM_URL (same as --prom-url)."

# --- guards ------------------------------------------------------------------------------------------

gd_require_tools() {
  local tool
  for tool in "$@"; do
    command -v "$tool" >/dev/null 2>&1 || gd_die "$tool is required but not on PATH"
  done
}

# The target guard. Checks only local state (arguments, environment, kubeconfig), so it works without a cluster.
gd_require_target() {
  [ -n "$GD_CONTEXT" ] || gd_usage_error "--context is required (there is no default: name the cluster explicitly)"
  [ -n "$GD_NAMESPACE" ] || gd_usage_error "--namespace is required (tasuki-dev or tasuki-prod)"
  case "$GD_NAMESPACE" in
    tasuki-dev) ;;
    tasuki-prod)
      [ "${ALLOW_PROD:-}" = "yes" ] ||
        gd_die "refusing to touch tasuki-prod without ALLOW_PROD=yes. Prod drills run only in a scheduled, announced window (docs/gameday/README.md)."
      ;;
    *) gd_usage_error "--namespace must be tasuki-dev or tasuki-prod, not '$GD_NAMESPACE'" ;;
  esac
  GD_ENV="${GD_NAMESPACE#tasuki-}"
  gd_require_tools kubectl
  # No grep -q: with pipefail, an early exit could kill kubectl with SIGPIPE and turn a match into a failure.
  if ! kubectl config get-contexts -o name 2>/dev/null | grep -Fx -- "$GD_CONTEXT" >/dev/null; then
    gd_die "kube context '$GD_CONTEXT' is not in your kubeconfig (see: kubectl config get-contexts)"
  fi
}

# --- confirmation ------------------------------------------------------------------------------------

# Prints the plan: the target and one line per action.
gd_plan() {
  {
    echo "== $GD_NAME"
    echo "   context:   $GD_CONTEXT"
    echo "   namespace: $GD_NAMESPACE"
    if [ -n "${GAMEDAY_LOG:-}" ]; then echo "   drill log: $GAMEDAY_LOG"; fi
    echo "   will:"
    local line
    for line in "$@"; do echo "     - $line"; done
  } >&2
}

# Asks before acting. In tasuki-prod the answer must be the namespace name, elsewhere "yes".
gd_confirm() {
  if [ "$GD_YES" = 1 ]; then
    gd_say "--yes given: not asking"
    return 0
  fi
  [ -t 0 ] || gd_die "stdin is not a terminal, so I cannot ask; pass --yes to run without the prompt"
  local expect="yes" answer=""
  if [ "$GD_NAMESPACE" = "tasuki-prod" ]; then expect="tasuki-prod"; fi
  printf 'Type "%s" to continue (anything else aborts): ' "$expect" >&2
  read -r answer || answer=""
  [ "$answer" = "$expect" ] || gd_die "aborted; nothing was changed"
}

# --- kubectl -----------------------------------------------------------------------------------------

# Namespaced and cluster-scoped kubectl, always with the explicit context.
k() { kubectl --context "$GD_CONTEXT" --namespace "$GD_NAMESPACE" "$@"; }
kc() { kubectl --context "$GD_CONTEXT" "$@"; }

# Waits until a command succeeds, polling every $2 seconds for at most $1 seconds. Returns 1 on timeout.
gd_wait_until() {
  local timeout="$1" interval="$2" start
  shift 2
  start="$(date +%s)"
  until "$@"; do
    if [ $(($(date +%s) - start)) -ge "$timeout" ]; then return 1; fi
    sleep "$interval"
  done
}

# Where scripts keep what their --rollback needs (version IDs, cordoned nodes). Outside the repository.
gd_state_dir() {
  local dir="${XDG_STATE_HOME:-$HOME/.local/state}/tasuki-gameday/${GD_CONTEXT//[^A-Za-z0-9._-]/_}/$GD_NAMESPACE"
  mkdir -p "$dir"
  chmod 700 "$dir"
  echo "$dir"
}

# --- Prometheus --------------------------------------------------------------------------------------

# Port-forwards to monitoring/prometheus-operated unless --prom-url or PROM_URL is set. Stopped on exit.
gd_prom_start() {
  gd_require_tools curl jq
  if [ -n "$GD_PROM_URL" ]; then return 0; fi
  local port="${GAMEDAY_PROM_PORT:-19090}"
  kc --namespace monitoring port-forward svc/prometheus-operated "$port:9090" >/dev/null 2>&1 &
  GD_PF_PID=$!
  GD_PROM_URL="http://127.0.0.1:$port"
  if ! gd_wait_until 30 1 curl -fsS --max-time 2 -o /dev/null "$GD_PROM_URL/-/ready"; then
    gd_die "Prometheus did not answer on $GD_PROM_URL (port-forward to monitoring/prometheus-operated failed?)"
  fi
}

# Runs an instant query and prints the raw JSON result array, or nothing on error.
gd_prom_query() {
  curl -fsS --max-time 15 --get --data-urlencode "query=$1" "$GD_PROM_URL/api/v1/query" 2>/dev/null |
    jq -c 'select(.status == "success") | .data.result' 2>/dev/null || true
}

# Prints one number for a query: the single series' value (or "-" for no data, "error" for a failed query).
# Several series are printed as "label=value" pairs on one line.
gd_prom_value() {
  local result
  result="$(gd_prom_query "$1")"
  if [ -z "$result" ]; then
    echo "error"
    return 0
  fi
  printf '%s' "$result" | jq -r '
    def fmt: if test("^-?[0-9.]+(e[-+]?[0-9]+)?$") then (tonumber * 10000 | round / 10000 | tostring) else . end;
    if length == 0 then "-"
    elif length == 1 and (.[0].metric | del(.__name__) | length) == 0 then (.[0].value[1] | fmt)
    else [ .[] | ((.metric | del(.__name__) | to_entries | map(.value) | join("/")) + "=" + (.value[1] | fmt)) ]
         | join("  ")
    end'
}

# --- exit handling -----------------------------------------------------------------------------------

# Registers a function to run on exit (after Ctrl-C too). Hooks run in the order they were added.
gd_on_exit() { GD_CLEANUP_HOOKS="$GD_CLEANUP_HOOKS $1"; }

gd_cleanup() {
  local rc=$? hook
  trap - EXIT
  for hook in $GD_CLEANUP_HOOKS; do
    "$hook" || true
  done
  if [ -n "$GD_PF_PID" ]; then kill "$GD_PF_PID" 2>/dev/null || true; fi
  exit "$rc"
}

trap gd_cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
