#!/usr/bin/env bash
# Hands a fresh EKS cluster (infra/platform) over to git:
#   1. reads the account-specific values from `terraform output`,
#   2. installs Argo CD with Helm (the only thing ever installed by hand),
#   3. registers the cluster with Argo CD, carrying those values as annotations (the "GitOps bridge"),
#   4. applies the root Application; Argo CD installs everything else from deploy/platform/.
# Safe to re-run: every step is an upgrade or an apply.
#
# Usage: deploy/bootstrap/bootstrap.sh     (or `make gitops-bootstrap`)
# Needs: kubectl (context pointing at the cluster), helm, terraform (state readable), jq.
set -euo pipefail

# Keep equal to targetRevision in deploy/platform/argocd.yaml; `make deploy-lint` fails if they differ.
ARGOCD_CHART_VERSION="10.10.3"
ARGOCD_NAMESPACE="argocd"

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
tf_dir="${TF_DIR:-${repo_root}/infra/platform}"

die() {
  echo "bootstrap: $*" >&2
  exit 1
}

for tool in kubectl helm terraform jq; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

# --- 1. Values from Terraform ------------------------------------------------------------------
echo "Reading outputs from ${tf_dir}"
outputs="$(terraform -chdir="$tf_dir" output -json)"
tf_output() {
  jq -er --arg key "$1" '.[$key].value' <<<"$outputs" || die "terraform output '$1' is missing (has infra/platform been applied?)"
}

cluster_name="$(tf_output cluster_name)"
cluster_endpoint="$(tf_output cluster_endpoint)"
region="$(tf_output region)"
vpc_id="$(tf_output vpc_id)"
karpenter_queue_name="$(tf_output karpenter_queue_name)"
# PostgreSQL backups: the bucket (infra/bootstrap, outlives the platform) and this platform's session prefix.
backup_bucket="$(tf_output backup_bucket)"
backup_session="$(tf_output backup_session)"
account_id="$(jq -r '.account_id.value // empty' <<<"$outputs")"
if [[ -z "$account_id" ]]; then
  command -v aws >/dev/null 2>&1 || die "terraform output 'account_id' is missing and the aws CLI is not installed"
  account_id="$(aws sts get-caller-identity --query Account --output text)"
fi

# These values end up in YAML and in Helm values: accept only what they can legitimately look like.
[[ "$cluster_name" =~ ^[A-Za-z0-9][A-Za-z0-9-]{0,99}$ ]] || die "unexpected cluster_name: $cluster_name"
[[ "$region" =~ ^[a-z]{2}(-[a-z]+)+-[0-9]$ ]] || die "unexpected region: $region"
[[ "$vpc_id" =~ ^vpc-[0-9a-f]{8,17}$ ]] || die "unexpected vpc_id: $vpc_id"
[[ "$account_id" =~ ^[0-9]{12}$ ]] || die "unexpected account_id: $account_id"
[[ "$karpenter_queue_name" =~ ^[A-Za-z0-9_-]{1,80}$ ]] || die "unexpected karpenter_queue_name: $karpenter_queue_name"
[[ "$backup_bucket" =~ ^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$ ]] || die "unexpected backup_bucket: $backup_bucket"
[[ "$backup_session" =~ ^[0-9]{8}T[0-9]{4}Z$ ]] || die "unexpected backup_session: $backup_session"

# Refuse to install into whatever cluster the current context happens to point at.
current_server="$(kubectl config view --minify --output 'jsonpath={.clusters[0].cluster.server}')"
if [[ "$current_server" != "$cluster_endpoint" ]]; then
  die "kubectl context '$(kubectl config current-context)' points at ${current_server}, not ${cluster_endpoint}.
Run: $(tf_output kubeconfig_command)"
fi
echo "Cluster ${cluster_name} (${region}), account ${account_id}, ${vpc_id}"

# --- 2. Argo CD --------------------------------------------------------------------------------
# Release name "argocd" matches the argocd Application, which adopts this install on its first sync.
echo "Installing Argo CD (chart ${ARGOCD_CHART_VERSION})"
helm upgrade --install argocd argo-cd \
  --repo https://argoproj.github.io/argo-helm \
  --version "$ARGOCD_CHART_VERSION" \
  --namespace "$ARGOCD_NAMESPACE" --create-namespace \
  --values "${repo_root}/deploy/bootstrap/argocd-values.yaml" \
  --wait --timeout 10m

# --- 3. The GitOps bridge: register this cluster, with Terraform's values as annotations ----------
# ApplicationSets with a cluster generator (Karpenter, AWS Load Balancer Controller, orders-db backups) select
# clusters labelled tasuki.io/platform=true and template these annotations into Helm values.
echo "Registering the cluster with Argo CD"
kubectl apply --filename - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: in-cluster
  namespace: ${ARGOCD_NAMESPACE}
  labels:
    argocd.argoproj.io/secret-type: cluster
    tasuki.io/platform: "true"
  annotations:
    cluster_name: "${cluster_name}"
    region: "${region}"
    vpc_id: "${vpc_id}"
    aws_account_id: "${account_id}"
    karpenter_queue_name: "${karpenter_queue_name}"
    backup_bucket: "${backup_bucket}"
    backup_session: "${backup_session}"
type: Opaque
stringData:
  name: in-cluster
  server: https://kubernetes.default.svc
  # No credentials: for this address Argo CD uses its own service account.
  config: '{"tlsClientConfig":{"insecure":false}}'
EOF

# --- 4. Root app: from here on, git is the source of truth -------------------------------------
kubectl apply --filename "${repo_root}/deploy/bootstrap/root.yaml"

cat <<EOF

Argo CD is installed and syncing deploy/platform/ (about 15 minutes for the whole platform).
  UI:       kubectl -n ${ARGOCD_NAMESPACE} port-forward svc/argocd-server 8080:443   -> https://localhost:8080
  Login:    admin / \$(kubectl -n ${ARGOCD_NAMESPACE} get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d)
  Progress: kubectl -n ${ARGOCD_NAMESPACE} get applications
EOF
