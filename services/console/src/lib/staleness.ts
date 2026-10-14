/**
 * "Data age": how old the newest event on a stream is, now − received_at. It includes the whole
 * pipeline (ingest → Kafka → tracking → browser), so it is what the user actually experiences,
 * and it is the same quantity as the tracking-freshness SLO (design.md §9: 99 % younger than 10 s).
 * It grows when the stream stalls, even if the connection itself stays up.
 */
export type Freshness = 'none' | 'fresh' | 'stale' | 'critical';

/** Matches the tracking-freshness SLO threshold. */
export const STALE_AFTER_MS = 10_000;
export const CRITICAL_AFTER_MS = 30_000;

/** Age of the newest event; null before any event. Clock skew can make it negative: clamp to 0. */
export function dataAgeMs(now: number, latestReceivedAt: number | null): number | null {
  if (latestReceivedAt === null) return null;
  return Math.max(0, now - latestReceivedAt);
}

export function freshness(ageMs: number | null): Freshness {
  if (ageMs === null) return 'none';
  if (ageMs >= CRITICAL_AFTER_MS) return 'critical';
  if (ageMs >= STALE_AFTER_MS) return 'stale';
  return 'fresh';
}
