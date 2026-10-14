"""Delivery fee: distance-based, in integer yen, with 10 % consumption tax.

    fee excl. tax = ¥300 base + ¥75 for every started 500 m of straight-line distance (at least one)
    tax           = floor(fee excl. tax * 10 / 100)   rounded down, once per order (端数切り捨て)
    fee           = fee excl. tax + tax

Example: 2.4 km starts 5 steps of 500 m: ¥300 + 5 x ¥75 = ¥675; tax ¥67 (67.5 down); fee ¥742.

Tax is rounded down once per order, on the order's fee; integer arithmetic only, so no float
rounding. A qualified invoice (インボイス制度) rounds tax once per invoice and tax rate,
so the weekly merchant statement (ledger, P3) recomputes tax on its own total; the per-order tax
here is what the quote shows.
"""

from __future__ import annotations

from dataclasses import dataclass

BASE_FEE_YEN = 300
STEP_M = 500
STEP_FEE_YEN = 75
CONSUMPTION_TAX_PERCENT = 10


@dataclass(frozen=True)
class Fee:
    excl_tax_yen: int
    tax_yen: int

    @property
    def total_yen(self) -> int:
        return self.excl_tax_yen + self.tax_yen


def consumption_tax(amount_yen: int) -> int:
    """10 % of an integer yen amount, rounded down."""
    return amount_yen * CONSUMPTION_TAX_PERCENT // 100


def delivery_fee(distance_m: int) -> Fee:
    if distance_m < 0:
        raise ValueError("distance cannot be negative")
    started_steps = max(1, -(-distance_m // STEP_M))  # ceiling division without floats
    excl_tax = BASE_FEE_YEN + started_steps * STEP_FEE_YEN
    return Fee(excl_tax_yen=excl_tax, tax_yen=consumption_tax(excl_tax))
