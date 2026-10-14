/** Two routes, so no router library: "/" is the ops map, "/track/:token" the tracking page. */
export type Route =
  { page: 'ops'; ward: string | null } | { page: 'track'; token: string } | { page: 'not_found' };

/** Tracking tokens are JWTs: base64url segments joined by dots. */
const TOKEN = /^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$/;
const MAX_TOKEN_LENGTH = 4096;

export function parseRoute(pathname: string, search: string, wardIds: ReadonlySet<string>): Route {
  const path = pathname.replace(/\/+$/, '') || '/';
  if (path === '/') {
    const ward = new URLSearchParams(search).get('ward');
    return { page: 'ops', ward: ward && wardIds.has(ward) ? ward : null };
  }
  const match = /^\/track\/([^/]+)$/.exec(path);
  if (match?.[1]) {
    let token: string;
    try {
      token = decodeURIComponent(match[1]);
    } catch {
      return { page: 'not_found' };
    }
    if (token.length <= MAX_TOKEN_LENGTH && TOKEN.test(token)) return { page: 'track', token };
  }
  return { page: 'not_found' };
}
