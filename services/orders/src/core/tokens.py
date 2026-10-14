"""Signed JWTs (HS256) shared with the Go services: courier tokens and customer tracking links.

Verification accepts HS256 only, so a token can't choose a weaker algorithm ("alg": "none").
"""

from __future__ import annotations

from datetime import datetime, timedelta

import jwt
from django.conf import settings

ALGORITHM = "HS256"
CLOCK_SKEW_LEEWAY_S = 10
TRACKING_LINK_VALID_AFTER_PROMISE = timedelta(hours=24)


class InvalidTokenError(Exception):
    pass


def issue_courier_token(courier_id: str, now: datetime) -> str:
    claims = {
        "sub": courier_id,
        "role": "courier",
        "iat": int(now.timestamp()),
        "exp": int((now + timedelta(seconds=settings.COURIER_TOKEN_TTL_SECONDS)).timestamp()),
    }
    return jwt.encode(claims, settings.COURIER_TOKEN_SECRET, algorithm=ALGORITHM)


def verify_courier_token(token: str) -> str:
    """Return the courier ID in a valid, unexpired courier token, or raise InvalidTokenError."""
    try:
        claims = jwt.decode(
            token,
            settings.COURIER_TOKEN_SECRET,
            algorithms=[ALGORITHM],
            options={"require": ["sub", "role", "exp"]},
            leeway=CLOCK_SKEW_LEEWAY_S,
        )
    except jwt.PyJWTError as exc:
        raise InvalidTokenError(str(exc)) from exc
    if claims.get("role") != "courier" or not isinstance(claims.get("sub"), str):
        raise InvalidTokenError("not a courier token")
    courier_id: str = claims["sub"]
    return courier_id


def tracking_token(order_id: str, promised_at: datetime) -> str:
    """A link token for one order. No `iat`, so the same order always gets the same link."""
    claims = {
        "sub": order_id,
        "role": "tracking",
        "exp": int((promised_at + TRACKING_LINK_VALID_AFTER_PROMISE).timestamp()),
    }
    return jwt.encode(claims, settings.TRACKING_TOKEN_SECRET, algorithm=ALGORITHM)


def tracking_url(order_id: str, promised_at: datetime) -> str:
    return f"{settings.TRACKING_PAGE_URL}/track/{tracking_token(order_id, promised_at)}"
