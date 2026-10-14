#!/usr/bin/env bash
# Build Tasuki's OSRM routing data for Tokyo's 23 wards without Docker. These are the same steps
# as services/routing/Dockerfile, for machines that have osmium-tool and the OSRM tools
# (osrm-extract, osrm-partition, osrm-customize, osrm-routed) on PATH.
#
# Map data © OpenStreetMap contributors, under the ODbL 1.0. The output is git-ignored; never
# commit it.
#
# Usage:    services/routing/build-data.sh [output-dir]          (default: services/routing/data)
# Settings: OSM_SNAPSHOT (default 260101, or "latest"), OSM_MD5 (pinned checksum; set it empty
#           to skip), OSM_BASE_URL, BBOX (default: from contracts/data/wards.json),
#           OSRM_PROFILE (path to bicycle.lua; default: next to the OSRM install)

set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "${here}/../.." && pwd)"
out="${1:-${here}/data}"

OSM_BASE_URL="${OSM_BASE_URL:-https://download.geofabrik.de/asia/japan}"
OSM_SNAPSHOT="${OSM_SNAPSHOT:-260101}"
OSM_MD5="${OSM_MD5-43de89c68af54ea9d1206afaa1ff0b1a}" # "-", not ":-": an empty value means "do not pin"
OSRM_PROFILE="${OSRM_PROFILE:-}"
BBOX="${BBOX:-}"

die() {
  echo "error: $*" >&2
  exit 1
}
need() { command -v "$1" >/dev/null 2>&1 || die "$1 is not installed"; }
md5_of() {
  if command -v md5sum >/dev/null 2>&1; then
    md5sum "$1" | cut -d ' ' -f 1
  else
    md5 -q "$1" # macOS
  fi
}

for tool in curl osmium osrm-extract osrm-partition osrm-customize; do need "${tool}"; done

# The bounding box comes from the contract, so the data always covers every ward.
if [[ -z "${BBOX}" ]]; then
  need jq
  BBOX="$(jq -r '.bbox | "\(.min_lng),\(.min_lat),\(.max_lng),\(.max_lat)"' "${repo}/contracts/data/wards.json")"
fi

if [[ -z "${OSRM_PROFILE}" ]]; then
  extract_bin="$(command -v osrm-extract)"
  bin_dir="$(dirname "${extract_bin}")"
  OSRM_PROFILE="${bin_dir}/../share/osrm/profiles/bicycle.lua" # where osrm-backend installs its profiles
fi
[[ -f "${OSRM_PROFILE}" ]] || die "bicycle profile not found at ${OSRM_PROFILE}; set OSRM_PROFILE"

file="kanto-${OSM_SNAPSHOT}.osm.pbf"
mkdir -p "${out}"
cd "${out}"

curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
  --output "${file}.md5" "${OSM_BASE_URL}/${file}.md5"
published="$(cut -d ' ' -f 1 "${file}.md5")"
if [[ -n "${OSM_MD5}" && "${published}" != "${OSM_MD5}" ]]; then
  die "Geofabrik publishes md5 ${published} for ${file}, but OSM_MD5 pins ${OSM_MD5}"
fi

have=""
if [[ -f "${file}" ]]; then
  have="$(md5_of "${file}")"
fi
if [[ "${have}" == "${published}" ]]; then
  echo "==> ${file} is already downloaded and verified"
else
  echo "==> Downloading ${OSM_BASE_URL}/${file} (about 500 MB)"
  curl --fail --show-error --location --retry 3 --proto '=https' --tlsv1.2 \
    --output "${file}" "${OSM_BASE_URL}/${file}"
  actual="$(md5_of "${file}")"
  [[ "${actual}" == "${published}" ]] || die "md5 ${actual} does not match Geofabrik's ${published}"
fi

echo "==> Clipping to the 23 wards (${BBOX})"
osmium extract --bbox "${BBOX}" --strategy complete_ways --overwrite --output tokyo-23.osm.pbf "${file}"

echo "==> Building the bicycle routing graph (MLD)"
osrm-extract --profile "${OSRM_PROFILE}" tokyo-23.osm.pbf
osrm-partition tokyo-23.osrm
osrm-customize tokyo-23.osrm
rm -f tokyo-23.osm.pbf

timestamp="$(osmium fileinfo --get header.option.osmosis_replication_timestamp "${file}" || echo unknown)"
{
  echo "source=${OSM_BASE_URL}/${file}"
  echo "md5=${published}"
  echo "replication_timestamp=${timestamp}"
  echo "bbox=${BBOX}"
} >osm-snapshot.txt

echo "==> Done: the graph is in ${out}. Serve it with:"
echo "    osrm-routed --algorithm mld --port 5000 ${out}/tokyo-23.osrm"
