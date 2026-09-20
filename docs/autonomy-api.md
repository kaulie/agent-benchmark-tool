# 对 autonomy 的数据 API 需求（benchmark tool 改为 API 取数）

本文是 **agent-benchmark-tool 向 autonomy 提的数据接口需求**，供 autonomy 侧实现（按它的
swag 注解契约流程：`scripts/register-contract.sh` → 服务注册）后，benchmark tool 改走 HTTP，**不再打开
autonomy 的 SQLite 文件**。

## 1. 为什么改

benchmark tool 现在的做法是 `-db <autonomy.db>`，以 `mode=ro` 只读挂上 autonomy 的库文件。两个问题：

1. **部署耦合**：库路径是部署机上的本地文件，benchmark 读的文件和 autonomy 真正在写的文件还得靠人配
   （`backend/.env` 里的 `AUTONOMY_DB`，默认 `$HOME/Projects/autonomy/data/autonomy.db`），一旦 autonomy
   换了 store 位置或搬到别的机器，benchmark 就静默读到旧数据。
2. **schema 耦合**：benchmark 必须自己探测列名与表的有无（`cycle` ↔ 旧 `step`、`raw_output` ↔ 旧 `output`、
   `tasks` 表在不在、`agent_id` 是 INTEGER 还是 TEXT、`tasks` 的列漂移），autonomy 每次改表都要在
   benchmark 侧补一段兼容——这些兼容逻辑应该由**持有库的那一方**收口。

改成 HTTP 后：benchmark 只依赖下面这份 JSON 契约，autonomy 负责把库读成契约（列名归一、join agent 名、
分页/排序/过滤都在 autonomy 侧实现），两边可以各自演进。

### 1.1 今天实拍到的故障（2026-09-20 16:28）

数据目录被统一挪到 `~/database/` 的过程中，就发生了一次「本地文件耦合」的典型事故：

| 事实 | 证据 |
|---|---|
| 旧库被归档 | `~/database/_archive/autonomy-projects-20260920.db`（29 MB，236 条 turn，含 `task-33`） |
| 新位置 | `~/database/autonomy/autonomy.db`（12 条 turn，`task-29`） |
| checkout 里改成软链 | `~/Projects/autonomy/data/autonomy.db` → `~/database/autonomy/autonomy.db` |
| autonomy 服务在写哪 | `runtime/autonomy/backend/data/autonomy.db`（12 条 / `task-29`） |
| benchmarkd 在显示哪 | **它启动时打开的那个 inode（已被归档的旧库，236 条 / `task-33`）** |

也就是说：`benchmarkd` 进程（pid 23297）从 9/17 起一直攥着旧文件的 fd，路径被换成软链、旧库被归档之后，
它**照旧展示 236 条旧数据**，而 autonomy 真正在跑的是 12 条新数据；两边都不报错，看页面的人无从察觉。
这正是改成 HTTP 取数的直接理由：**库的位置和 schema 由持有者（autonomy）负责，benchmark 只认服务地址。**

## 2. 通用约定（提交给 autonomy 的约定，可按 autonomy 现有风格调整）

- **Base URL**：autonomy 服务地址，benchmark 侧可配 `AUTONOMY_API_URL`，默认 `http://127.0.0.1:4300`
  （即服务契约里 autonomy 的端口）。
- **只读**：本需求全部是 `GET`；benchmark 不写任何数据，不要求鉴权（沿用现在的回环地址 + 无 auth 现状）。
- **JSON / UTF-8**，`Content-Type: application/json; charset=utf-8`。
- **时间**：库里就是 TEXT 的 RFC3339（如 `2026-09-19T04:57:30.090723Z`），按原样透出字符串即可，
  不做时区换算、不截断精度。
