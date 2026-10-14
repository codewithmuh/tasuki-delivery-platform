# GD-02: Rainy-Friday surge

| | |
|---|---|
| Status | **Planned, not run** |
| Design | [§4](../../design.md#4-scale-assumptions) conclusion 1; [§10](../../design.md#10-failure-modes-and-degradation) "Rain surge (×3 demand)" |
| SLOs at stake | orders availability and latency (must hold); location and tracking freshness (must hold); dispatch assignment speed (expected to burn: supply, not software) |
| Environment | `tasuki-prod` (SLOs, HPAs on fleet-ingest and tracking, 2 replicas, Rollouts). A dev dry run first checks the scenario starts |
| Time box | 2 hours per run (11:00–13:00 simulated: the rain starts at 11:30). Run 1 `rainy-friday` (one-tenth scale), then, in a later session, run 2 `rainy-friday-design-scale` |
| Scripts | `preflight.sh`, `watch-slis.sh`, `record-timeline.sh` (the fault is a scenario, not an injection) |

## Hypothesis

When demand triples at 11:30, couriers ride 15 % slower and 10 % of them log off (`rainy-friday` scenario):

1. **The order API stays up and fast.** Orders availability stays at 99.9 % or better and creates stay under 300 ms
   (95 %). At this scale (one-tenth of design, about 1,800 orders/h at the peak) the servers are not the bottleneck
   (design §4: "a surge is a capacity problem, not a throughput problem").
2. **Quotes get honest.** `eta_seconds` rises and dispatch's `GET /v1/eta` reports `supply: low` or `none` in the
   busiest wards. If dispatch is not running, every quote uses orders' fallback, which does not know about supply,
   and this point cannot be checked: record that instead.
3. **Dispatch speed burns, and that is correct.** More orders wait over 60 s or get
   `NoCourierAvailable`. Per [slo-dispatch-assignment-speed](../../runbooks/slo-dispatch-assignment-speed.md), this
   burn is supply, not software, and it stays in the budget.
4. **The location path holds.** Location and tracking freshness stay at or above 99 %.
5. **Autoscaling acts only if it must.** The HPAs (prod: fleet-ingest 2–6 at 70 % CPU, tracking 2–6 at 60 %) add
   replicas only if CPU crosses the target, and Karpenter adds a node (spot first) only if pods go Pending. **At
   one-tenth scale (run 1), neither is likely.** Seeing no scaling is the expected result there, not a failure.
   **Run 2 (`rainy-friday-design-scale`: ~3,000 couriers online, ~750 GPS points/s, about 5 orders/s in the rain)**
   should make the fleet-ingest HPA scale out, and may need a Karpenter node. Orders availability and location
   freshness must still hold; dispatch speed burns harder.
6. **The brownout cuts update volume, not freshness.** With `TRACKING_MIN_INTERVAL=10s`, throttled events rise
   (`tracking_sse_throttled_events_total`) and sent events fall, while the age of sent events (the SLI) stays low.

## Steady state

- **Dashboards:** Tasuki overview, Tasuki orders & outbox, Tasuki location pipeline, Tasuki SLOs, Tasuki simulator
  (intended demand vs accepted orders).
- **`watch-slis.sh`** with:

  ```bash
  scripts/gameday/watch-slis.sh --context tasuki --namespace tasuki-prod --interval 30 \
    --extra 'intended orders/h=sum(simulator_demand_orders_per_hour{namespace="{ns}"})' \
    --extra 'quote ETA p50 (s)=histogram_quantile(0.5, sum by (le) (rate(orders_quote_eta_seconds_bucket{namespace="{ns}"}[10m])))' \
    --extra 'HPA replicas=max by (horizontalpodautoscaler) (kube_horizontalpodautoscaler_status_current_replicas{namespace="{ns}"})' \
    --extra 'pods pending=sum(kube_pod_status_phase{namespace="{ns}",phase="Pending"})' \
    --extra 'throttled events/s=sum(rate(tracking_sse_throttled_events_total{namespace="{ns}"}[1m]))' \
    --extra 'no-courier share 10m=sum(rate(dispatch_assignment_duration_seconds_count{namespace="{ns}",outcome="no_courier"}[10m])) / sum(rate(dispatch_assignment_duration_seconds_count{namespace="{ns}"}[10m]))'
  ```

  `orders_quote_eta_seconds` is a placeholder name: orders does not export a quote-ETA histogram today (see gaps).
  Until it does, sample `eta_seconds` from the simulator's debug logs, or from quotes made by hand.
- **Nodes:** `kubectl --context tasuki get nodeclaims` and
  `kubectl --context tasuki get nodes -L karpenter.sh/capacity-type,topology.kubernetes.io/zone`.

## Blast radius

`tasuki-prod` only. The fault is load, so it ends when the scenario is switched back. Cost: Karpenter may add nodes
(the NodePool caps it at 32 vCPU). Budgets: dispatch assignment speed is expected to burn. Any burn on orders or the
location path is a finding.

## Preconditions

- Preflight GO with `--service orders`, and more than 50 % budget left on every SLO.
- Pull request 1, to switch the scenario: `deploy/apps/overlays/prod/values/simulator.yaml`

  ```yaml
  replicas: 1
  env:
    SCENARIO: rainy-friday   # the scenario runs until stopped; revert this PR at 13:00 simulated
  ```

  The pod gets a fresh `/tmp`, so the simulator seeds new merchants and couriers when it starts. Allow about 5 minutes
  for seeding and for couriers to come online before 11:30 simulated (the scenario starts at 11:00).
- **Run 2 only (design scale), a bigger pull request**, after run 1 behaved as predicted. The simulator's defaults
  (50m CPU, 64Mi, limit 256Mi) are sized for `small`; the scenario's own estimate
  ([services/simulator/README.md](../../../services/simulator/README.md), "rainy-friday-design-scale") is 1–2 vCPU and
  0.5–1 GiB, about 3,000 SSE connections, and 1–3 minutes to seed:

  ```yaml
  # deploy/apps/overlays/prod/values/simulator.yaml
  replicas: 1
  env:
    SCENARIO: rainy-friday-design-scale
    SEED_WAIT: 5m                    # seeding 4,600 couriers takes minutes
  resources:
    requests: {cpu: "1", memory: 768Mi}
    limits: {memory: 1536Mi}
  terminationGracePeriodSeconds: 90  # already the default in deploy/apps/services/simulator.yaml; keep it ≥ 90
  ```

  **Scale the receiving services first**, in the same or an earlier pull request, and let them settle: fleet-ingest at
  ~750 requests/s (raise `autoscaling.minReplicas` so the HPA does not start from 2 during the seeding burst),
  dispatch holding ~3,000 offer streams, orders at ~5 orders/s plus a quote, a pickup and a delivery call per order.
  Check the prod ResourceQuota (8 CPU requested in total) has room for the simulator's 1 CPU and the extra replicas,
  or raise it in the same pull request. The point is to watch the platform degrade gracefully, not to find a limit
  left at its laptop value. Treat these numbers as estimates: they have not been measured.
- Pull request 2, ready but **not merged**: the brownout, `deploy/apps/overlays/prod/values/tracking.yaml`

  ```yaml
  env:
    TRACKING_MIN_INTERVAL: 10s
  ```

  In prod tracking is an Argo Rollout, so this change is a canary like any other release: the error-budget gate
  first (tracking's freshness budget must not be spent), then 25 %, a 5-minute pause, 50 %, another pause, then
  100 %. Each new pod also replays `orders.events` and `dispatch.events` before it is ready. Expect the brownout to
  take well over 10 minutes to reach every viewer, which is slower than a plain restart. In an emergency,
  [surge-brownout](../../runbooks/surge-brownout.md) says when `kubectl argo rollouts promote tracking --full` is
  justified (and recorded).
- A fleet-stream viewer, as in [GD-01](01-redis-loss.md#preconditions), so tracking has someone to send to.

## Steps

```bash
CTX=tasuki NS=tasuki-prod; export ALLOW_PROD=yes
export GAMEDAY_LOG="$(scripts/gameday/record-timeline.sh start --context $CTX --namespace $NS \
  --experiment gd-02-rainy-friday --print-path)"
scripts/gameday/preflight.sh --context $CTX --namespace $NS --service orders
```

1. Merge pull request 1. `record-timeline.sh note "scenario PR merged"`. Wait for Argo CD to sync
   (`kubectl --context $CTX -n argocd get application simulator-prod`) and the simulator pod to be Ready.
   `mark steady-state` and `snapshot` at 11:00–11:25 simulated.
2. **11:30 simulated, the rain starts.** `mark inject`. Snapshot every 15 minutes (`record-timeline.sh snapshot`).
3. **Checkpoints:**
   - 11:45: accepted orders/h follows intended demand. Any gap is `backpressure` or errors
     (`simulator_orders_failed_total` by reason).
   - 12:00: orders availability and latency unchanged; freshness unchanged; dispatch queue (once dispatch runs) growing.
   - Whenever it happens: HPA scale-up, Pending pods, new NodeClaims (note the time and capacity type).
4. **12:15, brownout.** Merge pull request 2. Follow [surge-brownout](../../runbooks/surge-brownout.md). Watch
   `kubectl argo rollouts get rollout tracking -n tasuki-prod --watch`, note when each step started and when the last
   tracking pod became Ready, and compare sent and throttled events.
5. **13:00 simulated, end.** Revert both pull requests. `mark recovered` when the simulator is back on its usual
   scenario, then `snapshot` and `mark end`.

## Expected observations

| Signal | Expected |
|---|---|
| Intended orders/h | About ×3 from 11:30 (the scenario's curve × 3) |
| Orders 5xx, create latency | Flat |
| Simulator order failures | `backpressure` only if `max_in_flight` (400) is reached; no 5xx |
| Quote ETA, supply (dispatch running) | Longer, `supply: low` in the busiest wards |
| Dispatch speed share (dispatch running) | Falls; `no_courier` outcomes rise |
| Location and tracking freshness | ≥ 99 % |
| HPAs, Karpenter | Run 1: probably no change. Run 2: fleet-ingest scales out; maybe a new node |
| Brownout rollout | Gate passes, canary steps take about 10 minutes or more |
| After the brownout | Throttled events up, sent events down, event age unchanged |

## Abort criteria

In addition to the [global ones](../README.md#abort-criteria-all-experiments):

- Orders 5xx above 1 % for 5 minutes: the surge has found a software limit. Stop, and capture it as a finding.
- The NodePool approaches its 32 vCPU limit, or Pending pods do not schedule within 10 minutes.

## Rollback

Revert pull request 1 (or set `replicas: 0`) and pull request 2. Argo CD applies both within minutes. Nothing else to undo.

## Alerts and runbooks

| Alert | Should it fire? | Runbook |
|---|---|---|
| `DispatchAssignmentSpeedBudgetBurn` (dispatch running) | **Yes**, possibly page | [slo-dispatch-assignment-speed](../../runbooks/slo-dispatch-assignment-speed.md): "supply, not software" |
| `ETAFallbackHigh` (`component=orders`) | Yes whenever dispatch is not running, surge or not | [eta-fallback](../../runbooks/eta-fallback.md) |
| `OrdersAvailabilityBudgetBurn`, `OrdersLatencyBudgetBurn` | **No** | [slo-orders-availability](../../runbooks/slo-orders-availability.md), [slo-orders-latency](../../runbooks/slo-orders-latency.md) |
| `TrackingSSEDropsHigh`, `TrackingFreshnessBudgetBurn` | No | [tracking-sse-drops](../../runbooks/tracking-sse-drops.md), [slo-tracking-freshness](../../runbooks/slo-tracking-freshness.md) |
| `FleetIngestShedding` (over 5 % of points refused for 15 minutes) | Possible only if Kafka cannot keep up; not expected at this scale | [courier-locations-lost](../../runbooks/courier-locations-lost.md) |
| `KubeHpaMaxedOut` (upstream) | Only if an HPA sits at 6 replicas; not expected at this scale | [surge-brownout](../../runbooks/surge-brownout.md) |
| `RolloutDegraded` | No: the brownout canary should complete | [broken-secret-stuck-rollout](../../runbooks/broken-secret-stuck-rollout.md) |

The surge itself has a runbook: [surge-brownout](../../runbooks/surge-brownout.md). Follow it from 11:30 and note
where it was wrong or slow.

## Gaps found while planning

- **Design scale has never run.** `rainy-friday-design-scale` exists, but its resource numbers are estimates, the
  receiving services' sizes for ~750 requests/s are untested, and the prod ResourceQuota may be too small. Run 2 is
  where those meet reality.
- **The brownout is a deploy**, and in prod a gated canary. A runtime switch (an admin endpoint, or a value tracking
  re-reads) would make it a lever instead of a release.
- **Orders exports no quote ETA or supply metric**, so "ETAs widen honestly" can only be checked from responses.
- The prod ResourceQuota (8 CPU requested) also caps scale-out. A blocked HPA shows up as `exceeded quota` events.

## Results

<!-- fill in after the drill -->

| | |
|---|---|
| Date and window (JST) | <!-- fill in after the drill --> |
| Dispatch running? | <!-- fill in after the drill --> |
| Driver / observer / scribe | <!-- fill in after the drill --> |
| Drill log | <!-- link --> |
| Hypothesis held? | <!-- per numbered point --> |

| Signal | Expected | Observed |
|---|---|---|
| Peak intended vs accepted orders/h | equal | <!-- fill in after the drill --> |
| Orders availability / latency, worst 5 m | ≥ 99.9 % / ≥ 95 % | <!-- fill in after the drill --> |
| Location / tracking freshness, worst 5 m | ≥ 99 % | <!-- fill in after the drill --> |
| Dispatch speed share, worst 30 m | burns | <!-- fill in after the drill --> |
| HPA replicas, max (run 1 / run 2) | 2 / more | <!-- fill in after the drill --> |
| NodeClaims created, capacity type | probably none | <!-- fill in after the drill --> |
| Brownout: tracking restart duration; sent vs throttled events | | <!-- fill in after the drill --> |
| Alerts that fired | | <!-- fill in after the drill --> |
| Budget remaining before / after, per SLO | | <!-- fill in after the drill --> |

Surprises: <!-- fill in after the drill -->

Action items: <!-- fill in after the drill -->

Postmortem: <!-- link, or "not required" with the reason -->
