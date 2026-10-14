#!/usr/bin/env bash
# Lists anything still billing in the Tasuki region and exits 1 if there is something. make aws-down
# and infra-apply.yml run it after `terraform destroy`.
#
# It assumes a dedicated AWS account: everything it finds counts, whether Terraform, a Kubernetes
# controller or a person created it. The state bucket, KMS keys pending deletion and the budget cost
# nothing or close to it, so they are not checked.
set -euo pipefail

# shellcheck source=infra/scripts/common.sh
source "$(dirname "$0")/common.sh"

REGION="${TASUKI_REGION:-ap-northeast-1}"

require_tasuki_account

leftovers=""

# add <kind> <rows>: one table line per row of `aws ... --output text` (fields are tab-separated).
add() {
  local kind=$1 rows=$2 row
  if [ -z "$rows" ]; then
    return 0
  fi
  while IFS= read -r row; do
    leftovers+=$(printf '%-24s %s' "$kind" "${row//$'\t'/  }")$'\n'
  done <<<"$rows"
}

echo "Checking $REGION for billable leftovers..."

# Each query is assigned to a variable first, so a failed call stops the script (set -e) instead of
# looking like "nothing found".
clusters=$(aws eks list-clusters --region "$REGION" --query 'clusters[]' --output text | tr '\t' '\n')
add "EKS cluster" "$clusters"

instances=$(aws ec2 describe-instances --region "$REGION" \
  --filters "Name=instance-state-name,Values=pending,running,stopping,stopped" \
  --query 'Reservations[].Instances[].[InstanceId,InstanceType,State.Name]' --output text)
add "EC2 instance" "$instances"

# describe-nat-gateways spells it --filter, unlike the other EC2 calls.
nat_gateways=$(aws ec2 describe-nat-gateways --region "$REGION" \
  --filter "Name=state,Values=pending,available" \
  --query 'NatGateways[].[NatGatewayId,VpcId,State]' --output text)
add "NAT gateway" "$nat_gateways"

volumes=$(aws ec2 describe-volumes --region "$REGION" \
  --filters "Name=status,Values=available" \
  --query 'Volumes[].[VolumeId,VolumeType,Size]' --output text)
add "EBS volume (unattached)" "$volumes"

load_balancers=$(aws elbv2 describe-load-balancers --region "$REGION" \
  --query 'LoadBalancers[].[LoadBalancerName,Type,State.Code]' --output text)
add "Load balancer" "$load_balancers"

classic_load_balancers=$(aws elb describe-load-balancers --region "$REGION" \
  --query 'LoadBalancerDescriptions[].[LoadBalancerName]' --output text)
add "Classic load balancer" "$classic_load_balancers"

# shellcheck disable=SC2016 # the backticks are a JMESPath literal, not shell
addresses=$(aws ec2 describe-addresses --region "$REGION" \
  --query 'Addresses[?AssociationId==`null`].[PublicIp,AllocationId]' --output text)
add "Elastic IP (unattached)" "$addresses"

if [ -z "$leftovers" ]; then
  echo "Nothing billable left in $REGION."
  exit 0
fi

echo
printf '%-24s %s\n' "RESOURCE" "DETAILS"
printf '%s' "$leftovers"
echo
echo "Billable resources are still running in $REGION. Run make aws-down again or delete them by hand." >&2
exit 1
