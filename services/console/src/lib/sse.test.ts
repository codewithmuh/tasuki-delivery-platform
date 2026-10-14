import { describe, expect, it } from 'vitest';
import { SseParser } from './sse';

describe('SseParser', () => {
  it('parses named events and ignores comments such as the heartbeat', () => {
    const p = new SseParser();
    const out = p.push(': ping\n\nevent: courier\ndata: {"a":1}\n\n');
    expect(out).toEqual([{ event: 'courier', data: '{"a":1}', id: undefined }]);
  });

  it('defaults the event type to "message" and joins multi-line data', () => {
    const p = new SseParser();
    expect(p.push('data: one\ndata: two\n\n')).toEqual([
      { event: 'message', data: 'one\ntwo', id: undefined },
    ]);
  });

  it('handles messages split anywhere across chunks', () => {
    const p = new SseParser();
    expect(p.push('eve')).toEqual([]);
    expect(p.push('nt: status\nda')).toEqual([]);
    expect(p.push('ta: {"x":')).toEqual([]);
    expect(p.push('2}\n')).toEqual([]);
    expect(p.push('\n')).toEqual([{ event: 'status', data: '{"x":2}', id: undefined }]);
  });

  it('accepts CRLF and CR line endings, including a CRLF split between chunks', () => {
    const p = new SseParser();
    expect(p.push('data: a\r\n\r')).toEqual([]);
    expect(p.push('\ndata: b\r\r')).toEqual([{ event: 'message', data: 'a', id: undefined }]);
    // The final CR could still be half of a CRLF: it completes when the next byte arrives.
    expect(p.push(': ping\n')).toEqual([{ event: 'message', data: 'b', id: undefined }]);
  });

  it('strips one leading space only, and a byte-order mark at the start', () => {
    const p = new SseParser();
    expect(p.push('﻿data:  two spaces\n\n')).toEqual([
      { event: 'message', data: ' two spaces', id: undefined },
    ]);
  });

  it('does not dispatch a message without data, and resets the event type', () => {
    const p = new SseParser();
    expect(p.push('event: lonely\n\ndata: x\n\n')).toEqual([
      { event: 'message', data: 'x', id: undefined },
    ]);
  });

  it('tracks id and retry, ignores unknown fields and bad values', () => {
    const p = new SseParser();
    const out = p.push('id: 7\nretry: 2500\nfoo: bar\ndata\n\nid: a\0b\nretry: soon\ndata: y\n\n');
    expect(out).toEqual([
      { event: 'message', data: '', id: '7' },
      { event: 'message', data: 'y', id: '7' },
    ]);
    expect(p.retryMs).toBe(2500);
    expect(p.lastEventId).toBe('7');
  });
});