- **分页**：`limit`（默认 50，上限 500）+ `offset`，响应里带 `total`（满足过滤条件的总行数）。
- **未知参数**：忽略，不报错；空结果 → `200` + 空数组（不是 404）。
- **错误**：`{"error": "..."}`，与 autonomy 现有 `errResponse` 一致；只对「资源不存在」用 404。
- **字段名**：与下面给出的 JSON 一致（benchmark 直接接线，避免再做一层 mapping）。若 autonomy 想用别的命名，
  只要在契约里写清对应关系即可。

## 3. 端点总览

| # | 方法 | 路径 | 用途 | 优先级 |
|---|------|------|------|--------|
| 1 | GET | `/api/reason-turns` | 列表页/整表检索：过滤 + 排序 + 分页 | MUST |
| 2 | GET | `/api/reason-turns/{id}` | 详情页：单条 turn 全文 | MUST |
| 3 | GET | `/api/reason-turns/facets` | 筛选下拉：去重值 + 计数 | MUST |
| 4 | GET | `/api/tasks` | 对比页的 task 选择器（读；与现有 `POST /api/tasks` 同路径不同方法） | MUST |
| 5 | GET | `/api/tasks/{taskID}/turns` | 对比页加载 task 一侧的执行（按执行顺序的 turns） | MUST |
| 6 | GET | `/api/tasks/{taskID}` | 任务定义（**复用现有 `TaskProgress`**，只需确认字段稳定，见 §4.6） | SHOULD |
| 7 | GET | `/api/meta` | 能力/版本自述（省掉 schema 探测） | NICE |
| 8 | GET | `/health` | 上游探活（已存在，直接用） | 已有 |

## 4. 端点契约

### 4.0 `Turn`（下面多处复用）

一条 `reason_turns` 行 + 该 agent 的名字。**`output` 必须是未解析的原文 `raw_output`**（保留 fence、不做
JSON 解析），`normalized_output` 单独给。

| JSON 字段 | 来源（autonomy 当前 schema） | 说明 |
|---|---|---|
| `id` | `reason_turns.id` | 自增主键，benchmark 的详情页 URL 用它 |
| `task_id` | `reason_turns.task_id` | 所属 task |
| `cycle` | `reason_turns.cycle` | 决策轮次（**不是** `step`；旧库叫 `step`，由 autonomy 归一） |
| `mode` | `reason_turns.mode` | `plan` / `agent`，benchmark 的对比页按它分类 |
| `agent_id` | `reason_turns.agent_id` | 整数 |
| `agent` | `agents.name`（`LEFT JOIN agents ON agents.id = reason_turns.agent_id`） | 名字；join 不到给 `""` |
| `provider` | `reason_turns.llm_provider` | 注意库里叫 `llm_provider`，契约里叫 `provider` |
| `model` | `reason_turns.model` | |
| `llm_agent_id` | `reason_turns.llm_agent_id` | SDK 侧会话 id |
| `status` | `reason_turns.status` | `finished` / `error` / … |
| `error_code` / `error_message` | 同名列 | 失败时才有内容 |
| `input` | `reason_turns.input` | 发给 reasoner 的完整 prompt |
| `output` | `reason_turns.raw_output` | 原文输出 |
| `normalized_output` | `reason_turns.normalized_output` | 解析后的形态（可能为空） |
| `run_id` | `reason_turns.run_id` | |
| `duration_ms` / `event_count` | 同名列 | 整数 |
| `input_tokens` / `output_tokens` / `cache_read_tokens` / `cache_write_tokens` / `reasoning_tokens` / `total_tokens` | 同名列 | 整数，缺省 0 |
| `cost_cents` | `reason_turns.cost_cents` | 可为 `null`（benchmark 侧用指针区分「没有」和 0） |
| `started_at` / `ended_at` / `created_at` | 同名列 | 时间字符串，按库里原样 |

### 4.1 `GET /api/reason-turns` — 列表（MUST）

**查询参数**

