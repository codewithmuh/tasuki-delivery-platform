#!/usr/bin/env bash
# The drill log: a timestamped Markdown file that becomes the timeline of the results and the postmortem.
# shellcheck source-path=SCRIPTDIR
set -euo pipefail
# shellcheck source=lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

PHASES="steady-state inject detect mitigate recovered abort end"

usage() {
  cat <<EOF
Usage:
  $GD_NAME start --context CTX --namespace NS --experiment ID [--log FILE] [--driver NAME] [--print-path]
  $GD_NAME note [--log FILE] TEXT...
  $GD_NAME mark [--log FILE] PHASE        PHASE: ${PHASES// /, }
  $GD_NAME snapshot [--log FILE]          append one watch-slis.sh sample (context and namespace from the log)

start creates the log (default docs/gameday/logs/<date>-<experiment>.md) after confirming the target, and
prints the line to export it as GAMEDAY_LOG; with --print-path it prints only the path, for
  export GAMEDAY_LOG="\$($GD_NAME start ... --print-path)"
note, mark and snapshot append to \$GAMEDAY_LOG (or --log). The injection scripts append their own actions to
the same file. Times are JST. Never write secrets into the log: it is committed with the results.

$GD_COMMON_USAGE
EOF
}

[ $# -gt 0 ] || gd_usage_error "a subcommand is required: start, note, mark or snapshot"
cmd="$1"
shift
case "$cmd" in
  -h | --help)
    usage
    exit 0
    ;;
  start | note | mark | snapshot) ;;
  *) gd_usage_error "unknown subcommand: $cmd" ;;
esac

log="${GAMEDAY_LOG:-}"
experiment=""
driver="${USER:-unknown}"
print_path=0
words=""
while [ $# -gt 0 ]; do
  gd_common_flag "$@"
  if [ "$GD_SHIFT" -gt 0 ]; then
    shift "$GD_SHIFT"
    continue
  fi
  case "$1" in
    --log)
      gd_need_value "$1" "${2:-}"
      log="$2"
      shift 2
      ;;
    --experiment)
      gd_need_value "$1" "${2:-}"
      experiment="$2"
      shift 2
      ;;
    --driver)
      gd_need_value "$1" "${2:-}"
      driver="$2"
      shift 2
      ;;
    --print-path)
      print_path=1
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    --*) gd_usage_error "unknown option: $1" ;;
    *)
      words="${words:+$words }$1"
      shift
      ;;
  esac
done

append() { printf '%s\n' "$*" >>"$log"; }

require_log() {
  [ -n "$log" ] || gd_usage_error "no drill log: set GAMEDAY_LOG or pass --log (create one with: $GD_NAME start ...)"
  [ -f "$log" ] || gd_die "drill log $log does not exist (create it with: $GD_NAME start ...)"
}

# Reads "- context: X" style header fields from the log.
header() { sed -n "s/^- $1: //p" "$log" | head -n 1; }

case "$cmd" in
  start)
    [ -n "$experiment" ] || gd_usage_error "--experiment is required (e.g. gd-01-redis-loss)"
    case "$experiment" in *[!a-z0-9-]*) gd_usage_error "--experiment may use lowercase letters, digits and '-' only" ;; esac
    gd_require_target
    if [ -z "$log" ] || [ "$log" = "${GAMEDAY_LOG:-}" ]; then
      log="$GD_REPO_ROOT/docs/gameday/logs/$(TZ=Asia/Tokyo date +%Y-%m-%d)-$experiment.md"
    fi
    [ ! -e "$log" ] || gd_die "$log already exists; pass --log to choose another file, or keep appending with note/mark"
    gd_plan "create the drill log $log"
    gd_confirm
    mkdir -p "$(dirname "$log")"
    {
      echo "# Drill log: $experiment"
      echo
      echo "- experiment: $experiment"
      echo "- context: $GD_CONTEXT"
      echo "- namespace: $GD_NAMESPACE"
      echo "- started: $(gd_now)"
      echo "- driver: $driver"
      echo
      echo "Times are JST. Lines marked by a script name were written by that script. Nothing here is edited"
      echo "after the drill; corrections go in the results section of the plan."
      echo
      echo "## Timeline"
      echo
    } >"$log"
    if [ "$print_path" = 1 ]; then
      echo "$log"
    else
      echo "Drill log created. Use it in this shell with:"
      echo "  export GAMEDAY_LOG=\"$log\""
    fi
    ;;
  note)
    require_log
    [ -n "$words" ] || gd_usage_error "note needs text"
    append "- $(gd_now) note: $words"
    ;;
  mark)
    require_log
    [ -n "$words" ] || gd_usage_error "mark needs a phase: ${PHASES// /, }"
    case " $PHASES " in *" $words "*) ;; *) gd_usage_error "unknown phase '$words' (use: ${PHASES// /, })" ;; esac
    append "- $(gd_now) **${words}**"
    gd_say "marked $words"
    ;;
  snapshot)
    require_log
    GD_CONTEXT="$(header context)"
    GD_NAMESPACE="$(header namespace)"
    if [ -z "$GD_CONTEXT" ] || [ -z "$GD_NAMESPACE" ]; then
      gd_die "$log has no context/namespace header"
    fi
    gd_require_target
    gd_say "taking a snapshot of $GD_NAMESPACE"
    sample="$("$(dirname "${BASH_SOURCE[0]}")/watch-slis.sh" --context "$GD_CONTEXT" --namespace "$GD_NAMESPACE" \
      --once --yes ${GD_PROM_URL:+--prom-url "$GD_PROM_URL"})"
    {
      echo "- $(gd_now) snapshot:"
      echo
      echo '  ```text'
      printf '%s\n' "$sample" | sed 's/^/  /'
      echo '  ```'
      echo
    } >>"$log"
    printf '%s\n' "$sample"
    ;;
esac
