/** "/": the dispatcher's live map of every courier, from GET /v1/fleet/stream?ward=. */
import { useEffect, useMemo, useRef, useState } from 'react';
import { Badge } from '../components/Badge';
import { CourierMap, type CourierMapHandle } from '../components/CourierMap';
import { StatusIcon } from '../components/StatusIcon';
import { useNow, useStream } from '../hooks';
import { apiUrl, type RuntimeConfig } from '../lib/config';
import { parseFleetEvent } from '../lib/events';
import { ACTIVE_STATUSES, FleetStore, type FleetCounts } from '../lib/fleet';
import { formatAge } from '../lib/i18n';
import { STATUS_STYLE } from '../lib/legend';
import { dataAgeMs, freshness, type Freshness } from '../lib/staleness';
import type { ConnectionState } from '../lib/stream';
import { connectionTone, freshnessTone, retryInSeconds } from '../lib/tone';
import { findWard, TOKYO_BOUNDS, wardLabel, WARDS } from '../lib/wards';

/** How often the map is refreshed from the store (positions arrive every 4–15 s per courier). */
const MAP_REFRESH_MS = 500;
const WARD_ZOOM = 13;

const FRESHNESS_LABEL: Record<Freshness, string> = {
  none: 'No data yet',
  fresh: 'Fresh',
  stale: 'Stale',
  critical: 'Very stale',
};

function connectionLabel(state: ConnectionState, now: number): string {
  switch (state.kind) {
    case 'open':
      return 'Live';
    case 'connecting':
      return 'Connecting…';
    case 'reconnecting':
      return `Reconnecting in ${retryInSeconds(state, now)} s (attempt ${state.attempt})`;
    case 'closed':
      return state.reason === 'unauthorized' ? 'Not authorised' : 'Disconnected';
  }
}

interface Stats {
  counts: FleetCounts;
  latestReceivedAt: number | null;
}

export function OpsPage({
  config,
  initialWard,
}: {
  config: RuntimeConfig;
  initialWard: string | null;
}) {
  const [ward, setWard] = useState(initialWard);
  const store = useMemo(() => new FleetStore(ward), [ward]);
  const mapRef = useRef<CourierMapHandle>(null);
  const now = useNow(1000);
  const [stats, setStats] = useState<Stats>(() => ({
    counts: store.counts(),
    latestReceivedAt: null,
  }));

  const connection = useStream(apiUrl(config, '/v1/fleet/stream', { ward }), (message) => {
    const event = parseFleetEvent(message);
    if (event) store.apply(event);
  });

  // Push store changes to the map and the counters on a fixed cadence, not per event.
  useEffect(() => {
    let mapVersion = -1;
    const id = setInterval(() => {
      store.prune();
      if (store.version !== mapVersion) {
        mapVersion = store.version;
        mapRef.current?.setCouriers(store.toGeoJSON());
      }
      setStats({ counts: store.counts(), latestReceivedAt: store.latestReceivedAt });
    }, MAP_REFRESH_MS);
    return () => {
      clearInterval(id);
    };
  }, [store]);

  // Frame the selected ward and keep it in the URL, so a view can be shared or bookmarked.
  useEffect(() => {
    const selected = findWard(ward);
    if (selected) mapRef.current?.moveTo(selected.lng, selected.lat, WARD_ZOOM);
    else mapRef.current?.fitBounds(TOKYO_BOUNDS);
    window.history.replaceState(null, '', selected ? `/?ward=${selected.id}` : '/');
  }, [ward]);

  const age = dataAgeMs(now, stats.latestReceivedAt);
  const level = freshness(age);
  const { counts } = stats;

  return (
    <div className="ops">
      <CourierMap ref={mapRef} label="Live map of Tasuki couriers in Tokyo" />
      <aside className="panel" aria-label="Fleet status">
        <header className="panel-header">
          <h1>
            Tasuki <span className="muted">live fleet</span>
          </h1>
          <p className="muted">Tokyo 23 wards · 東京23区</p>
        </header>

        <label className="field-label" htmlFor="ward">
          Ward · 区
        </label>
        <select
          id="ward"
          value={ward ?? ''}
          onChange={(e) => {
            setWard(e.target.value || null);
          }}
        >
          <option value="">All 23 wards · 全23区</option>
          {WARDS.map((w) => (
            <option key={w.id} value={w.id}>
              {wardLabel(w)}
            </option>
          ))}
        </select>

        <div className="status-row">
          <Badge tone={connectionTone(connection)}>{connectionLabel(connection, now)}</Badge>
          <Badge tone={freshnessTone(level)}>
            {age === null
              ? FRESHNESS_LABEL.none
              : `Data age ${formatAge('en', age)} · ${FRESHNESS_LABEL[level]}`}
          </Badge>
        </div>

        {/* Announced to screen readers only when the state changes, not on every tick. */}
        <p className="visually-hidden" role="status">
          {`Stream ${connection.kind}. Data ${FRESHNESS_LABEL[level].toLowerCase()}.`}
        </p>

        <p className="online">
          <span className="online-count">{counts.online.toLocaleString('en-US')}</span> couriers
          online
        </p>
        <ul className="legend" aria-label="Couriers by status">
          {ACTIVE_STATUSES.map((status) => (
            <li key={status}>
              <StatusIcon status={status} />
              <span className="legend-label">{STATUS_STYLE[status].label}</span>
              <span className="legend-count">
                {counts.byStatus[status].toLocaleString('en-US')}
              </span>
            </li>
          ))}
        </ul>
        <p className="note">Simulated city: Tasuki is a fictional company.</p>
      </aside>
    </div>
  );
}
