#!/usr/bin/env bash
# Render every app for every deploy target the way the apps ApplicationSet
# does, for each possible value of dr.activeSite. Output is a multi-document
# YAML stream on stdout, suitable for piping into kubeconform.
#
#   hack/render.sh                 # all apps, all targets, all flag values
#   hack/render.sh orders-api      # one app
set -euo pipefail

cd "$(dirname "$0")/.."

CHART=charts/service
# env:role pairs, mirroring the cluster secret labels in bootstrap/clusters.
TARGETS=("local:primary" "local:dr" "dev:primary" "stage:primary" "prod:primary" "prod:dr")
FLAGS=("primary" "dr" "both")

apps=("$@")
if [[ ${#apps[@]} -eq 0 ]]; then
  for d in apps/*/; do apps+=("$(basename "$d")"); done
fi

for app in "${apps[@]}"; do
  for target in "${TARGETS[@]}"; do
    env="${target%%:*}"
    role="${target##*:}"
    for flag in "${FLAGS[@]}"; do
      # Same value file order as applicationsets/apps.yaml; missing files are
      # skipped there via ignoreMissingValueFiles.
      args=()
      for f in "apps/$app/values.yaml" "apps/$app/values-$env.yaml" "apps/$app/values-$env-$role.yaml"; do
        [[ -f "$f" ]] && args+=(-f "$f")
      done
      echo "# ---- $app $env/$role activeSite=$flag" >&2
      helm template "$app" "$CHART" \
        --namespace "$app" \
        "${args[@]}" \
        -f dr/active-site.yaml \
        --set "site.env=$env" \
        --set "site.role=$role" \
        --set "site.region=example-1" \
        --set "dr.activeSite=$flag"
    done
  done
done
