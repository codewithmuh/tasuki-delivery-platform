"""Field-level encryption for personal data (customer name, phone and address text).

Here: Fernet (AES-128-CBC + HMAC-SHA256) with the keys in ORDERS_FIELD_KEY. In production this
becomes AWS KMS envelope encryption: KMS issues a data key, the data key encrypts the fields, and
only the KMS-wrapped copy of the data key is stored, so no long-lived key material sits in an
environment variable.
"""

from __future__ import annotations

from functools import cache
from typing import Any

from cryptography.fernet import Fernet, MultiFernet
from django.conf import settings
from django.db import models


@cache
def _fernet(keys: tuple[str, ...]) -> MultiFernet:
    # MultiFernet encrypts with the first key and decrypts with any of them, which allows key
    # rotation.
    return MultiFernet([Fernet(key) for key in keys])


def encrypt(plaintext: str) -> str:
    return _fernet(tuple(settings.ORDERS_FIELD_KEYS)).encrypt(plaintext.encode()).decode()


def decrypt(token: str) -> str:
    return _fernet(tuple(settings.ORDERS_FIELD_KEYS)).decrypt(token.encode()).decode()


class EncryptedTextField(models.TextField):  # type: ignore[type-arg]
    """A text column stored encrypted. Ciphertexts are random, so it can't be filtered on."""

    def get_prep_value(self, value: Any) -> Any:
        value = super().get_prep_value(value)
        return None if value is None else encrypt(value)

    def from_db_value(self, value: str | None, expression: Any, connection: Any) -> str | None:
        return None if value is None else decrypt(value)
