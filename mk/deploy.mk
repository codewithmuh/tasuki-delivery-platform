# GitOps (deploy/): static checks of everything Argo CD applies, and the one-time Argo CD bootstrap.
# Included by the root Makefile, which sets SHELL to bash with -eu -o pipefail.
# Tools for deploy-lint (versions pinned in .github/workflows/deploy.yml): helm, kubectl or kustomize,
# yq, jq, kubeconform, kyverno, trivy, shellcheck.

.PHONY: deploy-lint gitops-bootstrap

deploy-lint: ## GitOps: render all charts and overlays, kubeconform, Kyverno tests, shellcheck (as in CI)
	deploy/scripts/lint.sh

gitops-bootstrap: ## GitOps: install Argo CD on the cluster and hand it deploy/platform/ (after aws-up, kubeconfig)
	deploy/bootstrap/bootstrap.sh
