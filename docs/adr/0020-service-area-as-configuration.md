# 0020. Make the service area, money and map region configuration, not code

- Status: proposed
- Date: 2026-10-16

## Context

Tasuki is moving from "a fictional Tokyo operator" to software any business can run for its own city
([landing page](../../website/), roadmap "Any city"). Today Tokyo is compiled in:

- **Zones.** Tokyo's 23 wards (IDs and centre points) are embedded four times: `contracts/data/wards.json`, and copies
  in orders, fleet and the simulator, plus the console. About 470 lines across seven areas mention a ward.
- **Clients choose the zone.** A drop-off carries `ward`, which the client must send and the server only checks
  against a list. A merchant in another city would have to learn zone IDs that mean nothing to it.
- **The service area is a rectangle in the API contract.** The OpenAPI schema bounds `lat` to 35.52–35.82 and `lng`
  to 139.56–139.92; anything else is a 400.
- **Zone of a courier.** Tracking and dispatch assign a courier to the ward with the nearest centre, "a P1
  approximation".
- **Money is yen.** Fields are `fee_yen`, `tax_yen`; the fee formula (¥300 + ¥75 per started 500 m) and the 10 %
  consumption tax rounded down are constants in `orders/pricing.py`.
- **Roads.** The OSRM image is built from the Kantō extract with the bicycle profile.

Constraints: the event and HTTP contracts may add fields but not rename or remove them without a new version
(`contracts/README.md`); Tokyo must stay the default, because it is the reference deployment and the demo city; the
location path must stay fast (zone lookups happen for every GPS point that tracking fans out).

## Decision

**A deployment describes its city in one service-area file**: a GeoJSON `FeatureCollection` where each feature is a
zone (Polygon or MultiPolygon) with `id` and `name` properties. Every service loads it from `SERVICE_AREA_FILE` at
start. The server derives a point's zone itself (point in polygon, with a bounding-box pre-check); a point in no zone
is outside the service area (`422 outside_service_area`). Clients stop sending `ward`: it becomes optional and
ignored in v1 requests, and every response and event gains `zone` next to `ward` (same value) until the v2
contracts drop `ward`.

**Money is integer minor units of one configured currency** (`PRICING_CURRENCY`, ISO 4217), with the fee formula and
tax (rate and rounding mode) as configuration. Responses and events gain `currency`, `fee`, `fee_excl_tax` and `tax`
in minor units; the `*_yen` fields stay, filled only when the currency is JPY, until v2.

**The map region is a build input.** The routing image takes the OpenStreetMap extract URL and the OSRM profile as
build arguments; the console centres its map on the service area's bounding box.

**Tokyo stays the default.** The repository ships `contracts/data/service-areas/tokyo-23-wards.geojson`, generated
from today's ward centres as Voronoi cells clipped to today's bounding box. That reproduces the current "nearest
ward centre" behaviour exactly, so nothing changes for Tokyo until someone swaps in official ward boundaries.

## Options considered

| Option | Pros | Cons |
|---|---|---|
| **GeoJSON zones, server-derived zone (chosen)** | Any city from a file anyone can draw (geojson.io, QGIS, official open data); clients send coordinates only; one source of truth for every service | A point-in-polygon lookup per request and per tracked point (cheap for tens of zones with a bounding-box pre-check); every service gains a config file to mount |
| H3 cells as zones (resolution 7–8) | Uniform cells, fast lookups, already planned for P3 batched dispatch (ADR 0008) | Cells do not follow anything a business recognises (districts, delivery areas); ops and pricing want named areas. H3 can still index couriers inside zones later |
| Keep client-supplied zone IDs, make the list configurable | Smallest change | Every integrating business must map its addresses to zone IDs; wrong IDs are silently accepted if they exist |
| A table in the orders database | Editable at runtime | The Go services would need to read it or receive it; the location path should not depend on the orders database |
| Floats for money with a currency field | Familiar | Rounding errors; the ledger (P3) needs exact integers. Minor units are what payment providers use |

## Consequences

- Easier: a new city is a GeoJSON file, five pricing variables and a routing build argument. The integration API
  gets simpler: a drop-off is an address and coordinates.
- Harder: the Tokyo constants in the OpenAPI schema go, so the contract no longer documents the service area; it
  moves to `GET /v1/service-area` (the zones as GeoJSON) for clients and the console.
- Contracts: additive v1 changes now (`zone`, `currency`, minor-unit amounts, `ward` optional); `ward` and `*_yen`
  removed only in v2, announced in the changelog.
- Watch: the zone lookup's cost in tracking and fleet-state (benchmark it at 750 points/s); a GeoJSON file with a
  hole or overlap (validate at load: zones must not overlap, ids must be unique, polygons must be valid).
- The simulator's scenarios name zones from the same file, so a scenario for another city is the file plus a demand
  curve.
