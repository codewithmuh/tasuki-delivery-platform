/**
 * Incremental parser for the server-sent events wire format (WHATWG HTML §9.2.6), kept small:
 * fields `event`, `data`, `id` and `retry`; comment lines (`: ping`) are skipped.
 *
 * We parse SSE ourselves (over fetch) instead of using EventSource because EventSource cannot
 * see the HTTP status: the tracking page must tell an expired link (401/404) from a network blip.
 */
export interface SseMessage {
  event: string;
  data: string;
  id: string | undefined;
}

export class SseParser {
  private buffer = '';
  private dataLines: string[] = [];
  private eventType = '';
  private started = false;
  /** Last `id:` seen; persists across messages, as in the spec. */
  lastEventId: string | undefined;
  /** Reconnection time the server asked for with `retry:`, in milliseconds. */
  retryMs: number | undefined;

  /** Feed the next decoded chunk of the body; returns every message it completes. */
  push(chunk: string): SseMessage[] {
    let text = this.buffer + chunk;
    if (!this.started && text.length > 0) {
      this.started = true;
      if (text.startsWith('﻿')) text = text.slice(1);
    }
    const messages: SseMessage[] = [];
    let start = 0;
    for (let i = 0; i < text.length; i++) {
      const c = text[i];
      if (c !== '\n' && c !== '\r') continue;
      // A trailing CR may be the first half of CRLF split across chunks: wait for more input.
      if (c === '\r' && i === text.length - 1) break;
      this.processLine(text.slice(start, i), messages);
      if (c === '\r' && text[i + 1] === '\n') i++;
      start = i + 1;
    }
    this.buffer = text.slice(start);
    return messages;
  }

  private processLine(line: string, out: SseMessage[]): void {
    if (line === '') {
      if (this.dataLines.length > 0) {
        out.push({
          event: this.eventType || 'message',
          data: this.dataLines.join('\n'),
          id: this.lastEventId,
        });
      }
      this.dataLines = [];
      this.eventType = '';
      return;
    }
    if (line.startsWith(':')) return; // comment, e.g. the 15 s `: ping` heartbeat
    const colon = line.indexOf(':');
    const field = colon === -1 ? line : line.slice(0, colon);
    let value = colon === -1 ? '' : line.slice(colon + 1);
    if (value.startsWith(' ')) value = value.slice(1);
    switch (field) {
      case 'event':
        this.eventType = value;
        break;
      case 'data':
        this.dataLines.push(value);
        break;
      case 'id':
        if (!value.includes('\0')) this.lastEventId = value;
        break;
      case 'retry':
        if (/^\d+$/.test(value)) this.retryMs = Number(value);
        break;
      default:
        break; // unknown fields are ignored
    }
  }
}
