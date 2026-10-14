#!/usr/bin/env python
"""Django's command-line utility: migrations, the outbox relay, the dispatch consumer, cleanup."""

import os
import sys
from pathlib import Path


def main() -> None:
    # The code lives in src/ (the image sets PYTHONPATH=/app/src; this makes `python manage.py` work
    # locally too).
    sys.path.insert(0, str(Path(__file__).resolve().parent / "src"))
    os.environ.setdefault("DJANGO_SETTINGS_MODULE", "config.settings")
    from django.core.management import execute_from_command_line

    execute_from_command_line(sys.argv)


if __name__ == "__main__":
    main()
