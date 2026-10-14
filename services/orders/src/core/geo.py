"""Tokyo's 23 wards (the dispatch zones) and great-circle distance.

data/wards.json is a copy of contracts/data/wards.json, so the image doesn't depend on files outside
this service; a test fails if the two drift apart.
"""

from __future__ import annotations

import json
import math
from dataclasses import dataclass
from pathlib import Path

EARTH_RADIUS_M = 6_371_008.8  # mean Earth radius


@dataclass(frozen=True)
class BoundingBox:
    min_lat: float
    max_lat: float
    min_lng: float
    max_lng: float

    def contains(self, lat: float, lng: float) -> bool:
        return self.min_lat <= lat <= self.max_lat and self.min_lng <= lng <= self.max_lng


def _load() -> tuple[frozenset[str], BoundingBox]:
    data = json.loads((Path(__file__).parent / "data" / "wards.json").read_text(encoding="utf-8"))
    return frozenset(ward["id"] for ward in data["wards"]), BoundingBox(**data["bbox"])


WARD_IDS, TOKYO_BBOX = _load()


def haversine_m(lat1: float, lng1: float, lat2: float, lng2: float) -> float:
    """Straight-line distance in metres over the Earth's surface."""
    phi1, phi2 = math.radians(lat1), math.radians(lat2)
    d_phi = phi2 - phi1
    d_lambda = math.radians(lng2 - lng1)
    a = math.sin(d_phi / 2) ** 2 + math.cos(phi1) * math.cos(phi2) * math.sin(d_lambda / 2) ** 2
    return 2 * EARTH_RADIUS_M * math.asin(math.sqrt(a))
