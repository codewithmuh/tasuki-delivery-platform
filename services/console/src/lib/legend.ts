/**
 * How each courier status looks. Colour is never the only signal: every status also has its own
 * shape (an SVG path in a 20×20 box, drawn on the map and in the legend) and a text label.
 * Colours are from the Okabe–Ito palette, which stays distinguishable with colour-vision deficiency.
 */
import type { ActiveStatus } from './fleet';

export interface StatusStyle {
  label: string;
  color: string;
  shape: string;
}

export const STATUS_STYLE: Record<ActiveStatus, StatusStyle> = {
  idle: { label: 'Idle', color: '#7d7d7d', shape: 'M10 4a6 6 0 1 1 0 12a6 6 0 1 1 0-12z' },
  to_store: { label: 'To store', color: '#e69f00', shape: 'M10 2.5L17.5 16H2.5z' },
  at_store: { label: 'At store', color: '#cc79a7', shape: 'M4 4h12v12H4z' },
  to_dropoff: { label: 'To drop-off', color: '#0072b2', shape: 'M10 1.5L18.5 10L10 18.5L1.5 10z' },
};

/** Map image id for a status icon. */
export function iconId(status: ActiveStatus): string {
  return `courier-${status}`;
}
