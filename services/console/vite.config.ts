import react from '@vitejs/plugin-react';
import { defineConfig } from 'vitest/config';

// In dev and preview, /v1/* is proxied to the tracking service (or `npm run mock`), so the browser
// talks to one origin, exactly like in the cluster where the ingress routes /v1 to tracking.
const trackingTarget = process.env['TRACKING_URL'] ?? 'http://localhost:8090';
const proxy = { '/v1': { target: trackingTarget, changeOrigin: true } };

export default defineConfig({
  plugins: [react()],
  server: { port: 5173, strictPort: true, proxy },
  preview: { port: 4173, strictPort: true, proxy },
  build: {
    target: 'es2023',
    rolldownOptions: {
      output: {
        // MapLibre (~1 MB) and React change rarely: separate chunks stay cached across app releases.
        codeSplitting: {
          groups: [
            { name: 'maplibre', test: /node_modules[\\/]maplibre-gl/ },
            { name: 'react', test: /node_modules[\\/](react|react-dom|scheduler)[\\/]/ },
          ],
        },
      },
    },
    chunkSizeWarningLimit: 1100, // the MapLibre chunk; app code is a few kB
  },
  test: {
    include: ['src/**/*.test.ts'],
    environment: 'node',
    coverage: {
      provider: 'v8',
      include: ['src/lib/**/*.ts'],
      exclude: ['src/lib/**/*.test.ts'],
      reporter: ['text', 'json-summary'],
      thresholds: { lines: 80, functions: 80, branches: 80, statements: 80 },
    },
  },
});
