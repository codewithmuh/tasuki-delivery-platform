"""Plumbing for the relay and consumer processes: heartbeat probes, metrics, stop signals."""

from __future__ import annotations

import json
import os
import signal
import threading
import time
import urllib.error
import urllib.request

from messaging.worker import Heartbeat, serve_worker_endpoints, stop_on_signals


def _get(port: int, path: str) -> tuple[int, bytes]:
    try:
        with urllib.request.urlopen(f"http://127.0.0.1:{port}{path}", timeout=2) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read()


def test_worker_endpoints() -> None:
    heartbeat = Heartbeat(max_silence_s=0.2)
    server = serve_worker_endpoints(0, heartbeat)  # port 0: any free port
    port = server.server_address[1]
    try:
        assert _get(port, "/healthz") == (200, b'{"status": "ok"}')
        status, body = _get(port, "/metrics")
        assert status == 200
        assert b"orders_outbox_lag_seconds" in body
        assert _get(port, "/nope")[0] == 404
        time.sleep(0.3)  # the loop stopped beating
        status, body = _get(port, "/readyz")
        assert status == 503
        assert json.loads(body) == {"status": "stalled"}
        heartbeat.beat()
        assert _get(port, "/healthz")[0] == 200
    finally:
        server.shutdown()


def test_sigterm_stops_the_loop() -> None:
    previous = {sig: signal.getsignal(sig) for sig in (signal.SIGTERM, signal.SIGINT)}
    stop = threading.Event()
    try:
        stop_on_signals(stop)
        os.kill(os.getpid(), signal.SIGTERM)
        assert stop.wait(1)
    finally:
        for sig, handler in previous.items():
            signal.signal(sig, handler)
