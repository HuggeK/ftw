#!/usr/bin/env bash
# go/protocol is the wire format that FTW Cloud imports. It must build without
# the box, so that a service holding the protocol links no control, planner or
# storage code. Go's internal rule does not stop it: github.com/srcfl/ftw/go/
# protocol sits under the box's path and may import the box's internal
# packages. This check does.
#
# It fails when the module, its tests included, reaches any package of
# github.com/srcfl/ftw/go outside go/protocol, or requires the box module.

set -euo pipefail

dir=${1:-go/protocol}
cd "${dir}"

box_packages=$(go list -deps -test -f '{{.ImportPath}}' ./... | awk '
  $1 ~ /^github\.com\/srcfl\/ftw\/go(\/|$)/ &&
  $1 !~ /^github\.com\/srcfl\/ftw\/go\/protocol(\/|\.test|$)/ { print $1 }
' | sort -u)

box_module=$(go list -m all | awk '$1 == "github.com/srcfl/ftw/go"')

if [ -n "${box_packages}" ] || [ -n "${box_module}" ]; then
  echo "go/protocol must not depend on the box module." >&2
  [ -z "${box_packages}" ] || printf 'It reaches:\n%s\n' "${box_packages}" >&2
  [ -z "${box_module}" ] || printf 'It requires:\n%s\n' "${box_module}" >&2
  exit 1
fi

echo "go/protocol: $(go list -deps ./... | wc -l | tr -d ' ') packages, none from the box"
