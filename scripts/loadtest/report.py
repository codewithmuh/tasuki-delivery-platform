#!/usr/bin/env python3
"""Measure a load test against the local stack and write the results as Markdown.

There is no Prometheus in the Compose stack, so this script plays its part: it scrapes every service's
/metrics at the start of a window, again every --interval seconds, and at the end, then computes rates,
latency percentiles and the SLIs from the differences, exactly as PromQL's rate() and histogram_quantile()
would. Read-only: it never calls an API that changes anything.

    python3 scripts/loadtest/report.py --window 900 --label "rainy-friday, rain peak" \
        --out docs/loadtests/2026-10-16-rainy-friday.md

Standard library only (Python 3.10+). Endpoints default to the host ports in compose.yaml.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import math
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
from collections import defaultdict
from collections.abc import Callable, Iterable
from dataclasses import dataclass, field

TARGETS = {
    "orders": "http://127.0.0.1:8000/metrics",
    "fleet-ingest": "http://127.0.0.1:8081/metrics",
    "dispatch": "http://127.0.0.1:8082/metrics",
    "tracking": "http://127.0.0.1:8083/metrics",
    "fleet-state": "http://127.0.0.1:8084/metrics",
    "simulator": "http://127.0.0.1:8090/metrics",
}

# The SLOs from observability/slo/specs, so a local run is judged by the same bar as production.
SLO_ORDERS_AVAILABILITY = 0.999
SLO_ORDERS_LATENCY = (0.3, 0.95)  # POST /v1/orders under 300 ms, 95 %
SLO_LOCATION_FRESHNESS = (2.0, 0.99)
SLO_TRACKING_FRESHNESS = (10.0, 0.99)
SLO_DISPATCH_SPEED = (60.0, 0.95)

Labels = frozenset[tuple[str, str]]
SAMPLE = re.compile(r"^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{(.*)\})?\s+(\S+)")
LABEL = re.compile(r'([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"')


@dataclass
class Scrape:
    """One scrape of every target: (service, metric name, labels) -> value."""

    at: float
    values: dict[tuple[str, str, Labels], float] = field(default_factory=dict)
    missing: list[str] = field(default_factory=list)


def parse(text: str) -> Iterable[tuple[str, Labels, float]]:
    for line in text.splitlines():
        if not line or line.startswith("#"):
            continue
        m = SAMPLE.match(line)
        if not m:
            continue
        labels = frozenset(LABEL.findall(m.group(3) or ""))
        try:
            yield m.group(1), labels, float(m.group(4))
        except ValueError:
            continue


def scrape(targets: dict[str, str]) -> Scrape:
    s = Scrape(at=time.time())
    for service, url in targets.items():
        try:
            with urllib.request.urlopen(url, timeout=5) as resp:
                text = resp.read().decode()
        except (urllib.error.URLError, TimeoutError, ConnectionError):
            s.missing.append(service)
            continue
        for name, labels, value in parse(text):
            s.values[(service, name, labels)] = value
    return s


Match = Callable[[dict[str, str]], bool]


def everything(_: dict[str, str]) -> bool:
    return True


class Window:
    """The difference between two scrapes, read like PromQL over that window."""

    def __init__(self, start: Scrape, end: Scrape):
        self.start, self.end = start, end
        self.seconds = max(end.at - start.at, 1e-9)

    def _series(
        self, service: str, name: str, match: Match
    ) -> Iterable[tuple[Labels, float, float]]:
        for (svc, n, labels), v1 in self.end.values.items():
            if svc == service and n == name and match(dict(labels)):
                v0 = self.start.values.get((svc, n, labels), 0.0)
                # A counter that went down means the process restarted: count from zero, like rate() does.
                yield labels, v0, v1

    def increase(self, service: str, name: str, match: Match = everything) -> float:
        return sum(
            v1 - v0 if v1 >= v0 else v1
            for _, v0, v1 in self._series(service, name, match)
        )

    def rate(self, service: str, name: str, match: Match = everything) -> float:
        return self.increase(service, name, match) / self.seconds

    def by(
        self, service: str, name: str, label: str, match: Match = everything
    ) -> dict[str, float]:
        out: dict[str, float] = defaultdict(float)
        for labels, v0, v1 in self._series(service, name, match):
            out[dict(labels).get(label, "")] += v1 - v0 if v1 >= v0 else v1
        return dict(out)

    def gauge(self, service: str, name: str, match: Match = everything) -> float | None:
        vals = [
            v
            for (svc, n, labels), v in self.end.values.items()
            if svc == service and n == name and match(dict(labels))
        ]
        return sum(vals) if vals else None

    def buckets(
        self, service: str, name: str, match: Match = everything
    ) -> list[tuple[float, float]]:
        """Cumulative (upper bound, count) pairs of a histogram's increase, summed over matching series."""
        acc: dict[float, float] = defaultdict(float)
        for labels, v0, v1 in self._series(service, name + "_bucket", match):
            le = dict(labels).get("le", "+Inf")
            acc[math.inf if le == "+Inf" else float(le)] += v1 - v0 if v1 >= v0 else v1
        return sorted(acc.items())

    def count(self, service: str, name: str, match: Match = everything) -> float:
        b = self.buckets(service, name, match)
        return b[-1][1] if b else 0.0

    def quantile(
        self, q: float, service: str, name: str, match: Match = everything
    ) -> float | None:
        """histogram_quantile(): linear interpolation inside the bucket that holds the rank."""
        b = self.buckets(service, name, match)
        if not b or b[-1][1] <= 0:
            return None
        rank = q * b[-1][1]
        prev_le, prev_count = 0.0, 0.0
        for le, cnt in b:
            if cnt >= rank:
                if math.isinf(le):
                    return prev_le  # Prometheus answers the highest finite bound
                if cnt == prev_count:
                    return le
                return prev_le + (le - prev_le) * (rank - prev_count) / (
                    cnt - prev_count
                )
            prev_le, prev_count = le, cnt
        return None

    def share_under(
        self, threshold: float, service: str, name: str, match: Match = everything
    ) -> float | None:
        """Share of observations at or below threshold (interpolated if no bucket sits exactly on it)."""
        b = self.buckets(service, name, match)
        if not b or b[-1][1] <= 0:
            return None
        total = b[-1][1]
        prev_le, prev_count = 0.0, 0.0
        for le, cnt in b:
            if le >= threshold:
                if le == threshold or math.isinf(le):
                    good = cnt if le == threshold else prev_count
                else:
                    good = prev_count + (cnt - prev_count) * (threshold - prev_le) / (
                        le - prev_le
                    )
                return good / total
            prev_le, prev_count = le, cnt
        return 1.0


