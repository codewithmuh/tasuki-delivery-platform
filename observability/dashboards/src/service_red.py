"""Service RED: rate, errors and duration of one service's HTTP routes, plus Kubernetes saturation hints.

Every Tasuki service exports the runtime contract's `http_requests_total{method,route,status}` and
`http_request_duration_seconds{method,route}` (docs/architecture.md). `route` is the route pattern, never the raw
path: `/v1/orders` (Django) or `POST /v1/locations` (Go's ServeMux pattern includes the method).
"""

from __future__ import annotations

from grafana import (
    Dashboard,
    dashboard,
    datasource_var,
    lines,
    namespace_var,
    q,
    query_var,
    runbook,
    stat,
    steps,
    timeseries,
)

UID = "tasuki-service-red"

# The selected service's application routes. Probes and scrapes (`/healthz`, `GET /healthz`, ...) are left out:
# they arrive every few seconds and would hide real traffic.
SEL = 'namespace="$namespace", service="$service", route=~"$route", route!~"(GET )?/(healthz|readyz|metrics)"'
# Server-sent event routes (`.../stream`) stay open for minutes; their "latency" is the stream's lifetime.
LATENCY_SEL = SEL + ', route!~".*/stream"'
# Pods of the selected service's Deployment: <service>-<ReplicaSet hash, 8-10 chars>-<pod suffix, 5 chars>.
# Prometheus regexes are anchored, so `orders` matches neither `orders-outbox-relay-...` (another Deployment) nor
# `orders-presync-...` (the migration Job's pods) nor `orders-<cronjob>-<schedule>-...`.
POD = 'namespace="$namespace", pod=~"$service-[a-z0-9]{8,10}-[a-z0-9]{5}"'


def _quantile(phi: str) -> str:
    return (
        f"histogram_quantile({phi}, sum by (le, route) ("
        f"rate(http_request_duration_seconds_bucket{{{LATENCY_SEL}}}[$__rate_interval])))"
    )


