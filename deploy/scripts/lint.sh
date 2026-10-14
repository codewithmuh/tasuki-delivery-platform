#!/usr/bin/env bash
# Static checks for everything Argo CD will apply, without a cluster (`make deploy-lint`, CI job "GitOps
# manifests"). It renders what Argo CD would render, from the same files:
#   - every Helm chart pinned in deploy/platform/ (version, values files and valuesObject from the
#     Application or ApplicationSet itself, cluster annotations replaced by sample values),
#   - every Kustomize directory (deploy/platform, its manifests/, deploy/policies, deploy/apps overlays),
#   - deploy/charts/service once per (service, environment) of the tasuki-services ApplicationSet, and for
#     the platform Applications that use it (rollout-gate),
#   - deploy/charts/postgres-backup once per environment of the orders-db-backups ApplicationSet,
#   - deploy/charts/prometheus-rules once per rule file in observability/,
# then validates it all with kubeconform (-strict, Kubernetes 1.35 and CRD schemas), runs the Kyverno
# policy unit tests, checks the rendered Tasuki workloads against those policies and with Trivy, and
# runs shellcheck on the scripts.
#
# Needs: helm, kubectl (or kustomize), yq (mikefarah v4), jq, kubeconform, kyverno, trivy, shellcheck.
# Charts and schemas are cached in ${DEPLOY_LINT_CACHE:-~/.cache/tasuki-deploy-lint}.
# Works with the bash 3.2 that macOS ships (no associative arrays, no mapfile).
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/../.."

KUBERNETES_VERSION="1.35.0" # infra/platform/variables.tf kubernetes_version
# Custom resources are validated against JSON schemas generated from the CRDs of the exact chart versions
# pinned in deploy/platform/ (step "CRD schemas" below). The datreeio CRDs-catalog, pinned to one commit,
# is the fallback for any custom resource whose CRD no chart here ships. (The catalog's own
# external-secrets.io schemas do not compile: a CRD field literally named "properties" breaks its
# strict conversion, which is why the exact-version schemas come first.)
CRDS_CATALOG="https://raw.githubusercontent.com/datreeio/CRDs-catalog/63669a570e231d4f1f8396d229a1de512bcf0a34/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json"

cache="${DEPLOY_LINT_CACHE:-${XDG_CACHE_HOME:-$HOME/.cache}/tasuki-deploy-lint}"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
out="$work/manifests"     # everything kubeconform validates
scratch="$work/scratch"   # values files, reports
schemas="$work/schemas"   # JSON schemas generated from CRDs
mkdir -p "$cache/charts" "$cache/kubeconform" "$out/platform" "$out/apps" "$out/chart-ci" "$out/rules" "$scratch" "$schemas"

step() { printf '\n==> %s\n' "$*"; }
fail() {
  echo "deploy-lint: $*" >&2
  exit 1
}

# Say which tool is missing instead of dying later with "command not found" (exit 127).
for tool in helm yq jq kubeconform kyverno trivy shellcheck; do
  command -v "$tool" >/dev/null 2>&1 || fail "missing tool: $tool (CI pins the versions in .github/workflows/deploy.yml)"
done
command -v kustomize >/dev/null 2>&1 || command -v kubectl >/dev/null 2>&1 || fail "missing tool: kustomize or kubectl"

kustomize_build() {
  if command -v kustomize >/dev/null 2>&1; then kustomize build "$1"; else kubectl kustomize "$1"; fi
}

# Argo CD renders Helm with the cluster's API versions. The CRD-gated templates in the charts (Argo CD's
# ServiceMonitors, ...) render once these exist, which the sync waves guarantee.
helm_template() {
  helm template "$@" \
    --kube-version "$KUBERNETES_VERSION" \
    --api-versions monitoring.coreos.com/v1 \
    --api-versions monitoring.coreos.com/v1/ServiceMonitor \
    --api-versions monitoring.coreos.com/v1/PodMonitor \
    --api-versions monitoring.coreos.com/v1/PrometheusRule \
    --include-crds --skip-tests
}

