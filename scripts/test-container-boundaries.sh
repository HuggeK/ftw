#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

if grep -Eq 'COPY optimizer/|--from=optimizer|/opt/venv|FTW_OPTIMIZER_(PYTHON|DIR)' Dockerfile; then
  echo "Dockerfile must contain only core, drivers and web assets; use Dockerfile.optimizer for Python/CVXPY" >&2
  exit 1
fi

# Core and the updater are meant to share one base layer, so a host pulls that
# rootfs once. Comparing the two runtime stages is what actually holds that;
# grepping for a fixed string in one file only ever proved half of it, and did
# so without printing anything when it failed.
core_base=$(grep -E '^FROM ' Dockerfile | tail -1 | awk '{print $2}')
updater_base=$(grep -E '^FROM ' Dockerfile.updater | tail -1 | awk '{print $2}')

case "${core_base}" in
  alpine:*) ;;
  *)
    echo "Dockerfile runtime stage is '${core_base}', expected an alpine: tag" >&2
    exit 1
    ;;
esac

if [ "${core_base}" != "${updater_base}" ]; then
  echo "core and updater runtime bases differ: '${core_base}' vs '${updater_base}'" >&2
  echo "they must be the same tag or the shared base layer silently stops being shared" >&2
  exit 1
fi

# The optimizer is knowingly on a different base: CVXPY publishes no musllinux
# wheels, so it cannot follow the other two onto alpine. Assert that rather than
# leave it looking like an oversight someone should "fix".
if grep -qE '^FROM python:[^ ]*alpine' Dockerfile.optimizer; then
  echo "Dockerfile.optimizer is on an alpine base, which cannot resolve CVXPY" >&2
  echo "cvxpy has no musllinux wheel (newest is 0.4.10); see the note in that file" >&2
  exit 1
fi

grep -q '^COPY optimizer/' Dockerfile.optimizer
grep -q '/out/ftw-backup' Dockerfile
grep -q '/app/ftw-backup' Dockerfile
grep -q -- '--chown=100:101 /out/ftw' Dockerfile
if grep -q 'chown -R 100:101 /app' Dockerfile; then
  echo "Dockerfile must set ownership while copying; a full-tree chown duplicates every app layer" >&2
  exit 1
fi
grep -q '^  ftw-optimizer:' docker-compose.yml
grep -q 'FTW_OPTIMIZER_SOCKET: /run/ftw-optimizer/optimizer.sock' docker-compose.yml

echo "container module boundaries verified"
