# syntax=docker/dockerfile:1
#
# Tasuki routing: OSRM (Open Source Routing Machine) on the roads of Tokyo's 23 wards.
#
# Map data © OpenStreetMap contributors, available under the Open Database License (ODbL 1.0):
# https://www.openstreetmap.org/copyright. The data is downloaded at build time and never
# committed to git. See README.md.
#
# Build:  docker build -t tasuki/routing:local services/routing     (downloads ~500 MB)
# Run:    docker run --rm -p 127.0.0.1:5000:5000 tasuki/routing:local

# Which OpenStreetMap snapshot to build from. Geofabrik keeps the yearly Kantō snapshots
# (kanto-YY0101) for years but daily ones only for a couple of weeks, so the default stays
# downloadable and every build sees the same roads. OSM_MD5 pins that exact file. For today's
# data: --build-arg OSM_SNAPSHOT=latest --build-arg OSM_MD5=
ARG OSM_BASE_URL=https://download.geofabrik.de/asia/japan
ARG OSM_SNAPSHOT=260101
ARG OSM_MD5=43de89c68af54ea9d1206afaa1ff0b1a
# The 23-ward bounding box from contracts/data/wards.json, as min_lng,min_lat,max_lng,max_lat.
# CI fails if it drifts from the contract.
ARG BBOX=139.56,35.52,139.92,35.82

# ---- 1. osm: download the Kantō extract, verify it, clip it to the 23 wards ----
FROM debian:trixie-20261005-slim AS osm
SHELL ["/bin/bash", "-o", "pipefail", "-c"]
# osmium-tool is pinned to Debian trixie's version because it shapes the output; curl and the
# CA certificates are left free to pick up security updates.
# hadolint ignore=DL3008
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates curl osmium-tool=1.18.0-1 \
 && rm -rf /var/lib/apt/lists/*
ARG OSM_BASE_URL
ARG OSM_SNAPSHOT
ARG OSM_MD5
ARG BBOX
WORKDIR /work
# One RUN, so the 500 MB Kantō file never lands in a layer: only the clipped Tokyo file does.
# The published .md5 catches a corrupt or truncated download; OSM_MD5 also catches a
# different file than the one this Dockerfile was written for.
RUN file="kanto-${OSM_SNAPSHOT}.osm.pbf" \
 && curl --fail --silent --show-error --location --retry 3 --proto '=https' --tlsv1.2 \
      --remote-name "${OSM_BASE_URL}/${file}" --remote-name "${OSM_BASE_URL}/${file}.md5" \
 && md5sum --check "${file}.md5" \
 && if [ -n "${OSM_MD5}" ]; then echo "${OSM_MD5}  ${file}" | md5sum --check -; fi \
 && osmium extract --bbox "${BBOX}" --strategy complete_ways --output tokyo-23.osm.pbf "${file}" \
 && { echo "source=${OSM_BASE_URL}/${file}"; \
      echo "md5=$(cut -d ' ' -f 1 "${file}.md5")"; \
      echo "replication_timestamp=$(osmium fileinfo --get header.option.osmosis_replication_timestamp "${file}" || echo unknown)"; \
      echo "bbox=${BBOX}"; } > osm-snapshot.txt \
 && rm "${file}" "${file}.md5" \
 && cat osm-snapshot.txt

# ---- 2. graph: the routing graph, bicycle profile, multi-level Dijkstra (MLD) ----
# Bicycle, not car: Tasuki's couriers ride bicycles and scooters through streets, bike lanes and
# shortcuts a car cannot use. MLD (partition + customize) instead of contraction hierarchies
# keeps the door open to live traffic weights without rebuilding the whole graph.
FROM ghcr.io/project-osrm/osrm-backend:v26.10.0-debian AS graph
COPY --from=osm /work/tokyo-23.osm.pbf /work/osm-snapshot.txt /data/
RUN osrm-extract --profile /opt/bicycle.lua /data/tokyo-23.osm.pbf \
 && osrm-partition /data/tokyo-23.osrm \
 && osrm-customize /data/tokyo-23.osrm \
 && rm /data/tokyo-23.osm.pbf

# ---- runtime: the same OSRM image (osrm-routed and its libraries) plus only the graph ----
FROM ghcr.io/project-osrm/osrm-backend:v26.10.0-debian
ARG OSM_BASE_URL
ARG OSM_SNAPSHOT
ARG BBOX
LABEL org.opencontainers.image.title="tasuki-routing" \
      org.opencontainers.image.description="OSRM (MLD, bicycle profile) on the roads of Tokyo's 23 wards" \
      org.opencontainers.image.source="https://github.com/codewithmuh/tasuki-delivery-platform" \
      org.opencontainers.image.licenses="BSD-2-Clause AND ODbL-1.0" \
      io.tasuki.osm.snapshot="${OSM_SNAPSHOT}" \
      io.tasuki.osm.source="${OSM_BASE_URL}/kanto-${OSM_SNAPSHOT}.osm.pbf" \
      io.tasuki.osm.bbox="${BBOX}" \
      io.tasuki.osm.attribution="© OpenStreetMap contributors (ODbL 1.0)"
COPY --from=graph /data/ /data/
# A numeric non-root user, so Kubernetes' runAsNonRoot can verify it. The graph files are
# world-readable and never written, so the user needs no home directory or ownership.
USER 65532:65532
EXPOSE 5000
# The image has no curl: bash's /dev/tcp sends a minimal HTTP request to the nearest-road API.
HEALTHCHECK --interval=30s --timeout=3s --start-period=60s --retries=3 \
  CMD ["bash", "-c", "exec 3<>/dev/tcp/127.0.0.1/5000 && printf 'GET /nearest/v1/bicycle/139.7016,35.6580 HTTP/1.0\\r\\n\\r\\n' >&3 && head -n 1 <&3 | grep -q ' 200 '"]
ENTRYPOINT ["osrm-routed"]
# --max-table-size covers dispatch's courier × order matrices (/table) with room to spare.
CMD ["--algorithm", "mld", "--ip", "0.0.0.0", "--port", "5000", "--max-table-size", "1000", "/data/tokyo-23.osrm"]
