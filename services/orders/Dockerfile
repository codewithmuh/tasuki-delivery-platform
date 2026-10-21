# syntax=docker/dockerfile:1

FROM ghcr.io/astral-sh/uv:0.13.4 AS uv

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
COPY manage.py ./
COPY src ./src
# The runtime filesystem is read-only, so Python can't cache bytecode there: compile it now.
# unchecked-hash: the .pyc files stay valid whatever the timestamps after COPY.
RUN python -m compileall -q --invalidation-mode unchecked-hash src

# ---- runtime: slim image, only the virtualenv and the code, non-root ----
FROM python:3.13-slim-trixie
ARG APP_VERSION=0.0.0
ENV PATH="/app/.venv/bin:${PATH}" \
    PYTHONPATH=/app/src \
    PYTHONUNBUFFERED=1 \
    PYTHONDONTWRITEBYTECODE=1 \
    DJANGO_SETTINGS_MODULE=config.settings \
    APP_VERSION=${APP_VERSION}
# pip is only needed to build, and its bundled libraries (urllib3, msgpack, …) show up in CVE scans.
RUN python -m pip uninstall --yes pip \
 && groupadd --system --gid 10001 app && useradd --system --uid 10001 --gid app --no-create-home app
WORKDIR /app
# Owned by root and read-only to the app user: the process can't modify its own code or libraries.
COPY --from=build /app/.venv /app/.venv
COPY --from=build /app/manage.py /app/manage.py
COPY --from=build /app/src /app/src
USER 10001:10001
EXPOSE 8000
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD ["python", "-c", "import sys, urllib.request; sys.exit(urllib.request.urlopen('http://127.0.0.1:8000/healthz', timeout=2).status != 200)"]
# The API. The same image runs the workers with a different command:
#   python manage.py migrate --noinput          (a pre-deploy job)
#   python manage.py relay_outbox               (outbox -> Kafka)
#   python manage.py consume_dispatch_events    (dispatch.events -> orders)
#   python manage.py purge_expired              (hourly)
# gunicorn handles SIGTERM: readiness fails, in-flight requests get 20 s to finish, then exit 0.
CMD ["gunicorn", "--config", "python:config.gunicorn", "config.wsgi"]
