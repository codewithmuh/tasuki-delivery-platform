"""Test settings: fake secrets, then the real settings module.

DATABASE_URL decides the database. Unset, tests run on in-memory SQLite (tests marked `postgres` are
skipped); CI points it at a PostgreSQL service so every test runs.
"""

import os

from cryptography.fernet import Fernet

_FAKE = {
    "DJANGO_SECRET_KEY": "test-only-django-secret-key-not-used-anywhere-else",
    "DATABASE_URL": "sqlite://:memory:",
    # A fresh key per run: the test database is created and dropped within the run anyway.
    "ORDERS_FIELD_KEY": Fernet.generate_key().decode(),
    "COURIER_TOKEN_SECRET": "test-only-courier-token-secret-32-chars-or-more",
    "TRACKING_TOKEN_SECRET": "test-only-tracking-token-secret-32-chars-or-more",
    "ADMIN_TOKEN": "test-only-admin-token",
    "DISPATCH_URL": "http://dispatch.test",
    "TRACKING_BASE_URL": "https://track.tasuki.test",
}
for _name, _value in _FAKE.items():
    os.environ.setdefault(_name, _value)

from config.settings import *  # noqa: E402, F403