# ---- Formatting ----------------------------------------------------------------------------------------


def pct(x: float | None, digits: int = 2) -> str:
    return "n/a" if x is None else f"{x * 100:.{digits}f} %"


def ms(x: float | None) -> str:
    return "n/a" if x is None else f"{x * 1000:.0f} ms"


def secs(x: float | None) -> str:
    return "n/a" if x is None else f"{x:.1f} s"


def num(x: float | None, digits: int = 1) -> str:
    return "n/a" if x is None else f"{x:,.{digits}f}"


def verdict(value: float | None, target: float) -> str:
    if value is None:
        return "no data"
    return "met" if value >= target else "**missed**"


def route(path: str, method: str = "POST") -> Match:
    """orders labels routes "/v1/orders" (plus a method label); the Go services "POST /v1/locations"."""

    def match(labels: dict[str, str]) -> bool:
        r = labels.get("route")
        return labels.get("method", method) == method and r in (
            path,
            f"{method} {path}",
        )

    return match


def is_accepted(labels: dict[str, str]) -> bool:
    return labels.get("result") == "accepted"


def is_assigned(labels: dict[str, str]) -> bool:
    return labels.get("outcome") == "assigned"


def is_no_courier(labels: dict[str, str]) -> bool:
    return labels.get("outcome") == "no_courier"


