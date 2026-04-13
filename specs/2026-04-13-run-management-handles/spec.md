# `looper ps` 持久数字 ID 与运行态管理命令方案

## 1. 背景

`looper ps` 现在能列出正在运行的 loop run，但默认展示的是 `runId`。当前 `runId` / `loopId` / `executionId` 都是 UUID，适合机器，不适合人手敲。

这会直接卡住一组高频管理动作：

- `jump`：快速跳到当前 worktree
- `stop`：停掉当前正在跑的执行
- `logs`：看当前执行日志

如果没有一个短、稳定、可复制、跨命令可复用的 ID，这些命令都会被迫要求用户输入长 UUID，体验会很差。

用户希望 `ps` 第一列就是**纯数字自增 ID**，并且这个 ID 不是临时编号，而是整个运行生命周期里都可复用的管理入口。

---

## 2. 方案结论

### 2.1 核心决策

引入一个**持久化的 loop 数字 ID**，命名为 `seq`。

例如：

- planner loop → `#12`
- worker loop → `#13`
- reviewer loop → `#14`

`looper ps` 第一列默认显示这个 `seq`，后续管理命令统一使用它：

```txt
looper jump 12
looper stop 12
looper logs 12
```

其中：

- `seq` 是**人类交互主 ID**
- `loop.id` / `run.id` / `agentExecution.id` 继续保留为内部真实主键
- `seq` 必须在 daemon 重启、run 重试、step 切换后仍保持不变

### 2.2 为什么 ID 绑定 `loop`，不是 `run` 或 `agent execution`

最合适的归属是 `loop`：

1. 用户管理的是一条持续推进的 worker / reviewer / fixer / planner 流程
2. 同一个 loop 可能有多个 run，run 会反复重启、暂停、恢复
3. 同一个 run 也可能带一个或多个 agent execution
4. `jump` / `stop` / `logs` 最终都能从 loop 再解析到当前 active run、worktree、agent execution

如果把数字 ID 绑定到 `run`：

- 每次新 run 都会换 ID
- 用户无法把“12 号 reviewer”当作一个稳定对象管理

如果把数字 ID 绑定到 `agent execution`：

- 没有 agent 的 step（如 validate / push）就丢了统一入口
- 同一 loop 会出现多个不连续的数字身份

所以：

> **管理入口用 loop seq；运行详情继续展开到 active run / agent execution。**

---

## 3. `seq` 设计

## 3.1 数据模型

在 `loops` 表新增字段：

```txt
seq INTEGER NOT NULL UNIQUE
```

要求：

- 全局单调递增
- 不复用
- 允许出现空洞，不要求连续无缺口
- 对已有 loop 回填历史序号

对于 migration 前已经存在的存量数据：

- `seq` 不会缺失，而是通过 migration 一次性回填
- 回填顺序只要求**稳定**，不要求精确表达历史业务优先级
- 建议按 `createdAt ASC, id ASC` 回填，保证结果可重复

这意味着：

- `loop.id` 仍然是 UUID 主键
- `loop.seq` 是持久的人类可读次级标识

## 3.2 分配规则

新建 loop 时分配下一个 `seq`：

1. 从独立计数器读取当前值
2. 原子加一
3. 将结果写入新 loop 的 `seq`

建议新增一个轻量计数器表，例如：

```txt
counters(name TEXT PRIMARY KEY, value INTEGER NOT NULL)
```

用于管理 `loop_seq`。

不建议用“按当前 max(seq)+1 计算”的无锁方案，避免未来并发写入时出现竞态。

## 3.3 为什么不用 CLI 临时编号

不采用“`ps` 当前列表第 1 行就是 1”这种设计，因为它会漂移：

- active run 集合变化，编号就会变化
- 用户两次 `ps` 之间，`1` 可能已不是同一对象
- 对 `stop/logs/jump` 这类管理动作来说不够稳

因此这次方案明确采用**持久化数字 ID**，而不是会话级临时序号。

---

## 4. API 与聚合视图设计

## 4.1 `/api/v1/runs/active` 返回结构