# ---------------------------------------------------------------------------------------------------
step "Pinned versions agree"
bootstrap_version="$(sed -n 's/^ARGOCD_CHART_VERSION="\(.*\)"$/\1/p' deploy/bootstrap/bootstrap.sh)"
app_version="$(yq 'select(.metadata.name == "argocd") | .spec.sources[0].targetRevision' deploy/platform/argocd.yaml)"
[[ -n "$bootstrap_version" && "$bootstrap_version" == "$app_version" ]] ||
  fail "bootstrap.sh installs argo-cd ${bootstrap_version}, deploy/platform/argocd.yaml pins ${app_version}"
echo "argo-cd chart ${app_version} in bootstrap.sh and deploy/platform/argocd.yaml"

# Every cluster annotation an ApplicationSet reads must be one that bootstrap.sh writes.
grep -rhoE '\.metadata\.annotations\.[a-z_]+' deploy/platform | sed 's/.*\.//' | sort -u >"$scratch/annotations.txt"
while IFS= read -r key; do
  grep -qE "^    ${key}: " deploy/bootstrap/bootstrap.sh || fail "ApplicationSets read annotation '${key}', which bootstrap.sh never sets"
  echo "cluster annotation ${key} is set by bootstrap.sh"
done <"$scratch/annotations.txt"

