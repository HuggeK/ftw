#!/usr/bin/env bash
# go/internal/cloudlink streams the box's state to FTW Cloud, and FTW Cloud
# reads and never controls. This check keeps the uplink's build graph honest:
# it may import go/protocol and nothing else of the box's own, so no control,
# API, loadpoint, planner or driver code can be reached from the code that
# talks to the service. Its tests are held to the same rule.

set -euo pipefail

cd "${1:-go}"

reached=$(go list -deps -test -f '{{.ImportPath}}' ./internal/cloudlink | awk '
  $1 ~ /^github\.com\/srcfl\/ftw\/go\// &&
  $1 !~ /^github\.com\/srcfl\/ftw\/go\/protocol(\/|$)/ &&
  $1 !~ /^github\.com\/srcfl\/ftw\/go\/internal\/cloudlink(\.test| |$)/ { print $1 }
' | sort -u)

if [ -n "${reached}" ]; then
  echo "go/internal/cloudlink may import only go/protocol from the box. It reaches:" >&2
  printf '%s\n' "${reached}" >&2
  exit 1
fi

echo "go/internal/cloudlink: $(go list -deps ./internal/cloudlink | wc -l | tr -d ' ') packages, none from the box but go/protocol"
