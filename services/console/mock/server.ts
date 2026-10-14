/**
 * Mock of the tracking service's SSE streams, for demos without the backend: `npm run mock`.
 * It emits exactly the shapes in contracts/README.md ("Server-sent event streams"):
 *
 *   GET  /v1/fleet/stream?ward=    `courier`, `courier_gone`; `: ping` every 15 s
 *   GET  /v1/track/{token}/stream  `status`, `position`, `end` (then closes)
 *                                  token "expired" → 401, token "unknown" → 404, anything else → a demo order
 *   POST /mock/pause, /mock/resume stop or restart events (pings continue): the data-age badge goes
 *                                  amber, then red, like a Kafka outage on a game day
 *
 * Couriers ride in straight lines between points near ward centres (the real simulator uses OSRM).
 * Env: PORT (8090), COURIERS (300), SPEED (3 = three times faster than real time), SEED (42).
 */
import { readFileSync } from 'node:fs';
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';

interface Ward {
  id: string;
  lat: number;
  lng: number;
}
type Status = 'idle' | 'to_store' | 'at_store' | 'to_dropoff';
interface Point {
  lat: number;
  lng: number;
}
interface Courier extends Point {
  id: string;
  status: Status;
  ward: string;
  target: Point | null;
  dropoff: Point | null;
  orderId: string | null;
  speedMps: number;
  /** When the current idle or at-store wait ends (ms). */
  waitUntil: number;
  nextEmitAt: number;
  offlineUntil: number | null;
  goneSent: boolean;
}
interface Client {
  res: ServerResponse;
  ward: string | null;
}

const PORT = Number(process.env['PORT'] ?? 8090);
const COURIERS = Number(process.env['COURIERS'] ?? 300);
const SPEED = Number(process.env['SPEED'] ?? 3);
const SEED = Number(process.env['SEED'] ?? 42);
const TICK_MS = 500;
const PING_MS = 15_000;
const MOVING_EVERY_MS = 4_000; // design.md §7.2: every 4 s when moving with an order
const IDLE_EVERY_MS = 15_000; // every 15 s when idle
const GONE_AFTER_MS = 60_000; // `courier_gone` after 60 s without a point

const wardsFile = new URL('../src/data/wards.json', import.meta.url);
const WARDS = (JSON.parse(readFileSync(wardsFile, 'utf8')) as { wards: Ward[] }).wards;

// Deterministic randomness (mulberry32), so every demo run looks the same for a given SEED.
let seed = SEED >>> 0;
function random(): number {
  seed = (seed + 0x6d2b79f5) >>> 0;
  let t = seed;
  t = Math.imul(t ^ (t >>> 15), t | 1);
  t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
  return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
}
const pick = <T>(items: readonly T[]): T => items[Math.floor(random() * items.length)] as T;
const between = (min: number, max: number) => min + random() * (max - min);

const CROCKFORD = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';
function ulidLike(prefix: string): string {
  let s = '';
  for (let i = 0; i < 26; i++) s += CROCKFORD.charAt(Math.floor(random() * 32));
  return `${prefix}_${s}`;
}

/** A random point within ~1.2 km of a ward centre. */
function near(ward: Ward, spread = 0.011): Point {
  return { lat: ward.lat + between(-spread, spread), lng: ward.lng + between(-spread, spread) };
}

function metres(a: Point, b: Point): number {
  const dLat = (b.lat - a.lat) * 111_320;
  const dLng = (b.lng - a.lng) * 111_320 * Math.cos((a.lat * Math.PI) / 180);
  return Math.hypot(dLat, dLng);
}

function nearestWard(p: Point): Ward {
  let best = WARDS[0] as Ward;
  for (const w of WARDS) if (metres(p, w) < metres(p, best)) best = w;
  return best;
}

/** Move `p` toward `to` by up to `step` metres; returns true on arrival. */
function moveToward(p: Point, to: Point, step: number): boolean {
  const d = metres(p, to);
  if (d <= step) {
    p.lat = to.lat;
    p.lng = to.lng;
    return true;
  }
  p.lat += ((to.lat - p.lat) * step) / d;
  p.lng += ((to.lng - p.lng) * step) / d;
  return false;
}