`GET /api/v1/runs/active` 的每个 item 增加：

```json
{
  "seq": 12,
  "loopId": "550e8400-e29b-41d4-a716-446655440000",
  "runId": "0d93...",
  "projectId": "project_1",
  "type": "worker",
  "status": "running",
  "currentStep": "execute",
  "startedAt": "...",
  "target": { "...": "..." },
  "agent": { "...": "..." },
  "worktree": {
    "id": "...",
    "path": "/abs/path",
    "branch": "looper/worker/foo"
  }
}
```

其中：

- `seq`：CLI 第一优先展示和输入的持久数字 ID
- `loopId`：真实 loop 主键
- `runId`：当前 active run 主键
- `worktree`：给 `jump` 直接复用

本次先**不引入 `capabilities` 字段**。当前 v1 命令能力没有足够复杂到需要服务端再返回一组布尔矩阵。

## 4.2 统一解析规则

服务端和 CLI 都应支持双通道解析：

1. 纯数字输入 → 按 `loop.seq` 解析
2. 非纯数字输入 → 回退为真实 ID（`loopId` / `runId`）解析

这样可以兼容：

- 日常人手操作：`looper stop 12`
- 旧脚本或排障：`looper stop 550e8400-e29b-...`

---

## 5. `looper ps` 改造

## 5.1 默认表格

建议默认表格改成：

| # | type | target | step | agent | pid | status | age |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 12 | worker | acme/looper#42 | execute | opencode | 81234 | running | 5m |
| 14 | reviewer | acme/looper#42 | review | claude | 81288 | running | 2m |

默认表格里不再优先展示长 `runId`。

原因：

- 用户最需要的是一个可复制、可输入的管理入口
- 数字 ID 比 UUID 更适合日常使用
- `ps` 更接近进程管理工具的使用习惯

## 5.2 JSON 输出

`looper ps --json` 继续保留完整真实 ID：

- `seq`
- `loopId`
- `runId`
- `agent.executionId`
- `worktree.*`

这样脚本和排障仍然能使用真实标识。

---

## 6. 管理命令设计

## 6.1 通用选择器规则

后续命令统一接受 `<id>`：

- `looper jump <id>`
- `looper stop <id>`
- `looper logs <id>`

解析优先级：

1. 若参数是纯数字，按 `loop.seq` 解析
2. 否则先尝试 `loopId`
3. 如命令上下文确有需要，再允许 `runId` 兼容解析

CLI help 中应明确写：

> 推荐直接使用 `looper ps` 第一列的数字 ID。

## 6.2 `looper jump <id>`

目标：快速定位当前 loop 的 worktree。

这里需要先明确一个约束：

> 普通 CLI 子进程不能直接修改父 shell 的当前目录，因此 `looper jump 12` 不可能仅靠裸 CLI 进程本身完成真正的 `cd`。

因此建议把 `jump` 设计成 **shell-integrated command**，目标是“对用户表现为直接跳转”，而不是要求用户手敲 `cd "$(...)"`。

建议行为：

- 默认输出一段适合 `eval` 的 shell 片段，用于切到目标 worktree
- 提供官方 shell integration，例如：
  - `function lj() { eval "$(looper jump "$@")"; }`
  - 或提供 zsh/bash/fish 安装脚本写入同类 wrapper
- `--print-path` 时只输出 worktree 绝对路径，便于编辑器 / 脚本消费
- `--json` 输出 `seq`、`worktree.path`、`branch`、`projectId`

推荐用户体验：

- 用户平时执行 `lj 12`
- `lj` 内部调用 `looper jump 12` 并 `eval`
- 对用户表现为“直接跳转”

服务端只需要提供可解析的 `worktree.path`；真正 `cd` 通过 shell integration 完成。

### worktree 解析规则

这里需要补充实现约束：当前代码里 worktree 不是 loop 的一等外键关系。

本次 v1 明确采用：

1. **优先读取 active run 的 `checkpointJson`** 中的 `worktree.path/branch/id`
2. **其次读取 `loop.metadataJson`** 中已写入的 `worktreePath/branch/worktreeId`
3. 不额外修改 `worktrees` 表 schema，不新增 `loopId/runId` 外键