| 参数 | 取值 | 说明 |
|---|---|---|
| `task_id` `mode` `model` `status` | 精确匹配 | 值取自 §4.3 的 facets |
| `agent` | 精确匹配 `agents.name` | 注意是**名字**，不是 `agent_id` |
| `q` | 子串 | 在 `input` 与 `raw_output` 里做**字面**匹配：`%` `_` `\` 不当通配符（转义后再 LIKE） |
| `order` | `id`（默认）/ `created_at` / `duration_ms` / `total_tokens` | 白名单，非法值回落 `id` |
| `dir` | `desc`（默认）/ `asc` | |
| `limit` / `offset` | 整数 | 默认 50 / 0，`limit` 上限 500 |
| `preview` | `1`/`true` | 可选：只回 `input`/`output` 的前 N 字符（见 `truncate`），列表页不用拉全文 |
| `truncate` | 整数，默认 400 | `preview=1` 时每种文本截断的字符数（按 rune） |

**响应 `200`**

```json
{
  "turns": [ { "id": 12, "task_id": "task-29", "cycle": 3, "mode": "plan", "agent_id": 10001,
               "agent": "agent-10001", "provider": "cline", "model": "deepseek-v4-flash",
               "status": "finished", "input": "…", "output": "…", "normalized_output": "…",
               "duration_ms": 445086, "total_tokens": 0, "cost_cents": 2.1322308,
               "created_at": "2026-09-20T07:49:46.033072Z" } ],
  "total": 12,
  "limit": 50,
  "offset": 0
}
```

- 排序稳定性：`ORDER BY <order> <dir>, id DESC`（同 `created_at` 时用 id 兜底，分页不跳行/不重复）。
- `total` 是**过滤后**的总数（用于分页器）。

### 4.2 `GET /api/reason-turns/{id}` — 单条（MUST）

响应 `200`：一个 `Turn` 对象（字段同 §4.0，**不做截断**）。
`id` 非数字 → `400`；不存在 → `404` `{"error":"turn not found"}`。

### 4.3 `GET /api/reason-turns/facets` — 筛选项（MUST）

响应 `200`：

```json
{
  "tasks":     [ { "value": "task-29", "count": 12 } ],
  "agents":    [ { "value": "agent-10001", "count": 8 }, { "value": "agent-10002", "count": 1 } ],
  "modes":     [ { "value": "plan", "count": 8 }, { "value": "agent", "count": 4 } ],
  "models":    [ { "value": "deepseek-v4-flash", "count": 9 }, { "value": "composer-2", "count": 3 } ],
  "providers": [ { "value": "cline", "count": 9 }, { "value": "cursor", "count": 3 } ],
  "statuses":  [ { "value": "finished", "count": 9 }, { "value": "error", "count": 3 } ]
}
```

- 每一类都是对该列去重 + 计数，**跳过空值**，按 `count` 降序（同 count 按 value 升序）。
- `agents` 是**名字**（join `agents` 表）；`providers` 取自 `llm_provider`。
- 一次请求给全部类别（benchmark 的筛选栏一次渲染完）。

### 4.4 `GET /api/tasks` — task 选择器（MUST）

现在 `/api/tasks` 只有 `POST`（受理指令）。对比页需要一个**读**的版本（同路径 + GET）：

```json
{
  "tasks": [
    { "id": "task-29",
      "description": "主界面增加显示当前agent已经执行的轮次，提交commit，提PR,merge代码后部署上线",
      "status": "blocked",
      "turns": 12,
      "last_at": "2026-09-20T07:49:46.033072Z" },
    { "id": "task-28",
      "description": "开放服务契约的前端入口，提交commit，提PR,merge代码后部署上线",
      "status": "error",
      "turns": 0,
      "last_at": "" }
  ]
}
```

- `turns` = 该 task 的 `reason_turns` 行数；`last_at` = 该 task 最近一条 turn 的 `created_at`（没有 turn 则 `""`）。
- **候选 = `tasks` 表的行 ∪ 只出现在日志里的 `task_id`**（日志里可能有 task 行已删/未落库的历史数据），
  两个来源按 `id` 合并去重，`description`/`status` 取 `tasks` 表（没有就给 `""`）。
- 排序：`last_at` 降序（最近的在前），同则 `id` 升序。不分页（task 数量级不大，benchmark 一次渲染下拉）。

### 4.5 `GET /api/tasks/{taskID}/turns` — 一个 task 的执行序列（MUST）

对比页要按**执行顺序**把两侧的 turn 排起来（不是按 id 或时间倒序）。

- 查询参数：`limit`（默认 1000，上限 1000）；`task_id` 不存在或没有 turn → `200` + `turns: []`。
- 响应 `200`：

```json
{ "task_id": "task-29", "turns": [ /* Turn，按 created_at ASC, id ASC */ ], "total": 12, "capped": false }
```

- `capped=true` 表示因为 `limit` 截断了（benchmark 页面会提示「只显示前 N 条」）。

### 4.6 `GET /api/tasks/{taskID}` — 任务定义（SHOULD，复用现有端点即可）

对比页顶部要展示「这个 task 本来是什么」。这个端点**已经有了**（`TaskProgress`），我们要的就是它里面的：
`task_id / description / domain / status / error / agent_id / created_at / updated_at / goal_type / context_ref`
（`plans` / `project` 我们暂时不用，但留着无妨）。只请确认这几项**稳定存在、语义不变**：

```json
{ "task_id": "task-29", "description": "主界面增加显示当前agent已经执行的轮次，…", "domain": "software_development",
  "status": "blocked", "error": "", "goal_type": "dev_feature",
  "context_ref": {"task": "task-446e4fcb823d4b53"}, "agent_id": 10001,
  "created_at": "2026-09-20T04:01:16.18929Z", "updated_at": "2026-09-20T07:57:12.377109Z" }