// ---------------------------------------------------------------- SSE plumbing
function openSse(res: ServerResponse): void {
  res.writeHead(200, {
    'Content-Type': 'text/event-stream; charset=utf-8',
    'Cache-Control': 'no-cache',
    Connection: 'keep-alive',
    'X-Accel-Buffering': 'no',
    'Access-Control-Allow-Origin': '*',
  });
  res.write(': connected\n\n');
}

function send(res: ServerResponse, event: string, data: object): void {
  res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
}

function sendError(res: ServerResponse, status: number, code: string, message: string): void {
  res.writeHead(status, {
    'Content-Type': 'application/json',
    'Access-Control-Allow-Origin': '*',
  });
  res.end(JSON.stringify({ error: { code, message } }));
}

let paused = false;
const fleetClients = new Set<Client>();
const allStreams = new Set<ServerResponse>();

setInterval(() => {
  for (const res of allStreams) res.write(': ping\n\n');
}, PING_MS);

// ---------------------------------------------------------------- the fleet
const couriers: Courier[] = [];
const start = Date.now();
for (let i = 0; i < COURIERS; i++) {
  const ward = pick(WARDS);
  const p = near(ward, 0.02);
  couriers.push({
    id: ulidLike('cou'),
    ...p,
    status: 'idle',
    ward: nearestWard(p).id,
    target: null,
    dropoff: null,
    orderId: null,
    speedMps: between(3.5, 6.5), // bicycles and scooters in city traffic
    waitUntil: start + between(0, 30_000) / SPEED,
    nextEmitAt: start + between(0, IDLE_EVERY_MS),
    offlineUntil: null,
    goneSent: false,
  });
}

function courierEvent(c: Courier, now: number): object {
  return {
    courier_id: c.id,
    lat: Number(c.lat.toFixed(6)),
    lng: Number(c.lng.toFixed(6)),
    status: c.status,
    order_id: c.orderId,
    ward: c.ward,
    device_time: new Date(now - between(200, 1500)).toISOString(),
    received_at: new Date(now).toISOString(),
  };
}

function broadcast(c: Courier, previousWard: string, now: number): void {
  if (paused) return;
  const data = courierEvent(c, now);
  for (const client of fleetClients) {
    // Viewers of the ward a courier just left get one last point (with the new ward) so they drop it.
    if (client.ward === null || client.ward === c.ward || client.ward === previousWard) {
      send(client.res, 'courier', data);
    }
  }
}

function setStatus(c: Courier, status: Status, now: number): void {
  c.status = status;
  c.nextEmitAt = now; // status changes are sent at once
}

function step(c: Courier, now: number, dtMs: number): void {
  const metresThisTick = (c.speedMps * SPEED * dtMs) / 1000;
  switch (c.status) {
    case 'idle':
      if (now >= c.waitUntil) {
        // A new order: a store nearby, a drop-off one or two wards away.
        const here = nearestWard(c);
        const store = near(pick(WARDS.filter((w) => metres(w, here) < 4_000)));
        c.target = store;
        c.dropoff = near(pick(WARDS.filter((w) => metres(w, store) < 6_000)));
        c.orderId = ulidLike('ord');
        setStatus(c, 'to_store', now);
      }
      break;
    case 'to_store':
      if (c.target && moveToward(c, c.target, metresThisTick)) {
        c.waitUntil = now + between(20_000, 90_000) / SPEED;
        setStatus(c, 'at_store', now);
      }
      break;
    case 'at_store':
      if (now >= c.waitUntil) {
        c.target = c.dropoff;
        setStatus(c, 'to_dropoff', now);
      }
      break;
    case 'to_dropoff':
      if (c.target && moveToward(c, c.target, metresThisTick)) {
        c.target = null;
        c.dropoff = null;
        c.orderId = null;
        c.waitUntil = now + between(10_000, 60_000) / SPEED;
        setStatus(c, 'idle', now);
      }
      break;
  }
}

