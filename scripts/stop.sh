#!/usr/bin/env bash
#
# 停止 agent-benchmark-tool（TERM → 等待 → KILL）。
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUNTIME_DIR="${RUNTIME_DIR:-$(cd "${DIR}/.." && pwd)}"
PID_FILE="${RUNTIME_DIR}/backend/runtime.pid"

log() { echo "[stop] $*"; }

if [ ! -f "${PID_FILE}" ]; then
  log "没有 pid 文件，视为未运行"
  exit 0
fi

pid="$(tr -d '[:space:]' < "${PID_FILE}" || true)"
if [ -z "${pid}" ] || ! kill -0 "${pid}" 2>/dev/null; then
  log "进程 ${pid:-?} 不存在，清理 pid 文件"
  rm -f "${PID_FILE}"
  exit 0
fi

log "TERM → pid=${pid}"
kill "${pid}" 2>/dev/null || true
for _ in $(seq 1 30); do
  if ! kill -0 "${pid}" 2>/dev/null; then
    rm -f "${PID_FILE}"
    log "已停止"
    exit 0
  fi
  sleep 0.5
done

log "未在 15s 内退出，KILL → pid=${pid}"
kill -9 "${pid}" 2>/dev/null || true
rm -f "${PID_FILE}"
log "已强制停止"
