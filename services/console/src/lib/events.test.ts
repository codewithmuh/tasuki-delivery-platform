import { describe, expect, it } from 'vitest';
import { parseFleetEvent, parseTrackEvent } from './events';

const msg = (event: string, data: unknown) => ({
  event,
  data: typeof data === 'string' ? data : JSON.stringify(data),
  id: undefined,
});

const courier = {
  courier_id: 'cou_01J0000000000000000000000A',
  lat: 35.664,
  lng: 139.698,
  status: 'to_dropoff',
  order_id: 'ord_01J0000000000000000000000B',
  ward: 'shibuya',
  device_time: '2026-10-14T03:00:00.000Z',
  received_at: '2026-10-14T03:00:01.000Z',
};

describe('parseFleetEvent', () => {
  it('decodes a courier event, parsing times once', () => {
    expect(parseFleetEvent(msg('courier', courier))).toEqual({
      type: 'courier',
      courier: {
        courierId: courier.courier_id,
        lat: 35.664,
        lng: 139.698,
        status: 'to_dropoff',
        orderId: courier.order_id,
        ward: 'shibuya',
        deviceTime: Date.parse(courier.device_time),
        receivedAt: Date.parse(courier.received_at),
      },
    });
  });

  it('accepts a null order and ward', () => {
    const event = parseFleetEvent(msg('courier', { ...courier, order_id: null, ward: null }));
    expect(event?.type === 'courier' && event.courier.orderId).toBeNull();
    expect(event?.type === 'courier' && event.courier.ward).toBeNull();
  });

  it('decodes courier_gone', () => {
    expect(parseFleetEvent(msg('courier_gone', { courier_id: 'cou_1' }))).toEqual({
      type: 'courier_gone',
      courierId: 'cou_1',
    });
  });

  it.each([
    ['not JSON', msg('courier', '{oops')],
    ['an array', msg('courier', '[1,2]')],
    ['JSON null', msg('courier', 'null')],
    ['no courier_id', msg('courier', { ...courier, courier_id: undefined })],
    ['latitude out of range', msg('courier', { ...courier, lat: 91 })],
    ['longitude not a number', msg('courier', { ...courier, lng: '139.7' })],
    ['unknown status', msg('courier', { ...courier, status: 'flying' })],
    ['bad device_time', msg('courier', { ...courier, device_time: 'yesterday' })],
    ['missing received_at', msg('courier', { ...courier, received_at: undefined })],
    ['unknown event', msg('courier_moved', courier)],
  ])('drops %s', (_name, m) => {
    expect(parseFleetEvent(m)).toBeNull();
  });
});

describe('parseTrackEvent', () => {
  it('decodes status with and without an ETA', () => {
    const base = { order_id: 'ord_1', status: 'assigned', updated_at: '2026-10-14T03:00:00Z' };
    expect(parseTrackEvent(msg('status', { ...base, eta: '2026-10-14T03:30:00Z' }))).toEqual({
      type: 'status',
      orderId: 'ord_1',
      status: 'assigned',
      eta: Date.parse('2026-10-14T03:30:00Z'),
      updatedAt: Date.parse(base.updated_at),
    });
    const noEta = parseTrackEvent(msg('status', { ...base, eta: null }));
    expect(noEta?.type === 'status' && noEta.eta).toBeNull();
  });

  it('decodes position and end', () => {
    expect(
      parseTrackEvent(
        msg('position', {
          courier_id: 'cou_1',
          lat: 35.7,
          lng: 139.7,
          received_at: '2026-10-14T03:00:00Z',
        }),
      ),
    ).toEqual({
      type: 'position',
      courierId: 'cou_1',
      lat: 35.7,
      lng: 139.7,
      receivedAt: Date.parse('2026-10-14T03:00:00Z'),
    });
    expect(parseTrackEvent(msg('end', { reason: 'expired' }))).toEqual({
      type: 'end',
      reason: 'expired',
    });
  });

  it.each([
    ['not JSON', msg('status', 'nope')],
    [
      'status without order_id',
      msg('status', { status: 'assigned', updated_at: '2026-10-14T03:00:00Z' }),
    ],
    [
      'unknown order status',
      msg('status', { order_id: 'o', status: 'lost', updated_at: '2026-10-14T03:00:00Z' }),
    ],
    ['status without updated_at', msg('status', { order_id: 'o', status: 'assigned' })],
    [
      'position without coordinates',
      msg('position', { courier_id: 'c', received_at: '2026-10-14T03:00:00Z' }),
    ],
    ['end with unknown reason', msg('end', { reason: 'bored' })],
    ['unknown event', msg('courier', {})],
  ])('drops %s', (_name, m) => {
    expect(parseTrackEvent(m)).toBeNull();
  });
});
