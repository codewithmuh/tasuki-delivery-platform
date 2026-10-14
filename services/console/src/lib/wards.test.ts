import { describe, expect, it } from 'vitest';
import contract from '../../../../contracts/data/wards.json';
import copy from '../data/wards.json';
import { findWard, TOKYO_BOUNDS, WARD_IDS, wardLabel, WARDS } from './wards';

describe('wards', () => {
  it('is an exact copy of contracts/data/wards.json', () => {
    expect(copy).toEqual(contract);
  });

  it('has the 23 special wards with unique ids', () => {
    expect(WARDS).toHaveLength(23);
    expect(WARD_IDS.size).toBe(23);
  });

  it('finds wards by id and labels them in English and Japanese', () => {
    const shibuya = findWard('shibuya');
    expect(shibuya && wardLabel(shibuya)).toBe('Shibuya · 渋谷区');
    expect(findWard(null)).toBeUndefined();
    expect(findWard('atlantis')).toBeUndefined();
  });

  it('puts every ward centre inside the Tokyo bounds', () => {
    const [[west, south], [east, north]] = TOKYO_BOUNDS;
    for (const w of WARDS) {
      expect(w.lng).toBeGreaterThan(west);
      expect(w.lng).toBeLessThan(east);
      expect(w.lat).toBeGreaterThan(south);
      expect(w.lat).toBeLessThan(north);
    }
  });
});
