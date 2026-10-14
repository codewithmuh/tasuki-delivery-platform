# console

The dispatcher's live map of Tokyo and the customer tracking page. React + TypeScript + MapLibre GL,
served as static files by an unprivileged nginx.

> Tasuki is a fictional company. Every courier and order on these screens is simulated.

| Route                  | Who        | Data                                                                           |
| ---------------------- | ---------- | ------------------------------------------------------------------------------ |
| `/` (`/?ward=shibuya`) | Dispatcher | `GET /v1/fleet/stream?ward=` from tracking: every courier in a ward, or all 23 |
| `/track/:token`        | Customer   | `GET /v1/track/{token}/stream` from tracking: one order's courier and status   |

Both streams are server-sent events. The event shapes are defined in
[contracts/README.md](../../contracts/README.md#server-sent-event-streams).

## What it shows

**Live map (`/`).** Couriers as one GeoJSON layer, shaped and coloured by status: idle ●, to store ▲,
at store ■, to drop-off ◆. A ward filter (23 wards, English and 日本語) frames the ward and reconnects
the stream with `?ward=`. The panel shows couriers online, a count per status, the connection state
(live / reconnecting in N s) and the **data age**.

**Data age** is `now − received_at` of the newest event on the stream. It covers the whole path
(ingest → Kafka → tracking → browser), so it is what a dispatcher actually experiences, and it is the
same quantity as the tracking-freshness SLO ([design.md §9](../../docs/design.md#9-slos): 99 % of events
younger than 10 s). The badge is green under 10 s, amber from 10 s, red from 30 s. It keeps growing when
events stop even though heartbeats keep the connection open, which is how a stalled pipeline shows up
during a game day. The map keeps the last known positions instead of going blank (design.md §5: "serve
the last known position and say how old it is").

**Tracking page (`/track/:token`).** The courier on a map, the steps confirmed → courier assigned →
picked up → delivered, the ETA in Tokyo time, "updated N s ago", and an English / 日本語 toggle (a small
typed dictionary, no i18n library; the choice is saved in `localStorage`). An expired or unknown link
(401 / 404, or an `end` event with reason `expired`) shows a friendly message instead of a broken map.

## Run it

Needs Node 22.12+ (Node 24 LTS recommended; CI and the image use 24).

```bash
npm ci
npm run mock      # terminal 1: mock tracking service on :8090
npm run dev       # terminal 2: http://localhost:5173
# or both at once from the repo root: make console-dev
```

Then open:

- <http://localhost:5173/> and <http://localhost:5173/?ward=shibuya>: the live map
- <http://localhost:5173/track/demo>: a delivery that runs from confirmed to delivered in about 4 minutes
- <http://localhost:5173/track/expired> (401) and <http://localhost:5173/track/unknown> (404): the expired-link page

To see degradation, stop events while keeping the connection open, then start them again:

```bash
curl -X POST localhost:8090/mock/pause    # data age goes amber after 10 s, red after 30 s
curl -X POST localhost:8090/mock/resume
```

The mock replays couriers riding in straight lines between points near ward centres (the real
simulator uses OSRM roads). Settings: `COURIERS=3000 SPEED=1 SEED=7 npm run mock` (defaults 300, 3×, 42).
It emits the same event shapes, heartbeat and status codes as the contract.

In dev and `npm run preview`, Vite proxies `/v1/*` to `TRACKING_URL` (default `http://localhost:8090`),
so to use the real tracking service run `TRACKING_URL=http://localhost:8081 npm run dev`.

## Configuration

The built files are environment-agnostic. At start the page reads `/config.json`:

```json
{ "trackingBaseUrl": "" }
```

`trackingBaseUrl` is the tracking service's origin (`https://host[:port]`, no path). Empty means same
origin, which is the cluster default: the ingress routes `/v1/*` to tracking and everything else to
the console, so no CORS is needed. In the container, the file is written at start from one
environment variable:

| Variable                    | Default             | Effect                                                                                                                              |
| --------------------------- | ------------------- | ----------------------------------------------------------------------------------------------------------------------------------- |
| `CONSOLE_TRACKING_BASE_URL` | empty (same origin) | `trackingBaseUrl` in `/config.json` and the CSP `connect-src`. Anything that is not an http(s) origin stops the container at start. |

If tracking runs on another origin (for example locally in Compose), tracking must answer with CORS
headers for the console's origin.

## Container

```bash
docker build -t tasuki/console:local .
docker run --rm --read-only --tmpfs /tmp --cap-drop ALL -p 8088:8080 \
  -e CONSOLE_TRACKING_BASE_URL=http://localhost:8090 tasuki/console:local
```

- Build stage `node:24` (pinned), runtime `nginxinc/nginx-unprivileged` (pinned, uid 101, port 8080).
- Read-only root filesystem: nginx writes only to `/tmp` (pid, temp files, and the config rendered from
  `docker/templates` by the image's entrypoint). Mount an `emptyDir` at `/tmp` in Kubernetes.
- `/healthz` and `/readyz` return 200 (no dependencies to check).
- Security headers: a Content-Security-Policy that allows only this origin, the tracking origin and the
  tile host (`script-src 'self'`, `worker-src 'self'`, nothing inline), `X-Content-Type-Options`,
  `Referrer-Policy: no-referrer` (tracking URLs carry a token, so no Referer goes to the tile server),
  `X-Frame-Options`, `Permissions-Policy`, `noindex`.
- Caching: hashed `/assets/*` for a year (`immutable`); `index.html` `no-cache`; `config.json` `no-store`.
- gzip on; access logs are one JSON line per request with `request_id`, and `/track/<token>` paths are
  logged as `/track/[redacted]`.
- Stop signal is `SIGQUIT` (graceful in nginx).
- No `/metrics`: nginx's `stub_status` listens on `127.0.0.1:8081` inside the pod for an
  `nginx-prometheus-exporter` sidecar.

## Map tiles

The basemap is the public instance of [OpenFreeMap](https://openfreemap.org): vector tiles built from
OpenStreetMap, styles "Positron" (light) and "Dark" (following the OS setting). It was chosen because it
needs **no API key, no account and no cookies**, has no request limits, and its
[terms](https://openfreemap.org/tos/) allow commercial use. Map data is © OpenStreetMap contributors
under the ODbL; the attribution ("OpenFreeMap © OpenMapTiles Data from OpenStreetMap") comes with the
tiles and is always shown, uncollapsed, in the corner of the map.

The trade-off: a free public service has no SLA and could change. For a real deployment the same styles
can be self-hosted (OpenFreeMap is open source, or Protomaps PMTiles for the Kantō region on S3 behind
CloudFront), which keeps tiles inside our own infrastructure. Switching means changing
`src/lib/basemap.ts` and the CSP host in `docker/templates/tasuki/console.conf.template`; a unit test
fails if the two disagree.

## How it stays smooth at 3,000 couriers

- One GeoJSON source and one symbol layer, updated in place with `setData`: no DOM marker per courier.
- Events update a plain store (`src/lib/fleet.ts`), not React state. The map and counters are
  refreshed from it every 500 ms, so ~750 events/s at peak cost two `setData` calls per second.
- Out-of-order or replayed points are ignored (latest `device_time` wins, as in the event contract).
- Couriers more than 90 s behind the newest point are dropped as a backstop for a missed
  `courier_gone`. Ages use the server clock, so nothing is dropped when the whole stream stalls.
- MapLibre and React are separate long-cached chunks; app code is about 10 kB gzipped.

## Resilience

The streams are read with `fetch` and a small SSE parser instead of `EventSource`, because
`EventSource` cannot see HTTP status codes and the tracking page must tell an expired link from a
network error. The client (`src/lib/stream.ts`):

- reconnects after network errors, 5xx, 429 or a closed stream, with exponential backoff and full
  jitter (1 s base, 30 s cap), so thousands of browsers do not reconnect in lockstep after a deploy;
- stops on 401 / 403 / 404 / 410 and other 4xx;
- aborts a connection that has been silent for 45 s (three missed 15 s heartbeats) and reconnects;
- stops for good after the tracking stream's `end` event.

## Accessibility

Every control is a native, keyboard-focusable element with a visible focus ring. Status is never shown
by colour alone: each status has a shape and a label, and badges carry a symbol and words. Text meets
WCAG AA contrast in light and dark mode. Status changes are announced politely to screen readers (not
the per-second counters). Animations are skipped when the OS asks for reduced motion.

## Checks

```bash
npm run check   # prettier, eslint, tsc, vitest with coverage, build
```

Unit tests cover the pure logic in `src/lib` (SSE parsing, event decoding, the courier store,
staleness, backoff, the stream client, the tracking reducer, i18n, config, routes) with an 80 %
coverage gate. `src/data/wards.json` is a copy of `contracts/data/wards.json`, kept because the image
is built from this directory alone; a test fails if they differ.

## Layout

```text
src/lib/         pure logic, unit-tested (sse, events, stream, backoff, fleet, staleness, track, i18n, …)
src/components/  CourierMap (MapLibre), Badge, StatusIcon
src/pages/       OpsPage (/), TrackPage (/track/:token)
mock/server.ts   mock tracking SSE server (run with Node's type stripping)
docker/          nginx.conf, templates rendered at start, start-up config check
```

## Screenshots

_To be added: the live map (all wards and one ward), a stale stream during a game day, and the tracking
page in English and Japanese._
