import type { ActiveStatus } from '../lib/fleet';
import { STATUS_STYLE } from '../lib/legend';

/** The same shape and colour as the courier icon on the map, for the legend. */
export function StatusIcon({ status }: { status: ActiveStatus }) {
  const style = STATUS_STYLE[status];
  return (
    <svg className="status-icon" viewBox="0 0 20 20" width="18" height="18" aria-hidden="true">
      <path d={style.shape} fill={style.color} stroke="var(--icon-stroke)" strokeWidth="1.5" />
    </svg>
  );
}