也就是说：

> v1 的 worktree 解析基于现有 checkpoint / metadata 聚合完成，不引入新的 worktree 关系建模。

## 6.3 `looper stop <id>`

`stop` 不能只 kill agent 进程，否则 scheduler 可能把 loop 重新拉起。

建议定义为组合动作：

1. pause loop
2. 如果该 loop 当前有 active agent execution，则按 vendor 执行对应 stop 流程
3. 将当前 active run 标记为 `cancelled` 或 `manual_intervention`
4. 写入一条 system event，说明是用户手动 stop

即：

> `stop 12` 停的是 12 号 loop 的当前活跃执行，不只是某个子进程。

这里不能只停留在“server route 里直接 `process.kill`”的描述，因为 HTTP server 当前拿不到 runtime 内存里的 subprocess handle。

因此需要显式增加一层 runtime bridge。

### runtime stop bridge

建议在 `LooperdApiContext` 中注入一个运行态控制接口，例如：

```ts
interface RuntimeController {
  stopLoop(input: { loopId: string; reason: string }): Promise<{
    stopped: boolean;
    runId?: string;
    executionId?: string;
    vendor?: string;
    pid?: number | null;
  }>;
}
```

职责分层：

- server route：解析 `seq/id`，做鉴权和参数校验
- runtime controller：定位 live execution，发送信号，更新 run/loop 状态，写 event

没有这个 bridge，`stop` 无法真正落地。

这里仍需要补充 vendor 语义。

### vendor-aware stop 原则

对于不同 agent vendor，`stop` 的目标应分层处理：

1. **优先终止本地受管进程**
   - 当前 Looper 已经通过 `Bun.spawn()` 持有 agent subprocess
   - v1 基础行为仍然是向该 subprocess 发送终止信号
2. **如果未来某 vendor 引入额外会话/子进程/守护进程语义**
   - 需要在 vendor 适配层补充专门 stop 行为
   - 例如先走 vendor-native cancel，再回退本地 kill
3. **v1 先不引入复杂 strategy 枚举**
   - 先把 vendor、pid、是否成功终止写入 event
   - 等未来真的出现 vendor-native cancel，再补 strategy 字段

### v1 vendor 细节建议

基于当前代码形态，v1 先定义为：

- `codex` / `claude` / `opencode` / 其他当前通过本地 CLI 拉起的 vendor
  - 一律先按“本地 subprocess managed by Looper”处理
  - 首先发送 `SIGTERM`
  - 超时未退出则升级 `SIGKILL`
  - 记录 `vendor`、`pid` 到 event

也就是说：

> v1 的 vendor-aware stop 重点不是立即做出一堆不同 kill 命令，而是先把“stop 逻辑由 runtime 按 vendor 决策”的扩展点立住。

### 后续扩展方向

若未来某 vendor 具备更好的取消机制，可以继续扩展：

- vendor adapter 暴露 `cancelExecution()`
- runtime 先尝试 vendor cancel
- 若失败或超时，再回退本地 signal kill

这样 stop 语义会更完整，也能覆盖未来不完全受本地 pid 控制的 vendor。

## 6.4 `looper logs <id>`

`logs` 不应只返回裸 `stdout/stderr`，而应返回“日志内容 + 当前上下文”的组合结果。

原因：

- 用户需要知道这些日志对应哪个 loop / run / agent
- 用户需要知道当前是 still running、completed、failed 还是 killed
- 用户需要区分“没有日志输出”与“当前 step 根本没有 agent”

因此建议 v1 返回一个 envelope，而不是单独一段文本。

### v1 数据来源

主数据源：

- `AgentExecutionRecord.outputJson`

补充元数据来源：

- `RunRecord.status/currentStep/startedAt/endedAt/summary/errorMessage`
- `LoopRecord.seq/id/type/status`

v1 先**不做 events fallback**。如果当前 run 没有 agent execution，则直接返回 `agent = null`，CLI 提示“当前 step 无 agent 输出”。

