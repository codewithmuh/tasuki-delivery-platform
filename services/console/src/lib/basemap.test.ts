import { describe, expect, it } from 'vitest';
import template from '../../docker/templates/tasuki/console.conf.template?raw';
import { BASEMAP_STYLES } from './basemap';

/** The basemap host lives in two places (code and nginx CSP); this keeps them in step. */
describe('basemap and Content-Security-Policy', () => {
  const csp = /Content-Security-Policy "([^"]+)"/.exec(template)?.[1] ?? '';
  const directive = (name: string) =>
    (new RegExp(`${name} ([^;]+)`).exec(csp)?.[1] ?? '').trim().split(/\s+/);

  it('allows the basemap host in connect-src', () => {
    for (const url of Object.values(BASEMAP_STYLES)) {
      expect(directive('connect-src')).toContain(new URL(url).origin);
    }
  });

  it('allows the tracking origin from the environment, and nothing inline', () => {
    expect(directive('connect-src')).toContain('${CONSOLE_TRACKING_BASE_URL}');
    expect(directive('script-src')).toEqual(["'self'"]);
    expect(directive('worker-src')).toEqual(["'self'"]);
    expect(csp).not.toContain('unsafe-');
  });
});
