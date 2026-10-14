import { useEffect, useEffectEvent, useState, useSyncExternalStore } from 'react';
import type { SseMessage } from './lib/sse';
import { openStream, type ConnectionState, type StreamHandle } from './lib/stream';

/** Current time, refreshed every `intervalMs`: drives "N s ago" and the data-age indicator. */
export function useNow(intervalMs = 1000): number {
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    const id = setInterval(() => {
      setNow(Date.now());
    }, intervalMs);
    return () => {
      clearInterval(id);
    };
  }, [intervalMs]);
  return now;
}

/** Keeps an SSE stream open while mounted (and while `url` is unchanged). */
export function useStream(
  url: string,
  onMessage: (message: SseMessage, stream: StreamHandle) => void,
): ConnectionState {
  const [state, setState] = useState<ConnectionState>({ kind: 'connecting' });
  const handleMessage = useEffectEvent(onMessage);
  useEffect(() => {
    const stream = openStream({
      url,
      onMessage: (message) => {
        handleMessage(message, stream);
      },
      onState: setState,
    });
    return () => {
      stream.close();
    };
  }, [url]);
  return state;
}

function mediaQuery(query: string): boolean {
  return typeof window !== 'undefined' && window.matchMedia(query).matches;
}

/** Follows the OS light/dark setting live. */
export function usePrefersDark(): boolean {
  const query = '(prefers-color-scheme: dark)';
  return useSyncExternalStore(
    (onChange) => {
      const mql = window.matchMedia(query);
      mql.addEventListener('change', onChange);
      return () => {
        mql.removeEventListener('change', onChange);
      };
    },
    () => mediaQuery(query),
  );
}

export function prefersReducedMotion(): boolean {
  return mediaQuery('(prefers-reduced-motion: reduce)');
}
