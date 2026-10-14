/** State of the customer tracking page, built from the `/v1/track/{token}/stream` events. */
import type { EndReason, OrderStatus, TrackEvent } from './events';

export interface TrackState {
  status: OrderStatus | null;
  /** When the order is expected, epoch ms, if the server gave an estimate. */
  eta: number | null;
  statusUpdatedAt: number | null;
  courier: { lat: number; lng: number; receivedAt: number } | null;
  /** Newest server timestamp on the stream: drives "updated N s ago". */
  lastUpdateAt: number | null;
  ended: EndReason | null;
}

export const initialTrackState: TrackState = {
  status: null,
  eta: null,
  statusUpdatedAt: null,
  courier: null,
  lastUpdateAt: null,
  ended: null,
};

function newest(a: number | null, b: number): number {
  return a === null || b > a ? b : a;
}

export function trackReducer(state: TrackState, event: TrackEvent): TrackState {
  switch (event.type) {
    case 'status':
      // Events are at-least-once and may arrive out of order: never step back in time.
      if (state.statusUpdatedAt !== null && event.updatedAt < state.statusUpdatedAt) return state;
      return {
        ...state,
        status: event.status,
        eta: event.eta,
        statusUpdatedAt: event.updatedAt,
        lastUpdateAt: newest(state.lastUpdateAt, event.updatedAt),
      };
    case 'position':
      if (state.courier && event.receivedAt < state.courier.receivedAt) return state;
      return {
        ...state,
        courier: { lat: event.lat, lng: event.lng, receivedAt: event.receivedAt },
        lastUpdateAt: newest(state.lastUpdateAt, event.receivedAt),
      };
    case 'end':
      return { ...state, ended: event.reason };
  }
}

/** The four steps the customer sees. Cancelled and failed are shown as a message instead. */
export const STEPS = ['confirmed', 'assigned', 'picked_up', 'delivered'] as const;
export type Step = (typeof STEPS)[number];
export type StepState = 'done' | 'current' | 'todo';

export function stepStates(status: OrderStatus | null): { step: Step; state: StepState }[] {
  const index = STEPS.findIndex((s) => s === status);
  return STEPS.map((step, i) => {
    // Delivered is the last step: once reached it is done, not "in progress".
    if (index === STEPS.length - 1) return { step, state: 'done' };
    if (index === -1 || i > index) return { step, state: 'todo' };
    return { step, state: i === index ? 'current' : 'done' };
  });
}

/** The order status to show: a final `end` event outranks a status event we may have missed. */
export function effectiveStatus(state: TrackState): OrderStatus | null {
  if (state.ended && state.ended !== 'expired') return state.ended;
  return state.status;
}

/** The single message that best describes the order right now (an i18n key suffix). */
export type Headline = 'waiting' | OrderStatus | 'expired';

export function headline(state: TrackState): Headline {
  if (state.ended === 'expired') return 'expired';
  return effectiveStatus(state) ?? 'waiting';
}
