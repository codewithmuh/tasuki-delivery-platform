/** Tokyo's 23 wards. src/data/wards.json is a copy of contracts/data/wards.json (a test checks). */
import data from '../data/wards.json';

export interface Ward {
  id: string;
  name_en: string;
  name_ja: string;
  lat: number;
  lng: number;
}

export const WARDS: readonly Ward[] = data.wards;
export const WARD_IDS: ReadonlySet<string> = new Set(WARDS.map((w) => w.id));

/** [[west, south], [east, north]] around the 23 wards. */
export const TOKYO_BOUNDS: [[number, number], [number, number]] = [
  [data.bbox.min_lng, data.bbox.min_lat],
  [data.bbox.max_lng, data.bbox.max_lat],
];

export function findWard(id: string | null): Ward | undefined {
  return id === null ? undefined : WARDS.find((w) => w.id === id);
}

export function wardLabel(ward: Ward): string {
  return `${ward.name_en} · ${ward.name_ja}`;
}