let lastTick = Date.now();
setInterval(() => {
  const now = Date.now();
  const dt = now - lastTick;
  lastTick = now;
  for (const c of couriers) {
    if (c.offlineUntil !== null) {
      // Logged off: silent; `courier_gone` after 60 s; back online later.
      if (!c.goneSent && now - c.nextEmitAt > GONE_AFTER_MS) {
        c.goneSent = true;
        if (!paused) {
          for (const client of fleetClients) send(client.res, 'courier_gone', { courier_id: c.id });
        }
      }
      if (now >= c.offlineUntil) {
        c.offlineUntil = null;
        c.goneSent = false;
        setStatus(c, 'idle', now);
      }
      continue;
    }
    step(c, now, dt);
    if (now >= c.nextEmitAt) {
      const previousWard = c.ward;
      c.ward = nearestWard(c).id;
      broadcast(c, previousWard, now);
      const moving = c.status === 'to_store' || c.status === 'to_dropoff';
      c.nextEmitAt = now + (moving ? MOVING_EVERY_MS : IDLE_EVERY_MS);
    }
  }
}, TICK_MS);

// Some churn: every 20 s one idle courier logs off for about three minutes.
setInterval(() => {
  const idle = couriers.filter((c) => c.status === 'idle' && c.offlineUntil === null);
  if (idle.length === 0) return;
  const c = pick(idle);
  c.offlineUntil = Date.now() + 180_000;
  c.nextEmitAt = Date.now();
}, 20_000);

function handleFleet(req: IncomingMessage, res: ServerResponse, url: URL): void {
  const ward = url.searchParams.get('ward') || null;
  if (ward !== null && !WARDS.some((w) => w.id === ward)) {
    sendError(res, 400, 'unknown_ward', `Unknown ward: ${ward}`);
    return;
  }
  openSse(res);
  const client: Client = { res, ward };
  // Start with the latest point of every online courier, so the map fills at once.
  const now = Date.now();
  if (!paused) {
    for (const c of couriers) {
      if (c.offlineUntil === null && (ward === null || c.ward === ward)) {
        send(res, 'courier', courierEvent(c, now));
      }
    }
  }
  fleetClients.add(client);
  allStreams.add(res);
  req.on('close', () => {
    fleetClients.delete(client);
    allStreams.delete(res);
  });
}

// ---------------------------------------------------------------- one demo order per token
interface Order {
  id: string;
  courierId: string;
  courier: Point;
  store: Point;
  dropoff: Point;
  status: 'confirmed' | 'assigned' | 'picked_up' | 'delivered';
  statusAt: number;
  assignAt: number;
  pickupAt: number | null;
}
const orders = new Map<string, Order>();
const ORDER_SPEED_MPS = 5;

function newOrder(now: number): Order {
  const ward = pick(WARDS);
  const store = near(ward, 0.006);
  return {
    id: ulidLike('ord'),
    courierId: ulidLike('cou'),
    courier: { lat: store.lat + 0.009, lng: store.lng - 0.008 },
    store,
    dropoff: { lat: store.lat - 0.012, lng: store.lng + 0.014 },
    status: 'confirmed',
    statusAt: now,
    assignAt: now + 8_000 / SPEED,
    pickupAt: null,
  };
}

function remainingMs(o: Order): number {
  const toStore = o.status === 'picked_up' ? 0 : metres(o.courier, o.store);
  const toDrop = metres(o.status === 'picked_up' ? o.courier : o.store, o.dropoff);
  return ((toStore + toDrop) / (ORDER_SPEED_MPS * SPEED)) * 1000 + 60_000 / SPEED;
}

function statusEvent(o: Order, now: number): object {
  return {
    order_id: o.id,
    status: o.status,
    eta: o.status === 'delivered' ? null : new Date(now + remainingMs(o)).toISOString(),
    updated_at: new Date(o.statusAt).toISOString(),
  };
}

