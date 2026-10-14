import { describe, expect, it } from 'vitest';
import { parseRoute } from './routes';

const wards = new Set(['shibuya', 'minato']);

describe('parseRoute', () => {
  it('routes / to the ops map, with a known ward from the query', () => {
    expect(parseRoute('/', '', wards)).toEqual({ page: 'ops', ward: null });
    expect(parseRoute('/', '?ward=shibuya', wards)).toEqual({ page: 'ops', ward: 'shibuya' });
    expect(parseRoute('/', '?ward=atlantis', wards)).toEqual({ page: 'ops', ward: null });
  });

  it('routes /track/:token to the tracking page', () => {
    const jwt = 'eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJvcmRfMSJ9.c2lnbmF0dXJl-_'; // gitleaks:allow (test fixture)
    expect(parseRoute(`/track/${jwt}`, '', wards)).toEqual({ page: 'track', token: jwt });
    expect(parseRoute('/track/demo/', '', wards)).toEqual({ page: 'track', token: 'demo' });
  });

  it.each([
    ['/track/', 'no token'],
    ['/track/a/b', 'extra segments'],
    ['/track/%3Cscript%3E', 'characters a JWT never has'],
    ['/track/%E0%A4%A', 'broken percent-encoding'],
    ['/track/a..b', 'empty JWT segment'],
    [`/track/${'a'.repeat(5000)}`, 'an absurdly long token'],
    ['/admin', 'unknown path'],
  ])('%s is not found (%s)', (path) => {
    expect(parseRoute(path, '', wards)).toEqual({ page: 'not_found' });
  });
});
