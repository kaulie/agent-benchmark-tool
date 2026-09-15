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

## 后续阶段（规划中）

2. **行为标记**：对本工具的 turn 建立标注（标签、评分、问题归类）与提示词修订记录，
   形成「标注 → 分析 → 下一阶段提示词」的闭环。
3. **model 对比**：按 `provider / model / mode` 分组聚合（token、耗时、成本、失败率、
   行为标签分布），给出差异结论。
