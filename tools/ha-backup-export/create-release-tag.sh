#!/usr/bin/env bash
set -euo pipefail

tag=${1:?tool tag required}
commit=${2:?exact commit required}
[[ "$tag" =~ ^ha-export-v[1-9][0-9]*-beta\.[1-9][0-9]*$ ]]
[[ "$commit" =~ ^[0-9a-f]{40}$ ]]
existing=$(git ls-remote --tags origin "refs/tags/$tag")
if [ -n "$existing" ]; then
  echo 'STOP: tool tag already exists; choose a new beta tag.' >&2
  exit 1
fi
# A non-forced push also refuses a different tag created after the check.
git push origin "$commit:refs/tags/$tag"
