"""Process lifecycle: set once SIGTERM arrives, so /readyz starts failing while requests drain."""

from __future__ import annotations

import threading

_shutting_down = threading.Event()


def mark_shutting_down() -> None:
    _shutting_down.set()


def is_shutting_down() -> bool:
    return _shutting_down.is_set()


def reset() -> None:
    """For tests only."""
    _shutting_down.clear()
