# GD-10: Dispatch replica loss

| | |
|---|---|
| Status | **Planned, not run** |
| Design | [§10](../../design.md#10-failure-modes-and-degradation) "Dispatch replica dies"; §7.3 |
| SLOs at stake | dispatch assignment speed |
| Environment | `tasuki-dev`, then `tasuki-prod` (2 replicas) |
| Time box | 30 minutes |
| Scripts | `pod-kill.sh`, `watch-slis.sh`, `record-timeline.sh` |

## Hypothesis

In P1, dispatch replicas own Kafka partitions through a consumer group (`KAFKA_GROUP: dispatch`) and keep their state
in Valkey. When one of two replicas is killed:

1. Its partitions move to the survivor after a consumer-group rebalance (seconds; faster on a graceful shutdown,
   which leaves the group).
2. Orders it owned wait at most the rebalance plus one matching round: assignment speed holds for nearly all of them.
3. Offers in flight are not duplicated: no courier gets two offers for one order, and no order gets two couriers.
   `dispatch_offers_total` and `dispatch.events` let the drill check this.
4. Couriers whose offer stream was on the dead replica reconnect (SSE retry) to the survivor.

## Steady state

`watch-slis.sh` with:

```bash
--extra 'pending orders=sum(dispatch_pending_orders{namespace="{ns}"})' \
--extra 'offers by outcome=sum by (outcome) (rate(dispatch_offers_total{namespace="{ns}"}[1m]))' \
--extra 'offer streams by pod=sum by (pod) (dispatch_offer_streams{namespace="{ns}"})'
```

## Blast radius

Dispatch in one namespace; assignment speed budget in prod.

## Preconditions

Dispatch running with 2 replicas (prod: a Rollout, gate in `dryRun`), simulator running at a meal peak, preflight GO (`--service dispatch`).

## Steps

1. `record-timeline.sh start ...`, `preflight.sh`, `mark steady-state`, `snapshot`.
2. `scripts/gameday/pod-kill.sh --context $CTX --namespace $NS --app dispatch --count 1`.
3. Note the rebalance in the survivor's logs, pending orders, and offer streams moving to the survivor.
4. After recovery, check for duplicates: no `order_id` with two `CourierAssigned` events for different couriers in
   `dispatch.events` during the window (read the topic with `kafka-console-consumer.sh` inside a broker pod).
5. `mark recovered`, `snapshot`, `mark end`.

## Abort criteria

The [global ones](../README.md#abort-criteria-all-experiments), plus: any order assigned to two couriers.

## Rollback

The Deployment replaces the pod; `pod-kill.sh --rollback --app dispatch` waits for it.

## Alerts and runbooks

| Alert | Should it fire? | Runbook |
|---|---|---|
| `DispatchAssignmentSpeedBudgetBurn` | **No**, for one replica | [slo-dispatch-assignment-speed](../../runbooks/slo-dispatch-assignment-speed.md) |
| `KafkaConsumerStalled` (dispatch: page after 2 min) | Only if the dead replica's partitions are not picked up: messages waiting and no commit | [kafka-consumer-lag](../../runbooks/kafka-consumer-lag.md) |

## Results

<!-- fill in after the drill -->

Drill log: <!-- link --> · Hypothesis held? <!-- fill in after the drill --> · Surprises: <!-- fill in after the drill --> · Action items: <!-- fill in after the drill -->