```

> 说明：benchmark 现在的对比页还会尝试读 `context` / `target` / `goal` / `expected_state` 四个字段，但
> **autonomy 的 `tasks` 表里并没有这些列**（只有 `description/domain/goal_type/context_ref/status/error/agent_id/created_at/updated_at`），
> 所以它们在页面上一直是空的——这次直接从需求里删掉，**不要**为迁移临时加列。
> 这个端点不存在（或某 task 没有行）时 benchmark 侧会降级：任务定义栏提示「原始内容见 planner 入口 prompt」，
> 其余照常对比，所以它可以是 SHOULD 而不是 MUST。

### 4.7 `GET /api/meta` — 能力自述（NICE TO HAVE）

```json
{ "service": "autonomy", "version": "cabb1e98",
  "reason_turns": { "cycle_column": "cycle", "raw_output_column": "raw_output" },
  "has_tasks_table": true, "turns": 12 }
```

用途：benchmark 用它决定「有没有 tasks 定义 / 用什么列名 / 一共多少条」，就不用再像现在这样
`PRAGMA table_info` 去探测（`internal/store/sqlite.go` 里那一大段兼容逻辑可以整段删掉），
`/health` 里也能直接展示上游版本。

### 4.8 `GET /health` — 已存在

benchmark 用它做上游探活与健康展示，不需要改动。若方便，加上 `"turns": <count>` 这类轻量计数更好。

## 5. 对 autonomy 侧实现的建议

- 走它既有的流程：新路由加进 `src/http_server.go` 的 `routes()`，处理函数带 swag 注解
  （`@Summary/@Tags/@Param/@Success/@Router`），`scripts/register-contract.sh` 注册进服务契约，
  `src/contract_test.go` 保证「注解 ↔ 路由表」一致。
- 只读实现即可：SQL 全部走 `SELECT`，不要在这些 handler 里触发写/迁移副作用。
- 性能：`reason_turns` 上的过滤/排序走已有索引；`q` 的 `LIKE '%…%'` 无法走索引，属于可接受的全表扫描
  （列表页 `limit≤500`，`total` 用同条件的 `COUNT(*)`）。`facets` 是 6 个 `GROUP BY`，建议加缓存
  （TTL 几秒）或按 `(task_id, agents, mode, model, llm_provider, status)` 建索引；
  库里已有 `idx_llm_events_turn_kind` 之类的先例可参照。
- `agents` 的 join 只在 `agents` 表存在时做；旧库/turn 的 `agent_id` 是 TEXT 的历史形态也要能读
  （join 不上时 `agent=""`，不要报错）。
- 大文本量：`input`/`raw_output` 可能几十 KB，列表接口务必支持 §4.1 的 `preview`/`truncate`，
  否则一次 50 条的列表页会传几 MB。

## 6. benchmarkd 侧改造（已实现）

> 状态：已实现（本仓库 PR #9，`internal/autonomyapi`）。取数实现见 `internal/autonomyapi/client.go`（HTTP 客户端，实现
> `httpapi.TurnReader`），删除了 `internal/store/sqlite.go` 与 `modernc.org/sqlite` 依赖；
> `-db` / `AUTONOMY_DB` 已被 `-autonomy-url` / `AUTONOMY_API_URL` 取代，`/health` 的 `db`
> 字段换成 `upstream`（地址 + 可达性 + 版本 + 条数，不可达时 503）。下面是当时的改造计划，留档。

1. 新增 `internal/autonomyapi`：HTTP client，实现 `httpapi.TurnReader` 接口
   （`List / Get / Facets / CountAll / Tasks / Task / TaskTurns / Path`），内部把 §4 的 JSON 解成
   `store.Turn` / `store.TaskInfo` / `store.TaskOption` / `store.Facets`。**interface 不变，页面代码零改动。**
2. 启动参数：`-db <path>` → `-autonomy-url <base>`（env `AUTONOMY_API_URL`，默认 `http://127.0.0.1:4300`）；
   `AUTONOMY_DB` 一并删除（不再有任何打开 SQLite 的路径）。
