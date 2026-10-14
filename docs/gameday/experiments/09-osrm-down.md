# GD-09: Routing (OSRM) down

| | |
|---|---|
| Status | **Planned, not run** |
| Design | [§10](../../design.md#10-failure-modes-and-degradation) "OSRM down" |
| SLOs at stake | dispatch assignment speed (should hold); ETA accuracy (business SLO, not built) |
| Environment | `tasuki-dev` first, then `tasuki-prod` once dispatch runs |
| Time box | 30 minutes |
| Scripts | `pod-kill.sh`, `watch-slis.sh`, `record-timeline.sh` |

## Hypothesis

When every `routing` pod is gone:

1. **Dispatch falls back** to straight-line distance × 1.35 (`dispatch_eta_requests_total{source="fallback"}`), keeps
   matching, and quotes widen slightly. Assignment speed holds.
2. **Orders is unaffected**: it asks dispatch for ETAs, never OSRM.
3. **The simulator's couriers ride straight lines** (`simulator_routes_total{source="straight_line"}`) until routing
   is back. That is the simulator's own fallback, not a platform behaviour.
4. **Routing comes back quickly.** The Kantō graph is baked into the image and loads in seconds; the startup probe
   (`/nearest/v1/bicycle/...`) holds traffic until it is loaded.

A pod deletion only takes routing away for as long as the restart takes. A longer outage needs `replicas: 0` through a
pull request (`deploy/apps/overlays/<env>/values/routing.yaml`), which is the variant to run once dispatch exists, so
that `ETAFallbackHigh` (10 minutes over 10 %) can be checked.

## Steady state

`watch-slis.sh` with:

```bash
--extra 'dispatch ETA fallback share=sum(rate(dispatch_eta_requests_total{namespace="{ns}",source="fallback"}[5m])) / sum(rate(dispatch_eta_requests_total{namespace="{ns}"}[5m]))' \
--extra 'sim routes by source=sum by (source) (rate(simulator_routes_total{namespace="{ns}"}[1m]))'
```

## Blast radius

Routing in one namespace. ETA quality only; no errors expected anywhere.

## Preconditions

Preflight GO; simulator running; for the dispatch part, dispatch deployed and assigning.

## Steps

1. `record-timeline.sh start ...`, `preflight.sh`, `mark steady-state`, `snapshot`.
2. `scripts/gameday/pod-kill.sh --context $CTX --namespace $NS --app routing --count all`.
3. Note when routes switch to `straight_line`, when routing is Ready again, and when routes are back on `osrm`.
4. (Dispatch running, longer variant) merge `replicas: 0` for routing; wait 15 minutes for `ETAFallbackHigh`
   (`component=dispatch`); follow [eta-fallback](../../runbooks/eta-fallback.md); revert.
5. `mark recovered`, `snapshot`, `mark end`.

## Expected observations

| Signal | Expected |
|---|---|
| Routing back after a pod deletion | Under a minute |
| Simulator routes | `straight_line` while routing is away |
| Dispatch ETA fallback share | 100 % while away, then 0 |
| Errors anywhere | None |

## Abort criteria

The [global ones](../README.md#abort-criteria-all-experiments), plus: any 5xx from orders or dispatch (a fallback has failed).

## Rollback

`pod-kill.sh --rollback --app routing` waits for the Deployment to be available. For the longer variant, revert the pull request.

## Alerts and runbooks

| Alert | Should it fire? | Runbook |
|---|---|---|
| `ETAFallbackHigh` (`component=dispatch`) | Only in the longer variant (10 minutes) | [eta-fallback](../../runbooks/eta-fallback.md) (exists) |
| `ETAFallbackHigh` (`component=orders`) | Not because of routing; it fires whenever dispatch itself is missing | same |

## Gaps found while planning

- The runbook's dispatch log filter assumes a log message containing "osrm" or "fallback"; check it against the real
  dispatch logs when they exist.
- OSRM has no `/metrics` (`metrics.serviceMonitor.enabled: false`), so its health is only visible through dispatch and
  the simulator.

## Results

<!-- fill in after the drill -->

| Signal | Expected | Observed |
|---|---|---|
| Routing unavailable (s) | < 60 | <!-- fill in after the drill --> |
| Dispatch ETA fallback share during the outage | 100 % | <!-- fill in after the drill --> |
| Errors seen anywhere | none | <!-- fill in after the drill --> |
| Alerts that fired | | <!-- fill in after the drill --> |

Drill log: <!-- link --> · Surprises: <!-- fill in after the drill --> · Action items: <!-- fill in after the drill -->
