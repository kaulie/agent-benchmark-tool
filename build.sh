#!/usr/bin/env bash
#
# 打包脚本 —— 遵循「agent-control-plane-deployment」部署系统规范。
#
# 调用方（二选一，均从仓库根执行）：
#   - 控制面流水线：POST /api/deploy-notify {serviceId:"agent-benchmark-tool"}
#   - 独立发版：/Users/gaolei/deployment/bin/release.sh agent-benchmark-tool [ref]
#
# 约定：
#   - cwd = 仓库根；环境变量 APP_VERSION = <8 位短 hash>
#   - 必须产出 outputs/，其中必须包含 scripts/restart.sh（平台硬性要求）
#   - VERSION / COMMIT / GIT_REPO_URL 由调用方写入发版包，本脚本不写
#   - 运行期可变内容一律不放进 outputs/（pid、日志、.env 都在 backend/ 下，
#     由平台在部署时保留，见 scripts/start.sh）
#
# 前端页面用 go:embed 编进二进制（internal/httpapi/templates），发版包里不需要
# 再单独拷贝模板。
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${ROOT}"

VERSION="${APP_VERSION:-dev}"
OUT="${ROOT}/outputs"

echo "[build] agent-benchmark-tool version=${VERSION}"
rm -rf "${OUT}"
mkdir -p "${OUT}/bin" "${OUT}/scripts"

# Go 缓存/临时目录隔离在仓库之外（可覆盖）：控制面调用本脚本时 HOME 可能指向
# runtime 目录，缓存落到那里会被下一次 rsync --delete 波及。
BUILD_CACHE="${BENCHMARK_BUILD_CACHE:-${TMPDIR:-/tmp}/agent-benchmark-tool-build-cache}"
export GOMODCACHE="${BUILD_CACHE}/gomodcache"
export GOCACHE="${BUILD_CACHE}/gocache"
export GOPATH="${BUILD_CACHE}/gopath"
export GOTMPDIR="${BUILD_CACHE}/gotmp"
mkdir -p "${GOMODCACHE}" "${GOCACHE}" "${GOPATH}" "${GOTMPDIR}"
# 默认用可达的模块代理（部分网络下 proxy.golang.org 走 IPv6 不可达）。
[ -n "${GOPROXY:-}" ] || export GOPROXY="https://goproxy.cn,direct"

LDFLAGS="-s -w -X main.version=${VERSION}"

# 本机二进制：平台 runtime 直接运行它（纯 Go SQLite 驱动，CGO_ENABLED=0）。
CGO_ENABLED=0 go build -trimpath -ldflags "${LDFLAGS}" \
  -o "${OUT}/bin/benchmarkd" ./cmd/benchmarkd

cp "${ROOT}/scripts/start.sh" "${ROOT}/scripts/stop.sh" "${ROOT}/scripts/restart.sh" \
  "${OUT}/scripts/"

chmod +x "${OUT}/bin/"* "${OUT}/scripts/"*.sh

echo "[build] outputs 就绪："
ls -1 "${OUT}" "${OUT}/bin" "${OUT}/scripts" | sed 's/^/  /'
