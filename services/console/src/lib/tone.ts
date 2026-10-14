/** Maps connection and freshness states to the badge tone shown on both pages. */
import type { Freshness } from './staleness';
import type { ConnectionState } from './stream';

export type Tone = 'ok' | 'warn' | 'bad' | 'neutral';

export function connectionTone(state: ConnectionState): Tone {
  switch (state.kind) {
    case 'open':
      return 'ok';
    case 'connecting':
      return 'neutral';
    case 'reconnecting':
      return 'warn';
    case 'closed':
      return 'bad';
  }
}

/** Whole seconds until the next reconnect attempt (0 when not waiting). */
export function retryInSeconds(state: ConnectionState, now: number): number {
  return state.kind === 'reconnecting' ? Math.max(0, Math.ceil((state.retryAt - now) / 1000)) : 0;
}

const FRESHNESS_TONE: Record<Freshness, Tone> = {
  none: 'neutral',
  fresh: 'ok',
  stale: 'warn',
  critical: 'bad',
};

export function freshnessTone(level: Freshness): Tone {
  return FRESHNESS_TONE[level];
}
