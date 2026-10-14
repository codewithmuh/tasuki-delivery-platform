/** "/track/:token": the customer's view of one delivery, from GET /v1/track/{token}/stream. */
import { useEffect, useReducer, useRef, useState } from 'react';
import { Badge } from '../components/Badge';
import { CourierMap, type CourierMapHandle } from '../components/CourierMap';
import { useNow, useStream } from '../hooks';
import { apiUrl, type RuntimeConfig } from '../lib/config';
import { parseTrackEvent } from '../lib/events';
import type { ActiveStatus } from '../lib/fleet';
import { detectLang, formatAge, formatTokyoTime, t, type Lang } from '../lib/i18n';
import { dataAgeMs, freshness } from '../lib/staleness';
import type { ConnectionState } from '../lib/stream';
import { connectionTone, retryInSeconds } from '../lib/tone';
import {
  effectiveStatus,
  headline,
  initialTrackState,
  stepStates,
  trackReducer,
  type TrackState,
} from '../lib/track';

const LANG_KEY = 'tasuki.lang';
const COURIER_ZOOM = 15;

function savedLang(): string | null {
  try {
    return window.localStorage.getItem(LANG_KEY);
  } catch {
    return null; // storage can be blocked (private mode, settings): fall back to the browser
  }
}

function saveLang(lang: Lang): void {
  try {
    window.localStorage.setItem(LANG_KEY, lang);
  } catch {
    // not saved; the toggle still works for this visit
  }
}

/** The courier icon follows the order: heading to the store, then to the customer. */
function courierIcon(state: TrackState): ActiveStatus {
  const status = effectiveStatus(state);
  return status === 'picked_up' || status === 'delivered' ? 'to_dropoff' : 'to_store';
}

function etaText(lang: Lang, state: TrackState, now: number): string | null {
  const status = effectiveStatus(state);
  if (
    state.eta === null ||
    (status !== 'assigned' && status !== 'picked_up' && status !== 'confirmed')
  ) {
    return null;
  }
  const minutes = Math.ceil((state.eta - now) / 60_000);
  if (minutes <= 0) return t(lang, 'eta_due');
  return t(lang, 'eta', { time: formatTokyoTime(lang, state.eta), n: minutes });
}

function connectionText(lang: Lang, connection: ConnectionState, now: number): string {
  switch (connection.kind) {
    case 'open':
      return t(lang, 'conn_open');
    case 'connecting':
      return t(lang, 'conn_connecting');
    case 'reconnecting':
      return t(lang, 'conn_reconnecting', { n: retryInSeconds(connection, now) });
    case 'closed':
      return t(lang, 'conn_closed');
  }
}

export function TrackPage({ config, token }: { config: RuntimeConfig; token: string }) {
  const [lang, setLang] = useState<Lang>(() => detectLang(savedLang(), navigator.languages));
  const [state, dispatch] = useReducer(trackReducer, initialTrackState);
  const mapRef = useRef<CourierMapHandle>(null);
  const followedRef = useRef(false);
  const now = useNow(1000);

  const connection = useStream(
    apiUrl(config, `/v1/track/${encodeURIComponent(token)}/stream`),
    (message, stream) => {
      const event = parseTrackEvent(message);
      if (!event) return;
      dispatch(event);
      if (event.type === 'end') stream.close(); // the server closes too; do not reconnect
    },
  );

  useEffect(() => {
    document.documentElement.lang = lang;
    document.title = `${t(lang, 'page_title')} · Tasuki`;
  }, [lang]);

  // Show the courier and keep it in view: zoom in on the first position, then glide along.
  const { courier } = state;
  const icon = courierIcon(state);
  useEffect(() => {
    if (!courier) return;
    mapRef.current?.setCouriers({
      type: 'FeatureCollection',
      features: [
        {
          type: 'Feature',
          geometry: { type: 'Point', coordinates: [courier.lng, courier.lat] },
          properties: { courier_id: 'courier', status: icon },
        },
      ],
    });
    mapRef.current?.moveTo(
      courier.lng,
      courier.lat,
      followedRef.current ? undefined : COURIER_ZOOM,
    );
    followedRef.current = true;
  }, [courier, icon]);

  const linkInvalid =
    connection.kind === 'closed' && connection.reason !== 'stopped' && state.ended === null;
  const toggle = (
    <button
      type="button"
      className="lang-toggle"
      lang={lang === 'en' ? 'ja' : 'en'}
      aria-label={t(lang, 'switch_language_label')}
      onClick={() => {
        const next = lang === 'en' ? 'ja' : 'en';
        setLang(next);
        saveLang(next);
      }}
    >
      {t(lang, 'switch_language')}
    </button>
  );

  if (linkInvalid || state.ended === 'expired') {
    return (
      <div className="track">
        <header className="track-header">
          <span className="brand">Tasuki</span>
          {toggle}
        </header>
        <main className="card">
          <h1>{t(lang, 'link_invalid')}</h1>
          <p>{t(lang, 'link_invalid_hint')}</p>
        </main>
      </div>
    );
  }

  const status = effectiveStatus(state);
  const age = dataAgeMs(now, state.lastUpdateAt);
  const eta = etaText(lang, state, now);
  const moving = status === 'assigned' || status === 'picked_up';
  const showStale = moving && courier !== null && freshness(age) === 'critical';
  const steps = status === 'cancelled' || status === 'failed' ? [] : stepStates(status);

  return (
    <div className="track">
      <header className="track-header">
        <span className="brand">Tasuki</span>
        {toggle}
      </header>
      <div className="track-map">
        <CourierMap ref={mapRef} label={t(lang, 'map_label')} />
        {!courier && <p className="map-overlay">{t(lang, 'map_waiting')}</p>}
      </div>
      <main className="card">
        <h1 aria-live="polite">{t(lang, `headline_${headline(state)}`)}</h1>
        {eta && <p className="eta">{eta}</p>}
        {steps.length > 0 && (
          <ol className="steps" aria-label={t(lang, 'progress_label')}>
            {steps.map(({ step, state: s }) => (
              <li
                key={step}
                className={`step step-${s}`}
                aria-current={s === 'current' ? 'step' : undefined}
              >
                <span className="step-mark" aria-hidden="true">
                  {s === 'done' ? '✓' : s === 'current' ? '●' : '○'}
                </span>
                <span>{t(lang, `step_${step}`)}</span>
                {s !== 'todo' && (
                  <span className="visually-hidden">{` (${t(lang, `step_${s}`)})`}</span>
                )}
              </li>
            ))}
          </ol>
        )}
        {showStale && <p className="warning">{t(lang, 'location_stale')}</p>}
        <div className="status-row">
          {/* Once the order has ended the stream is closed on purpose: no connection badge. */}
          {state.ended === null && (
            <Badge tone={connectionTone(connection)}>{connectionText(lang, connection, now)}</Badge>
          )}
          <span className="muted">
            {age === null
              ? t(lang, 'updated_never')
              : t(lang, 'updated_ago', { age: formatAge(lang, age) })}
          </span>
        </div>
        <p className="note">{t(lang, 'demo_note')}</p>
      </main>
    </div>
  );
}
