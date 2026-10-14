import { describe, expect, it } from 'vitest';
import { backoffDelay, DEFAULT_BACKOFF } from './backoff';

const max = () => 0.999999;

describe('backoffDelay', () => {
  it('doubles the ceiling with each attempt', () => {
    expect(backoffDelay(0, DEFAULT_BACKOFF, max)).toBe(1000);
    expect(backoffDelay(1, DEFAULT_BACKOFF, max)).toBe(2000);
    expect(backoffDelay(3, DEFAULT_BACKOFF, max)).toBe(8000);
  });

  it('never exceeds the cap, even for huge attempt numbers', () => {
    expect(backoffDelay(5, DEFAULT_BACKOFF, max)).toBe(30_000);
    expect(backoffDelay(10_000, DEFAULT_BACKOFF, max)).toBe(30_000);
  });

  it('applies full jitter but never goes below the floor', () => {
    expect(backoffDelay(4, DEFAULT_BACKOFF, () => 0.5)).toBe(8000);
    expect(backoffDelay(4, DEFAULT_BACKOFF, () => 0)).toBe(250);
  });

  it('treats a negative attempt as the first', () => {
    expect(backoffDelay(-3, DEFAULT_BACKOFF, max)).toBe(1000);
  });

  it('uses Math.random by default and stays in range', () => {
    for (let i = 0; i < 50; i++) {
      const d = backoffDelay(2);
      expect(d).toBeGreaterThanOrEqual(250);
      expect(d).toBeLessThanOrEqual(4000);
    }
  });
});
