# Shared by the scripts in this folder and by mk/infra.mk. Source it; don't run it.
# shellcheck shell=bash

# AWS CLI v2 pipes long output through a pager; a script must never wait for a keypress.
export AWS_PAGER=""

# Stops unless AWS_PROFILE is set, so the machine's default AWS identity is never used by accident.
require_aws_profile() {
  if [ -z "${AWS_PROFILE:-}" ]; then
    echo "Set AWS_PROFILE to your personal Tasuki AWS profile first (export AWS_PROFILE=...)." >&2
    exit 1
  fi
}

# Stops unless the credentials in use belong to the Tasuki account. This is the real guard: it holds
# whichever way the credentials were found (profile, environment variables or CI's OIDC role).
require_tasuki_account() {
  if [ -z "${TASUKI_ACCOUNT_ID:-}" ]; then
    echo "Set TASUKI_ACCOUNT_ID to the 12-digit ID of your Tasuki AWS account." >&2
    exit 1
  fi
  local account
  account=$(aws sts get-caller-identity --query Account --output text)
  if [ "$account" != "$TASUKI_ACCOUNT_ID" ]; then
    # IDs are not printed: in CI this line would end up in a public log.
    echo "These AWS credentials are not for TASUKI_ACCOUNT_ID. Stopping before touching anything." >&2
    exit 1
  fi
}

# This machine's public IPv4 address, as AWS sees it.
my_public_ip() {
  curl -fsS --max-time 10 https://checkip.amazonaws.com
}

# The CIDRs that may reach the Kubernetes API, as a Terraform list: API_ALLOWED_CIDRS if it is set,
# otherwise this machine's public IP as a /32.
api_allowed_cidrs() {
  if [ -n "${API_ALLOWED_CIDRS:-}" ]; then
    echo "$API_ALLOWED_CIDRS"
    return 0
  fi
  local ip
  ip=$(my_public_ip) || return 1
  echo "[\"$ip/32\"]"
}