### 默认查看范围

`looper logs <id>` 默认查看：

> 目标 loop 的 **latest run** 的 **latest agent execution**。

不建议默认把多个 run 的日志混在一起。

如果后续需要历史查看，再补：

- `?runId=...`
- 或 CLI 侧 `--run <runId>`

### 推荐 API 响应结构

```json
{
  "seq": 12,
  "loopId": "550e8400-e29b-41d4-a716-446655440000",
  "loopType": "worker",
  "loopStatus": "running",
  "run": {
    "runId": "run_123",
    "status": "running",
    "currentStep": "execute",
    "startedAt": "2026-04-13T10:00:00.000Z",
    "endedAt": null,
    "summary": null,
    "errorMessage": null
  },
  "agent": {
    "executionId": "agent_exec_1",
    "vendor": "codex",
    "status": "running",
    "pid": 12345,
    "startedAt": "2026-04-13T10:00:03.000Z",
    "endedAt": null,
    "heartbeatCount": 42,
    "lastHeartbeatAt": "2026-04-13T10:03:00.000Z",
    "summary": null,
    "parseStatus": null,
    "stdout": "...",
    "stderr": "..."
  }
}
```

约束：

- 有 agent execution 时，返回 `agent`
- 当前 run 没有 agent execution 时，`agent = null`
- loop 存在但尚无 run 时，`run = null`、`agent = null`

### 非 agent step 的处理

对于没有 agent 的 step，例如某些：

- discover
- prepare-worktree
- validate
- push

此时不应伪造 stdout/stderr。

v1 直接返回：

- 当前 run 元数据
- `agent = null`

### CLI 默认行为

建议支持：

- `looper logs 12`
- `looper logs 12 --stderr`
- `looper logs 12 --tail 100`
- `looper logs 12 --full`
- `looper logs 12 --json`

其中：

- 默认人类模式显示 `stdout` 最近 100 行
- `--stderr` 改看 `stderr`
- `--full` 显示完整 `stdout/stderr`
- `--json` 输出完整 envelope

推荐的人类输出形态：

```txt
Loop #12 · worker · running
Run run_123 · step: execute
Agent: codex · pid 12345 · running

...stdout tail...
```

无 agent 时：

```txt
Loop #12 · reviewer · running
Run run_123 · step: discover
No agent output for the current step.
```

### 路由建议

`logs` 更适合 loop-centric，而不是 run-centric。

建议：

```txt
GET /api/v1/loops/:id/logs
```

其中 `:id` 支持：

- `seq`
- `loopId`

服务端内部再解析为：

1. target loop
2. latest run
3. latest agent execution

### 风险与约束

1. 当前日志本质是 snapshot，不是流式 tail
2. `outputJson` 可能较大，因此 CLI 默认应采用 tail，而不是全量打印
3. 当前 output 可能受现有 `maxOutputBytes` 限制，后续可再补充 truncation 标识

因此本次 spec 明确：

> v1 的 `logs` 是带上下文的 snapshot viewer，不是 `tail -f` 替代品。

---

## 7. 服务端实现建议

## 7.1 migration

建议新增一个 SQLite migration：

1. `loops` 表增加 `seq`
2. 为历史 loops 回填 `seq`
3. 新增 `counters` 表，初始化 `loop_seq`
4. 为 `loops.seq` 建唯一索引

回填时只需保证稳定顺序即可，建议按：

- `createdAt ASC`
- 若并列，则 `id ASC`

建议 migration 顺序：

1. 使用 SQLite table rebuild 方案为 `loops` 增加 `seq`
2. 批量为历史 rows 写入稳定序号
3. 校验无空值、无重复值
4. 为 `loops.seq` 建唯一索引
5. 最后将 `loop_seq` 计数器初始化为 `MAX(seq)`

这样可以确保存量数据在 migration 后立即拥有可用的持久数字 ID。

### TypeScript 接入点

本次必须同步更新：

- `storage/types.ts` 中的 `LoopRecord` 增加 `seq: number`
- SQLite row mapper / upsert SQL / schema health 检查同步支持 `seq`
- `createLoopRecord()` 在创建新 loop 时调用 `allocateSeq()` 并写入 `LoopRecord.seq`

