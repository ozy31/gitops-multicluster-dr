#!/usr/bin/env bash
# Delete the kind clusters created by hack/kind-up.sh.
set -euo pipefail
for c in "${PRIMARY:-gitops-primary}" "${DR:-gitops-dr}"; do
  kind delete cluster --name "$c"
done
