import pytest

from orders.pricing import Fee, consumption_tax, delivery_fee


@pytest.mark.parametrize(
    ("distance_m", "excl_tax", "tax", "total"),
    [
        (0, 375, 37, 412),  # minimum one step; 37.5 rounds down
        (1, 375, 37, 412),
        (500, 375, 37, 412),  # exactly one step
        (501, 450, 45, 495),  # a started step is a whole step
        (2_400, 675, 67, 742),  # the docstring's example: 67.5 rounds down
        (3_000, 750, 75, 825),
        (10_000, 1_800, 180, 1_980),
    ],
)
def test_delivery_fee(distance_m: int, excl_tax: int, tax: int, total: int) -> None:
    fee = delivery_fee(distance_m)
    assert fee == Fee(excl_tax_yen=excl_tax, tax_yen=tax)
    assert fee.total_yen == total


@pytest.mark.parametrize(
    ("amount", "tax"), [(0, 0), (9, 0), (10, 1), (19, 1), (999, 99), (1_000, 100)]
)
def test_consumption_tax_rounds_down(amount: int, tax: int) -> None:
    assert consumption_tax(amount) == tax


def test_fee_is_integer_yen_all_the_way() -> None:
    fee = delivery_fee(12_345)
    assert all(isinstance(v, int) for v in (fee.excl_tax_yen, fee.tax_yen, fee.total_yen))


def test_negative_distance_is_a_bug() -> None:
    with pytest.raises(ValueError, match="negative"):
        delivery_fee(-1)
