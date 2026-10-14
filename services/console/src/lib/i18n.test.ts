import { describe, expect, it } from 'vitest';
import { detectLang, formatAge, formatTokyoTime, LANGS, t } from './i18n';

describe('t', () => {
  it('returns the message in each language', () => {
    expect(t('en', 'step_delivered')).toBe('Delivered');
    expect(t('ja', 'step_delivered')).toBe('お届け完了');
  });

  it('fills placeholders and leaves unknown ones visible', () => {
    expect(t('en', 'updated_ago', { age: '5 s' })).toBe('Updated 5 s ago');
    expect(t('ja', 'conn_reconnecting', { n: 3 })).toBe('3秒後に再接続します');
    expect(t('en', 'updated_ago')).toBe('Updated {age} ago');
  });

  it('has a non-empty translation for every key in every language', () => {
    for (const lang of LANGS) {
      for (const key of [
        'page_title',
        'link_invalid',
        'headline_picked_up',
        'demo_note',
      ] as const) {
        expect(t(lang, key).length).toBeGreaterThan(0);
      }
    }
  });
});

describe('detectLang', () => {
  it('prefers a saved choice', () => {
    expect(detectLang('ja', ['en-US'])).toBe('ja');
    expect(detectLang('en', ['ja-JP'])).toBe('en');
  });

  it('falls back to the browser languages, then English', () => {
    expect(detectLang(null, ['en-GB', 'ja-JP'])).toBe('ja');
    expect(detectLang('fr', ['JA'])).toBe('ja');
    expect(detectLang(null, ['de-DE'])).toBe('en');
    expect(detectLang(null, [])).toBe('en');
  });
});

describe('formatAge', () => {
  it('uses seconds under two minutes, then minutes', () => {
    expect(formatAge('en', 0)).toBe('0 s');
    expect(formatAge('en', 4_999)).toBe('4 s');
    expect(formatAge('en', 119_000)).toBe('119 s');
    expect(formatAge('en', 180_000)).toBe('3 min');
    expect(formatAge('ja', 7_000)).toBe('7秒');
    expect(formatAge('ja', 600_000)).toBe('10分');
    expect(formatAge('en', -5)).toBe('0 s');
  });
});

describe('formatTokyoTime', () => {
  it('shows the time in Tokyo whatever the viewer time zone', () => {
    const at = Date.parse('2026-10-14T03:05:00Z'); // 12:05 JST
    expect(formatTokyoTime('en', at)).toBe('12:05');
    expect(formatTokyoTime('ja', at)).toBe('12:05');
  });
});
