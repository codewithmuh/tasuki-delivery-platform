/**
 * Reconnect delay: exponential backoff with "full jitter" (AWS Architecture Blog, 2015).
 * When tracking restarts, thousands of browsers reconnect at once; jitter spreads them out
 * instead of letting them retry in lockstep.
 */
export interface BackoffOptions {
  /** Delay ceiling for the first retry, in ms. */
  baseMs: number;
  /** Upper bound for any retry, in ms. */
  maxMs: number;
  /** Lower bound, so a retry is never immediate, in ms. */
  minMs: number;
}

export const DEFAULT_BACKOFF: BackoffOptions = { baseMs: 1_000, maxMs: 30_000, minMs: 250 };

/** Delay before retry number `attempt` (0 = first retry). `random` returns [0, 1). */
export function backoffDelay(
  attempt: number,
  options: BackoffOptions = DEFAULT_BACKOFF,
  random: () => number = Math.random,
): number {
  const exponent = Math.min(Math.max(0, attempt), 30); // avoid 2 ** huge
  const ceiling = Math.min(options.maxMs, options.baseMs * 2 ** exponent);
  return Math.round(Math.max(options.minMs, random() * ceiling));
}
