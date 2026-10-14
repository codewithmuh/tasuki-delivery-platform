#!/usr/bin/env bash
# Deletes what Kubernetes controllers created in AWS outside Terraform (load balancers, EBS volumes,
# Karpenter nodes and instance profiles), so `terraform destroy` can delete the VPC and nothing is
# left billing. make aws-down and infra-apply.yml run it right before destroy.
#
# Order matters: the controllers that delete AWS resources (AWS Load Balancer Controller, Karpenter,
# EBS CSI driver) live in kube-system, which is never deleted, so they keep running until the end.
# Safe to run twice. Exits 0 when the cluster does not exist.
set -euo pipefail

# shellcheck source=infra/scripts/common.sh
source "$(dirname "$0")/common.sh"

REGION="${TASUKI_REGION:-ap-northeast-1}"
CLUSTER="${TASUKI_CLUSTER:-tasuki}"

require_tasuki_account

# Every kubectl call names the context, so this can never act on another cluster in ~/.kube/config.
k() {
  kubectl --context "$CLUSTER" "$@"
}

has_crd() {
  k get crd "$1" >/dev/null 2>&1
}

# Runs a command every 15 s until it prints nothing; gives up after 15 minutes.
wait_until_empty() {
  local what=$1
  shift
  local deadline=$((SECONDS + 900))
  local remaining
  while true; do
    remaining=$("$@")
    if [ -z "$remaining" ]; then
      return 0
    fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      echo "Timed out waiting for $what to go away. Still there: $remaining" >&2
      exit 1
    fi
    echo "Waiting for $what to go away..."
    sleep 15
  done
}

# --- 1. Is there a cluster? --------------------------------------------------------------------------
exists=$(aws eks list-clusters --region "$REGION" --query "contains(clusters, '$CLUSTER')" --output text)
if [ "$exists" != "True" ]; then
  echo "No cluster $CLUSTER in $REGION; nothing to clean up."
  exit 0
fi
# Returns at once if the cluster is ACTIVE; waits out an update in progress; fails if it is being deleted.
aws eks wait cluster-active --region "$REGION" --name "$CLUSTER"

# --- 2. Make sure this machine can reach the Kubernetes API -------------------------------------------
# The public endpoint only accepts api_allowed_cidrs. A CI runner, or a laptop whose IP changed since
# aws-up, is not on that list, so swap in this machine's IP. Destroy deletes the cluster right after,
# so the difference from the Terraform config does not matter.
my_cidr="$(my_public_ip)/32"
allowed=$(aws eks describe-cluster --region "$REGION" --name "$CLUSTER" \
  --query "contains(cluster.resourcesVpcConfig.publicAccessCidrs, '$my_cidr')" --output text)
if [ "$allowed" != "True" ]; then
  echo "Allowing this machine's IP to reach the Kubernetes API (takes a few minutes)..."
  update_id=$(aws eks update-cluster-config --region "$REGION" --name "$CLUSTER" \
    --resources-vpc-config "endpointPublicAccess=true,endpointPrivateAccess=true,publicAccessCidrs=$my_cidr" \
    --query update.id --output text)
  while true; do
    update_status=$(aws eks describe-update --region "$REGION" --name "$CLUSTER" --update-id "$update_id" \
      --query update.status --output text)
    case "$update_status" in
      Successful) break ;;
      Failed | Cancelled)
        echo "The endpoint update ended as $update_status." >&2
        exit 1
        ;;
    esac
    sleep 20
  done
fi

aws eks update-kubeconfig --region "$REGION" --name "$CLUSTER" --alias "$CLUSTER"

# --- 3. Stop Argo CD, so it cannot recreate what the next steps delete --------------------------------
if k get namespace argocd >/dev/null 2>&1; then
  echo "Stopping Argo CD..."
  k -n argocd scale deployment,statefulset --all --replicas=0

  # Deleting the argocd namespace deletes its Applications. Their finalizer waits for the (now stopped)
  # controller to delete each app's resources, which would hang forever. Steps 4-6 delete those
  # resources directly, so drop the finalizers.
  for kind in applications.argoproj.io applicationsets.argoproj.io appprojects.argoproj.io; do
    if has_crd "$kind"; then
      names=$(k -n argocd get "$kind" -o jsonpath='{.items[*].metadata.name}')
      for name in $names; do
        k -n argocd patch "$kind" "$name" --type merge -p '{"metadata":{"finalizers":null}}'
      done
    fi
  done
fi

# --- 4. Load balancers ---------------------------------------------------------------------------------
# A finalizer keeps each object until its controller has deleted the AWS load balancer, so these
# commands return only when the ALBs and NLBs are gone.
echo "Deleting Ingresses (ALBs)..."
k delete ingress --all --all-namespaces --timeout=10m

echo "Deleting Services of type LoadBalancer (NLBs)..."
lb_services=$(k get services --all-namespaces \
  -o jsonpath='{range .items[?(@.spec.type=="LoadBalancer")]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}')
while read -r namespace name; do
  if [ -n "$name" ]; then
    k -n "$namespace" delete service "$name" --timeout=10m
  fi
done <<<"$lb_services"

if has_crd gateways.gateway.networking.k8s.io; then
  echo "Deleting Gateway API Gateways (load balancers)..."
  k delete gateways.gateway.networking.k8s.io --all --all-namespaces --timeout=10m
fi

# --- 5. Every namespace except the system ones -----------------------------------------------------------
# kube-system stays: the controllers that clean up AWS resources run there.
all_namespaces=$(k get namespaces -o jsonpath='{.items[*].metadata.name}')
app_namespaces=""
for namespace in $all_namespaces; do
  case "$namespace" in
    kube-system | kube-public | kube-node-lease | default) ;;
    *) app_namespaces="$app_namespaces $namespace" ;;
  esac
done
if [ -n "$app_namespaces" ]; then
  echo "Deleting namespaces:$app_namespaces"
  # shellcheck disable=SC2086 # one argument per namespace; namespace names never contain spaces
  k delete namespace $app_namespaces --timeout=15m
fi

# --- 6. Volumes ------------------------------------------------------------------------------------------
echo "Deleting PersistentVolumeClaims left in the default namespace..."
k -n default delete pvc --all --timeout=10m

# A PersistentVolume with reclaim policy Delete disappears only after the EBS CSI driver has deleted its
# EBS volume. Retained volumes are kept on purpose; check-leftovers.sh lists them afterwards.
wait_until_empty "EBS-backed PersistentVolumes" \
  k get pv -o jsonpath='{.items[?(@.spec.persistentVolumeReclaimPolicy=="Delete")].metadata.name}'

# --- 7. Karpenter nodes ----------------------------------------------------------------------------------
if has_crd nodepools.karpenter.sh; then
  echo "Deleting Karpenter NodePools; Karpenter drains and terminates their nodes..."
  k delete nodepools.karpenter.sh --all --timeout=10m
  wait_until_empty "Karpenter nodes" k get nodes -l karpenter.sh/nodepool -o name
fi

# Karpenter creates an instance profile per EC2NodeClass and deletes it with the class. Left behind,
# it would stop Terraform from deleting the Karpenter node IAM role.
if has_crd ec2nodeclasses.karpenter.k8s.aws; then
  echo "Deleting EC2NodeClasses (and their instance profiles)..."
  k delete ec2nodeclasses.karpenter.k8s.aws --all --timeout=10m
fi

echo "Pre-destroy cleanup done. Terraform can now destroy the platform."