3. 删掉 `internal/store/sqlite.go` 与 `modernc.org/sqlite` 依赖（二进制变小，`mode=ro` 的顾虑消失）。
4. `/health` 的 `db` 字段换成 `upstream`（base URL + 上游 `/health` 是否可达 + turns 计数）。
5. runtime `scripts/start.sh`：生成的 `backend/.env` 从 `AUTONOMY_DB=…` 改为 `AUTONOMY_API_URL=http://127.0.0.1:4300`；
   不再要求数据源文件存在。
6. 测试：`internal/store/*_test.go` 的 fixture DB 换成 `httptest` 起的假 autonomy（返回固定 JSON），
   断言页面/接口逐字段不变。

## 7. 验收（怎么算对）

- benchmarkd 进程 `lsof` 里**不再有任何 `.db`**；仓库里搜不到 `autonomy.db` 路径（除文档示例）。
- 同一份数据下，改造前后四个页面**逐字段一致**：对同一 task（例如 `task-29`：12 条 turn、8 条 `plan` + 4 条 `agent`）
  比较 `#id`/`cycle`/`mode`/`status`、tokens、`duration_ms`、`cost_cents`、`input/output` 字符数。
- 过滤/排序/搜索边界一致：`q` 里的 `%`/`_` 是字面量；非法 `order` 回落 `id`；`limit>500` 被夹到 500。
- autonomy 不可达时：benchmark 的 `/health` 返回 503 且 `upstream` 字段说明原因，页面给一行明确提示
  （不是 500 + 空页）。

## 8. 不在本次范围

- 任何写接口（benchmark 只读）。
- `llm_events` / `llm_messages` 的原始流（行为标注、逐 token 回放）——后续阶段需要时再单独提，
  大致的形态是 `GET /api/reason-turns/{id}/events?last_seq=N`。
- 鉴权 / 多租户：沿用现状（回环地址、无 auth）。

