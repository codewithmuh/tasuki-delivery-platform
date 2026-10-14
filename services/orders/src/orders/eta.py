"""ETA for a quote: ask dispatch, and fall back to a local estimate when it is slow or down.

A quote must not wait on dispatch: the call has a tight timeout (DISPATCH_TIMEOUT_MS, 300 ms), and
on any failure the quote uses the straight-line distance times a Tokyo detour factor at bicycle
speed, plus the store's prep time. Every fallback is counted (orders_eta_fallbacks_total{reason}).
"""

from __future__ import annotations

import logging
import math
from dataclasses import dataclass
from functools import cache
from typing import Any, Literal

import httpx
from django.conf import settings

from accounts.models import Store
from core.metrics import ETA_FALLBACKS

log = logging.getLogger("orders.eta")

DETOUR_FACTOR = 1.35  # road distance / straight-line distance, a common figure for dense cities
FALLBACK_SPEED_MPS = 15 * 1000 / 3600  # about 15 km/h: a bicycle or scooter in city traffic
MAX_PLAUSIBLE_ETA_S = 6 * 3600  # anything above this from dispatch is treated as a bad answer

EtaSource = Literal["dispatch", "fallback"]


@dataclass(frozen=True)
class Eta:
    seconds: int
    source: EtaSource


def travel_seconds(distance_m: float) -> int:
    return math.ceil(distance_m * DETOUR_FACTOR / FALLBACK_SPEED_MPS)


def fallback_eta_seconds(distance_m: float, prep_minutes: int) -> int:
    """The courier is assumed to reach the store by the time the order is ready."""
    return prep_minutes * 60 + travel_seconds(distance_m)


@cache
def _client(base_url: str, timeout_ms: int) -> httpx.Client:
    # One pooled client per process keeps connections to dispatch warm, which matters with a 300 ms
    # budget. httpx applies the timeout to each phase (connect, write, read, pool), not the whole
    # call.
    return httpx.Client(base_url=base_url, timeout=httpx.Timeout(timeout_ms / 1000))


def _seconds(data: dict[str, Any], field: str) -> int:
    value: object = data[field]
    # bool is a subclass of int in Python, so `true` would otherwise pass as 1 second.
    if isinstance(value, bool) or not isinstance(value, int):
        raise ValueError(f"{field} is not an integer")
    if not 0 <= value <= MAX_PLAUSIBLE_ETA_S:
        raise ValueError(f"{field} is not a plausible number of seconds")
    return value


def ask_dispatch(store: Store, dropoff_lat: float, dropoff_lng: float) -> int:
    """Dispatch's ETA in seconds, made no earlier than the store's prep time plus the ride."""
    response = _client(settings.DISPATCH_URL, settings.DISPATCH_TIMEOUT_MS).get(
        "/v1/eta",
        params={
            "ward": store.ward,
            "store_lat": store.lat,
            "store_lng": store.lng,
            "dropoff_lat": dropoff_lat,
            "dropoff_lng": dropoff_lng,
        },
    )
    response.raise_for_status()
    data = response.json()
    if not isinstance(data, dict):
        raise ValueError("expected a JSON object")
    eta = _seconds(data, "eta_seconds")
    # Dispatch knows couriers and roads but not the kitchen, so the order can't arrive before it is
    # ready and has been ridden over.
    travel = _seconds(data, "travel_s") if "travel_s" in data else 0
    return max(eta, store.prep_minutes * 60 + travel)


def estimate_eta(store: Store, dropoff_lat: float, dropoff_lng: float, distance_m: float) -> Eta:
    reason: str
    try:
        return Eta(ask_dispatch(store, dropoff_lat, dropoff_lng), "dispatch")
    except httpx.TimeoutException:
        reason = "timeout"
    except httpx.HTTPError:  # connection refused, DNS, 5xx, ...
        reason = "unavailable"
    except (ValueError, KeyError, TypeError):  # a 200 we can't use
        reason = "bad_response"
    ETA_FALLBACKS.labels(reason).inc()
    log.warning("dispatch ETA unavailable; using local estimate", extra={"reason": reason})
    return Eta(fallback_eta_seconds(distance_m, store.prep_minutes), "fallback")
