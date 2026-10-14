"""Idempotent order creation: a retried POST /v1/orders creates at most one order.

The merchant sends an `Idempotency-Key`. We store, under (merchant, key), a hash of the request and
the response we gave, for 24 hours:

- same key, same body: the stored response is returned again (status and body), marked
  `Idempotent-Replayed: true`;
- same key, different body: 422, because the key is being reused for a different request;
- the same key twice at the same moment: one creates the order, the other waits and then replays it.

How the race is handled. The key is claimed with an INSERT at the start of the transaction that
creates the order, and the response is written at its end. A concurrent request with the same key
blocks on the unique index (merchant, key) until the first transaction finishes. If it committed,
the insert fails and the second request replays the stored response; if it rolled back, the second
request goes ahead. This relies on PostgreSQL's default READ COMMITTED isolation, where the next
statement sees the committed row.

Only successful creations are stored. A request that fails (an expired quote, say) leaves no record,
so retrying it with the same key evaluates it again.
"""

from __future__ import annotations

import hashlib
import json
import re
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any

from django.db import IntegrityError, transaction

from accounts.models import Merchant
from core.errors import DomainError, UnprocessableError
from orders.models import IdempotencyRecord

HEADER = "Idempotency-Key"
# Printable ASCII without spaces, up to 255 characters (UUIDs, ULIDs and the like).
_VALID_KEY = re.compile(r"[\x21-\x7e]{1,255}")


class IdempotencyKeyMissingError(DomainError):
    status = 400
    code = "idempotency_key_required"
    default_message = (
        "Send an Idempotency-Key header (1-255 printable characters, e.g. a UUID) with every order."
    )


class IdempotencyKeyReusedError(UnprocessableError):
    code = "idempotency_key_reused"
    default_message = "This Idempotency-Key was already used with a different request body."


@dataclass(frozen=True)
class StoredResponse:
    status: int
    body: dict[str, Any]
    replayed: bool


def validate_key(raw: str | None) -> str:
    if raw is None or not _VALID_KEY.fullmatch(raw):
        raise IdempotencyKeyMissingError()
    return raw


def request_hash(method: str, path: str, body: Any) -> str:
    """Hash of the request as parsed JSON, so whitespace and key order don't make it 'different'."""
    canonical = json.dumps(
        {"method": method, "path": path, "body": body},
        sort_keys=True,
        separators=(",", ":"),
        ensure_ascii=False,
    )
    return hashlib.sha256(canonical.encode()).hexdigest()


def run_once(
    merchant: Merchant,
    key: str,
    fingerprint: str,
    operation: Callable[[], tuple[int, dict[str, Any]]],
) -> StoredResponse:
    """Run `operation` (in the same transaction) unless this key already has a response."""
    with transaction.atomic():
        try:
            # A savepoint, so a failed insert doesn't abort the outer transaction.
            with transaction.atomic():
                record = IdempotencyRecord.objects.create(
                    merchant=merchant, key=key, request_hash=fingerprint
                )
        except IntegrityError:
            existing = IdempotencyRecord.objects.get(merchant=merchant, key=key)
            if existing.request_hash != fingerprint:
                raise IdempotencyKeyReusedError() from None
            if existing.response_status is None or existing.response_body is None:
                raise RuntimeError("committed idempotency record without a response") from None
            return StoredResponse(existing.response_status, existing.response_body, replayed=True)

        status, body = operation()
        record.response_status = status
        record.response_body = body
        record.save(update_fields=["response_status", "response_body"])
        return StoredResponse(status, body, replayed=False)
