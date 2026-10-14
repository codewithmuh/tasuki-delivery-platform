import type { ReactNode } from 'react';
import type { Tone } from '../lib/tone';

/** The symbol repeats the tone in shape, so the state never depends on colour alone. */
const SYMBOL: Record<Tone, string> = { ok: '●', warn: '▲', bad: '■', neutral: '○' };

export function Badge({ tone, children }: { tone: Tone; children: ReactNode }) {
  return (
    <span className={`badge badge-${tone}`}>
      <span className="badge-symbol" aria-hidden="true">
        {SYMBOL[tone]}
      </span>
      {children}
    </span>
  );
}
