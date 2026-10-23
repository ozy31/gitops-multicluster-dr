#!/usr/bin/env bash
# Point every ArgoCD manifest at a different Git repository (your fork).
#
#   hack/set-repo.sh https://github.com/<you>/gitops-multicluster-dr.git
set -euo pipefail

new="${1:?usage: $0 <repo-url>}"
cd "$(dirname "$0")/.."

old="$(grep -m1 'repoURL:' bootstrap/root-app.yaml | awk '{print $2}')"
if [[ "$old" == "$new" ]]; then
  echo "already pointing at $new"
  exit 0
fi

files="$(grep -rlF "$old" bootstrap applicationsets)"
for f in $files; do
  # Portable in-place edit (GNU and BSD sed differ on -i).
  tmp="$(mktemp)"
  sed "s|${old}|${new}|g" "$f" >"$tmp" && cat "$tmp" >"$f" && rm -f "$tmp"
  echo "updated $f"
done
