import { afterEach, describe, expect, it, vi } from 'vitest';
import type { SseMessage } from './sse';
import { openStream, type ConnectionState, type StreamHandle, type StreamOptions } from './stream';

/** A controllable SSE response whose body errors when the request is aborted, like real fetch. */
function sseBody(signal: AbortSignal | null | undefined) {
  let ctrl!: ReadableStreamDefaultController<Uint8Array>;
  const body = new ReadableStream<Uint8Array>({
    start(c) {
      ctrl = c;
    },
  });
  signal?.addEventListener('abort', () => {
    ctrl.error(signal.reason);
  });
  const enc = new TextEncoder();
  return {
    body,
    push: (text: string) => {
      ctrl.enqueue(enc.encode(text));
    },
    end: () => {
      ctrl.close();
    },
  };
}

type Reply = (signal: AbortSignal | null | undefined) => Response | Promise<Response>;

function harness(replies: Reply[], extra: Partial<StreamOptions> = {}) {
  const states: ConnectionState[] = [];
  const messages: SseMessage[] = [];
  const fetchImpl = vi.fn((_url: string | URL | Request, init?: RequestInit) => {
    const reply = replies.shift();
    if (!reply) return new Promise<Response>(() => undefined); // hang: no more scripted replies
    return Promise.resolve(reply(init?.signal));
  });
  const handle: StreamHandle = openStream({
    url: '/v1/fleet/stream',
    onMessage: (m) => messages.push(m),
    onState: (s) => states.push(s),
    fetchImpl,
    backoff: { baseMs: 5, maxMs: 20, minMs: 1 },
    random: () => 0.5,
    ...extra,
  });
  return { states, messages, fetchImpl, handle, kinds: () => states.map((s) => s.kind) };
}

const sse = (status = 200, contentType = 'text/event-stream') => {
  const parts: ReturnType<typeof sseBody>[] = [];
  const reply: Reply = (signal) => {
    const b = sseBody(signal);
    parts.push(b);
    return new Response(b.body, { status, headers: { 'content-type': contentType } });
  };
  return { reply, part: () => parts[0] };
};

const empty =
  (status: number): Reply =>
  () =>
    new Response(null, { status });

let open: StreamHandle[] = [];
afterEach(() => {
  for (const h of open) h.close();
  open = [];
});

