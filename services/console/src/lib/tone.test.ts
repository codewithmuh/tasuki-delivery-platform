import { describe, expect, it } from 'vitest';
import { ACTIVE_STATUSES } from './fleet';
import { iconId, STATUS_STYLE } from './legend';
import { connectionTone, freshnessTone, retryInSeconds } from './tone';

describe('tones', () => {
  it('maps connection states', () => {
    expect(connectionTone({ kind: 'open' })).toBe('ok');
    expect(connectionTone({ kind: 'connecting' })).toBe('neutral');
    expect(connectionTone({ kind: 'reconnecting', attempt: 1, retryAt: 0, error: 'x' })).toBe(
      'warn',
    );
    expect(connectionTone({ kind: 'closed', reason: 'not_found' })).toBe('bad');
  });

  it('counts down whole seconds to the next retry', () => {
    const state = { kind: 'reconnecting', attempt: 2, retryAt: 10_000, error: 'x' } as const;
    expect(retryInSeconds(state, 7_100)).toBe(3);
    expect(retryInSeconds(state, 12_000)).toBe(0);
    expect(retryInSeconds({ kind: 'open' }, 0)).toBe(0);
  });

  it('maps freshness levels', () => {
    expect(freshnessTone('none')).toBe('neutral');
    expect(freshnessTone('fresh')).toBe('ok');
    expect(freshnessTone('stale')).toBe('warn');
    expect(freshnessTone('critical')).toBe('bad');
  });
});

describe('legend', () => {
  it('gives every status a distinct shape, colour and label', () => {
    const styles = ACTIVE_STATUSES.map((s) => STATUS_STYLE[s]);
    expect(new Set(styles.map((s) => s.shape)).size).toBe(4);
    expect(new Set(styles.map((s) => s.color)).size).toBe(4);
    expect(new Set(styles.map((s) => s.label)).size).toBe(4);
    expect(iconId('to_store')).toBe('courier-to_store');
  });
});
