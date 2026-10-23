#!/usr/bin/env bash
# Local playground: two kind clusters acting as the primary and DR sites,
# with ArgoCD running in the DR cluster (the management plane must survive
# the loss of the primary region, see README.md).
#
# ArgoCD pulls from a Git URL, so it needs a repository it can reach. Fork
# this repo, point the manifests at the fork and push first:
#
#   make set-repo REPO_URL=https://github.com/<you>/gitops-multicluster-dr.git
#   git commit -am "chore: point ArgoCD at my fork" && git push
#   make kind-up
set -euo pipefail

PRIMARY="${PRIMARY:-gitops-primary}"
DR="${DR:-gitops-dr}"
ARGOCD_VERSION="${ARGOCD_VERSION:-v2.13.3}"
CTX_PRIMARY="kind-${PRIMARY}"
CTX_DR="kind-${DR}"

cd "$(dirname "$0")/.."

need() { command -v "$1" >/dev/null || { echo "missing dependency: $1" >&2; exit 1; }; }
need kind
need kubectl
need docker

log() { printf '\n==> %s\n' "$*"; }

for c in "$PRIMARY" "$DR"; do
  if kind get clusters 2>/dev/null | grep -qx "$c"; then
    log "kind cluster $c already exists"
  else
    log "creating kind cluster $c"
    kind create cluster --name "$c" --wait 120s
  fi
done

log "installing ArgoCD ${ARGOCD_VERSION} into ${CTX_DR}"
kubectl --context "$CTX_DR" create namespace argocd --dry-run=client -o yaml | kubectl --context "$CTX_DR" apply -f -
kubectl --context "$CTX_DR" apply -n argocd --server-side --force-conflicts \
  -f "https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/install.yaml"
kubectl --context "$CTX_DR" -n argocd rollout status deploy/argocd-repo-server --timeout=300s
kubectl --context "$CTX_DR" -n argocd rollout status deploy/argocd-applicationset-controller --timeout=300s
kubectl --context "$CTX_DR" -n argocd rollout status statefulset/argocd-application-controller --timeout=300s

# Labels shared by both clusters; env=local selects apps/*/values-local.yaml.
labels() {
  cat <<YAML
    argocd.argoproj.io/secret-type: cluster
    env: local
    role: $1
    region: $2
    gitops.example.com/workloads: "true"
    addons.example.com/keda: enabled
    addons.example.com/metrics-server: enabled
    addons.example.com/ingress-nginx: enabled
YAML
}

log "registering ${DR} (in-cluster) as role=dr"
kubectl --context "$CTX_DR" apply -f - <<YAML
apiVersion: v1
kind: Secret
metadata:
  name: cluster-local-dr
  namespace: argocd
  labels:
$(labels dr local-b)
stringData:
  name: local-dr
  server: https://kubernetes.default.svc
  config: '{"tlsClientConfig":{"insecure":false}}'
YAML

log "creating a deployer service account in ${PRIMARY}"
kubectl --context "$CTX_PRIMARY" apply -f - <<'YAML'
apiVersion: v1
kind: ServiceAccount
metadata:
  name: argocd-manager
  namespace: kube-system
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: argocd-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: argocd-manager
    namespace: kube-system
---
apiVersion: v1
kind: Secret
metadata:
  name: argocd-manager-token
  namespace: kube-system
  annotations:
    kubernetes.io/service-account.name: argocd-manager
type: kubernetes.io/service-account-token
YAML

token=""
for _ in $(seq 1 30); do
  token="$(kubectl --context "$CTX_PRIMARY" -n kube-system get secret argocd-manager-token -o jsonpath='{.data.token}' 2>/dev/null | base64 -d || true)"
  [[ -n "$token" ]] && break
  sleep 2
done
[[ -n "$token" ]] || { echo "service account token was not issued" >&2; exit 1; }
ca="$(kubectl --context "$CTX_PRIMARY" -n kube-system get secret argocd-manager-token -o jsonpath='{.data.ca\.crt}')"

# kind nodes share the "kind" docker network, so the DR cluster reaches the
# primary API server by container name.
log "registering ${PRIMARY} as role=primary"
kubectl --context "$CTX_DR" apply -f - <<YAML
apiVersion: v1
kind: Secret
metadata:
  name: cluster-local-primary
  namespace: argocd
  labels:
$(labels primary local-a)
stringData:
  name: local-primary
  server: https://${PRIMARY}-control-plane:6443
  config: |
    {"bearerToken": "${token}", "tlsClientConfig": {"insecure": false, "caData": "${ca}"}}
YAML

log "applying the root app"
kubectl --context "$CTX_DR" apply -n argocd -f bootstrap/root-app.yaml

repo="$(grep -m1 'repoURL:' bootstrap/root-app.yaml | awk '{print $2}')"
cat <<MSG

Done. ArgoCD (in ${CTX_DR}) syncs from: ${repo}

  UI:        kubectl --context ${CTX_DR} -n argocd port-forward svc/argocd-server 8080:443
  password:  kubectl --context ${CTX_DR} -n argocd get secret argocd-initial-admin-secret -o jsonpath='{.data.password}' | base64 -d
  apps:      kubectl --context ${CTX_DR} -n argocd get applications

Try a failover (pushes the flag commit so ArgoCD sees it):

  go run ./cmd/drswitch -c dr/drswitch.local.yaml status
  go run ./cmd/drswitch -c dr/drswitch.local.yaml failover --dry-run
  go run ./cmd/drswitch -c dr/drswitch.local.yaml failover --reason "local drill"

MSG
