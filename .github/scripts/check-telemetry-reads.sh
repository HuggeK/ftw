#!/usr/bin/env bash
# Counts cached telemetry reads per file, locally and in CI.
# Usage: check-telemetry-reads.sh [base-ref]
set -euo pipefail
exec python3 "$(dirname "$0")/check_telemetry_reads.py" "$@"
