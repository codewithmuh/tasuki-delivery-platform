# routing: OSRM on Tokyo's roads

Road travel times and routes on the real streets of Tokyo's 23 wards, for `dispatch` (ETAs and
courier-to-store times) and the `simulator` (couriers riding along real roads). It is the open
source [OSRM](https://project-osrm.org/) routing engine with a graph built from OpenStreetMap,
packed into one container image.

> **Map data © [OpenStreetMap contributors](https://www.openstreetmap.org/copyright)**, available
> under the [Open Database License (ODbL 1.0)](https://opendatacommons.org/licenses/odbl/1-0/).
> The routing graph in the image is a derived database. Any map or route shown to users must
> carry the "© OpenStreetMap contributors" credit. OSM data is downloaded at build time and is
> never committed to this repository (`data/` is git-ignored).

## What's inside

| | |
|---|---|
| Engine | `ghcr.io/project-osrm/osrm-backend:v26.10.0-debian` (BSD-2-Clause), pinned by tag |
| Data | Geofabrik's Kantō extract, snapshot `kanto-260101` (pinned by MD5), clipped to the 23-ward bounding box in [contracts/data/wards.json](../../contracts/data/wards.json) |
| Profile | `bicycle.lua` (see below) |
| Algorithm | Multi-Level Dijkstra (MLD): `osrm-extract` → `osrm-partition` → `osrm-customize`, served by `osrm-routed --algorithm mld` |
| Runtime user | `65532` (non-root, numeric), read-only data, port `5000` |

The [Dockerfile](Dockerfile) has three stages:

1. **osm** (Debian trixie + `osmium-tool` 1.18): downloads `kanto-<snapshot>.osm.pbf` and checks
   it against Geofabrik's published `.md5`, and against the pinned `OSM_MD5` when it is set.
   It then clips the file to the 23 wards with `osmium extract --strategy complete_ways`, so
   roads crossing the edge stay whole. The 500 MB Kantō file is deleted in the same layer.
2. **graph** (the OSRM image): builds the bicycle routing graph and deletes the input.
3. **runtime** (the same OSRM image, so `osrm-routed` and its libraries match exactly): only
   the graph files plus `osm-snapshot.txt`, which records the source URL, MD5, OSM replication
   timestamp and bounding box.

### Which OSM snapshot, and how it is recorded

The snapshot is a build argument, so builds are reproducible: the same arguments give the same
roads. The default is Geofabrik's yearly snapshot `260101` (2026-01-01). Geofabrik keeps yearly
snapshots for years, but daily ones only for about two weeks, so a dated daily snapshot would
stop building soon after. The snapshot date ends up in the image in two places:

- labels: `io.tasuki.osm.snapshot`, `io.tasuki.osm.source`, `io.tasuki.osm.bbox`, `io.tasuki.osm.attribution`
  (`docker inspect --format '{{json .Config.Labels}}' <image>`);
- `/data/osm-snapshot.txt`, which includes the replication timestamp read from the file itself.

To build from today's data instead:

```bash
docker build --build-arg OSM_SNAPSHOT=latest --build-arg OSM_MD5= -t tasuki/routing:latest-osm services/routing
```

To move the pinned snapshot to a newer one, change `OSM_SNAPSHOT` and `OSM_MD5` together. The MD5
is published next to each file at `https://download.geofabrik.de/asia/japan/kanto-<snapshot>.osm.pbf.md5`.

## Why the bicycle profile

Tasuki's couriers ride bicycles and scooters, not cars. The bicycle profile:

- routes through bike lanes, narrow streets and paths that a car profile excludes. These
  shortcuts are everywhere in Tokyo's residential blocks;
- lets riders push the bike the wrong way down a one-way street at walking speed, as couriers do;
- avoids motorways (the Shuto Expressway) and other roads closed to bicycles.

Its own speed model (about 15 km/h) also suits a bicycle. Scooters (原付, 50 cc) can use most of
the same roads, and the simulator rides them along the bicycle route at its own scooter speed
(25 km/h). Expressway shortcuts that a 125 cc scooter could take are the error we accept. One
graph keeps the image and its build time down. If scooter accuracy matters later, build a
second graph with a custom profile. Record that decision in an ADR.

## Endpoints Tasuki uses

The profile name in the URL is `bicycle`. OSRM answers with whatever graph it loaded, but the
name documents the intent. Coordinates are **longitude,latitude**.

| Endpoint | Caller | Purpose |
|---|---|---|
| `GET /route/v1/bicycle/{lng},{lat};{lng},{lat}?overview=full&geometries=polyline&steps=false` | simulator | The road path a courier follows (polyline, precision 5) |
| `GET /table/v1/bicycle/{lng},{lat};...?sources=...&destinations=...` | dispatch | Courier × order travel-time matrix (up to `--max-table-size` 1000) |
| `GET /route/v1/bicycle/...?overview=false` | dispatch | One ETA (store → drop-off) |
| `GET /nearest/v1/bicycle/{lng},{lat}` | health checks | Answers 200 once the graph is loaded |

Example: Shibuya station to Shinjuku station.

```bash
curl -s 'http://127.0.0.1:5000/route/v1/bicycle/139.7016,35.6580;139.7005,35.6900?overview=false' | jq '.routes[0] | {distance, duration}'
```

OSRM has no `/healthz`. Probe `/nearest/v1/bicycle/139.7016,35.6580` (HTTP 200 when ready) or
TCP port 5000. The image's `HEALTHCHECK` does that with bash's `/dev/tcp`, since there is no curl.

## Build and run

```bash
make routing-build      # docker build -t tasuki/routing:local services/routing
make routing-run        # serve on http://127.0.0.1:5000
```

Without Docker, if you have `osmium-tool` and the OSRM tools installed:

```bash
services/routing/build-data.sh               # writes services/routing/data/ (git-ignored)
osrm-routed --algorithm mld --port 5000 services/routing/data/tokyo-23.osrm
```

The script runs the same steps as the Dockerfile. It reads the bounding box from
`contracts/data/wards.json` with `jq`, keeps the Kantō download for re-runs, and accepts the
same `OSM_SNAPSHOT` / `OSM_MD5` settings.

### Expected build time and size (estimates, not yet measured)

These figures are **estimates**, scaled from OSRM's published numbers for city extracts of
similar size. They will be replaced with numbers from the first CI build.

| | Estimate |
|---|---|
| Download | ~500 MB Kantō extract, about 1 min on a GitHub runner |
| Clipped Tokyo extract | ~60–100 MB |
| Build time (4 vCPU, 16 GB runner) | ~10–20 min; `osrm-extract` is the slow step |
| Peak memory while building | ~4–6 GB |
| Graph files | ~0.7–1.5 GB |
| Final image | ~1–2 GB uncompressed |
| Memory while serving | ~1–2 GiB |

Because of this cost, CI builds the image only on pushes to `main` and on manual runs, not on
every pull request (see [.github/workflows/simulator.yml](../../.github/workflows/simulator.yml)).
The repo-wide Trivy scan in `ci.yml` checks the Dockerfile on every PR. The image scan is
informational, because the base image is upstream OSRM and fixes come from bumping its tag.
