#!/usr/bin/env bash
# Runs one load test against the local stack and writes a Markdown report.
#
#   scripts/loadtest/run.sh --scenario rainy-friday --start 11:45 --warmup 300 --window 900 \
#       --label "rainy-friday, one-tenth scale, rain peak" --out docs/loadtests/2026-10-16-rainy-friday.md
#
# It starts the simulator (Compose profile "sim") with the scenario and simulated start time you give,
# waits --warmup seconds for couriers to come online and orders to build up, measures for --window seconds
# with report.py, then stops the simulator. The rest of the stack keeps running: `make up` first.
set -euo pipefail

usage() {
  cat <<EOF
Usage: $(basename "$0") --scenario NAME [--start HH:MM] [--warmup SECONDS] [--window SECONDS]
                        [--interval SECONDS] [--label TEXT] [--out FILE] [--keep]

  --scenario   built-in simulator scenario (small, normal-weekday, rainy-friday, rainy-friday-design-scale)
  --start      simulated time of day to start at (default: the scenario's clock.start)
  --warmup     seconds between starting the simulator and starting to measure (default 300)
  --window     seconds to measure (default 900)
  --interval   seconds between timeline samples (default 60)
  --label      report title (default: the scenario name)
  --out        where to write the report (default: print it)
  --keep       leave the simulator running afterwards
EOF
}

scenario="" start="" warmup=300 window=900 interval=60 label="" out="" keep=false
while [ $# -gt 0 ]; do
  case "$1" in
    --scenario) scenario="${2:?}"; shift 2 ;;
    --start) start="${2:?}"; shift 2 ;;
    --warmup) warmup="${2:?}"; shift 2 ;;
    --window) window="${2:?}"; shift 2 ;;
    --interval) interval="${2:?}"; shift 2 ;;
    --label) label="${2:?}"; shift 2 ;;
    --out) out="${2:?}"; shift 2 ;;
    --keep) keep=true; shift ;;
    -h | --help) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done
[ -n "$scenario" ] || { usage >&2; exit 2; }
label="${label:-$scenario}"

repo="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repo"
compose=(docker compose --profile sim)

if ! curl -fsS -o /dev/null http://127.0.0.1:8000/readyz; then
  echo "The orders API is not ready on :8000. Start the stack first: make up" >&2
  exit 1
fi

args=(run -scenario "$scenario" -fresh)
[ -n "$start" ] && args+=(-start "$start")

echo "Building the simulator image, then starting: simulator ${args[*]}"
"${compose[@]}" build --quiet simulator
"${compose[@]}" rm --force --stop simulator >/dev/null 2>&1 || true
docker rm --force tasuki-loadtest-simulator >/dev/null 2>&1 || true
"${compose[@]}" run --detach --service-ports --name tasuki-loadtest-simulator simulator "${args[@]}" >/dev/null

cleanup() {
  if [ "$keep" = false ]; then
    echo "Stopping the simulator (it drains deliveries in hand first)."
    docker stop --time 40 tasuki-loadtest-simulator >/dev/null 2>&1 || true
    docker rm tasuki-loadtest-simulator >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

echo "Waiting for the simulator to seed and start..."
for _ in $(seq 1 120); do
  curl -fsS -o /dev/null http://127.0.0.1:8090/readyz 2>/dev/null && break
  sleep 5
done
curl -fsS -o /dev/null http://127.0.0.1:8090/readyz || { echo "simulator never became ready" >&2; exit 1; }

echo "Warming up for ${warmup}s, then measuring for ${window}s."
sleep "$warmup"

report=(python3 "$repo/scripts/loadtest/report.py" --window "$window" --interval "$interval" --label "$label"
  --scenario "$scenario${start:+, starting at $start simulated time}")
[ -n "$out" ] && report+=(--out "$out")
"${report[@]}"