describe('openStream', () => {
  it('reports open and delivers parsed messages', async () => {
    const s = sse();
    const h = harness([s.reply]);
    open.push(h.handle);
    await vi.waitFor(() => {
      expect(h.kinds()).toEqual(['connecting', 'open']);
    });
    s.part()?.push('event: courier\ndata: {"courier_id":"c1"}\n\n: ping\n\n');
    await vi.waitFor(() => {
      expect(h.messages).toEqual([
        { event: 'courier', data: '{"courier_id":"c1"}', id: undefined },
      ]);
    });
    expect(h.fetchImpl).toHaveBeenCalledWith(
      '/v1/fleet/stream',
      expect.objectContaining({ headers: { Accept: 'text/event-stream' }, cache: 'no-store' }),
    );
  });

  it.each([
    [401, 'unauthorized'],
    [403, 'unauthorized'],
    [404, 'not_found'],
    [410, 'not_found'],
    [400, 'rejected'],
  ] as const)('stops for good on HTTP %i (%s)', async (status, reason) => {
    const h = harness([empty(status), empty(200)]);
    await vi.waitFor(() => {
      expect(h.states.at(-1)).toEqual({ kind: 'closed', reason, status });
    });
    await new Promise((r) => setTimeout(r, 30));
    expect(h.fetchImpl).toHaveBeenCalledTimes(1);
  });

  it.each([
    ['a 5xx', empty(503)],
    ['429 Too Many Requests', empty(429)],
    ['a response that is not an event stream', sse(200, 'text/html').reply],
    ['a network error', () => Promise.reject(new TypeError('Failed to fetch'))],
  ])('retries after %s', async (_name, first) => {
    const s = sse();
    const h = harness([first, s.reply]);
    open.push(h.handle);
    await vi.waitFor(() => {
      expect(h.kinds()).toEqual(['connecting', 'reconnecting', 'connecting', 'open']);
    });
    const retry = h.states[1];
    expect(retry?.kind === 'reconnecting' && retry.attempt).toBe(1);
  });

  it('reconnects when the server closes the stream, with growing delays', async () => {
    const a = sse();
    const h = harness([a.reply, empty(500), empty(500)]);
    open.push(h.handle);
    await vi.waitFor(() => {
      expect(h.kinds()).toContain('open');
    });
    a.part()?.end();
    await vi.waitFor(() => {
      expect(h.fetchImpl).toHaveBeenCalledTimes(4);
    });
    const attempts = h.states.flatMap((s) => (s.kind === 'reconnecting' ? [s.attempt] : []));
    expect(attempts.slice(0, 3)).toEqual([1, 2, 3]);
    expect(h.states).toContainEqual(
      expect.objectContaining({ kind: 'reconnecting', error: 'stream closed by server' }),
    );
  });

  it('starts again from the base delay once a connection delivers data', async () => {
    const a = sse();
    const b = sse();
    const h = harness([empty(500), a.reply, b.reply]);
    open.push(h.handle);
    await vi.waitFor(() => {
      expect(h.kinds().filter((k) => k === 'open')).toHaveLength(1);
    });
    a.part()?.push(': ping\n\n');
    await new Promise((r) => setTimeout(r, 5));
    a.part()?.end();
    await vi.waitFor(() => {
      expect(h.kinds().filter((k) => k === 'open')).toHaveLength(2);
    });
    const attempts = h.states.flatMap((s) => (s.kind === 'reconnecting' ? [s.attempt] : []));
    expect(attempts).toEqual([1, 1]);
  });

  it('aborts a silent connection after the idle timeout and reconnects', async () => {
    const a = sse();
    const b = sse();
    const h = harness([a.reply, b.reply], { idleTimeoutMs: 60 });
    open.push(h.handle);
    await vi.waitFor(
      () => {
        expect(h.kinds().filter((k) => k === 'open')).toHaveLength(2);
      },
      { timeout: 2000 },
    );
    expect(h.states).toContainEqual(
      expect.objectContaining({ kind: 'reconnecting', error: 'stream idle' }),
    );
  });

  it('close() aborts the connection and stops reconnecting', async () => {
    const a = sse();
    const h = harness([a.reply, empty(200)]);
    await vi.waitFor(() => {
      expect(h.kinds()).toContain('open');
    });
    h.handle.close();
    h.handle.close(); // idempotent
    await new Promise((r) => setTimeout(r, 30));
    expect(h.states.at(-1)).toEqual({ kind: 'closed', reason: 'stopped' });
    expect(h.states.filter((s) => s.kind === 'closed')).toHaveLength(1);
    expect(h.fetchImpl).toHaveBeenCalledTimes(1);
  });

  it('close() cancels a pending retry', async () => {
    const h = harness([empty(500)], { backoff: { baseMs: 10_000, maxMs: 10_000, minMs: 10_000 } });
    await vi.waitFor(() => {
      expect(h.kinds()).toContain('reconnecting');
    });
    h.handle.close();
    await new Promise((r) => setTimeout(r, 20));
    expect(h.fetchImpl).toHaveBeenCalledTimes(1);
    expect(h.states.at(-1)).toEqual({ kind: 'closed', reason: 'stopped' });
  });

  it('a message handler can close the stream (the `end` event), dropping later messages', async () => {
    const a = sse();
    const messages: string[] = [];
    let handle: StreamHandle | null = null;
    const h = harness([a.reply], {
      onMessage: (m) => {
        messages.push(m.event);
        if (m.event === 'end') handle?.close();
      },
    });
    handle = h.handle;
    await vi.waitFor(() => {
      expect(h.kinds()).toContain('open');
    });
    a.part()?.push('event: end\ndata: {}\n\nevent: status\ndata: {}\n\n');
    await vi.waitFor(() => {
      expect(h.states.at(-1)).toEqual({ kind: 'closed', reason: 'stopped' });
    });
    expect(messages).toEqual(['end']);
    expect(h.fetchImpl).toHaveBeenCalledTimes(1);
  });

  it('uses the global fetch by default', async () => {
    const spy = vi.spyOn(globalThis, 'fetch').mockRejectedValue(new TypeError('offline'));
    const states: ConnectionState[] = [];
    const handle = openStream({
      url: 'http://example.test/v1/track/t/stream',
      onMessage: () => undefined,
      onState: (s) => states.push(s),
    });
    await vi.waitFor(() => {
      expect(states.map((s) => s.kind)).toContain('reconnecting');
    });
    handle.close();
    expect(spy).toHaveBeenCalledOnce();
    spy.mockRestore();
  });
});