def call_is(call: str) -> Match:
    def match(labels: dict[str, str]) -> bool:
        return labels.get("call") == call

    return match


def server_error(inner: Match) -> Match:
    def match(labels: dict[str, str]) -> bool:
        return inner(labels) and labels.get("status", "").startswith("5")

    return match


def docker_stats() -> list[tuple[str, str, str]]:
    try:
        out = subprocess.run(
            [
                "docker",
                "stats",
                "--no-stream",
                "--format",
                "{{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}",
            ],
            capture_output=True,
            text=True,
            timeout=30,
            check=True,
        ).stdout
    except (OSError, subprocess.SubprocessError):
        return []
    # Only this project's containers: Compose names them tasuki-*, and so does run.sh.
    lines = out.strip().splitlines()
    rows = [tuple(line.split("\t")) for line in lines if line.startswith("tasuki")]
    return sorted((r for r in rows if len(r) == 3), key=lambda r: r[0])  # type: ignore[misc]


# ---- The report ----------------------------------------------------------------------------------------


def dispatch_speed(w: Window) -> float | None:
    """Orders with an accepted courier within 60 s, over every order dispatch finished timing (incl. no courier)."""
    hist = "dispatch_assignment_duration_seconds"
    total = w.count("dispatch", hist)
    share = w.share_under(SLO_DISPATCH_SPEED[0], "dispatch", hist, is_assigned)
    if not total or share is None:
        return None
    return share * w.count("dispatch", hist, is_assigned) / total


def summary_line(w: Window) -> str:
    created = w.rate("simulator", "simulator_orders_created_total") * 3600
    points = w.rate("simulator", "simulator_location_points_total", is_accepted)
    p95 = w.quantile(
        0.95, "orders", "http_request_duration_seconds", route("/v1/orders")
    )
    fresh = w.share_under(2.0, "fleet-state", "fleet_state_freshness_seconds")
    return (
        f"orders/h {created:,.0f} · GPS pts/s {points:,.0f} · POST /v1/orders p95 {ms(p95)} · "
        f"location fresh ≤2s {pct(fresh)} · assigned ≤60s {pct(dispatch_speed(w))}"
    )


