#!/usr/bin/env bash
# GD-07: break one key of tasuki/<env>/app in AWS Secrets Manager, force External Secrets to sync it, and
# (optionally) restart one Deployment so it picks the broken secret up. Values are never printed or logged.
# shellcheck source-path=SCRIPTDIR
set -euo pipefail
# shellcheck source=lib/common.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib/common.sh"

usage() {
  cat <<EOF
Usage: $GD_NAME --context CTX --namespace NS --key KEY --mode remove-key|wrong-value [--restart DEPLOYMENT]
       $GD_NAME --context CTX --namespace NS --rollback [--restart DEPLOYMENT]

  remove-key    write a new version of tasuki/<env>/app without KEY
  wrong-value   write a new version where KEY has a fresh random value (openssl rand -hex 32)
Then forces the ExternalSecret 'tasuki-app' to sync, reports its Ready condition and whether the Kubernetes Secret
still has KEY (the ExternalSecret lists every key, so a missing key fails the sync and the last good Secret stays),
and, with --restart, restarts that workload (a Deployment, or in tasuki-prod an Argo Rollout through the
'kubectl argo rollouts' plugin) and shows its pods. The secret name comes from the namespace (tasuki-dev -> tasuki/dev/app).
Keys and their readers (deploy/README.md): FLEET_STREAM_TOKEN (tracking only: the smallest blast radius),
COURIER_TOKEN_SECRET (orders, fleet-ingest, dispatch), TRACKING_TOKEN_SECRET (orders, tracking), ADMIN_TOKEN
(orders, simulator), DJANGO_SECRET_KEY and ORDERS_FIELD_KEY (orders).

Needs the aws CLI, AWS_PROFILE and TASUKI_ACCOUNT_ID (the same account guard as 'make aws-*'). Values only
pass through memory and a mode-600 temporary file that is deleted on exit. The version IDs are saved in the
script's state directory and written to GAMEDAY_LOG.

Undo: --rollback moves AWSCURRENT back to the saved version, forces a sync and, with --restart, restarts the
Deployment again. By hand: aws secretsmanager update-secret-version-stage --secret-id tasuki/<env>/app
  --version-stage AWSCURRENT --move-to-version-id <saved> --remove-from-version-id <bad>

$GD_COMMON_USAGE
EOF
}

REGION="ap-northeast-1"
key=""
mode=""
restart=""
rollback=0
while [ $# -gt 0 ]; do
  gd_common_flag "$@"
  if [ "$GD_SHIFT" -gt 0 ]; then
    shift "$GD_SHIFT"
    continue
  fi
  case "$1" in
    --key)
      gd_need_value "$1" "${2:-}"
      key="$2"
      shift 2
      ;;
    --mode)
      gd_need_value "$1" "${2:-}"
      mode="$2"
      shift 2
      ;;
    --restart)
      gd_need_value "$1" "${2:-}"
      restart="$2"
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
if [ "$rollback" = 0 ]; then
  case "$key" in
    "") gd_usage_error "--key is required" ;;
    *[!A-Z0-9_]*) gd_usage_error "--key: uppercase letters, digits and '_' only" ;;
  esac
  case "$mode" in remove-key | wrong-value) ;; *) gd_usage_error "--mode is required: remove-key or wrong-value" ;; esac
fi
case "$restart" in *[!a-z0-9-]*) gd_usage_error "--restart: a Deployment name" ;; esac

gd_require_target
gd_require_tools aws jq openssl
secret_id="tasuki/$GD_ENV/app"
state_file="$(gd_state_dir)/broken-secret"

# shellcheck source=../../infra/scripts/common.sh
source "$GD_REPO_ROOT/infra/scripts/common.sh"

current_version() {
  aws secretsmanager describe-secret --region "$REGION" --secret-id "$secret_id" --output json |
    jq -r '.VersionIdsToStages | to_entries[] | select(.value | index("AWSCURRENT")) | .key'
}
force_sync() {
  k annotate externalsecret tasuki-app "force-sync=$(date +%s)" --overwrite >/dev/null
  sleep 5
  k get externalsecret tasuki-app -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}{"\n"}' >&2 || true
}
# Key names only; never values.
secret_has_key() { k get secret tasuki-app -o json | jq -e --arg k "$1" '.data | has($k)' >/dev/null; }
es_ready() { k get externalsecret tasuki-app -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}/{.status.conditions[?(@.type=="Ready")].reason}' 2>/dev/null || echo "?"; }
restart_and_show() {
  [ -n "$restart" ] || return 0
  if k get rollouts.argoproj.io "$restart" >/dev/null 2>&1; then
    kubectl argo rollouts version >/dev/null 2>&1 || gd_die "$restart is an Argo Rollout: install the kubectl argo rollouts plugin"
    gd_record "$1" "restart rollout/$restart (pods cycle; no new revision, so no canary steps)"
    kc argo rollouts restart "$restart" --namespace "$GD_NAMESPACE"
    sleep 10
    if kc argo rollouts status "$restart" --namespace "$GD_NAMESPACE" --timeout 180s; then
      gd_record observe "rollout/$restart healthy after the restart"
    else
      gd_record observe "rollout/$restart NOT healthy within 180s of the restart"
    fi
  else
    gd_record "$1" "rollout restart deployment/$restart"
    k rollout restart "deployment/$restart"
    if k rollout status "deployment/$restart" --timeout=180s; then
      gd_record observe "deployment/$restart rollout completed"
    else
      gd_record observe "deployment/$restart rollout did NOT complete within 180s"
    fi
  fi
  k get pods -l "app.kubernetes.io/name=$restart" -o wide
}