也就是说：

> `seq` 不只是 migration 字段，还要成为 `LoopRecord` 的正式一部分。

## 7.2 storage

建议为 `store.loops` 增加：

- `getBySeq(seq: number)`
- `allocateSeq()` 或等价计数器接口

分配 `seq` 必须在事务里完成，避免将来出现并发冲突。

## 7.3 active-run descriptor

建议把 `buildActiveRunViews()` 升级为统一 descriptor builder：

- 继续输出当前 `ActiveRunView`
- 补齐：
  - `seq`
  - `worktree`

这样 `ps` / `jump` / `stop` / `logs` 都能复用同一份聚合视图。

## 7.4 路由建议

建议统一支持数字 `seq` 作为 loop selector。

可选实现有两种：

### 方案 A：复用现有 loop 路由

- `POST /api/v1/loops/:id/pause`
- `POST /api/v1/loops/:id/start`

其中 `:id` 允许既是 UUID，也可以是纯数字 `seq`。

### 方案 B：新增运行态管理路由

- `POST /api/v1/runs/active/:id/stop`

其中 `:id` 同样支持 `seq`。

本方案更推荐：

- loop 生命周期动作复用 `/api/v1/loops/:id/*`
- `logs` 走 `/api/v1/loops/:id/logs`
- 运行态 stop 走 `/api/v1/runs/active/:id/stop`

这样语义边界更清楚。

---

## 8. CLI 实现建议

建议在 `apps/cli/src/index.ts` 中：

1. `looper ps` 第一列显示 `#`
2. `loop pause` / `loop start` 支持直接输入数字 `seq`
3. 后续新增：

```txt
looper jump <id>
looper stop <id>
looper logs <id>
```

共享一个 selector helper：

- 纯数字 → `seq`
- 否则 → 真实 ID

---

## 9. 测试建议

## 9.1 migration / storage

- migration 后 `loops.seq` 存在且唯一
- 历史 loop 回填顺序稳定
- `allocateSeq()` 单调递增
- `getBySeq()` 正确返回 loop

## 9.2 API

- `GET /api/v1/runs/active` 返回 `seq`
- loop 路由可通过数字 `seq` 命中
- `POST /api/v1/runs/active/:id/stop` 可通过数字 `seq` 命中
- `worktree` 拼装正确
- `stop` 会 pause loop 并终止当前 active execution
- `logs` 在有 agent / 无 agent 两种情况下都能返回合理结果

## 9.3 CLI

- `looper ps` 第一列为 `#`
- `loop pause 12` / `loop start 12` 可解析到目标 loop
- `jump/logs/stop` 默认接受数字 ID
- `--json` 同时返回 `seq` 和真实 ID

---

## 10. 推荐分阶段落地

### Phase 1：持久数字 ID 基础设施

- migration
- `loops.seq`
- storage lookup / allocation
- `LoopRecord.seq` 接入
- `ps` 默认显示数字 ID
- 现有 loop 路由支持数字 ID

### Phase 2：只读管理命令

- `jump`
- `logs`
- active-run detail API

### Phase 3：可变更管理动作

- `stop`
- runtime stop bridge
- 命令确认与错误提示

### Phase 4：高级交互

- `logs --follow`
- SSE / streaming
- 真正的 PTY attach（若未来重新引入 attach）

---

## 11. 最终建议

这件事最小、最稳、也最符合用户心智的做法是：

1. 给 `loop` 引入持久数字 ID `seq`
2. 让 `looper ps` 第一列显示它
3. 让 `jump/stop/logs` 全部优先接受它
4. 内部继续用 UUID 做真实主键与关联
5. `jump` 通过 shell integration 实现“直接跳转”体验
6. `logs` v1 收口为带上下文的 snapshot viewer
7. `stop` 通过 runtime bridge 落到真实进程控制

这样用户拿到的是一个**真正可记、可复制、可长期引用**的数字入口，而不是每次刷新都会变的临时编号。
