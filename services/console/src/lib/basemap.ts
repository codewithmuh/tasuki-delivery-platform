/**
 * Basemap: OpenFreeMap's public instance. Free, no API key, no registration, commercial use
 * allowed (openfreemap.org, Terms of Service). Map data © OpenStreetMap contributors (ODbL);
 * the attribution comes with the tiles and is always shown on the map.
 * The host must match the Content-Security-Policy in docker/templates/tasuki/console.conf.template.
 */
export const BASEMAP_STYLES = {
  light: 'https://tiles.openfreemap.org/styles/positron',
  dark: 'https://tiles.openfreemap.org/styles/dark',
} as const;