def build() -> Dashboard:
    d = Dashboard(
        uid=UID,
        title="Tasuki service RED",
        description="Requests, errors and latency by route for one service, with CPU and memory saturation.",
        tags=["red"],
        variables=[
            datasource_var(),
            namespace_var(),
            query_var(
                "service",
                "Service",
                'label_values(http_requests_total{namespace="$namespace"}, service)',
                current="orders",
                description="Kubernetes Service name; Prometheus adds it as the `service` label when it scrapes.",
            ),
            query_var(
                "route",
                "Route",
                'label_values(http_requests_total{namespace="$namespace", service="$service"}, route)',
                current="All",
                include_all=True,
                all_value=".*",
                multi=True,
                description="Route pattern. Probe and metrics routes are always excluded from the panels.",
            ),
        ],
    )

    d.row("Rate and errors")
    d.add(
        stat(
            "Requests / s",
            "All application requests to the selected routes (probes and /metrics excluded).",
            [q(f"sum(rate(http_requests_total{{{SEL}}}[$__rate_interval]))", "requests / s")],
            unit="reqps",
        ),
        w=6,
        h=5,
    )
    d.add(
        stat(
            "5xx ratio",
            """
            Share of requests answered with a 5xx: the server's fault. For orders, POST /v1/quotes and
            POST /v1/orders together have a 99.9 % availability SLO, so 0.1 % is the whole budget (orange).
            """,
            [
                q(
                    f'sum(rate(http_requests_total{{{SEL}, status=~"5.."}}[$__rate_interval]))'
                    f" / sum(rate(http_requests_total{{{SEL}}}[$__rate_interval]))",
                    "5xx",
                )
            ],
            unit="percentunit",
            decimals=2,
            thresholds=steps("green", (0.001, "orange"), (0.01, "red")),
            links=[runbook("slo-orders-availability"), runbook("service-unavailable")],
        ),
        w=6,
        h=5,
    )
    d.add(
        stat(
            "4xx ratio",
            """
            Share of requests answered with a 4xx: the client's request was refused. Not an SLO, but a jump
            usually means a broken client or a secret mismatch (401 when a token secret was rotated on one side).
            """,
            [
                q(
                    f'sum(rate(http_requests_total{{{SEL}, status=~"4.."}}[$__rate_interval]))'
                    f" / sum(rate(http_requests_total{{{SEL}}}[$__rate_interval]))",
                    "4xx",
                )
            ],
            unit="percentunit",
            decimals=2,
            thresholds=steps("green", (0.05, "orange"), (0.2, "red")),
        ),
        w=6,
        h=5,
    )
    d.add(
        stat(
            "p95 latency",
            "95th percentile over the selected routes, streaming (SSE) routes excluded.",
            [
                q(
                    "histogram_quantile(0.95, sum by (le) ("
                    f"rate(http_request_duration_seconds_bucket{{{LATENCY_SEL}}}[$__rate_interval])))",
                    "p95",
                )
            ],
            unit="s",
            thresholds=steps("green", (0.3, "orange"), (1, "red")),
        ),
        w=6,
        h=5,
    )
    d.add(
        timeseries(
            "Requests / s by route",
            "Traffic per route pattern. A route that drops to zero while others carry on points at a client.",
            [
                q(
                    f"sum by (method, route) (rate(http_requests_total{{{SEL}}}[$__rate_interval]))",
                    "{{method}} {{route}}",
                )
            ],
            unit="reqps",
        ),
        w=12,
        h=8,
    )
    d.add(
        timeseries(
            "Responses / s by status class",
            """
            The same traffic split by status class, stacked. 503 from fleet-ingest is deliberate load shedding
            (Kafka is full or slow), and from any service it can mean "not ready" during a rollout.
            """,
            [
                q(f'sum(rate(http_requests_total{{{SEL}, status=~"2.."}}[$__rate_interval]))', "2xx"),
                q(f'sum(rate(http_requests_total{{{SEL}, status=~"3.."}}[$__rate_interval]))', "3xx"),
                q(f'sum(rate(http_requests_total{{{SEL}, status=~"4.."}}[$__rate_interval]))', "4xx"),
                q(f'sum(rate(http_requests_total{{{SEL}, status=~"5.."}}[$__rate_interval]))', "5xx"),
            ],
            unit="reqps",
            stack=True,
        ),
        w=12,
        h=8,
    )
    d.add(
        timeseries(
            "5xx ratio by route",
            """
            Share of each route's requests that failed with a 5xx. Only routes with at least one 5xx in the
            window appear. The dashed line is the orders availability SLO's whole budget (0.1 %).
            """,
            [
                q(
                    f'sum by (route) (rate(http_requests_total{{{SEL}, status=~"5.."}}[$__rate_interval]))'
                    f" / sum by (route) (rate(http_requests_total{{{SEL}}}[$__rate_interval]))",
                    "{{route}}",
                )
            ],
            unit="percentunit",
            thresholds=lines((0.001, "red")),
            links=[runbook("slo-orders-availability"), runbook("service-unavailable")],
        ),
        w=12,
        h=8,
    )
    d.add(
        timeseries(
            "4xx / s by route and status",
            """
            Refused requests by exact status: 401 (token), 404, 409 (illegal transition, offer taken),
            422 (validation, idempotency key reused with another body), 429 (rate limit).
            """,
            [
                q(
                    f'sum by (route, status) (rate(http_requests_total{{{SEL}, status=~"4.."}}[$__rate_interval]))',
                    "{{status}} {{route}}",
                )
            ],
            unit="reqps",
        ),
        w=12,
        h=8,
    )

    d.row("Duration (streaming routes excluded)")
    d.add(
        timeseries(
            "p50 latency by route",
            "Median request duration per route: the typical request.",
            [q(_quantile("0.5"), "{{route}}")],
            unit="s",
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "p95 latency by route",
            """
            95th percentile per route. The dashed line is the orders latency SLO (95 % of POST /v1/orders under
            300 ms); it applies to that route only. Quantiles from classic histograms are interpolated inside a
            bucket, so read them as "between two bucket edges".
            """,
            [q(_quantile("0.95"), "{{route}}")],
            unit="s",
            thresholds=lines((0.3, "orange")),
            links=[runbook("slo-orders-latency")],
        ),
        w=8,
        h=8,
    )
    d.add(
        timeseries(
            "p99 latency by route",
            "99th percentile per route: the slowest 1 %, where timeouts and lock waits show first.",
            [q(_quantile("0.99"), "{{route}}")],
            unit="s",
        ),
        w=8,
        h=8,
    )

    d.row("Saturation (cAdvisor, kube-state-metrics)")
    d.add(
        timeseries(
            "CPU per pod vs request",
            """
            CPU used by each pod of the service, in cores, against the CPU request per pod (dashed series).
            Tasuki sets no CPU limits, so there is no throttling: a pod far above its request competes with its
            neighbours for the node, and the request should grow.
            """,
            [
                q(
                    f'sum by (pod) (rate(container_cpu_usage_seconds_total{{{POD}, container!=""}}[$__rate_interval]))',
                    "{{pod}}",
                ),
                q(
                    f'max(sum by (pod) (kube_pod_container_resource_requests{{{POD}, resource="cpu"}}))',
                    "request per pod",
                ),
            ],
            unit="suffix:cores",
            dashed=["request per pod"],
        ),
        w=6,
        h=8,
    )
    d.add(
        timeseries(
            "Memory per pod vs limit",
            """
            Working-set memory (what the kernel counts for the OOM killer) per pod, against the memory limit per
            pod. A pod reaching the limit is OOM-killed and restarted.
            """,
            [
                q(f'sum by (pod) (container_memory_working_set_bytes{{{POD}, container!=""}})', "{{pod}}"),
                q(
                    f'max(sum by (pod) (kube_pod_container_resource_limits{{{POD}, resource="memory"}}))',
                    "limit per pod",
                ),
            ],
            unit="bytes",
            dashed=["limit per pod"],
        ),
        w=6,
        h=8,
    )
    d.add(
        timeseries(
            "Ready pods and restarts",
            """
            Pods passing their readiness probe, and container restarts in the last hour. Fewer ready pods than
            replicas during a rollout is normal; restarts outside one are not.
            """,
            [
                q(f'sum(kube_pod_status_ready{{{POD}, condition="true"}})', "ready pods"),
                q(f"sum(increase(kube_pod_container_status_restarts_total{{{POD}}}[1h]))", "restarts (last 1 h)"),
            ],
            unit="short",
            links=[runbook("service-unavailable")],
        ),
        w=6,
        h=8,
    )
    d.add(
        stat(
            "Containers last killed by OOM",
            """
            Containers whose most recent termination was an OOM kill (kube-state-metrics keeps the last reason
            until the next one). Above zero: raise the memory limit or find the leak.
            """,
            [
                q(
                    f'sum(kube_pod_container_status_last_terminated_reason{{{POD}, reason="OOMKilled"}}) or vector(0)',
                    "OOM killed",
                )
            ],
            unit="short",
            thresholds=steps("green", (1, "red")),
            graph=False,
            links=[dashboard("Tasuki overview", "tasuki-overview")],
        ),
        w=6,
        h=8,
    )
    return d
