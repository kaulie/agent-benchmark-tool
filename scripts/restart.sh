#!/usr/bin/env bash
#
# 重启 agent-benchmark-tool（控制面默认 restartCmd）。
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
bash "${DIR}/stop.sh"
exec bash "${DIR}/start.sh"
