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

- 列表页：<http://127.0.0.1:4231/> —— **只列元信息**：`#id` `task_id` `agent` `mode` `model` `status`
  `step` `created_at` + tokens / 耗时 / **input、output 的字符数** / 是否有 normalized_output。
  列表**不渲染 input / output 正文**（页面里没有 <pre> 预览），点 `#id` 或 `view ›`（整行可点）进详情页读全文。
  保留过滤、搜索、排序、分页
- 单条详情（含完整 input / raw_output / normalized_output）：`/turns/{id}`，
  带 **output 视图开关**：切换 `raw_output` ⇄ `normalized_output`（同一时刻只显示一个，不再同时铺开），
  切换是纯前端即时生效（URL 同步为 `?out=normalized`，可分享/刷新保持）；关闭 JS 时退化为普通链接跳转；
  某条没有归一化输出时该栏提示而不是空白
- **task 对比**（左右分布，见下）：`/compare?a={task_id}&b={task_id}`
- JSON 列表：`/api/reason-turns`
- JSON 单条：`/api/reason-turns/{id}`
- 过滤选项（facets）：`/api/facets`
- 可对比的 task 列表：`/api/tasks`
- JSON 对比：`/api/compare?a={task_id}&b={task_id}`
- 健康检查：`/healthz`

### task 对比（compare two tasks）

同一个 task 描述会有多次 run（重跑、换 model、改 prompt），`/compare` 用来把**两个 task 的执行过程**
左右摆在一起看：

- 入口：列表页顶部「⇄ task 对比」，列表里点 `task_id`（带下划虚线的那个），或任意详情页的「⇄ 对比这个 task」
- 选择：页面顶部两个下拉（`task A` 左 / `task B` 右）+「对比 compare」，`⇄ 交换` 一秒换左右
- 页面结构（左右两列，A 一律在左、B 一律在右）：
  1. **概览**：turn 数（plan / agent 拆分）、agent、tokens、耗时、成本、时间窗、planner turn 与 result turn
  2. **task 原始内容**：`tasks` 表里该 task 的定义（description / domain / status / agent_id / 时间戳…）
  3. **planner 入口 prompt**：该 run **第一个 plan turn 的 input**（bootstrap prompt + task 定义 + Decision Cycle），
     以及该 plan turn 的输出
  4. **返回结果**：该 run **最后一个 turn 的输出**（raw_output，可切 normalized_output）
  5. **执行过程 · step by step**：按执行顺序（`created_at, id`）**逐 turn 左右对齐**——
     A 的第 1 步对 B 的第 1 步，即使两侧 step 编号不同；中间一列标出该步在两侧**不一样的字段**
     （`mode` / `model` / `status` / `tokens` / `duration`，差 1.5 倍以上才标记），少 turn 的一侧显示「该侧没有这一步」而不是错位
- 正文默认：三个关键内容展开、每步正文折叠；工具条的「全部展开 / 全部折叠」用 `?open=all|none` 表达（可分享、刷新保持）
- output 视图：整个页面共用一个 `raw_output` ⇄ `normalized_output` 开关（`?out=normalized`），切换即时生效、URL 同步
- 数据来源：execution 来自 `reason_turns`（按 `task_id` 取全量，顺序 `created_at ASC, id ASC`，单侧上限 1000 turn）；
  task 原始内容来自 `tasks` 表。**库里没有 `tasks` 表时不会报错**：该栏提示「原始内容见 planner 入口 prompt」，
  其余照常对比
- 未选/选错都能用：只选一侧、选到不存在的 task（页面内联提示）、两侧选同一个 task（顶部黄条提醒）都不会 500

`GET /api/compare?a=…&b=…` 返回同一份数据（`a` / `b` 各含 `task` / `planner_turn` / `result_turn` / `turns` /
`summary`，外加按位置对齐的 `aligned` 行与 `diff`），`GET /api/tasks` 返回选择器里的 task 候选
（`tasks` 表行 ∪ 日志里出现过的 task_id，各带 turn 数与最近活动时间）。

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
| `out` | **仅影响 HTML**：`out=normalized` 让 output 栏显示 `normalized_output`（默认 `raw`）；作用于详情页 `/turns/{id}` 与对比页 `/compare`。列表页不渲染正文，JSON 接口始终同时返回两个字段 |

`GET /compare`

| 参数 | 说明 |
|------|------|
| `a` `b` | 左右两侧的 task_id（`/api/tasks` 给出的值）。都可省略：只选一侧时另一侧留空提示 |
| `out` | `raw`（默认）/ `normalized`，控制页面上所有 output 栏显示哪一个 |
| `open` | `all` 全部展开 / `none` 全部折叠（默认：关键内容展开、逐 turn 正文折叠） |

`GET /api/compare?a={task_id}&b={task_id}` 返回左右两侧的 task 定义、planner 入口 turn、最后一个 turn（结果）、
该 task 的全部 turn 与汇总（turn 数 / tokens / 耗时 / 成本 / agent），以及按执行位置对齐的 `aligned` 行。

`GET /api/tasks` 返回可对比的 task 候选：`tasks` 表行 ∪ 日志里出现过的 `task_id`，各带 turn 数与最近活动时间，
并用 `tasks_table` 说明该库是否有 `tasks` 表。

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

兼容旧库：若 `reason_turns` 只有旧的 `output` 列（无 `raw_output`），或没有 `agents` / `tasks` 表、
`agent_id` 为 TEXT，服务会自动降级读取而不会报错（没有 `tasks` 表时对比页只是不显示 task 定义）。

`step` 同理：autonomy 已原地把 `reason_turns.step` 改名为 `cycle`，两者存的是同一个数，
服务优先读 `cycle`、其次回退 `step`（都没有则读作 0）；对外字段名仍叫 `step`，UI / JSON / 模板不变。

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
| task 对比（取数、对齐、JSON） | `internal/httpapi/compare.go` |
| 页面模板 | `internal/httpapi/templates/*.gohtml`（`compare.gohtml` = 对比页） |
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
