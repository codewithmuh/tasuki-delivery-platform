/**
 * Runtime configuration. The built files are environment-agnostic: the page reads /config.json at
 * start, which the container writes from environment variables (docker/templates), so one image
 * runs locally and on EKS.
 */
export interface RuntimeConfig {
  /**
   * Origin of the tracking service, e.g. "https://tracking.tasuki.example". Empty means same
   * origin (the ingress routes /v1/* to tracking), which is the default.
   */
  trackingBaseUrl: string;
}

export function parseConfig(raw: unknown): RuntimeConfig {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    throw new Error('config.json must be a JSON object');
  }
  const base = (raw as Record<string, unknown>)['trackingBaseUrl'] ?? '';
  if (typeof base !== 'string') throw new Error('trackingBaseUrl must be a string');
  if (base === '') return { trackingBaseUrl: '' };

  let url: URL;
  try {
    url = new URL(base);
  } catch {
    throw new Error(`trackingBaseUrl is not a URL: ${base}`);
  }
  // An origin only: the Content-Security-Policy allows exactly this origin in connect-src.
  if (!/^https?:$/.test(url.protocol) || url.pathname !== '/' || url.search || url.hash) {
    throw new Error(`trackingBaseUrl must be an http(s) origin, like https://host:port: ${base}`);
  }
  return { trackingBaseUrl: url.origin };
}

export async function loadConfig(fetchImpl: typeof fetch = fetch): Promise<RuntimeConfig> {
  const res = await fetchImpl('/config.json', { cache: 'no-store' });
  if (!res.ok) throw new Error(`GET /config.json returned ${res.status}`);
  return parseConfig(await res.json());
}

/** Build a stream URL under the tracking base, skipping empty query parameters. */
export function apiUrl(
  config: RuntimeConfig,
  path: string,
  params: Record<string, string | null> = {},
): string {
  const query = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value) query.set(key, value);
  }
  const qs = query.toString();
  return `${config.trackingBaseUrl}${path}${qs ? `?${qs}` : ''}`;
}