if [ "$rollback" = 1 ]; then
  [ -f "$state_file" ] || gd_die "no saved state in $state_file; restore by hand (see --help)"
  saved="$(sed -n 's/^saved=//p' "$state_file")"
  bad="$(sed -n 's/^bad=//p' "$state_file")"
  [ -n "$saved" ] && [ -n "$bad" ] || gd_die "state file $state_file is incomplete"
  gd_plan "make version $saved of $secret_id AWSCURRENT again (instead of $bad)" "force the ExternalSecret to sync" \
    "${restart:+restart deployment/$restart}"
  gd_confirm
  require_aws_profile
  require_tasuki_account
  aws secretsmanager update-secret-version-stage --region "$REGION" --secret-id "$secret_id" \
    --version-stage AWSCURRENT --move-to-version-id "$saved" --remove-from-version-id "$bad" >/dev/null
  gd_record rollback "$secret_id AWSCURRENT is version $saved again"
  force_sync
  gd_record observe "ExternalSecret tasuki-app Ready=$(es_ready)"
  restart_and_show rollback
  rm -f "$state_file"
  exit 0
fi

case "$key" in
  FLEET_STREAM_TOKEN) readers="tracking" ;;
  COURIER_TOKEN_SECRET) readers="orders (3 processes), fleet-ingest, dispatch" ;;
  TRACKING_TOKEN_SECRET) readers="orders (3 processes), tracking" ;;
  ADMIN_TOKEN) readers="orders, simulator" ;;
  DJANGO_SECRET_KEY | ORDERS_FIELD_KEY) readers="orders (3 processes)" ;;
  *) readers="unknown (not in deploy/README.md)" ;;
esac
[ ! -f "$state_file" ] || gd_die "a previous run was not rolled back ($state_file); run --rollback first"
case "$mode" in
  remove-key) change="remove $key" ;;
  wrong-value) change="replace $key with a random value" ;;
esac
gd_plan "write a new version of $secret_id: $change" \
  "force ExternalSecret tasuki-app to sync it into Secret tasuki-app" \
  "readers of $key: $readers. remove-key: the sync should fail and keep the last good Secret; wrong-value: every reader picks the bad value up on its next start" \
  "${restart:+then restart deployment/$restart}"
gd_confirm
require_aws_profile
require_tasuki_account

saved="$(current_version)"
[ -n "$saved" ] || gd_die "could not read the AWSCURRENT version of $secret_id"

umask 077
tmp="$(mktemp -d)"
cleanup_tmp() { rm -rf "$tmp"; }
gd_on_exit cleanup_tmp
aws secretsmanager get-secret-value --region "$REGION" --secret-id "$secret_id" \
  --query SecretString --output text >"$tmp/current.json"
jq -e --arg k "$key" 'type == "object" and has($k)' "$tmp/current.json" >/dev/null ||
  gd_die "$secret_id has no key $key (or is not a JSON object)"
case "$mode" in
  remove-key) jq -c --arg k "$key" 'del(.[$k])' "$tmp/current.json" >"$tmp/new.json" ;;
  wrong-value) jq -c --arg k "$key" --arg v "$(openssl rand -hex 32)" '.[$k] = $v' "$tmp/current.json" >"$tmp/new.json" ;;
esac
bad="$(aws secretsmanager put-secret-value --region "$REGION" --secret-id "$secret_id" \
  --secret-string "file://$tmp/new.json" --query VersionId --output text)"
cleanup_tmp
printf 'saved=%s\nbad=%s\n' "$saved" "$bad" >"$state_file"
gd_record inject "$secret_id: $change (new version $bad; previous version $saved saved for --rollback)"

force_sync
# Explicit keys: a removed key should fail the sync and leave the last good Secret in place.
if [ "$mode" = "remove-key" ]; then
  es_failed() { case "$(es_ready)" in False/*) return 0 ;; *) return 1 ;; esac }
  gd_wait_until 60 5 es_failed || gd_record observe "ExternalSecret still Ready 60s after the sync (expected SecretSyncedError)"
fi
if secret_has_key "$key"; then has="still has"; else has="NO LONGER has"; fi
gd_record observe "ExternalSecret tasuki-app Ready=$(es_ready); Secret tasuki-app $has $key"
restart_and_show inject
