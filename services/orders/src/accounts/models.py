"""Merchants (tenants), their API keys and stores, and couriers."""

from __future__ import annotations

import hashlib
import hmac
import secrets
from typing import ClassVar

from django.db import models

from core import ids

API_KEY_PREFIX = "tsk"


class Merchant(models.Model):
    """A tenant. Every merchant-facing query is filtered by the authenticated merchant."""

    id = models.CharField(
        primary_key=True, max_length=ids.ID_LENGTH, default=ids.new_merchant_id, editable=False
    )
    name = models.CharField(max_length=200)
    is_active = models.BooleanField(default=True)
    created_at = models.DateTimeField(auto_now_add=True)

    # DRF's IsAuthenticated permission asks the authenticated principal this.
    is_authenticated: ClassVar[bool] = True

    def __str__(self) -> str:
        return self.id


class ApiKeyQuerySet(models.QuerySet["ApiKey"]):
    def active(self) -> ApiKeyQuerySet:
        return self.filter(revoked_at__isnull=True, merchant__is_active=True)


class ApiKey(models.Model):
    """A merchant API key: `tsk_<lookup>_<secret>`. Only a SHA-256 hash of the whole key is stored.

    A fast hash is right here (unlike passwords): the key carries 256 random bits, so there is
    nothing to brute-force, and every request has to check it. The lookup part is public and only
    finds the row.
    """

    id = models.CharField(
        primary_key=True, max_length=ids.ID_LENGTH, default=ids.new_api_key_id, editable=False
    )
    merchant = models.ForeignKey(Merchant, on_delete=models.CASCADE, related_name="api_keys")
    lookup = models.CharField(max_length=16, unique=True)
    key_hash = models.CharField(max_length=64)
    created_at = models.DateTimeField(auto_now_add=True)
    revoked_at = models.DateTimeField(null=True, blank=True)

    objects = ApiKeyQuerySet.as_manager()

    def __str__(self) -> str:
        return f"{API_KEY_PREFIX}_{self.lookup}_…"  # never the secret part

    @classmethod
    def issue(cls, merchant: Merchant) -> tuple[ApiKey, str]:
        """Create a key and return it with its plaintext, which is shown once and never stored."""
        lookup = secrets.token_hex(6)
        plaintext = f"{API_KEY_PREFIX}_{lookup}_{secrets.token_urlsafe(32)}"
        key = cls.objects.create(merchant=merchant, lookup=lookup, key_hash=_sha256(plaintext))
        return key, plaintext

    @classmethod
    def authenticate(cls, plaintext: str) -> ApiKey | None:
        """The active key matching `plaintext`, or None."""
        parts = plaintext.split("_", 2)
        if len(parts) != 3 or parts[0] != API_KEY_PREFIX:
            return None
        key = cls.objects.active().select_related("merchant").filter(lookup=parts[1]).first()
        if key is None or not hmac.compare_digest(key.key_hash, _sha256(plaintext)):
            return None
        return key


def _sha256(value: str) -> str:
    return hashlib.sha256(value.encode()).hexdigest()


class Store(models.Model):
    """A merchant's pickup location. `ward` is a dispatch zone from contracts/data/wards.json."""

    id = models.CharField(
        primary_key=True, max_length=ids.ID_LENGTH, default=ids.new_store_id, editable=False
    )
    merchant = models.ForeignKey(Merchant, on_delete=models.PROTECT, related_name="stores")
    name = models.CharField(max_length=200)
    lat = models.FloatField()
    lng = models.FloatField()
    ward = models.CharField(max_length=20)
    # How long the store needs to get an order ready; the earliest a courier can leave with it.
    prep_minutes = models.PositiveSmallIntegerField(default=10)
    created_at = models.DateTimeField(auto_now_add=True)

    def __str__(self) -> str:
        return self.id


class Courier(models.Model):
    """A freelance courier. Orders issues their tokens; the Go services verify them."""

    class Vehicle(models.TextChoices):
        BICYCLE = "bicycle"
        SCOOTER = "scooter"

    id = models.CharField(
        primary_key=True, max_length=ids.ID_LENGTH, default=ids.new_courier_id, editable=False
    )
    name = models.CharField(max_length=200)
    vehicle = models.CharField(max_length=10, choices=Vehicle.choices)
    is_active = models.BooleanField(default=True)
    created_at = models.DateTimeField(auto_now_add=True)

    is_authenticated: ClassVar[bool] = True

    def __str__(self) -> str:
        return self.id
