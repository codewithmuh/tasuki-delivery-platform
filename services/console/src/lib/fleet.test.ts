import { describe, expect, it } from 'vitest';
import type { CourierUpdate, FleetEvent } from './events';
import { FleetStore, PRUNE_AFTER_MS } from './fleet';

const T0 = Date.parse('2026-10-14T03:00:00Z');

function courier(id: string, patch: Partial<CourierUpdate> = {}): FleetEvent {
  return {
    type: 'courier',
    courier: {
      courierId: id,
      lat: 35.66,
      lng: 139.7,
      status: 'idle',
      orderId: null,
      ward: 'shibuya',
      deviceTime: T0,
      receivedAt: T0,
      ...patch,
    },
  };
}

describe('FleetStore', () => {
  it('adds and moves couriers, newest device_time wins', () => {
    const s = new FleetStore();
    s.apply(courier('a'));
    s.apply(courier('a', { lat: 35.7, deviceTime: T0 + 4000, receivedAt: T0 + 4100 }));
    s.apply(courier('a', { lat: 35.5, deviceTime: T0 + 1000, receivedAt: T0 + 5000 })); // late replay
    expect(s.size).toBe(1);
    expect(s.toGeoJSON().features[0]?.geometry.coordinates).toEqual([139.7, 35.7]);
    expect(s.latestReceivedAt).toBe(T0 + 5000);
  });

  it('counts couriers by status', () => {
    const s = new FleetStore();
    s.apply(courier('a'));
    s.apply(courier('b', { status: 'to_store' }));
    s.apply(courier('c', { status: 'to_dropoff' }));
    s.apply(courier('d', { status: 'to_dropoff' }));
    expect(s.counts()).toEqual({
      online: 4,
      byStatus: { idle: 1, to_store: 1, at_store: 0, to_dropoff: 2 },
    });
  });

  it('removes couriers that go offline or are reported gone', () => {
    const s = new FleetStore();
    s.apply(courier('a'));
    s.apply(courier('b'));
    s.apply(courier('a', { status: 'offline', deviceTime: T0 + 1 }));
    s.apply({ type: 'courier_gone', courierId: 'b' });
    s.apply({ type: 'courier_gone', courierId: 'never-seen' });
    s.apply(courier('z', { status: 'offline' })); // unknown and offline: nothing to do
    expect(s.size).toBe(0);
  });

  it('with a ward filter, drops a courier that moved to another ward', () => {
    const s = new FleetStore('shibuya');
    s.apply(courier('a'));
    s.apply(courier('b', { ward: null })); // unknown ward: keep
    s.apply(courier('a', { ward: 'meguro', deviceTime: T0 + 1 }));
    s.apply(courier('c', { ward: 'meguro' }));
    expect(s.toGeoJSON().features.map((f) => f.properties.courier_id)).toEqual(['b']);
  });

  it('bumps the version only when the map needs redrawing', () => {
    const s = new FleetStore();
    const v0 = s.version;
    s.apply(courier('a'));
    const v1 = s.version;
    s.apply(courier('a', { deviceTime: T0 - 1 })); // stale
    s.apply({ type: 'courier_gone', courierId: 'x' });
    expect(v1).toBeGreaterThan(v0);
    expect(s.version).toBe(v1);
  });

  it('prunes couriers far behind the newest point (server clock), not when the stream stalls', () => {
    const s = new FleetStore();
    expect(s.prune()).toBe(0); // nothing seen yet
    s.apply(courier('old'));
    s.apply(courier('new', { receivedAt: T0 + PRUNE_AFTER_MS + 1 }));
    const before = s.version;
    expect(s.prune()).toBe(1);
    expect(s.version).toBeGreaterThan(before);
    expect(s.prune()).toBe(0);
    expect(s.size).toBe(1);
  });

  it('builds GeoJSON points as [lng, lat] with status', () => {
    const s = new FleetStore();
    s.apply(courier('a', { status: 'at_store', lat: 35.1, lng: 139.2 }));
    expect(s.toGeoJSON()).toEqual({
      type: 'FeatureCollection',
      features: [
        {
          type: 'Feature',
          geometry: { type: 'Point', coordinates: [139.2, 35.1] },
          properties: { courier_id: 'a', status: 'at_store' },
        },
      ],
    });
  });

  it('keeps up with 3,000 couriers at peak event rates', () => {
    const s = new FleetStore();
    const start = performance.now();
    for (let i = 0; i < 30_000; i++) {
      s.apply(courier(`c${i % 3000}`, { deviceTime: T0 + i, receivedAt: T0 + i }));
    }
    const geo = s.toGeoJSON();
    expect(geo.features).toHaveLength(3000);
    expect(performance.now() - start).toBeLessThan(1000);
  });
});
