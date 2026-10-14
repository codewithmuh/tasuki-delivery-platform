"""Gunicorn settings, loaded with `gunicorn --config python:config.gunicorn config.wsgi`.

Shutdown (runtime contract): on SIGTERM the master closes the listening socket and tells the workers
to stop; each worker flips /readyz to 503, finishes in-flight requests for up to `graceful_timeout`,
and the process exits 0.
"""

from __future__ import annotations

import os
import shutil
import signal
from types import FrameType
from typing import Any

# Several worker processes share one /metrics view: prometheus_client writes each worker's samples
# to files in this directory and sums them at scrape time. It must be on a writable tmpfs (/tmp).
os.environ.setdefault("PROMETHEUS_MULTIPROC_DIR", "/tmp/prometheus-multiproc")  # noqa: S108

bind = f"0.0.0.0:{os.environ.get('PORT', '8000')}"
workers = int(os.environ.get("WEB_CONCURRENCY", "2"))
worker_class = "gthread"
threads = int(os.environ.get("GUNICORN_THREADS", "4"))
timeout = 30  # a worker that stops answering the master for this long is killed and replaced
graceful_timeout = 20  # in-flight requests get this long to finish after SIGTERM
# Longer than the load balancer's idle timeout (60 s on AWS ALB), so the balancer always closes idle
# connections first and never sends a request down a socket gunicorn is closing (a classic 502).
keepalive = 75
accesslog = None  # core.middleware writes one JSON access line per request, with its request_id
logconfig_dict: dict[str, Any] = {
    "version": 1,
    "disable_existing_loggers": False,
    "formatters": {"json": {"()": "core.logs.JsonFormatter"}},
    "handlers": {
        "stdout": {
            "class": "logging.StreamHandler",
            "stream": "ext://sys.stdout",
            "formatter": "json",
        }
    },
    # gunicorn merges this over its own defaults, so name the root logger too (it would otherwise
    # point at gunicorn's default "console" handler, which this dict replaces).
    "root": {"handlers": ["stdout"], "level": "INFO"},
    "loggers": {"gunicorn.error": {"handlers": ["stdout"], "level": "INFO", "propagate": False}},
}


def on_starting(server: Any) -> None:
    """Start each run with an empty metrics directory, so old counters don't leak in."""
    directory = os.environ["PROMETHEUS_MULTIPROC_DIR"]
    shutil.rmtree(directory, ignore_errors=True)
    os.makedirs(directory, exist_ok=True)


def post_worker_init(worker: Any) -> None:
    """Make SIGTERM fail readiness before gunicorn's own handler starts draining the worker."""
    gunicorn_handler = signal.getsignal(signal.SIGTERM)

    def on_sigterm(signum: int, frame: FrameType | None) -> None:
        from core.lifecycle import mark_shutting_down

        mark_shutting_down()
        if callable(gunicorn_handler):
            gunicorn_handler(signum, frame)

    signal.signal(signal.SIGTERM, on_sigterm)


def child_exit(server: Any, worker: Any) -> None:
    """Drop a dead worker's live gauges from the shared metrics (its counters are kept)."""
    from prometheus_client import multiprocess

    multiprocess.mark_process_dead(worker.pid)  # type: ignore[no-untyped-call]
