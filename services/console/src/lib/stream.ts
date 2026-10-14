/**
 * A resilient SSE connection over fetch:
 * - reconnects with jittered exponential backoff after network errors, 5xx, or a closed stream;
 * - stops for good on 401/404 (expired or unknown tracking link) and other 4xx;
 * - a watchdog aborts a connection that has gone silent (the server pings every 15 s), because a
 *   half-open TCP connection through a proxy can otherwise hang forever without an error.
 */
import { backoffDelay, DEFAULT_BACKOFF, type BackoffOptions } from './backoff';
import { SseParser, type SseMessage } from './sse';

export type ClosedReason = 'stopped' | 'unauthorized' | 'not_found' | 'rejected';

export type ConnectionState =
  | { kind: 'connecting' }
  | { kind: 'open' }
  | { kind: 'reconnecting'; attempt: number; retryAt: number; error: string }
  | { kind: 'closed'; reason: ClosedReason; status?: number };

export interface StreamOptions {
  url: string;
  onMessage: (message: SseMessage) => void;
  onState: (state: ConnectionState) => void;
  backoff?: BackoffOptions;
  /** Abort and reconnect when no bytes (data or `: ping`) arrive for this long. */
  idleTimeoutMs?: number;
  fetchImpl?: typeof fetch;
  random?: () => number;
  now?: () => number;
}

export interface StreamHandle {
  /** Stop for good: abort the connection and cancel any pending retry. */
  close: () => void;
}

/** Three missed 15 s heartbeats. */
export const DEFAULT_IDLE_TIMEOUT_MS = 45_000;

class TerminalError extends Error {
  constructor(
    readonly reason: ClosedReason,
    readonly status: number,
  ) {
    super(`HTTP ${status}`);
  }
}

function terminalReason(status: number): ClosedReason | null {
  if (status === 401 || status === 403) return 'unauthorized';
  if (status === 404 || status === 410) return 'not_found';
  if (status >= 400 && status < 500 && status !== 408 && status !== 429) return 'rejected';
  return null;
}

export function openStream(options: StreamOptions): StreamHandle {
  const fetchImpl = options.fetchImpl ?? globalThis.fetch.bind(globalThis);
  const backoff = options.backoff ?? DEFAULT_BACKOFF;
  const idleTimeoutMs = options.idleTimeoutMs ?? DEFAULT_IDLE_TIMEOUT_MS;
  const random = options.random ?? Math.random;
  const now = options.now ?? Date.now;

  let stopped = false;
  let controller: AbortController | null = null;
  let cancelRetry: (() => void) | null = null;
  let attempt = 0;

  // Read through a function: close() flips `stopped` from another callback while we await.
  const isStopped = () => stopped;
  const report = (state: ConnectionState) => {
    if (!isStopped()) options.onState(state);
  };

  async function connectOnce(): Promise<void> {
    const ctrl = new AbortController();
    controller = ctrl;
    let lastByteAt = now();
    const watchdog = setInterval(
      () => {
        if (now() - lastByteAt > idleTimeoutMs) ctrl.abort(new Error('stream idle'));
      },
      Math.ceil(idleTimeoutMs / 3),
    );
    try {
      const res = await fetchImpl(options.url, {
        headers: { Accept: 'text/event-stream' },
        cache: 'no-store',
        signal: ctrl.signal,
      });
      const terminal = terminalReason(res.status);
      if (terminal) throw new TerminalError(terminal, res.status);
      if (!res.ok || !res.body) throw new Error(`HTTP ${res.status}`);
      if (!(res.headers.get('content-type') ?? '').includes('text/event-stream')) {
        throw new Error('not an event stream');
      }

      report({ kind: 'open' });
      const parser = new SseParser();
      const decoder = new TextDecoder();
      const reader = res.body.getReader();
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        lastByteAt = now();
        attempt = 0; // the connection works: the next failure starts from the base delay
        for (const message of parser.push(decoder.decode(value, { stream: true }))) {
          if (isStopped()) return;
          options.onMessage(message);
        }
      }
      throw new Error('stream closed by server');
    } finally {
      clearInterval(watchdog);
    }
  }

  async function run(): Promise<void> {
    while (!isStopped()) {
      report({ kind: 'connecting' });
      try {
        await connectOnce();
      } catch (err) {
        if (isStopped()) return;
        if (err instanceof TerminalError) {
          stopped = true;
          options.onState({ kind: 'closed', reason: err.reason, status: err.status });
          return;
        }
        const delay = backoffDelay(attempt, backoff, random);
        const error = err instanceof Error ? err.message : String(err);
        report({ kind: 'reconnecting', attempt: attempt + 1, retryAt: now() + delay, error });
        attempt++;
        await new Promise<void>((resolve) => {
          const timer = setTimeout(resolve, delay);
          cancelRetry = () => {
            clearTimeout(timer);
            resolve();
          };
        });
        cancelRetry = null;
      }
    }
  }

  void run();

  return {
    close() {
      if (stopped) return;
      stopped = true;
      controller?.abort();
      cancelRetry?.();
      options.onState({ kind: 'closed', reason: 'stopped' });
    },
  };
}
