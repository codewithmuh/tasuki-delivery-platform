#!/usr/bin/env bash
# Proves every game-day script's guards without a cluster (make gameday-test):
#   --help exits 0; no arguments exits 2; tasuki-prod without ALLOW_PROD is refused (exit 1, says ALLOW_PROD);
#   an unknown kube context is refused (exit 1); a namespace other than tasuki-dev/tasuki-prod is a usage error.
# It uses an empty kubeconfig, so no command can reach a real cluster even by mistake.
set -euo pipefail

dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
printf 'apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n' >"$work/kubeconfig"
export KUBECONFIG="$work/kubeconfig"
unset ALLOW_PROD GAMEDAY_LOG PROM_URL

# script <TAB> the extra arguments it needs to get past its own usage checks
cases="$(
  cat <<'EOF'
preflight.sh
watch-slis.sh	--once
redis-loss.sh	--mode delete-pod
kafka-broker-loss.sh	--brokers 1
pg-failover.sh	--mode kill-primary
node-loss.sh	--node ip-10-0-0-1 --mode drain
pod-kill.sh	--app routing
broken-secret.sh	--key FLEET_STREAM_TOKEN --mode remove-key
orders-5xx.sh	--duration 60
gate-check.sh	--service orders
pg-restore.sh
restore-check.sh	--cluster orders-db
record-timeline.sh start	--experiment gd-00-test
EOF
)"

fails=0
pass() { printf 'ok    %s\n' "$*"; }
fail() {
  printf 'FAIL  %s\n' "$*"
  fails=$((fails + 1))
}

# expect <code> <grep pattern or ""> <description> -- command...
expect() {
  local want="$1" pattern="$2" what="$3" out rc=0
  shift 4
  out="$("$@" </dev/null 2>&1)" || rc=$?
  if [ "$rc" != "$want" ]; then
    fail "$what: exit $rc, want $want: $(printf '%s' "$out" | tail -n 1)"
  elif [ -n "$pattern" ] && ! printf '%s' "$out" | grep -E -- "$pattern" >/dev/null; then
    fail "$what: output does not match /$pattern/: $(printf '%s' "$out" | tail -n 1)"
  else
    pass "$what"
  fi
}

while IFS=$'\t' read -r script extra; do
  [ -n "$script" ] || continue
  # "record-timeline.sh start" is a script plus its subcommand.
  # shellcheck disable=SC2086 # split "record-timeline.sh start" into the script and its subcommand
  set -- $script
  file="$1"
  shift
  sub=("$@")
  # shellcheck disable=SC2206 # extra is a fixed, space-separated argument list from the table above
  args=($extra)
  run=("$dir/$file" ${sub[@]+"${sub[@]}"})
  expect 0 "Usage" "$script --help" -- "${run[@]}" --help
  expect 2 "required" "$script with no arguments" -- "${run[@]}"
  expect 2 "" "$script with a bad namespace" -- "${run[@]}" ${args[@]+"${args[@]}"} --context x --namespace kube-system
  expect 1 "ALLOW_PROD" "$script refuses tasuki-prod without ALLOW_PROD" -- \
    "${run[@]}" ${args[@]+"${args[@]}"} --context x --namespace tasuki-prod
  expect 1 "not in your kubeconfig" "$script refuses an unknown context (even with ALLOW_PROD=yes)" -- \
    env ALLOW_PROD=yes "${run[@]}" ${args[@]+"${args[@]}"} --context no-such-context --namespace tasuki-prod
  expect 1 "not in your kubeconfig" "$script refuses an unknown context in tasuki-dev" -- \
    "${run[@]}" ${args[@]+"${args[@]}"} --context no-such-context --namespace tasuki-dev
done <<<"$cases"

# record-timeline.sh note/mark need a log; mark needs a known phase.
expect 2 "no drill log" "record-timeline.sh note without a log" -- "$dir/record-timeline.sh" note hello
printf '# Drill log: test\n\n- context: x\n- namespace: tasuki-dev\n' >"$work/log.md"
expect 0 "" "record-timeline.sh note appends" -- "$dir/record-timeline.sh" note --log "$work/log.md" hello world
expect 2 "unknown phase" "record-timeline.sh mark rejects an unknown phase" -- \
  "$dir/record-timeline.sh" mark --log "$work/log.md" lunch
expect 0 "" "record-timeline.sh mark inject" -- "$dir/record-timeline.sh" mark --log "$work/log.md" inject
if grep -q 'note: hello world' "$work/log.md" && grep -q '\*\*inject\*\*' "$work/log.md"; then
  pass "drill log contains the note and the mark"
else
  fail "drill log is missing the note or the mark"
fi

echo
if [ "$fails" -gt 0 ]; then
  echo "$fails guard check(s) failed"
  exit 1
fi
echo "all guard checks passed"
