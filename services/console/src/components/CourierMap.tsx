/**
 * The map: MapLibre GL with one GeoJSON source of couriers drawn as a single symbol layer.
 * React owns the page around it; courier data goes straight to MapLibre through the handle
 * (`setCouriers`), so 3,000 moving couriers cost one `setData` call per refresh, not 3,000 DOM nodes.
 */
import {
  type GeoJSONSource,
  type LngLatBoundsLike,
  Map as MapLibreMap,
  NavigationControl,
  setWorkerUrl,
} from 'maplibre-gl';
import 'maplibre-gl/dist/maplibre-gl.css';
// MapLibre's worker, emitted by Vite as a hashed same-origin file: CSP can say worker-src 'self'.
import workerUrl from 'maplibre-gl/dist/maplibre-gl-worker.mjs?url';
import { useEffect, useImperativeHandle, useRef, type Ref } from 'react';
import { prefersReducedMotion, usePrefersDark } from '../hooks';
import { BASEMAP_STYLES } from '../lib/basemap';
import { ACTIVE_STATUSES, type ActiveStatus, type CourierFeatureCollection } from '../lib/fleet';
import { iconId, STATUS_STYLE } from '../lib/legend';
import { TOKYO_BOUNDS } from '../lib/wards';

setWorkerUrl(workerUrl);

export interface CourierMapHandle {
  setCouriers: (data: CourierFeatureCollection) => void;
  fitBounds: (bounds: LngLatBoundsLike) => void;
  moveTo: (lng: number, lat: number, zoom?: number) => void;
}

interface CourierMapProps {
  label: string;
  ref?: Ref<CourierMapHandle>;
}

const SOURCE = 'couriers';
const EMPTY: CourierFeatureCollection = { type: 'FeatureCollection', features: [] };
const ICON_PX = 20;
const PIXEL_RATIO = 2;

/** Draw a status icon (its shape, filled with its colour, white outline) for the map. */
function drawIcon(status: ActiveStatus): ImageData | null {
  const size = ICON_PX * PIXEL_RATIO;
  const canvas = document.createElement('canvas');
  canvas.width = size;
  canvas.height = size;
  const ctx = canvas.getContext('2d');
  if (!ctx) return null;
  ctx.scale(PIXEL_RATIO, PIXEL_RATIO);
  const path = new Path2D(STATUS_STYLE[status].shape);
  ctx.fillStyle = STATUS_STYLE[status].color;
  ctx.fill(path);
  ctx.lineWidth = 1.5;
  ctx.strokeStyle = '#ffffff';
  ctx.stroke(path);
  return ctx.getImageData(0, 0, size, size);
}

/** Add our icons, source and layer. Runs on every style load: a theme switch replaces the style. */
function addCourierLayer(map: MapLibreMap, data: CourierFeatureCollection): void {
  for (const status of ACTIVE_STATUSES) {
    const image = map.hasImage(iconId(status)) ? null : drawIcon(status);
    if (image) map.addImage(iconId(status), image, { pixelRatio: PIXEL_RATIO });
  }
  if (!map.getSource(SOURCE)) map.addSource(SOURCE, { type: 'geojson', data });
  if (map.getLayer(SOURCE)) return;
  map.addLayer({
    id: SOURCE,
    type: 'symbol',
    source: SOURCE,
    layout: {
      'icon-image': ['concat', 'courier-', ['get', 'status']],
      'icon-size': ['interpolate', ['linear'], ['zoom'], 9, 0.5, 13, 0.75, 16, 1],
      'icon-allow-overlap': true,
      'icon-ignore-placement': true,
      // Couriers carrying an order are drawn on top of idle ones.
      'symbol-sort-key': [
        'match',
        ['get', 'status'],
        'to_dropoff',
        3,
        'at_store',
        2,
        'to_store',
        1,
        0,
      ],
    },
  });
}

export function CourierMap({ label, ref }: CourierMapProps) {
  const containerRef = useRef<HTMLDivElement>(null);
  const mapRef = useRef<MapLibreMap | null>(null);
  const dataRef = useRef<CourierFeatureCollection>(EMPTY);
  const dark = usePrefersDark();
  const styleUrl = dark ? BASEMAP_STYLES.dark : BASEMAP_STYLES.light;
  const styleRef = useRef(styleUrl);

  useImperativeHandle(ref, () => ({
    setCouriers(data) {
      dataRef.current = data;
      const source = mapRef.current?.getSource<GeoJSONSource>(SOURCE);
      if (source) void source.setData(data);
    },
    fitBounds(bounds) {
      mapRef.current?.fitBounds(bounds, {
        padding: 32,
        duration: prefersReducedMotion() ? 0 : 800,
      });
    },
    moveTo(lng, lat, zoom) {
      const map = mapRef.current;
      if (!map) return;
      map.easeTo({
        center: [lng, lat],
        zoom: zoom ?? map.getZoom(),
        duration: prefersReducedMotion() ? 0 : 800,
      });
    },
  }));

  useEffect(() => {
    const container = containerRef.current;
    if (!container) return;
    let map: MapLibreMap;
    try {
      map = new MapLibreMap({
        container,
        style: styleRef.current,
        bounds: TOKYO_BOUNDS,
        fitBoundsOptions: { padding: 32 },
        attributionControl: { compact: false },
        // A calm, flat city map: no rotation or tilt.
        dragRotate: false,
        pitchWithRotate: false,
        touchPitch: false,
        maxPitch: 0,
      });
    } catch {
      container.textContent = 'The map needs WebGL, which is not available in this browser.';
      return;
    }
    map.addControl(new NavigationControl({ showCompass: false }), 'bottom-right');
    map.on('style.load', () => {
      addCourierLayer(map, dataRef.current);
    });
    mapRef.current = map;
    return () => {
      map.remove();
      mapRef.current = null;
    };
  }, []);

  useEffect(() => {
    if (styleRef.current === styleUrl) return;
    styleRef.current = styleUrl;
    // A full reload fires 'style.load', which adds the courier layer back.
    mapRef.current?.setStyle(styleUrl, { diff: false });
  }, [styleUrl]);

  return <div ref={containerRef} className="map" role="region" aria-label={label} />;
}
