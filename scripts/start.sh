#!/usr/bin/env bash
#
# 启动 agent-benchmark-tool —— 遵循「agent-control-plane-deployment」部署系统规范。
#
# 由控制面以 restartCmd 调用：cwd = runtimeDir，且注入
#   PORT        = 服务契约 healthUrl 里的端口（本脚本据此绑定监听地址）
#   RUNTIME_DIR = runtimeDir
#   APP_VERSION = 本次部署的 8 位短 hash
#
# runtime 布局（backend/ 下的内容由平台在部署时保留，不会被 --delete 清掉）：
#   bin/benchmarkd          可执行文件（来自发版包）
#   scripts/*.sh            本目录（来自发版包）
#   backend/.env            可选覆盖项（首次启动自动生成，权限 600）
#   backend/runtime.pid     进程号
#   backend/server.log      标准输出/错误
#
# 本服务**只读**读取 agent runtime 的 reason_turns 库，自身不写任何数据；
# 因此 backend/data 里没有库文件，数据源路径由 backend/.env 的 AUTONOMY_DB 指定。
set -euo pipefail

DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RUNTIME_DIR="${RUNTIME_DIR:-$(cd "${DIR}/.." && pwd)}"
PORT="${PORT:-4231}"
APP_VERSION="${APP_VERSION:-dev}"

BIN="${RUNTIME_DIR}/bin/benchmarkd"
BACKEND="${RUNTIME_DIR}/backend"
ENV_FILE="${BACKEND}/.env"
PID_FILE="${BACKEND}/runtime.pid"
LOG_FILE="${BACKEND}/server.log"

log() { echo "[start] $*"; }
die() { echo "[start][错误] $*" >&2; exit 1; }

[ -x "${BIN}" ] || die "缺少可执行文件 ${BIN}（发版包内容不完整？）"

mkdir -p "${BACKEND}"

# 首次启动生成 backend/.env：只放可覆盖项（数据源路径），权限 600，绝不入 git。
# 监听端口/地址由本脚本按平台注入的 PORT 推导，避免契约换端口后 .env 里的
# 旧值把服务卡在旧端口上。
if [ ! -f "${ENV_FILE}" ]; then
  umask 077
  cat > "${ENV_FILE}" <<EOF
# agent-benchmark-tool 运行期配置（首次启动自动生成，权限 600，请勿提交到 git）
# 被评测的数据源：agent runtime（autonomy）的 SQLite 库，只读打开。
AUTONOMY_DB=${HOME}/Projects/autonomy/data/autonomy.db
EOF
  chmod 600 "${ENV_FILE}"
  log "已生成 ${ENV_FILE}"
fi

# shellcheck disable=SC1090
set -a; . "${ENV_FILE}"; set +a

AUTONOMY_DB="${AUTONOMY_DB:-${HOME}/Projects/autonomy/data/autonomy.db}"
[ -f "${AUTONOMY_DB}" ] || die "数据源不存在: ${AUTONOMY_DB}（改 ${ENV_FILE} 里的 AUTONOMY_DB）"

# 平台注入的值优先：端口永远跟随服务契约的 healthUrl。
ADDR="127.0.0.1:${PORT}"

# 已在运行则不重复拉起（平台重启前都会先 stop，这里是防御性检查）。
if [ -f "${PID_FILE}" ]; then
  old="$(tr -d '[:space:]' < "${PID_FILE}" || true)"
  if [ -n "${old}" ] && kill -0 "${old}" 2>/dev/null; then
    log "已在运行 pid=${old}"
    exit 0
  fi
  rm -f "${PID_FILE}"
fi

log "启动 部署版本=${APP_VERSION} 监听=${ADDR} 数据源=${AUTONOMY_DB}(只读)"
nohup "${BIN}" -db "${AUTONOMY_DB}" -addr "${ADDR}" >> "${LOG_FILE}" 2>&1 &
echo $! > "${PID_FILE}"
pid="$(cat "${PID_FILE}")"

# 探活：/health 是平台对每个服务统一探的路径。
for _ in $(seq 1 40); do
  if ! kill -0 "${pid}" 2>/dev/null; then
    rm -f "${PID_FILE}"
    echo "[start][错误] 进程已退出，最近日志：" >&2
    tail -20 "${LOG_FILE}" >&2 || true
    exit 1
  fi
  if curl -fsS -m 2 "http://127.0.0.1:${PORT}/health" >/dev/null 2>&1; then
    log "启动成功 pid=${pid} log=${LOG_FILE}"
    log "列表页：http://127.0.0.1:${PORT}/"
    exit 0
  fi
  sleep 0.5
done

echo "[start][错误] 20s 内 /health 未就绪，最近日志：" >&2
tail -20 "${LOG_FILE}" >&2 || true
kill "${pid}" 2>/dev/null || true
rm -f "${PID_FILE}"
exit 1
