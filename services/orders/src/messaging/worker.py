"""Plumbing for the non-web processes (relay, consumer): stop signals and a tiny HTTP server for
/healthz, /readyz and /metrics.

Liveness means "the main loop is still turning": each iteration calls Heartbeat.beat(), and the
probe fails if it hasn't for `max_silence_s`. That catches a stuck worker, which a crash-only probe
can't.
"""

from __future__ import annotations

import json
import signal
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

from core.metrics import render_latest


class Heartbeat:
    def __init__(self, max_silence_s: float = 60.0) -> None:
        self._max_silence_s = max_silence_s
        self._last = time.monotonic()

    def beat(self) -> None:
        self._last = time.monotonic()

    def alive(self) -> bool:
        return time.monotonic() - self._last < self._max_silence_s


def serve_worker_endpoints(port: int, heartbeat: Heartbeat) -> ThreadingHTTPServer:
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:
            if self.path == "/metrics":
                body, content_type = render_latest()
                self._send(200, body, content_type)
            elif self.path in ("/healthz", "/readyz"):
                ok = heartbeat.alive()
                body = json.dumps({"status": "ok" if ok else "stalled"}).encode()
                self._send(200 if ok else 503, body, "application/json")
            else:
                self._send(
                    404,
                    b'{"error": {"code": "not_found", "message": "Not found."}}',
                    "application/json",
                )

        def _send(self, status: int, body: bytes, content_type: str) -> None:
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, format: str, *args: Any) -> None:
            pass  # probes every few seconds would flood stdout

    # Containers must listen on all interfaces for the kubelet and Prometheus to reach them.
    server = ThreadingHTTPServer(("0.0.0.0", port), Handler)  # noqa: S104
    threading.Thread(target=server.serve_forever, name="worker-http", daemon=True).start()
    return server


def stop_on_signals(stop: threading.Event) -> None:
    """SIGTERM (Kubernetes) or SIGINT (Ctrl-C) ends the loop after the current batch; exit 0."""

    def handle(signum: int, frame: Any) -> None:
        stop.set()

    signal.signal(signal.SIGTERM, handle)
    signal.signal(signal.SIGINT, handle)
