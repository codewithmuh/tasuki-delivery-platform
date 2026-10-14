/**
 * Typed decoding of the tracking service's SSE streams (contracts/README.md, "Server-sent event
 * streams"). Data crosses a network boundary, so every field is checked; anything malformed
 * decodes to `null` and is dropped instead of breaking the page.
 */
import type { SseMessage } from './sse';

export const COURIER_STATUSES = ['idle', 'to_store', 'at_store', 'to_dropoff', 'offline'] as const;
export type CourierStatus = (typeof COURIER_STATUSES)[number];

export const ORDER_STATUSES = [
  'confirmed',
  'assigned',
  'picked_up',
  'delivered',
  'cancelled',
  'failed',
] as const;
export type OrderStatus = (typeof ORDER_STATUSES)[number];

export const END_REASONS = ['delivered', 'cancelled', 'failed', 'expired'] as const;
export type EndReason = (typeof END_REASONS)[number];

/** Times are epoch milliseconds, parsed once here. */
export interface CourierUpdate {
  courierId: string;
  lat: number;
  lng: number;
  status: CourierStatus;
  orderId: string | null;
  ward: string | null;
  deviceTime: number;
  receivedAt: number;
}

export type FleetEvent =
  { type: 'courier'; courier: CourierUpdate } | { type: 'courier_gone'; courierId: string };

export type TrackEvent =
  | { type: 'status'; orderId: string; status: OrderStatus; eta: number | null; updatedAt: number }
  | { type: 'position'; courierId: string; lat: number; lng: number; receivedAt: number }
  | { type: 'end'; reason: EndReason };

type Json = Record<string, unknown>;

function parseObject(data: string): Json | null {
  try {
    const value: unknown = JSON.parse(data);
    return typeof value === 'object' && value !== null && !Array.isArray(value)
      ? (value as Json)
      : null;
  } catch {
    return null;
  }
}

function str(o: Json, key: string): string | null {
  const v = o[key];
  return typeof v === 'string' && v !== '' ? v : null;
}

function num(o: Json, key: string, min: number, max: number): number | null {
  const v = o[key];
  return typeof v === 'number' && Number.isFinite(v) && v >= min && v <= max ? v : null;
}

function time(o: Json, key: string): number | null {
  const v = o[key];
  if (typeof v !== 'string') return null;
  const ms = Date.parse(v);
  return Number.isNaN(ms) ? null : ms;
}

function oneOf<T extends string>(values: readonly T[], v: unknown): T | null {
  return typeof v === 'string' && (values as readonly string[]).includes(v) ? (v as T) : null;
}

export function parseFleetEvent(msg: SseMessage): FleetEvent | null {
  const o = parseObject(msg.data);
  if (!o) return null;
  const courierId = str(o, 'courier_id');
  if (!courierId) return null;
  if (msg.event === 'courier_gone') return { type: 'courier_gone', courierId };
  if (msg.event !== 'courier') return null;

  const lat = num(o, 'lat', -90, 90);
  const lng = num(o, 'lng', -180, 180);
  const status = oneOf(COURIER_STATUSES, o['status']);
  const deviceTime = time(o, 'device_time');
  const receivedAt = time(o, 'received_at');
  if (lat === null || lng === null || !status || deviceTime === null || receivedAt === null) {
    return null;
  }
  return {
    type: 'courier',
    courier: {
      courierId,
      lat,
      lng,
      status,
      orderId: str(o, 'order_id'),
      ward: str(o, 'ward'),
      deviceTime,
      receivedAt,
    },
  };
}

export function parseTrackEvent(msg: SseMessage): TrackEvent | null {
  const o = parseObject(msg.data);
  if (!o) return null;
  switch (msg.event) {
    case 'status': {
      const orderId = str(o, 'order_id');
      const status = oneOf(ORDER_STATUSES, o['status']);
      const updatedAt = time(o, 'updated_at');
      if (!orderId || !status || updatedAt === null) return null;
      // `eta` is optional: an RFC 3339 time, or null/absent when there is no estimate.
      return { type: 'status', orderId, status, eta: time(o, 'eta'), updatedAt };
    }
    case 'position': {
      const courierId = str(o, 'courier_id');
      const lat = num(o, 'lat', -90, 90);
      const lng = num(o, 'lng', -180, 180);
      const receivedAt = time(o, 'received_at');
      if (!courierId || lat === null || lng === null || receivedAt === null) return null;
      return { type: 'position', courierId, lat, lng, receivedAt };
    }
    case 'end': {
      const reason = oneOf(END_REASONS, o['reason']);
      return reason ? { type: 'end', reason } : null;
    }
    default:
      return null;
  }
}
