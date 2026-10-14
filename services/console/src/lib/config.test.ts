import { describe, expect, it, vi } from 'vitest';
import { apiUrl, loadConfig, parseConfig } from './config';

describe('parseConfig', () => {
  it('accepts an empty base (same origin) or a missing key', () => {
    expect(parseConfig({ trackingBaseUrl: '' })).toEqual({ trackingBaseUrl: '' });
    expect(parseConfig({})).toEqual({ trackingBaseUrl: '' });
  });

  it('normalises an origin and drops a trailing slash', () => {
    expect(parseConfig({ trackingBaseUrl: 'https://tracking.tasuki.example/' })).toEqual({
      trackingBaseUrl: 'https://tracking.tasuki.example',
    });
    expect(parseConfig({ trackingBaseUrl: 'http://localhost:8081' }).trackingBaseUrl).toBe(
      'http://localhost:8081',
    );
  });

  it.each([
    ['a non-object', 'https://x'],
    ['null', null],
    ['an array', []],
    ['a non-string base', { trackingBaseUrl: 42 }],
    ['a relative URL', { trackingBaseUrl: '/tracking' }],
    ['a URL with a path', { trackingBaseUrl: 'https://x.example/tracking' }],
    ['a URL with a query', { trackingBaseUrl: 'https://x.example/?a=1' }],
    ['a non-http scheme', { trackingBaseUrl: 'javascript:alert(1)' }],
  ])('rejects %s', (_name, raw) => {
    expect(() => parseConfig(raw)).toThrow();
  });
});

describe('loadConfig', () => {
  it('fetches /config.json without caching', async () => {
    const fetchImpl = vi.fn(() =>
      Promise.resolve(Response.json({ trackingBaseUrl: 'https://t.example' })),
    );
    await expect(loadConfig(fetchImpl as unknown as typeof fetch)).resolves.toEqual({
      trackingBaseUrl: 'https://t.example',
    });
    expect(fetchImpl).toHaveBeenCalledWith('/config.json', { cache: 'no-store' });
  });

  it('fails clearly when the file is missing', async () => {
    const fetchImpl = () => Promise.resolve(new Response('nope', { status: 404 }));
    await expect(loadConfig(fetchImpl as unknown as typeof fetch)).rejects.toThrow('404');
  });
});

describe('apiUrl', () => {
  it('joins the base and path and skips empty parameters', () => {
    expect(apiUrl({ trackingBaseUrl: '' }, '/v1/fleet/stream', { ward: null })).toBe(
      '/v1/fleet/stream',
    );
    expect(
      apiUrl({ trackingBaseUrl: 'https://t.example' }, '/v1/fleet/stream', { ward: 'minato' }),
    ).toBe('https://t.example/v1/fleet/stream?ward=minato');
  });
});
