# agent-benchmark-tool —— agent 行为评测工具
#
# 常用：make lint test / make build / make run / make package / make runtime-check

BINARY    := bin/benchmarkd
PKG       := ./cmd/benchmarkd
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -X main.version=$(VERSION)

# Go 缓存隔离在仓库之外：控制面调用 build.sh 时 HOME 可能指向 runtime 目录，
# 缓存落到那里会被下一次 rsync --delete 波及。
BUILD_CACHE ?= $(TMPDIR)/agent-benchmark-tool-build-cache

.PHONY: all build test race lint fmt fmt-check vet run package clean help

all: lint test build

build: ## 构建本机二进制到 bin/
	@mkdir -p bin
	GOMODCACHE=$(BUILD_CACHE)/gomodcache GOCACHE=$(BUILD_CACHE)/gocache \
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) $(PKG)
	@echo "built $(BINARY) ($(VERSION))"

test: ## 全部单元测试
	go test ./...

race: ## 竞态检测
	go test -race -count=1 ./...

lint: fmt-check vet ## 格式 + go vet

fmt:
	gofmt -w .

fmt-check:
	@out=$$(gofmt -l . | grep -v '^$$' || true); \
	if [ -n "$$out" ]; then echo "gofmt required for:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...

run: ## 本机运行（默认读 ~/Projects/autonomy/data/autonomy.db，监听 127.0.0.1:4231）
	go run $(PKG)

package: ## 按平台规范打包到 outputs/（等价于控制面流水线的构建步骤）
	APP_VERSION=$(VERSION) ./build.sh

runtime-check: package ## 在临时 runtime 目录里跑一遍 start/health/stop（不碰线上 4231）
	@tmp=$$(mktemp -d "$${TMPDIR:-/tmp}/benchmark-runtime.XXXXXX"); \
	mkdir -p "$$tmp/bin" "$$tmp/scripts"; \
	cp outputs/bin/* "$$tmp/bin/"; cp outputs/scripts/*.sh "$$tmp/scripts/"; \
	PORT=4239 RUNTIME_DIR="$$tmp" APP_VERSION=$(VERSION) bash "$$tmp/scripts/start.sh"; \
	curl -fsS -m 3 http://127.0.0.1:4239/health; echo; \
	PORT=4239 RUNTIME_DIR="$$tmp" bash "$$tmp/scripts/stop.sh"; \
	rm -rf "$$tmp"

clean:
	rm -rf bin outputs

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "%-14s %s\n", $$1, $$2}'
