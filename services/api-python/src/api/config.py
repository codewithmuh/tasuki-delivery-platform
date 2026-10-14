"""Configuration from environment variables (12-factor). No secrets have defaults."""

from __future__ import annotations

import os
from collections.abc import Mapping
from dataclasses import dataclass


@dataclass(frozen=True)
class Settings:
    service_name: str = "api-python"
    version: str = "0.0.0"
    log_level: str = "INFO"


def load_settings(env: Mapping[str, str] | None = None) -> Settings:
    """Read settings from the environment; pass a mapping in tests."""
    env = os.environ if env is None else env
    return Settings(
        service_name=env.get("SERVICE_NAME", Settings.service_name),
        version=env.get("APP_VERSION", Settings.version),
        log_level=env.get("LOG_LEVEL", Settings.log_level).upper(),
    )
