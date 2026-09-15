# agent-benchmark-tool

对 agent 进行评测：标记 agent 的行为 → 比较分析 → 给出意见 → 用于下一阶段提示词优化；
同时分析不同 model 的行为模式差异。

分阶段实施，当前为 **第一阶段**。

## 第一阶段（已完成）：reason_turns 列表服务

一个独立的本地 HTTP 服务，只读地读取 agent runtime（autonomy）SQLite 库里的
`reason_turns` 表，并按 `task_id / agent / mode / model / input / output` 呈现。

- **只读**：以 `mode=ro` + `PRAGMA query_only` 打开数据库，绝不影响正在写入的 agent runtime，
  也不会修改被评测的数据。
- **零外部依赖服务**：Go 标准库 `net/http` + `html/template`，SQLite 用纯 Go 驱动
  `modernc.org/sqlite`（无 CGO，单二进制）。

### 运行

```bash
go run ./cmd/benchmarkd
# 等价于：
go run ./cmd/benchmarkd -db ~/Projects/autonomy/data/autonomy.db -addr 127.0.0.1:4231
```

启动后：

- 列表页：<http://127.0.0.1:4231/>
- 单条详情（含完整 input / raw_output / normalized_output）：`/turns/{id}`
- JSON 列表：`/api/reason-turns`
- JSON 单条：`/api/reason-turns/{id}`
- 过滤选项（facets）：`/api/facets`
- 健康检查：`/healthz`

### 配置

| 环境变量 | 含义 | 默认 |
|----------|------|------|
| `AUTONOMY_DB` | 要读取的 SQLite 库路径 | `~/Projects/autonomy/data/autonomy.db` |
| `BENCHMARK_ADDR` | 监听地址 | `127.0.0.1:4231` |

命令行 `-db` / `-addr` 优先级最高。服务**不读取**环境里的 `HOST` / `PORT`（那是别的服务用的）。

### 接口

`GET /api/reason-turns`

| 参数 | 说明 |
|------|------|
| `task_id` `agent` `mode` `model` `status` | 精确匹配（值取自 `/api/facets`） |
| `q` | input / raw_output 的子串搜索（字面匹配，`%` `_` 不当作通配符） |
| `limit` `offset` | 分页，默认 50，上限 500 |
| `order` | `id`（默认）/ `created_at` / `duration_ms` / `total_tokens` |
| `dir` | `desc`（默认）/ `asc` |
| `preview` `truncate` | `preview=1` 时把 input/output 截断为 `truncate` 个字符（默认 400），便于列表消费 |

返回：

```json
{
  "turns": [
    {
      "id": 106,
      "task_id": "task-8",
      "agent": "agent-10107",
      "agent_id": 10107,
      "mode": "agent",
      "model": "deepseek-v4-pro",
      "provider": "cline",
      "status": "finished",
      "input": "...",
      "output": "...",
      "normalized_output": "...",
      "duration_ms": 172417,
      "total_tokens": 1108037,
      "cost_cents": 3.5541327,
      "created_at": "2026-09-14T12:53:37.192512Z"
    }
  ],
  "total": 106,
  "limit": 50,
  "offset": 0,
  "filters": { "order": "id", "dir": "desc" }
}
```

`agent` 来自 `agents.name`（`reason_turns.agent_id` 左连接 `agents.id`），`output` 取
`raw_output`（模型原样输出），`normalized_output` 只在详情接口返回。
其余字段（provider / status / tokens / 耗时 / 成本）先一并带出，供第二、三阶段做
行为标记与 model 对比分析，无需再改 schema。

兼容旧库：若 `reason_turns` 只有旧的 `output` 列（无 `raw_output`），或没有 `agents` 表、
`agent_id` 为 TEXT，服务会自动降级读取而不会报错。

### 开发

```bash
go test ./...     # 单元测试（临时库构造 + httptest，不触碰真实数据）
go vet ./...
```

代码结构：

| 关注点 | 文件 |
|--------|------|
| 启动 / 配置 / 优雅退出 | `cmd/benchmarkd/main.go` |
| 领域模型、分页/过滤选项 | `internal/store/model.go` |
| 只读 SQLite 访问、方言与列探测 | `internal/store/sqlite.go` |
| HTTP 路由、JSON 接口、HTML 页面 | `internal/httpapi/server.go`、`internal/httpapi/pages.go` |
| 页面模板 | `internal/httpapi/templates/*.gohtml` |

## 部署（agent-control-plane-deployment 规范）

本服务已接入平台的部署系统（控制面：<http://127.0.0.1:4220/panel/>，部署域，与 runtime 分离）。

### 打包

仓库根目录的 `build.sh` 遵循平台规范：产出 `outputs/`（`bin/benchmarkd` + `scripts/*.sh`），
前端页面用 `go:embed` 编进二进制。

```bash
make package        # 等价：APP_VERSION=<hash> ./build.sh
make runtime-check  # 在临时 runtime 目录跑一遍 start → /health → stop（不动线上 4231）
```

### runtime 布局

| 路径 | 来源 | 说明 |
|------|------|------|
| `bin/benchmarkd` | 发版包 | 可执行文件 |
| `scripts/{start,stop,restart}.sh` | 发版包 | 平台按服务契约调用 |
| `backend/.env` | 首次启动生成（600） | 唯一可覆盖项：`AUTONOMY_DB`（被评测数据源） |
| `backend/runtime.pid`、`backend/server.log` | 运行期 | 部署时被平台保留 |

启动时平台注入 `PORT`（来自服务契约 healthUrl）、`RUNTIME_DIR`、`APP_VERSION`；
服务固定监听 `127.0.0.1:${PORT}`，平台统一探活 `GET /health`。

### 触发部署

平台把「打包 + 上线」做成一条流水线（服务契约 + ref）：

```bash
# 触发（异步；等价于面板上的「触发打包+部署」按钮）
curl -s -X POST http://127.0.0.1:4220/api/deploy-notify \
  -H 'Content-Type: application/json' \
  -d '{"serviceId":"agent-benchmark-tool","ref":"main"}'

# 查询
curl -s 'http://127.0.0.1:4220/api/pipelines?limit=5'
curl -s 'http://127.0.0.1:4220/api/pipelines/<requestId>'
```

服务契约（`PUT /api/services/agent-benchmark-tool`）：

| 字段 | 值 |
|------|-----|
| serviceId | `agent-benchmark-tool` |
| runtimeDir | `/Users/gaolei/runtime/agent-benchmark-tool` |
| healthUrl | `http://127.0.0.1:4231/health` |
| startCmd / stopCmd / restartCmd | `bash "<runtimeDir>/scripts/{start,stop,restart}.sh"` |
| gitRepoUrl | `https://github.com/kaulie/agent-benchmark-tool` |
| defaultBranch | `main` |

> 注意：流水线在**服务端异步执行**（独立部署代理），不要在本 agent 的 shell 里同步跑
> `bin/deploy.sh`——那会在 shell 存活期间把服务重启掉。

## 后续阶段（规划中）

2. **行为标记**：对本工具的 turn 建立标注（标签、评分、问题归类）与提示词修订记录，
   形成「标注 → 分析 → 下一阶段提示词」的闭环。
3. **model 对比**：按 `provider / model / mode` 分组聚合（token、耗时、成本、失败率、
   行为标签分布），给出差异结论。
