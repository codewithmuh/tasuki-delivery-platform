#!/bin/sh
# Runs before the image's template rendering (20-envsubst-on-templates.sh). Fail fast on a bad
# CONSOLE_TRACKING_BASE_URL, because it is written into the CSP header and config.json.
set -eu
url="${CONSOLE_TRACKING_BASE_URL:-}"
if [ -n "$url" ] && ! printf '%s' "$url" | grep -Eq '^https?://[A-Za-z0-9.-]+(:[0-9]+)?$'; then
  echo "CONSOLE_TRACKING_BASE_URL must be empty (same origin) or an origin like https://tracking.example.com, got: $url" >&2
  exit 1
fi
