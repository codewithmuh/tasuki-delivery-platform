# syntax=docker/dockerfile:1

# ---- build: static files with Vite ----
FROM node:24.21.0-trixie-slim AS build
WORKDIR /src
COPY package.json package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY index.html tsconfig.json vite.config.ts ./
COPY public ./public
COPY src ./src
# config.json is written at container start from the environment, not baked in.
RUN npm run build && rm dist/config.json

# ---- runtime: unprivileged nginx (uid 101), read-only root filesystem, writes only to /tmp ----
FROM nginxinc/nginx-unprivileged:1.30.5-alpine3.24
# The image's entrypoint renders /etc/nginx/templates into /tmp/tasuki at start, substituting
# only CONSOLE_* variables (nginx's own $variables are left alone).
ENV CONSOLE_TRACKING_BASE_URL="" \
    NGINX_ENVSUBST_OUTPUT_DIR=/tmp \
    NGINX_ENVSUBST_FILTER=^CONSOLE_
COPY docker/nginx.conf /etc/nginx/nginx.conf
COPY docker/templates /etc/nginx/templates
COPY --chmod=755 docker/05-check-config.sh /docker-entrypoint.d/05-check-config.sh
COPY --from=build /src/dist /srv/console
USER 101:101
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