def report(
    w: Window,
    label: str,
    scenario: str,
    stats: list[tuple[str, str, str]],
    samples: list[str],
) -> str:
    sim = "simulator"
    lines: list[str] = []
    add = lines.append
    started = time.strftime("%Y-%m-%d %H:%M:%S UTC", time.gmtime(w.start.at))
    add(f"# Load test: {label}\n")
    add("| | |\n|---|---|")
    add(f"| Started | {started} |")
    add(f"| Window | {w.seconds / 60:.1f} min ({w.seconds:.0f} s) |")
    add(f"| Scenario | {scenario or 'see the run notes'} |")
    add(
        "| Measured by | `scripts/loadtest/report.py` (scrapes every service's `/metrics`; PromQL-equivalent maths) |"
    )
    missing = sorted(set(w.start.missing) | set(w.end.missing))
    if missing:
        add(
            f"| Not reachable | {', '.join(missing)} (their numbers are missing below) |"
        )
    add("")

    # Load
    created = w.increase(sim, "simulator_orders_created_total")
    attempted = w.increase(sim, "simulator_orders_attempted_total")
    failed = w.by(sim, "simulator_orders_failed_total", "reason")
    demand = w.gauge(sim, "simulator_demand_orders_per_hour")
    add("## Load the simulator generated\n")
    add("| What | Total | Per second | Per hour |\n|---|---:|---:|---:|")
    add(
        f"| Orders attempted | {num(attempted, 0)} | {num(attempted / w.seconds, 2)} | {num(attempted / w.seconds * 3600, 0)} |"
    )
    add(
        f"| Orders created | {num(created, 0)} | {num(created / w.seconds, 2)} | {num(created / w.seconds * 3600, 0)} |"
    )
    for reason, n in sorted(failed.items()):
        add(f"| Orders failed: `{reason}` | {num(n, 0)} | {num(n / w.seconds, 2)} | |")
    for res, n in sorted(
        w.by(sim, "simulator_location_points_total", "result").items()
    ):
        add(f"| GPS points {res} | {num(n, 0)} | {num(n / w.seconds, 1)} | |")
    for res, n in sorted(
        w.by(sim, "simulator_location_batches_total", "result").items()
    ):
        add(f"| GPS batches {res} | {num(n, 0)} | {num(n / w.seconds, 1)} | |")
    for outcome, n in sorted(w.by(sim, "simulator_offers_total", "outcome").items()):
        add(f"| Offers {outcome} | {num(n, 0)} | {num(n / w.seconds, 2)} | |")
    deliveries = w.increase(sim, "simulator_deliveries_completed_total")
    add(
        f"| Deliveries completed | {num(deliveries, 0)} | {num(deliveries / w.seconds, 2)} | {num(deliveries / w.seconds * 3600, 0)} |"
    )
    add("")
    couriers = w.gauge(sim, "simulator_couriers_active")
    add(
        f"Couriers online at the end: **{num(couriers, 0)}**. "
        f"Intended demand at the end (scenario curve × weather): **{num(demand, 0)} orders/h**.\n"
    )

    # SLOs
    add("## SLOs\n")
    add("| SLO | Target | Measured | Verdict |\n|---|---:|---:|---|")
    o_total = sum(
        w.increase("orders", "http_requests_total", route(p))
        for p in ("/v1/quotes", "/v1/orders")
    )
    o_5xx = sum(
        w.increase("orders", "http_requests_total", server_error(route(p)))
        for p in ("/v1/quotes", "/v1/orders")
    )
    avail = None if not o_total else 1 - o_5xx / o_total
    add(
        f"| Quote and order requests succeed (not 5xx) | {pct(SLO_ORDERS_AVAILABILITY, 1)} | {pct(avail, 3)} "
        f"({num(o_5xx, 0)} of {num(o_total, 0)} failed) | {verdict(avail, SLO_ORDERS_AVAILABILITY)} |"
    )
    lat = w.share_under(
        SLO_ORDERS_LATENCY[0],
        "orders",
        "http_request_duration_seconds",
        route("/v1/orders"),
    )
    add(
        f"| `POST /v1/orders` under 300 ms | {pct(SLO_ORDERS_LATENCY[1], 0)} | {pct(lat)} | {verdict(lat, SLO_ORDERS_LATENCY[1])} |"
    )
    loc = w.share_under(
        SLO_LOCATION_FRESHNESS[0], "fleet-state", "fleet_state_freshness_seconds"
    )
    add(
        f"| GPS points in live state within 2 s | {pct(SLO_LOCATION_FRESHNESS[1], 0)} | {pct(loc)} | {verdict(loc, SLO_LOCATION_FRESHNESS[1])} |"
    )
    trk = w.share_under(
        SLO_TRACKING_FRESHNESS[0], "tracking", "tracking_event_age_seconds"
    )
    add(
        f"| Viewers see positions younger than 10 s | {pct(SLO_TRACKING_FRESHNESS[1], 0)} | {pct(trk)} | {verdict(trk, SLO_TRACKING_FRESHNESS[1])} |"
    )
    d_all = w.count("dispatch", "dispatch_assignment_duration_seconds")
    speed = dispatch_speed(w)
    add(
        f"| A courier accepts within 60 s | {pct(SLO_DISPATCH_SPEED[1], 0)} | {pct(speed)} | {verdict(speed, SLO_DISPATCH_SPEED[1])} |"
    )
    add("")

    # Latency
    add("## Latency\n")
    add("Server side (`http_request_duration_seconds`, what the SLO measures):\n")
    add("| Endpoint | Requests/s | p50 | p95 | p99 |\n|---|---:|---:|---:|---:|")
    server_rows = [
        ("orders", "POST /v1/quotes", route("/v1/quotes")),
        ("orders", "POST /v1/orders", route("/v1/orders")),
        ("fleet-ingest", "POST /v1/locations", route("/v1/locations")),
    ]
    for svc, title, m in server_rows:
        n = w.count(svc, "http_request_duration_seconds", m)
        add(
            f"| {title} ({svc}) | {num(n / w.seconds, 1)} | {ms(w.quantile(0.5, svc, 'http_request_duration_seconds', m))} | "
            f"{ms(w.quantile(0.95, svc, 'http_request_duration_seconds', m))} | {ms(w.quantile(0.99, svc, 'http_request_duration_seconds', m))} |"
        )
    add(
        "\nClient side, as the simulator saw it (includes the network and any queueing):\n"
    )
    add("| Call | Requests/s | p50 | p95 | p99 |\n|---|---:|---:|---:|---:|")
    calls = sorted(
        {
            dict(l).get("call", "")
            for (svc, n, l) in w.end.values
            if svc == sim and n == "simulator_client_request_duration_seconds_bucket"
        }
    )
    for call in calls:
        m = call_is(call)
        n = w.count(sim, "simulator_client_request_duration_seconds", m)
        if n:
            add(
                f"| `{call}` | {num(n / w.seconds, 1)} | {ms(w.quantile(0.5, sim, 'simulator_client_request_duration_seconds', m))} | "
                f"{ms(w.quantile(0.95, sim, 'simulator_client_request_duration_seconds', m))} | {ms(w.quantile(0.99, sim, 'simulator_client_request_duration_seconds', m))} |"
            )
    add("")

    # Pipeline
    add("## Location pipeline and dispatch\n")
    add("| Signal | Value |\n|---|---:|")
    add(
        f"| GPS points accepted by fleet-ingest | {num(w.rate('fleet-ingest', 'fleet_ingest_points_total', is_accepted), 1)}/s |"
    )
    add(
        f"| fleet-ingest requests shed (503) | {num(w.increase('fleet-ingest', 'fleet_ingest_shed_requests_total'), 0)} |"
    )
    add(
        f"| Location freshness p99 | {secs(w.quantile(0.99, 'fleet-state', 'fleet_state_freshness_seconds'))} |"
    )
    add(
        f"| Tracking event age p99 | {secs(w.quantile(0.99, 'tracking', 'tracking_event_age_seconds'))} |"
    )
    add(
        f"| Tracking SSE connections at the end | {num(w.gauge('tracking', 'tracking_sse_connections'), 0)} |"
    )
    add(
        f"| Tracking events dropped (slow viewers) | {num(w.increase('tracking', 'tracking_sse_dropped_events_total'), 0)} |"
    )
    add(
        f"| Assignment time p50 / p95 (assigned orders) | {secs(w.quantile(0.5, 'dispatch', 'dispatch_assignment_duration_seconds', is_assigned))} / "
        f"{secs(w.quantile(0.95, 'dispatch', 'dispatch_assignment_duration_seconds', is_assigned))} |"
    )
    no_courier = w.count(
        "dispatch", "dispatch_assignment_duration_seconds", is_no_courier
    )
    add(
        f"| Orders with no courier (first report) | {num(no_courier, 0)} of {num(d_all, 0)} |"
    )
    for outcome, n in sorted(
        w.by("dispatch", "dispatch_offers_total", "outcome").items()
    ):
        add(f"| Dispatch offers {outcome} | {num(n, 0)} |")
    add(
        f"| Dispatch pending orders at the end | {num(w.gauge('dispatch', 'dispatch_pending_orders'), 0)} |"
    )
    add(
        f"| Dispatch offer streams open at the end | {num(w.gauge('dispatch', 'dispatch_offer_streams'), 0)} |"
    )
    for src, n in sorted(
        w.by("dispatch", "dispatch_eta_requests_total", "source").items()
    ):
        add(f"| ETA requests from `{src}` | {num(n, 0)} |")
    add("")

    add("## What viewers saw\n")
    add(
        "Customers with the tracking page open, measured by the simulator (`simulator_tracking_*`).\n"
    )
    age = "simulator_tracking_position_age_seconds"
    add("| Signal | Value |\n|---|---:|")
    add(
        f"| Tracking pages open at the end | {num(w.gauge(sim, 'simulator_tracking_viewers'), 0)} |"
    )
    for event, n in sorted(
        w.by(sim, "simulator_tracking_events_total", "event").items()
    ):
        add(f"| `{event}` events received | {num(n, 0)} ({num(n / w.seconds, 1)}/s) |")
    for result, n in sorted(
        w.by(sim, "simulator_tracking_streams_total", "result").items()
    ):
        add(f"| Streams finished: `{result}` | {num(n, 0)} |")
    add(
        f"| Position age when received p50 / p99 | {secs(w.quantile(0.5, sim, age))} / "
        f"{secs(w.quantile(0.99, sim, age))} |"
    )
    add(f"| Positions younger than 10 s | {pct(w.share_under(10.0, sim, age))} |")
    states = {
        dict(labels).get("state", ""): v
        for (svc, n, labels), v in w.end.values.items()
        if svc == sim and n == "simulator_couriers"
    }
    if states:
        busy = ", ".join(f"{k} {v:,.0f}" for k, v in sorted(states.items()))
        add(f"| Couriers by state at the end | {busy} |")
    add("")

    if stats:
        add("## Resources at the end of the window\n")
        add("`docker stats` (CPU is per core: 100 % = one core).\n")
        add("| Container | CPU | Memory |\n|---|---:|---|")
        for name, cpu, mem in stats:
            add(f"| {name} | {cpu} | {mem} |")
        add("")

    if samples:
        add("## Timeline\n")
        add("One line per sample: the rate and SLIs over that interval only.\n")
        add("```text")
        lines.extend(samples)
        add("```\n")
    return "\n".join(lines)


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter
    )
    ap.add_argument(
        "--window", type=float, default=600, help="seconds to measure (default 600)"
    )
    ap.add_argument(
        "--interval",
        type=float,
        default=60,
        help="seconds between timeline samples (default 60)",
    )
    ap.add_argument("--label", default="local stack", help="title of the report")
    ap.add_argument(
        "--scenario", default="", help="scenario name or notes, for the report header"
    )
    ap.add_argument(
        "--out", help="write the Markdown report here (default: stdout only)"
    )
    ap.add_argument("--json", help="also write the raw start and end scrapes here")
    ap.add_argument(
        "--no-docker", action="store_true", help="skip the docker stats table"
    )
    args = ap.parse_args(argv)

    first = scrape(TARGETS)
    if len(first.missing) == len(TARGETS):
        print(
            "No service answered on its metrics port. Is the local stack up (make up, make sim-up)?",
            file=sys.stderr,
        )
        return 1
    if first.missing:
        print(f"warning: not reachable: {', '.join(first.missing)}", file=sys.stderr)

    samples: list[str] = []
    prev = first
    deadline = first.at + args.window
    while (now := time.time()) < deadline:
        time.sleep(min(args.interval, deadline - now))
        cur = scrape(TARGETS)
        line = f"{dt.datetime.now().strftime('%H:%M:%S')}  {summary_line(Window(prev, cur))}"
        print(line, flush=True)
        samples.append(line)
        prev = cur

    window = Window(first, prev)
    stats = [] if args.no_docker else docker_stats()
    md = report(window, args.label, args.scenario, stats, samples)
    if args.out:
        with open(args.out, "w", encoding="utf-8") as f:
            f.write(md + "\n")
        print(f"\nReport written to {args.out}")
    else:
        print("\n" + md)
    if args.json:
        dump = {
            "start": {
                "at": first.at,
                "values": [[*k[:2], sorted(k[2]), v] for k, v in first.values.items()],
            },
            "end": {
                "at": prev.at,
                "values": [[*k[:2], sorted(k[2]), v] for k, v in prev.values.items()],
            },
        }
        with open(args.json, "w", encoding="utf-8") as f:
            json.dump(dump, f)
    return 0


if __name__ == "__main__":
    sys.exit(main())