# ---------------------------------------------------------------------------------------------------
step "Helm lint (local charts)"
for values in deploy/charts/service/ci/*.yaml; do
  helm lint deploy/charts/service --strict --quiet --values "$values"
done
helm lint deploy/charts/prometheus-rules --strict --quiet --values deploy/charts/prometheus-rules/ci/example-values.yaml
helm lint deploy/charts/postgres-backup --strict --quiet --values deploy/charts/postgres-backup/ci/example-values.yaml

# ---------------------------------------------------------------------------------------------------
step "Render platform Helm charts (deploy/platform/*.yaml)"
# Sample values for the cluster generator's template variables; the real ones come from bootstrap.sh.
fill_cluster_values() {
  sed -e 's/{{ \.metadata\.annotations\.cluster_name }}/tasuki/g' \
    -e 's/{{ \.metadata\.annotations\.region }}/ap-northeast-1/g' \
    -e 's/{{ \.metadata\.annotations\.vpc_id }}/vpc-0123456789abcdef0/g' \
    -e 's/{{ \.metadata\.annotations\.karpenter_queue_name }}/tasuki-karpenter/g' \
    -e 's/{{ \.metadata\.annotations\.aws_account_id }}/123456789012/g' \
    -e 's/{{ \.metadata\.annotations\.backup_bucket }}/tasuki-pg-backups-123456789012/g' \
    -e 's/{{ \.metadata\.annotations\.backup_session }}/20261014T0930Z/g' \
    -e 's/{{ \.name }}/in-cluster/g'
}

kustomize_build deploy/platform >"$out/platform/applications.yaml"

# One JSON line per Helm chart source: Applications, and ApplicationSet templates.
# shellcheck disable=SC2016 # $app and $sources are yq variables, not shell ones
yq -o=json -I=0 '
  select(.kind == "Application" or .kind == "ApplicationSet")
  | (.spec.template // .) as $app
  | ($app.spec.sources // [$app.spec.source]) as $sources
  | $sources[] | select(has("chart"))
  | {
      "app": $app.metadata.name,
      "namespace": $app.spec.destination.namespace,
      "repo": .repoURL, "chart": .chart, "version": .targetRevision,
      "release": .helm.releaseName,
      "skipCrds": (.helm.skipCrds // false),
      "valueFiles": (.helm.valueFiles // []),
      "valuesObject": (.helm.valuesObject // {})
    }' "$out/platform/applications.yaml" >"$scratch/charts.jsonl"

while IFS= read -r source; do
  app="$(jq -r .app <<<"$source")"
  repo="$(jq -r .repo <<<"$source")"
  chart="$(jq -r .chart <<<"$source")"
  version="$(jq -r .version <<<"$source")"

  # Fetch each pinned chart once. Argo CD writes OCI repositories without the oci:// scheme.
  chart_dir="$cache/charts/${chart}-${version}"
  if [[ ! -d "$chart_dir" ]]; then
    tmp="$(mktemp -d)"
    if [[ "$repo" == *://* ]]; then
      helm pull "$chart" --repo "$repo" --version "$version" --untar --untardir "$tmp" >/dev/null
    else
      helm pull "oci://${repo}/${chart}" --version "$version" --untar --untardir "$tmp" >/dev/null
    fi
    mv "$tmp/$chart" "$chart_dir"
    rm -rf "$tmp"
  fi

  args=(--namespace "$(jq -r .namespace <<<"$source")")
  [[ "$(jq -r .skipCrds <<<"$source")" == "true" ]] && args+=(--skip-crds)
  # $values/ is the repository root (the "ref: values" source).
  while IFS= read -r file; do
    [[ -n "$file" ]] && args+=(--values "${file#\$values/}")
  done < <(jq -r '.valueFiles[]' <<<"$source")
  jq '.valuesObject' <<<"$source" | fill_cluster_values >"$scratch/${app}.values.json"
  if grep -q '{{' "$scratch/${app}.values.json"; then
    fail "${app}: valuesObject uses a template variable this script does not know"
  fi
  args+=(--values "$scratch/${app}.values.json")

  release="$(jq -r .release <<<"$source")"
  helm_template "$release" "$chart_dir" "${args[@]}" >"$out/platform/${app}.yaml"
  echo "${app}: ${chart} ${version}"
done <"$scratch/charts.jsonl"

# Platform Applications that render a chart from this repository (rollout-gate uses deploy/charts/service).
# Their value files are relative to the chart, as Argo CD reads them.
# shellcheck disable=SC2016 # yq expression, not shell
yq -o=json -I=0 '
  select(.kind == "Application" and ((.spec.source.path // "") | test("^deploy/charts/")))
  | {
      "app": .metadata.name, "namespace": .spec.destination.namespace, "path": .spec.source.path,
      "release": .spec.source.helm.releaseName, "valueFiles": (.spec.source.helm.valueFiles // [])
    }' "$out/platform/applications.yaml" >"$scratch/local-charts.jsonl"
while IFS= read -r source; do
  app="$(jq -r .app <<<"$source")"
  path="$(jq -r .path <<<"$source")"
  namespace="$(jq -r .namespace <<<"$source")"
  args=(--namespace "$namespace")
  while IFS= read -r file; do
    [[ -f "${path}/${file}" ]] || fail "${app}: value file ${path}/${file} does not exist"
    args+=(--values "${path}/${file}")
  done < <(jq -r '.valueFiles[]' <<<"$source")
  # Into apps/, so the policy and Trivy checks below see it too.
  helm_template "$(jq -r .release <<<"$source")" "$path" "${args[@]}" >"$out/apps/${namespace}-${app}.yaml"
  echo "${app}: ${path}"
done <"$scratch/local-charts.jsonl"

# ---------------------------------------------------------------------------------------------------
step "Render Kustomize directories"
for dir in deploy/platform/manifests/* deploy/policies deploy/apps/overlays/*; do
  name="$(echo "$dir" | tr '/' '_')"
  kustomize_build "$dir" >"$out/platform/kustomize${name}.yaml"
  echo "$dir"
done
# Synced by the grafana-dashboards Application; it needs a kustomization.yaml next to the JSON files.
if [[ -f observability/dashboards/kustomization.yaml ]]; then
  kustomize_build observability/dashboards >"$out/platform/kustomize_observability_dashboards.yaml"
  echo "observability/dashboards"
elif compgen -G "observability/dashboards/*.json" >/dev/null; then
  fail "observability/dashboards has dashboards but no kustomization.yaml (see deploy/platform/grafana-dashboards.yaml)"
fi
# The overlays again, separately, for the policy check below.
for env in dev prod; do
  kustomize_build "deploy/apps/overlays/${env}" >"$out/apps/${env}-namespace.yaml"
done

# ---------------------------------------------------------------------------------------------------
step "Render Tasuki services (deploy/charts/service x tasuki-services ApplicationSet)"
appset='select(.kind == "ApplicationSet" and .metadata.name == "tasuki-services")'
envs="$(yq "$appset | .spec.generators[0].matrix.generators[0].list.elements[].env" deploy/platform/tasuki.yaml)"
services="$(yq "$appset | .spec.generators[0].matrix.generators[1].list.elements[].service" deploy/platform/tasuki.yaml)"
for env in $envs; do
  for service in $services; do
    common="deploy/apps/overlays/${env}/values/common.yaml"
    base="deploy/apps/services/${service}.yaml"
    override="deploy/apps/overlays/${env}/values/${service}.yaml"
    [[ -f "$common" ]] || fail "missing ${common}"
    [[ -f "$base" ]] || fail "${service} is listed in deploy/platform/tasuki.yaml but ${base} does not exist"
    args=(--namespace "tasuki-${env}" --values "$common" --values "$base")
    [[ -f "$override" ]] && args+=(--values "$override")
    helm_template "$service" deploy/charts/service "${args[@]}" >"$out/apps/${env}-${service}.yaml"
    echo "${service}-${env}"
  done
done
# Every values file belongs to a listed service (a typo would otherwise be skipped silently).
for file in deploy/apps/services/*.yaml deploy/apps/overlays/*/values/*.yaml; do
  name="$(basename "$file" .yaml)"
  [[ "$name" == "common" ]] && continue
  grep -qxF "$name" <<<"$services" || fail "${file} matches no service in deploy/platform/tasuki.yaml"
done
# The chart's CI values cover the templates no service turns on yet (Ingress, HPA, ...).
for values in deploy/charts/service/ci/*.yaml; do
  helm_template ci deploy/charts/service --namespace tasuki-dev --values "$values" >"$out/chart-ci/$(basename "$values")"
done
# Every key a service reads from tasuki-app must be listed in the ExternalSecret, which fails loudly when Secrets
# Manager lacks one; a key missing from the list would instead stop the pods (CreateContainerConfigError).
listed_keys="$(yq '.spec.data[].secretKey' deploy/apps/base/external-secret.yaml)"
used_keys="$(yq -N '.secretEnv // {} | to_entries[] | select(.value.secret == "tasuki-app") | .value.key' \
  deploy/apps/services/*.yaml deploy/apps/overlays/*/values/*.yaml | sort -u)"
for key in $used_keys; do
  grep -qxF "$key" <<<"$listed_keys" || fail "tasuki-app key ${key} is used by a service but not listed in deploy/apps/base/external-secret.yaml"
done
echo "tasuki-app keys used by services are all listed in the ExternalSecret"

# ---------------------------------------------------------------------------------------------------
step "Render orders-db ObjectStores (deploy/charts/postgres-backup x orders-db-backups ApplicationSet)"
backups='select(.kind == "ApplicationSet" and .metadata.name == "orders-db-backups")'
yq -o=json "$backups | .spec.template.spec.source.helm.valuesObject" deploy/platform/tasuki.yaml >"$scratch/backups.values.json"
yq -o=json -I=0 "$backups | .spec.generators[0].matrix.generators[1].list.elements[]" deploy/platform/tasuki.yaml |
  while IFS= read -r element; do
    env="$(jq -r .env <<<"$element")"
    # Fill in the list element's keys ({{ .env }}, {{ .retention }}), then the cluster annotations.
    # shellcheck disable=SC2016 # jq program, not shell
    jq --argjson element "$element" '
      walk(if type == "string"
           then reduce ($element | to_entries[]) as $kv (.; gsub("\\{\\{ \\.\($kv.key) \\}\\}"; $kv.value | tostring))
           else . end)' "$scratch/backups.values.json" | fill_cluster_values >"$scratch/backups-${env}.values.json"
    if grep -q '{{' "$scratch/backups-${env}.values.json"; then
      fail "orders-db-backups: valuesObject uses a template variable this script does not know"
    fi
    helm_template orders-db-backups deploy/charts/postgres-backup --namespace "tasuki-${env}" \
      --values "$scratch/backups-${env}.values.json" >"$out/apps/${env}-orders-db-backups.yaml"
    echo "orders-db-backups-${env}"
  done

# ---------------------------------------------------------------------------------------------------
step "Render SLO and alert rules (observability/ x deploy/charts/prometheus-rules)"
for file in observability/slo/generated/*.yaml observability/alerts/*.yaml; do
  [[ -f "$file" ]] || continue
  release="rules-$(basename "$(dirname "$file")")-$(basename "$file" .yaml)"
  helm_template "$release" deploy/charts/prometheus-rules --namespace monitoring --values "$file" >"$out/rules/${release}.yaml"
  echo "$release"
done

# ---------------------------------------------------------------------------------------------------
step "CRD schemas from the pinned charts"
# Each CRD version's openAPIV3Schema becomes <group>/<kind>_<version>.json. Strict like kubeconform's own
# conversion: an object that lists its properties rejects unknown ones, unless the CRD says it keeps them
# (x-kubernetes-preserve-unknown-fields). The recursion follows schema positions only, so a field that
# happens to be called "properties" is treated as a field.
# shellcheck disable=SC2016 # $crd and $k are jq variables, not shell ones
yq -o=json -I=0 'select(.kind == "CustomResourceDefinition")' "$out"/platform/*.yaml |
  jq -r '
    def strict:
      if type != "object" then .
      else
        (if (.properties | type) == "object" then .properties |= map_values(strict) else . end)
        | (if (.items | type) == "object" then .items |= strict else . end)
        | (if (.additionalProperties | type) == "object" then .additionalProperties |= strict else . end)
        | reduce ("allOf", "anyOf", "oneOf") as $k (.; if (.[$k] | type) == "array" then .[$k] |= map(strict) else . end)
        | if has("properties") and (has("additionalProperties") | not)
             and ((.["x-kubernetes-preserve-unknown-fields"] // false) | not)
          then .additionalProperties = false else . end
      end;
    .spec as $crd
    | $crd.versions[]
    | select(.schema.openAPIV3Schema != null)
    | "\($crd.group)/\($crd.names.kind | ascii_downcase)_\(.name).json\t\(.schema.openAPIV3Schema | strict | tojson)"' |
  while IFS=$'\t' read -r path schema; do
    mkdir -p "$schemas/$(dirname "$path")"
    printf '%s\n' "$schema" >"$schemas/$path"
  done
echo "$(find "$schemas" -name '*.json' | wc -l | tr -d ' ') schemas"

# ---------------------------------------------------------------------------------------------------
step "kubeconform (strict, Kubernetes ${KUBERNETES_VERSION}, CRD schemas)"
# CustomResourceDefinitions themselves are skipped: no JSON schema exists for that kind, they come verbatim
# from the upstream charts, and the API server validates them on apply.
kubeconform -strict -summary \
  -kubernetes-version "$KUBERNETES_VERSION" \
  -cache "$cache/kubeconform" \
  -skip CustomResourceDefinition \
  -schema-location default \
  -schema-location "$schemas/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json" \
  -schema-location "$CRDS_CATALOG" \
  "$out"

# ---------------------------------------------------------------------------------------------------
step "Kyverno policy unit tests"
kyverno test deploy/policies/tests

step "Tasuki workloads pass the Tasuki policies"
# Kyverno's autogen and Trivy know Deployments, not Argo Rollouts, and would skip the prod canaries. A Rollout's
# pod template is exactly what its Deployment would have been, so check each Rollout as that Deployment.
policy_view="$work/policy-view"
mkdir -p "$policy_view"
for file in "$out"/apps/*.yaml; do
  yq 'with(select(.kind == "Rollout");
        .apiVersion = "apps/v1" | .kind = "Deployment" | del(.spec.strategy) | del(.spec.rollbackWindow))' \
    "$file" >"$policy_view/$(basename "$file")"
done
echo "$(cat "$out"/apps/*.yaml | grep -c '^kind: Rollout$' || true) Rollouts checked as Deployments"
# The image-signature policy needs the registry and Sigstore, so it is checked in-cluster (PolicyReports).
report="$scratch/kyverno-apply.txt"
if ! kyverno apply \
  deploy/policies/disallow-latest-tag.yaml \
  deploy/policies/require-requests-limits.yaml \
  deploy/policies/require-probes.yaml \
  deploy/policies/restrict-pod-security.yaml \
  --resource "$policy_view" \
  --values-file deploy/policies/tests/values.yaml >"$report" 2>&1; then
  cat "$report"
  fail "kyverno apply failed"
fi
grep -E '^pass: ' "$report"
grep -qE '^pass: [1-9][0-9]*, fail: 0, warn: 0, error: 0' "$report" || {
  cat "$report"
  fail "rendered Tasuki workloads violate the Tasuki policies"
}

# ---------------------------------------------------------------------------------------------------
step "Trivy misconfiguration scan of the rendered Tasuki workloads"
# The repository-wide Trivy scan cannot render deploy/charts/service (image.repository and image.tag are
# required on purpose), so it never sees the Deployments. Scan what Argo CD will actually apply instead.
trivy config --quiet --severity HIGH,CRITICAL --exit-code 1 "$policy_view"

# ---------------------------------------------------------------------------------------------------
step "shellcheck"
shellcheck deploy/bootstrap/*.sh deploy/scripts/*.sh

printf '\ndeploy-lint: all checks passed\n'