function advance(o: Order, now: number, dtMs: number): boolean {
  const stepM = (ORDER_SPEED_MPS * SPEED * dtMs) / 1000;
  const before = o.status;
  if (o.status === 'confirmed') {
    if (now >= o.assignAt) o.status = 'assigned';
  } else if (o.status === 'assigned') {
    // Ride to the store, then wait there for the food.
    if (moveToward(o.courier, o.store, stepM)) {
      o.pickupAt ??= now + 30_000 / SPEED;
      if (now >= o.pickupAt) o.status = 'picked_up';
    }
  } else if (o.status === 'picked_up') {
    if (moveToward(o.courier, o.dropoff, stepM)) o.status = 'delivered';
  }
  if (o.status === before) return false;
  o.statusAt = now;
  return true;
}

function handleTrack(req: IncomingMessage, res: ServerResponse, token: string): void {
  if (token === 'expired') {
    sendError(res, 401, 'token_expired', 'This tracking link has expired.');
    return;
  }
  if (token === 'unknown') {
    sendError(res, 404, 'order_not_found', 'No order for this tracking link.');
    return;
  }
  let order = orders.get(token);
  if (!order) {
    order = newOrder(Date.now());
    orders.set(token, order);
  }
  const o = order;
  openSse(res);
  allStreams.add(res);

  const position = (now: number) => {
    send(res, 'position', {
      courier_id: o.courierId,
      lat: Number(o.courier.lat.toFixed(6)),
      lng: Number(o.courier.lng.toFixed(6)),
      received_at: new Date(now).toISOString(),
    });
  };
  const finish = () => {
    send(res, 'end', { reason: 'delivered' });
    res.end();
  };

  let now = Date.now();
  send(res, 'status', statusEvent(o, now));
  if (o.status === 'delivered') {
    finish();
    return;
  }
  if (o.status !== 'confirmed') position(now);

  let last = now;
  let lastPosition = now;
  const timer = setInterval(() => {
    now = Date.now();
    const changed = advance(o, now, now - last);
    last = now;
    if (paused) return;
    if (changed) send(res, 'status', statusEvent(o, now));
    if (o.status === 'delivered') {
      clearInterval(timer);
      finish();
      return;
    }
    if (o.status !== 'confirmed' && (changed || now - lastPosition >= MOVING_EVERY_MS)) {
      lastPosition = now;
      position(now);
    }
  }, TICK_MS);
  req.on('close', () => {
    clearInterval(timer);
    allStreams.delete(res);
  });
}

// ---------------------------------------------------------------- HTTP
const server = createServer((req, res) => {
  const url = new URL(req.url ?? '/', 'http://localhost');
  const track = /^\/v1\/track\/([^/]+)\/stream$/.exec(url.pathname);
  if (req.method === 'GET' && url.pathname === '/healthz') {
    res.end('ok\n');
  } else if (req.method === 'GET' && url.pathname === '/v1/fleet/stream') {
    handleFleet(req, res, url);
  } else if (req.method === 'GET' && track?.[1]) {
    handleTrack(req, res, decodeURIComponent(track[1]));
  } else if (
    req.method === 'POST' &&
    (url.pathname === '/mock/pause' || url.pathname === '/mock/resume')
  ) {
    paused = url.pathname === '/mock/pause';
    console.log(paused ? 'events paused (pings continue)' : 'events resumed');
    res.end(paused ? 'paused\n' : 'resumed\n');
  } else {
    sendError(res, 404, 'not_found', 'Not found');
  }
});

server.listen(PORT, () => {
  console.log(`mock tracking on http://localhost:${PORT}: ${COURIERS} couriers, ${SPEED}x speed`);
  console.log('  fleet: /v1/fleet/stream?ward=shibuya   track: /v1/track/demo/stream');
  console.log(`  game day: curl -X POST localhost:${PORT}/mock/pause   (and /mock/resume)`);
});

for (const signal of ['SIGINT', 'SIGTERM'] as const) {
  process.on(signal, () => {
    for (const res of allStreams) res.end();
    server.close(() => process.exit(0));
    setTimeout(() => process.exit(0), 1_000).unref();
  });
}
