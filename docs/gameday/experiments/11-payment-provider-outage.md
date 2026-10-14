# GD-11: Payment provider slow or down

| | |
|---|---|
| Status | **Blocked**: the order saga (Temporal), the ledger and the mock payment provider are P3 (design §13) |
| Design | [§10](../../design.md#10-failure-modes-and-degradation) "Payment provider slow or down"; §7.5 |
| SLOs at stake | orders availability (must hold for merchants in good standing); ledger correctness |
| Environment | `tasuki-dev`, then `tasuki-prod` |

## Hypothesis (to confirm when P3 is built)

When the mock provider's failure injection makes authorisations slow (for example 5 s) or fail (100 %):

1. The circuit breaker opens; order creation for merchants in good standing keeps succeeding without waiting on the
   provider. The saga retries the authorisation later, and the business carries a small, bounded credit risk.
2. Merchants on probation are refused with a clear error, not a 5xx.
3. When the provider recovers, every pending authorisation completes or is compensated, and the next daily
   reconciliation balances to zero.

## What has to exist first

The mock provider with configurable failures, the Temporal saga with compensation, the per-merchant credit limit
(an open question in design §15), metrics for breaker state and pending authorisations, and alerts with runbooks for both.

## Steps, abort criteria, rollback, alerts

To be written with the P3 services, in the same format as [GD-01](01-redis-loss.md).

## Results

<!-- fill in after the drill -->
