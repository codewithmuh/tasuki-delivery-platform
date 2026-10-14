import { describe, expect, it } from 'vitest';
import { CRITICAL_AFTER_MS, dataAgeMs, freshness, STALE_AFTER_MS } from './staleness';

describe('dataAgeMs', () => {
  it('is null before the first event', () => {
    expect(dataAgeMs(1000, null)).toBeNull();
  });

  it('is now minus the newest received_at', () => {
    expect(dataAgeMs(10_000, 7_500)).toBe(2_500);
  });

  it('clamps clock skew to zero', () => {
    expect(dataAgeMs(1_000, 3_000)).toBe(0);
  });
});

describe('freshness', () => {
  it.each([
    [null, 'none'],
    [0, 'fresh'],
    [STALE_AFTER_MS - 1, 'fresh'],
    [STALE_AFTER_MS, 'stale'],
    [CRITICAL_AFTER_MS - 1, 'stale'],
    [CRITICAL_AFTER_MS, 'critical'],
    [600_000, 'critical'],
  ] as const)('age %s ms is %s', (age, level) => {
    expect(freshness(age)).toBe(level);
  });

  it('turns stale at the 10 s tracking-freshness SLO threshold', () => {
    expect(STALE_AFTER_MS).toBe(10_000);
  });
});
