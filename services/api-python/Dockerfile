# syntax=docker/dockerfile:1

FROM ghcr.io/astral-sh/uv:0.13.0 AS uv

# ---- build: resolve the locked dependencies into a virtualenv ----
FROM python:3.13-slim-trixie AS build
COPY --from=uv /uv /usr/local/bin/uv
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy UV_PYTHON_DOWNLOADS=never UV_PROJECT_ENVIRONMENT=/app/.venv
WORKDIR /app
# Dependencies first: this layer stays cached until uv.lock changes.
RUN --mount=type=cache,target=/root/.cache/uv \
    --mount=type=bind,source=uv.lock,target=uv.lock \
    --mount=type=bind,source=pyproject.toml,target=pyproject.toml \
    uv sync --locked --no-dev --no-install-project
COPY pyproject.toml uv.lock README.md ./
COPY src ./src
RUN --mount=type=cache,target=/root/.cache/uv uv sync --locked --no-dev --no-editable

# ---- runtime: slim image, only the virtualenv, non-root ----
FROM python:3.13-slim-trixie
ARG APP_VERSION=0.0.0
ENV PATH="/app/.venv/bin:${PATH}" PYTHONUNBUFFERED=1 APP_VERSION=${APP_VERSION}
# pip is only needed to build, and its bundled libraries (urllib3, msgpack, …) show up in CVE scans.
RUN python -m pip uninstall --yes pip \
 && groupadd --system --gid 10001 app && useradd --system --uid 10001 --gid app --no-create-home app
WORKDIR /app
COPY --from=build --chown=app:app /app/.venv /app/.venv
USER 10001:10001
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD ["python", "-c", "import sys, urllib.request; sys.exit(urllib.request.urlopen('http://127.0.0.1:8000/healthz', timeout=2).status != 200)"]
# uvicorn handles SIGTERM: stop accepting, finish in-flight requests for up to 20 s, then exit.
CMD ["uvicorn", "api.main:app", "--host", "0.0.0.0", "--port", "8000", "--no-access-log", "--timeout-graceful-shutdown", "20"]
