# Dashboards as code

Tasuki's Grafana dashboards are generated from a small Python program (standard library only), and the generated JSON
is committed. Grafana loads the JSON from ConfigMaps; nobody edits a dashboard in the UI and saves it.

| Path | What it is |
|---|---|
| [src/](src/) | The source of truth. One module per dashboard, plus the panel builders (`grafana.py`), the metric registry (`metrics.py`) and the checks (`lint.py`). **Edit these.** |
| [generated/](generated/) | One JSON file per dashboard, written by `make dashboards-generate`. Never edit by hand; CI fails when they are stale. |
| [kustomization.yaml](kustomization.yaml) | Generated. Turns each JSON file into a ConfigMap for the Grafana sidecar (see [Provisioning](#provisioning)). |

```bash
make dashboards-generate   # src/ → generated/*.json, kustomization.yaml, the metric table below (needs python3)
make dashboards-lint       # unit tests, "generated files are fresh", dashboard lint, promtool on every query
```

## Why generated JSON, and why plain Python

Grafana's dashboard JSON is the format Grafana loads, so it is what ships. Written by hand it is noisy: grid
coordinates, panel IDs, repeated data source blocks and a few hundred lines per dashboard, which makes reviews about
layout rather than queries. So the JSON is generated, from code where each panel is a title, a description, its
PromQL and a unit:

```python
timeseries(
    "Outbox lag (oldest unpublished event)",
    "Age of the oldest outbox row the relay has not published ... OrdersOutboxLagHigh pages above 60 s ...",
    [q('max(orders_outbox_lag_seconds{namespace="$namespace"})', "lag")],
    unit="s",
    thresholds=lines((60, "red")),          # the alert threshold, drawn as a dashed line
    links=[runbook("orders-outbox-lag")],   # lint checks the runbook file exists
)
```

The options were Grafonnet (Jsonnet), the Grafana Foundation SDK (Go, TypeScript, Python) and plain Python. Both
libraries are good, but each adds a toolchain or a dependency tree to install, pin and update, only to emit a JSON
schema that changes slowly. About 450 lines of `grafana.py` cover the five panel types used here. CI needs nothing
beyond the runner's `python3` and the `promtool` that the SLO checks already pin. Committing the generated JSON as
well keeps the deployed artefact reviewable, and lets Argo CD apply it without running any generator.

## The dashboards

Every dashboard has a `datasource` variable (the default Prometheus data source; no UID is hard-coded) and a
`namespace` variable defaulting to `tasuki-prod`. Every panel has a description (the ⓘ on the panel) saying what it
shows and why it matters, and panels backed by an alert link to that alert's runbook. A dashed line on a panel is
an SLO bound or an alert threshold. The dashboards link to each other through the "Tasuki dashboards" menu.

| Dashboard (UID) | The question it answers |
|---|---|
| **Tasuki overview** (`tasuki-overview`) | Is the city being served right now, and is any promise at risk? Orders and deliveries per minute, couriers online by status, time to an accepted courier vs 60 s, ETA fallback ratio, location freshness vs 2 s, tracking freshness vs 10 s, error budget left per SLO, firing alerts. The demo screen. |
| **Tasuki service RED** (`tasuki-service-red`) | Is one service healthy? Rate, 5xx/4xx ratio and p50/p95/p99 latency by route, then CPU and memory per pod against requests and limits, ready pods, restarts and OOM kills. Pick the service and routes with the variables. |
| **Tasuki location pipeline** (`tasuki-location`) | Where in GPS → fleet-ingest → Kafka → fleet-state (Redis) / tracking (SSE) are points late or lost? Points by result, shedding (503), consumer lag, freshness heatmap and SLI, Redis rebuilds, SSE streams, event age, drops, stalled replicas. |
| **Tasuki orders, outbox & dispatch** (`tasuki-orders`) | Is the order path correct and moving? Quotes and orders, idempotent replays, ETA source split, the order API SLIs, outbox lag and relay throughput, both Kafka consumers (dispatch.events into orders, orders.events into dispatch), and dispatch's matching: pending orders by ward, offers, its outbox and Redis-loss pauses. |
| **Tasuki SLOs** (`tasuki-slos`) | How much reliability is left to spend? For each SLO: SLI over 1 h and 28 days, error budget left, and the burn rate of every alert window with the page and ticket thresholds drawn in. Also the rollout gate's decisions, which read the same budgets. |
| **Tasuki simulator** (`tasuki-simulator`) | During a game day, is the simulator broken or the platform? Scenario demand against orders attempted and created, failures by reason, client-side vs server-side latency, offers seen by couriers vs sent by dispatch, deliveries on both sides, GPS points on both sides. |

Things to know when reading them:

- **SLO panels ignore the namespace variable.** Sloth's recording rules select `namespace="tasuki-prod"` themselves,
  because only production has SLOs ([../slo/README.md](../slo/README.md)).
- **Percentiles come from classic histograms**, so they are interpolated inside a bucket. The SLO panels use the
  exact bucket counts instead (`le="2.0"`, the way Prometheus 3 stores bucket bounds).
- **Streaming routes are left out of latency.** A server-sent event request lasts as long as the viewer watches, so
  `.../stream` routes are excluded from the latency panels (not from request counts).
- **Saturation panels find pods by name**: `<service>-<ReplicaSet hash>-<suffix>`, the Deployment's pods only, which
  excludes the migration Job and CronJob pods of the same release.

## Metrics and where they come from

`lint.py` fails when a query uses a metric that is not in [src/metrics.py](src/metrics.py), when a listed metric is
unused, and when an in-repo metric's name is missing from its source file. The table is generated from that registry.

<!-- metrics:start (generated by build.py from src/metrics.py; do not edit) -->
| Metric | Type | Labels used | Source | Dashboards |
|---|---|---|---|---|
| `http_requests_total` | counter | service, method, route, status | [services/orders/src/core/metrics.py](../../services/orders/src/core/metrics.py), [services/fleet/internal/kit/server.go](../../services/fleet/internal/kit/server.go) | service RED · orders, outbox & dispatch |
| `http_request_duration_seconds` | histogram | service, method, route | [services/orders/src/core/metrics.py](../../services/orders/src/core/metrics.py), [services/fleet/internal/kit/server.go](../../services/fleet/internal/kit/server.go) | service RED · orders, outbox & dispatch · simulator |
| `orders_quotes_created_total` | counter | eta_source | [services/orders/src/core/metrics.py](../../services/orders/src/core/metrics.py) | overview · orders, outbox & dispatch |
| `orders_created_total` | counter |  | [services/orders/src/core/metrics.py](../../services/orders/src/core/metrics.py) | overview · orders, outbox & dispatch |
| `orders_idempotent_replays_total` | counter |  | [services/orders/src/core/metrics.py](../../services/orders/src/core/metrics.py) | orders, outbox & dispatch · simulator |
| `orders_eta_fallbacks_total` | counter | reason | [services/orders/src/core/metrics.py](../../services/orders/src/core/metrics.py) | orders, outbox & dispatch |
| `orders_status_transitions_total` | counter | status | [services/orders/src/core/metrics.py](../../services/orders/src/core/metrics.py) | overview · orders, outbox & dispatch · simulator |
| `orders_outbox_lag_seconds` | gauge |  | [services/orders/src/core/metrics.py](../../services/orders/src/core/metrics.py) | orders, outbox & dispatch |
| `orders_outbox_pending_events` | gauge |  | [services/orders/src/core/metrics.py](../../services/orders/src/core/metrics.py) | orders, outbox & dispatch |
| `orders_outbox_published_total` | counter |  | [services/orders/src/core/metrics.py](../../services/orders/src/core/metrics.py) | orders, outbox & dispatch |
| `orders_outbox_publish_failures_total` | counter |  | [services/orders/src/core/metrics.py](../../services/orders/src/core/metrics.py) | orders, outbox & dispatch |
| `orders_dispatch_events_total` | counter | type, outcome | [services/orders/src/core/metrics.py](../../services/orders/src/core/metrics.py) | orders, outbox & dispatch |
| `fleet_ingest_points_total` | counter | result | [services/fleet/internal/ingest/handler.go](../../services/fleet/internal/ingest/handler.go) | overview · location pipeline · simulator |
| `fleet_ingest_shed_requests_total` | counter | reason | [services/fleet/internal/ingest/handler.go](../../services/fleet/internal/ingest/handler.go) | location pipeline |
| `fleet_ingest_batch_points` | histogram |  | [services/fleet/internal/ingest/handler.go](../../services/fleet/internal/ingest/handler.go) | location pipeline |
| `fleet_state_points_total` | counter | result | [services/fleet/internal/state/processor.go](../../services/fleet/internal/state/processor.go) | location pipeline |
| `fleet_state_freshness_seconds` | histogram |  | [services/fleet/internal/state/processor.go](../../services/fleet/internal/state/processor.go) | overview · location pipeline |
| `fleet_state_swept_couriers_total` | counter |  | [services/fleet/internal/state/processor.go](../../services/fleet/internal/state/processor.go) | location pipeline |
| `fleet_state_rebuilds_total` | counter |  | [services/fleet/internal/state/processor.go](../../services/fleet/internal/state/processor.go) | location pipeline |
| `fleet_state_last_rebuild_duration_seconds` | gauge |  | [services/fleet/internal/state/processor.go](../../services/fleet/internal/state/processor.go) | location pipeline |
| `fleet_state_history_errors_total` | counter |  | [services/fleet/internal/state/processor.go](../../services/fleet/internal/state/processor.go) | location pipeline |
| `tracking_event_age_seconds` | histogram | stream | [services/fleet/internal/tracking/feed.go](../../services/fleet/internal/tracking/feed.go) | overview · location pipeline |
| `tracking_sse_connections` | gauge | stream | [services/fleet/internal/tracking/feed.go](../../services/fleet/internal/tracking/feed.go) | location pipeline |
| `tracking_sse_dropped_events_total` | counter | stream | [services/fleet/internal/tracking/feed.go](../../services/fleet/internal/tracking/feed.go) | location pipeline |
| `tracking_sse_throttled_events_total` | counter | stream | [services/fleet/internal/tracking/feed.go](../../services/fleet/internal/tracking/feed.go) | location pipeline |
| `tracking_records_invalid_total` | counter | topic | [services/fleet/internal/tracking/feed.go](../../services/fleet/internal/tracking/feed.go) | location pipeline |
| `tracking_couriers` | gauge | pod | [services/fleet/internal/tracking/feed.go](../../services/fleet/internal/tracking/feed.go) | overview · location pipeline |
| `dispatch_assignment_duration_seconds` | histogram | outcome | [services/fleet/internal/dispatch](../../services/fleet/internal/dispatch) | overview |
| `dispatch_eta_requests_total` | counter | source | [services/fleet/internal/dispatch](../../services/fleet/internal/dispatch) | overview · orders, outbox & dispatch |
| `dispatch_offers_total` | counter | outcome | [services/fleet/internal/dispatch](../../services/fleet/internal/dispatch) | orders, outbox & dispatch · simulator |
| `dispatch_pending_orders` | gauge | ward | [services/fleet/internal/dispatch](../../services/fleet/internal/dispatch) | overview · orders, outbox & dispatch |
| `dispatch_couriers` | gauge | state | [services/fleet/internal/dispatch](../../services/fleet/internal/dispatch) | overview · orders, outbox & dispatch |
| `dispatch_outbox_events` | gauge |  | [services/fleet/internal/dispatch](../../services/fleet/internal/dispatch) | orders, outbox & dispatch |
| `dispatch_publish_failures_total` | counter |  | [services/fleet/internal/dispatch](../../services/fleet/internal/dispatch) | orders, outbox & dispatch |
| `dispatch_order_events_total` | counter | type, result | [services/fleet/internal/dispatch](../../services/fleet/internal/dispatch) | orders, outbox & dispatch |
| `dispatch_paused` | gauge | pod | [services/fleet/internal/dispatch](../../services/fleet/internal/dispatch) | orders, outbox & dispatch |
| `dispatch_offer_streams` | gauge |  | [services/fleet/internal/dispatch](../../services/fleet/internal/dispatch) | orders, outbox & dispatch |
| `simulator_orders_attempted_total` | counter |  | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `simulator_orders_created_total` | counter |  | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `simulator_orders_failed_total` | counter | stage, reason | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `simulator_orders_replayed_total` | counter |  | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `simulator_client_request_duration_seconds` | histogram | call, outcome | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `simulator_request_retries_total` | counter | call | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `simulator_location_batches_total` | counter | result | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `simulator_location_points_total` | counter | result | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `simulator_offers_total` | counter | outcome | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `simulator_deliveries_completed_total` | counter |  | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `simulator_jobs_abandoned_total` | counter | reason | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `simulator_couriers_active` | gauge |  | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `simulator_couriers` | gauge | state | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `simulator_routes_total` | counter | source | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `simulator_demand_orders_per_hour` | gauge |  | [services/simulator/internal/metrics/metrics.go](../../services/simulator/internal/metrics/metrics.go) | simulator |
| `rollout_gate_decisions_total` | counter | allowed, cause | [tools/rollout-gate/internal/server](../../tools/rollout-gate/internal/server) | SLOs |
| `slo:sli_error:ratio_rate5m` | recording rule | sloth_id, sloth_service, sloth_slo | [observability/slo/generated](../../observability/slo/generated) | SLOs |
| `slo:sli_error:ratio_rate30m` | recording rule | sloth_id, sloth_service, sloth_slo | [observability/slo/generated](../../observability/slo/generated) | SLOs |
| `slo:sli_error:ratio_rate1h` | recording rule | sloth_id, sloth_service, sloth_slo | [observability/slo/generated](../../observability/slo/generated) | SLOs |
| `slo:sli_error:ratio_rate2h` | recording rule | sloth_id, sloth_service, sloth_slo | [observability/slo/generated](../../observability/slo/generated) | SLOs |
| `slo:sli_error:ratio_rate6h` | recording rule | sloth_id, sloth_service, sloth_slo | [observability/slo/generated](../../observability/slo/generated) | SLOs |
| `slo:sli_error:ratio_rate1d` | recording rule | sloth_id, sloth_service, sloth_slo | [observability/slo/generated](../../observability/slo/generated) | SLOs |
| `slo:sli_error:ratio_rate3d` | recording rule | sloth_id, sloth_service, sloth_slo | [observability/slo/generated](../../observability/slo/generated) | SLOs |
| `slo:sli_error:ratio_rate4w` | recording rule | sloth_id, sloth_service, sloth_slo | [observability/slo/generated](../../observability/slo/generated) | SLOs |
| `slo:error_budget:ratio` | recording rule | sloth_id | [observability/slo/generated](../../observability/slo/generated) | SLOs |
| `slo:period_error_budget_remaining:ratio` | recording rule | sloth_service, sloth_slo | [observability/slo/generated](../../observability/slo/generated) | overview · SLOs |
| `kafka_consumergroup_lag` | gauge | consumergroup, topic, partition | Kafka Exporter: Strimzi `kafkaExporter` (deploy/apps/base/kafka.yaml), PodMonitor `kafka` | location pipeline · orders, outbox & dispatch |
| `container_cpu_usage_seconds_total` | counter | pod, container | cAdvisor via the kubelet ServiceMonitor (kube-prometheus-stack) | service RED |
| `container_memory_working_set_bytes` | gauge | pod, container | cAdvisor via the kubelet ServiceMonitor (kube-prometheus-stack) | service RED |
| `kube_pod_container_resource_requests` | gauge | pod, resource | kube-state-metrics (kube-prometheus-stack) | service RED |
| `kube_pod_container_resource_limits` | gauge | pod, resource | kube-state-metrics (kube-prometheus-stack) | service RED |
| `kube_pod_status_ready` | gauge | pod, condition | kube-state-metrics (kube-prometheus-stack) | service RED |
| `kube_pod_container_status_restarts_total` | counter | pod | kube-state-metrics (kube-prometheus-stack) | service RED |
| `kube_pod_container_status_last_terminated_reason` | gauge | pod, reason | kube-state-metrics (kube-prometheus-stack) | service RED |
| `up` | built-in | namespace, service | Prometheus (one series per scrape target) | overview · service RED · location pipeline · orders, outbox & dispatch · SLOs · simulator |
| `ALERTS` | built-in | alertname, alertstate, severity, category | Prometheus (firing alerts) | overview · SLOs |
<!-- metrics:end -->

Dependencies outside this repository's code:

- **Kafka Exporter** for `kafka_consumergroup_lag`: Strimzi runs it when the `Kafka` resource has `kafkaExporter`
  (deploy/apps/base/kafka.yaml), and the `kafka` PodMonitor scrapes it. Without it, the lag panels are empty and
  `KafkaConsumerLagHigh` cannot fire. Tracking reads its topics without a consumer group, so it never has lag.
- **kube-state-metrics and the kubelet's cAdvisor endpoint**, both part of kube-prometheus-stack, for the
  saturation panels.
- **Sloth's recording rules** ([../slo/generated/](../slo/generated/)) for every SLO panel. `lint.py` checks that the
  SLO list in [src/slos.py](src/slos.py) matches them: the same SLO IDs, objectives and runbooks.

## Provisioning

The Argo CD Application `grafana-dashboards` (deploy/platform/grafana-dashboards.yaml) syncs this directory into
the `monitoring` namespace. Because it contains [kustomization.yaml](kustomization.yaml), Argo CD runs Kustomize
on it, and the `configMapGenerator` turns each `generated/<uid>.json` into a ConfigMap `dashboard-<uid>` labelled
`grafana_dashboard: "1"`. The kube-prometheus-stack Grafana sidecar watches for that label (in every namespace,
`searchNamespace: ALL`) and loads the dashboards within seconds, without a Grafana restart. The Application uses
server-side apply, because client-side apply stores the whole object in an annotation capped at 256 KiB.

The ConfigMaps also carry the annotation `grafana_folder: Tasuki`. To put the dashboards in a "Tasuki" folder
instead of "General", the sidecar needs these values in deploy/platform/values/kube-prometheus-stack.yaml:

```yaml
grafana:
  sidecar:
    dashboards:
      searchNamespace: ALL
      folderAnnotation: grafana_folder
      provider:
        foldersFromFilesStructure: true
```

To preview without a cluster, import a file from `generated/` in any Grafana 11+ (Dashboards → New → Import) and
pick a Prometheus data source for the `datasource` variable.

## Changing a dashboard

1. Edit the dashboard's module in [src/](src/). A new metric goes into [src/metrics.py](src/metrics.py) with its
   source file; a new dashboard module goes into `MODULES` in [src/build.py](src/build.py).
2. Run `make dashboards-generate`, then `make dashboards-lint`.
3. Commit the source, the generated JSON, `kustomization.yaml` and this README together.

What the lint checks: unique UIDs equal to their file names (so links and bookmarks never break), the two
variables, a description, unit and the datasource variable on every panel, no hard-coded data source UID, panels
inside the grid without overlaps, links to repository files that exist and to dashboards that exist, metrics against
the registry, the SLO list against Sloth, and every query (panels, annotations, variables) through
`promtool check rules` after substituting Grafana's variables (`$__rate_interval` → `5m`, and so on).

## Screenshots

_To be added after the first game day: the overview during a rain surge, the location pipeline during a Redis loss,
and the SLOs dashboard during a fast burn._
