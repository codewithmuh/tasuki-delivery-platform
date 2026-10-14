import { describe, expect, it } from 'vitest';
import type { TrackEvent } from './events';
import { effectiveStatus, headline, initialTrackState, stepStates, trackReducer } from './track';

const T0 = Date.parse('2026-10-14T03:00:00Z');
const run = (...events: TrackEvent[]) => events.reduce(trackReducer, initialTrackState);
const status = (
  s: 'confirmed' | 'assigned' | 'picked_up' | 'delivered',
  at: number,
  eta: number | null = null,
) => ({ type: 'status', orderId: 'ord_1', status: s, eta, updatedAt: at }) as const;
const position = (lat: number, at: number) =>
  ({ type: 'position', courierId: 'cou_1', lat, lng: 139.7, receivedAt: at }) as const;

describe('trackReducer', () => {
  it('tracks status, ETA and the newest update time', () => {
    const s = run(
      status('confirmed', T0, T0 + 1_800_000),
      status('assigned', T0 + 5000, T0 + 1_700_000),
    );
    expect(s.status).toBe('assigned');
    expect(s.eta).toBe(T0 + 1_700_000);
    expect(s.lastUpdateAt).toBe(T0 + 5000);
  });

  it('ignores a status older than the one shown (out-of-order delivery)', () => {
    const s = run(status('picked_up', T0 + 9000), status('assigned', T0 + 5000));
    expect(s.status).toBe('picked_up');
  });

  it('moves the courier and ignores older positions', () => {
    const s = run(position(35.6, T0 + 4000), position(35.5, T0 + 1000));
    expect(s.courier).toEqual({ lat: 35.6, lng: 139.7, receivedAt: T0 + 4000 });
    expect(s.lastUpdateAt).toBe(T0 + 4000);
    expect(run(position(35.6, T0 + 4000), status('assigned', T0)).lastUpdateAt).toBe(T0 + 4000);
  });

  it('records why the stream ended', () => {
    expect(run({ type: 'end', reason: 'cancelled' }).ended).toBe('cancelled');
  });
});

describe('effectiveStatus and headline', () => {
  it('waits before the first status', () => {
    expect(headline(initialTrackState)).toBe('waiting');
    expect(effectiveStatus(initialTrackState)).toBeNull();
  });

  it('lets the end reason outrank a missed status event', () => {
    const s = run(status('picked_up', T0), { type: 'end', reason: 'delivered' });
    expect(effectiveStatus(s)).toBe('delivered');
    expect(headline(s)).toBe('delivered');
  });

  it('shows an expired link as expired', () => {
    const s = run(status('assigned', T0), { type: 'end', reason: 'expired' });
    expect(headline(s)).toBe('expired');
    expect(effectiveStatus(s)).toBe('assigned');
  });
});

describe('stepStates', () => {
  const states = (s: Parameters<typeof stepStates>[0]) => stepStates(s).map((x) => x.state);

  it('marks earlier steps done and the current one in progress', () => {
    expect(states('assigned')).toEqual(['done', 'current', 'todo', 'todo']);
    expect(states('confirmed')).toEqual(['current', 'todo', 'todo', 'todo']);
  });

  it('marks everything done once delivered', () => {
    expect(states('delivered')).toEqual(['done', 'done', 'done', 'done']);
  });

  it('marks nothing before the first status, or for a status outside the steps', () => {
    expect(states(null)).toEqual(['todo', 'todo', 'todo', 'todo']);
    expect(states('cancelled')).toEqual(['todo', 'todo', 'todo', 'todo']);
  });

  it('lists the four customer-facing steps in order', () => {
    expect(stepStates(null).map((x) => x.step)).toEqual([
      'confirmed',
      'assigned',
      'picked_up',
      'delivered',
    ]);
  });
});
