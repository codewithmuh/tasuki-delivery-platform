/**
 * The ops map's courier state. It lives outside React: at peak the "all wards" stream carries
 * ~750 events/s, so events update this store and the map pulls a GeoJSON snapshot a few times a
 * second, instead of re-rendering React per event.
 */
import type { CourierStatus, CourierUpdate, FleetEvent } from './events';

/** Statuses shown on the map; `offline` couriers are removed. */
export const ACTIVE_STATUSES = ['idle', 'to_store', 'at_store', 'to_dropoff'] as const;
export type ActiveStatus = (typeof ACTIVE_STATUSES)[number];

/** Server sends `courier_gone` after 60 s without a point; we prune a little later as a backstop. */
export const PRUNE_AFTER_MS = 90_000;

export interface FleetCounts {
  online: number;
  byStatus: Record<ActiveStatus, number>;
}

export interface CourierFeatureCollection {
  type: 'FeatureCollection';
  features: {
    type: 'Feature';
    geometry: { type: 'Point'; coordinates: [number, number] };
    properties: { courier_id: string; status: ActiveStatus };
  }[];
}

function isActive(status: CourierStatus): status is ActiveStatus {
  return status !== 'offline';
}

export class FleetStore {
  private readonly couriers = new Map<string, CourierUpdate & { status: ActiveStatus }>();
  /** Newest `received_at` seen on this stream (server clock), or null before the first event. */
  latestReceivedAt: number | null = null;
  /** Increments on every change, so the map only re-uploads data when something moved. */
  version = 0;

  /** @param ward when set, couriers reported in another ward are dropped (left the filter). */
  constructor(readonly ward: string | null = null) {}

  get size(): number {
    return this.couriers.size;
  }

  apply(event: FleetEvent): void {
    if (event.type === 'courier_gone') {
      if (this.couriers.delete(event.courierId)) this.version++;
      return;
    }
    const c = event.courier;
    if (this.latestReceivedAt === null || c.receivedAt > this.latestReceivedAt) {
      this.latestReceivedAt = c.receivedAt;
    }
    const known = this.couriers.get(c.courierId);
    // At-least-once delivery can replay or reorder points: the latest device_time wins.
    if (known && known.deviceTime > c.deviceTime) return;

    const leftWard = this.ward !== null && c.ward !== null && c.ward !== this.ward;
    if (!isActive(c.status) || leftWard) {
      if (known) {
        this.couriers.delete(c.courierId);
        this.version++;
      }
      return;
    }
    this.couriers.set(c.courierId, { ...c, status: c.status });
    this.version++;
  }

  /**
   * Remove couriers whose last point is much older than the stream's newest point. Ages are
   * measured on the server clock, so when the whole stream stalls (a game day) nothing is
   * pruned: the map keeps the last known positions and the data-age indicator says how old.
   */
  prune(maxAgeMs: number = PRUNE_AFTER_MS): number {
    if (this.latestReceivedAt === null) return 0;
    let removed = 0;
    for (const [id, c] of this.couriers) {
      if (this.latestReceivedAt - c.receivedAt > maxAgeMs) {
        this.couriers.delete(id);
        removed++;
      }
    }
    if (removed > 0) this.version++;
    return removed;
  }

  counts(): FleetCounts {
    const byStatus: Record<ActiveStatus, number> = {
      idle: 0,
      to_store: 0,
      at_store: 0,
      to_dropoff: 0,
    };
    for (const c of this.couriers.values()) byStatus[c.status]++;
    return { online: this.couriers.size, byStatus };
  }

  toGeoJSON(): CourierFeatureCollection {
    const features: CourierFeatureCollection['features'] = [];
    for (const c of this.couriers.values()) {
      features.push({
        type: 'Feature',
        geometry: { type: 'Point', coordinates: [c.lng, c.lat] },
        properties: { courier_id: c.courierId, status: c.status },
      });
    }
    return { type: 'FeatureCollection', features };
  }
}
