"""Prefixed ULIDs (`ord_01J…`): sortable by creation time, safe to generate anywhere, not guessable.

Each generator is a module-level function so migrations can reference it as a field default.
"""

from __future__ import annotations

from ulid import ULID

ID_LENGTH = 30  # 3-letter prefix + "_" + 26 Crockford base32 characters


def new_id(prefix: str) -> str:
    return f"{prefix}_{ULID()}"


def new_merchant_id() -> str:
    return new_id("mer")


def new_api_key_id() -> str:
    return new_id("key")


def new_store_id() -> str:
    return new_id("str")


def new_courier_id() -> str:
    return new_id("cou")


def new_quote_id() -> str:
    return new_id("quo")


def new_order_id() -> str:
    return new_id("ord")


def new_event_id() -> str:
    return new_id("evt")
