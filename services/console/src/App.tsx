import type { RuntimeConfig } from './lib/config';
import type { Route } from './lib/routes';
import { OpsPage } from './pages/OpsPage';
import { TrackPage } from './pages/TrackPage';

export function App({ config, route }: { config: RuntimeConfig; route: Route }) {
  switch (route.page) {
    case 'ops':
      return <OpsPage config={config} initialWard={route.ward} />;
    case 'track':
      return <TrackPage config={config} token={route.token} />;
    case 'not_found':
      return (
        <main className="card message">
          <h1>Page not found</h1>
          <p>
            <a href="/">Open the live map</a>
          </p>
        </main>
      );
  }
}
