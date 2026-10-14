import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { App } from './App';
import { loadConfig } from './lib/config';
import { parseRoute } from './lib/routes';
import { WARD_IDS } from './lib/wards';
import './styles.css';

const container = document.getElementById('root');
if (!container) throw new Error('#root is missing from index.html');
const root = createRoot(container);
const route = parseRoute(window.location.pathname, window.location.search, WARD_IDS);

loadConfig()
  .then((config) => {
    root.render(
      <StrictMode>
        <App config={config} route={route} />
      </StrictMode>,
    );
  })
  .catch((err: unknown) => {
    console.error('Cannot start: bad /config.json', err);
    root.render(
      <main className="card message">
        <h1>The console could not start</h1>
        <p>Its configuration (/config.json) is missing or invalid.</p>
      </main>,
    );
  });
