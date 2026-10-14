#!/usr/bin/env bash
# GD-08 Phase A: copy the orders database into a new CloudNativePG cluster (logical import) and time it.
# Phase B (physical backups, point-in-time recovery) needs backups in deploy/, which do not exist yet.
# shellcheck source-path=SCRIPTDIR
set -euo pipefail
# shellcheck source=lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

usage() {
  cat <<EOF
Usage: $GD_NAME --context CTX --namespace NS [--source NAME] [--timeout SECONDS]
       $GD_NAME --context CTX --namespace NS --rollback

GD-08 Phase A. Creates the CloudNativePG cluster orders-db-restore (always this name: its ServiceAccount has the
read-only backup role, and deploy/apps/base/network-policies.yaml admits the operator and Prometheus to it) whose
initdb imports database 'orders' from --source (default orders-db) with pg_dump/pg_restore
(bootstrap.initdb.import, type microservice), plus one temporary NetworkPolicy letting it read the source on 5432.
Waits until the copy is healthy and prints start, end and elapsed time (the RTO of a logical restore; also into
GAMEDAY_LOG). Both objects are labelled tasuki.io/gameday=true; Argo CD does not track them. Compare the two
clusters with restore-check.sh. A Phase B restore (from S3) uses the same name: never run both at once. The
script refuses if an orders-db-restore without the game-day label exists (a real restore).

Undo: --rollback deletes the copy (its volume goes with it) and the NetworkPolicy, only if they carry the label.

$GD_COMMON_USAGE
EOF
}

source_cluster="orders-db"
target="orders-db-restore"
rollback=0
timeout=1800
while [ $# -gt 0 ]; do
  gd_common_flag "$@"
  if [ "$GD_SHIFT" -gt 0 ]; then
    shift "$GD_SHIFT"
    continue
  fi
  case "$1" in
    --source)
      gd_need_value "$1" "${2:-}"
      source_cluster="$2"
      shift 2
      ;;
    --timeout)
      gd_need_value "$1" "${2:-}"
      timeout="$2"
      shift 2
      ;;
    --rollback)
      rollback=1
      shift
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *) gd_usage_error "unknown argument: $1" ;;
  esac
done
case "$source_cluster" in *[!a-z0-9-]*) gd_usage_error "cluster names: lowercase letters, digits and '-' only" ;; esac
[ "$source_cluster" != "$target" ] || gd_usage_error "--target must differ from --source"
case "$timeout" in '' | *[!0-9]*) gd_usage_error "--timeout must be a whole number of seconds" ;; esac

gd_require_target
gd_require_tools jq
selector="tasuki.io/gameday=true,tasuki.io/gameday-restore=$target"

if [ "$rollback" = 1 ]; then
  gd_plan "delete cluster $target and the NetworkPolicy labelled $selector (only game-day copies)"
  gd_confirm
  k delete clusters.postgresql.cnpg.io -l "$selector" --ignore-not-found
  k delete networkpolicies -l "$selector" --ignore-not-found
  gd_record rollback "deleted restore cluster $target and its temporary NetworkPolicy"
  exit 0
fi

src="$(k get clusters.postgresql.cnpg.io "$source_cluster" -o json)" || gd_die "cluster $source_cluster not found"
image="$(printf '%s' "$src" | jq -r '.spec.imageName // empty')"
size="$(printf '%s' "$src" | jq -r '.spec.storage.size // "5Gi"')"
class="$(printf '%s' "$src" | jq -r '.spec.storage.storageClass // "gp3"')"
[ -n "$image" ] || gd_die "cannot read spec.imageName of $source_cluster"
if k get clusters.postgresql.cnpg.io "$target" >/dev/null 2>&1; then
  if [ "$(k get clusters.postgresql.cnpg.io "$target" -o jsonpath='{.metadata.labels.tasuki\.io/gameday}')" = "true" ]; then
    gd_die "a game-day $target already exists (Phase A or B); delete it first (--rollback, or kubectl delete cluster $target)"
  fi
  gd_die "$target exists WITHOUT the game-day label: probably a real restore. Do not touch it."
fi

manifest="$(
  cat <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: gameday-$target-reads-source
  labels: {tasuki.io/gameday: "true", tasuki.io/gameday-restore: "$target"}
spec:
  podSelector:
    matchLabels: {cnpg.io/cluster: "$source_cluster"}
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector:
            matchLabels: {cnpg.io/cluster: "$target"}
      ports: [{port: 5432}]
---
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: $target
  labels: {tasuki.io/gameday: "true", tasuki.io/gameday-restore: "$target"}
spec:
  instances: 1
  imageName: $image
  bootstrap:
    initdb:
      database: orders
      owner: orders
      import:
        type: microservice
        databases: [orders]
        source:
          externalCluster: source
  externalClusters:
    - name: source
      connectionParameters:
        host: $source_cluster-rw
        user: orders
        dbname: orders
        sslmode: require
      password:
        name: $source_cluster-app
        key: password
  storage:
    storageClass: $class
    size: $size
  resources:
    requests: {cpu: 100m, memory: 256Mi}
    limits: {memory: 512Mi}
EOF
)"

gd_plan "create the temporary NetworkPolicy gameday-$target-reads-source ($target may read $source_cluster:5432)" \
  "create cluster $target ($size $class, image $image) importing database orders from $source_cluster" \
  "wait up to ${timeout}s for it to be healthy, and time it"
gd_confirm

start="$(date +%s)"
gd_record inject "restore started: $target imports orders from $source_cluster (logical, pg_dump at about this time)"
printf '%s\n' "$manifest" | k apply -f -

healthy() {
  [ "$(k get clusters.postgresql.cnpg.io "$target" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Cluster in healthy state" ]
}
if gd_wait_until "$timeout" 10 healthy; then
  gd_record observe "restore cluster $target healthy $(($(date +%s) - start))s after the start (logical-restore RTO)"
else
  gd_record observe "restore cluster $target NOT healthy after ${timeout}s"
  gd_die "check: kubectl -n $GD_NAMESPACE get pods -l cnpg.io/cluster=$target; logs of the import job"
fi
gd_say "next: restore-check.sh on $source_cluster and $target, then --compare; finally $GD_NAME --rollback"
