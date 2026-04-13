# Run Management Numeric ID Checklist

## Phase 1 - 持久 loop 数字 ID 基础设施

- [x] 为 `loops` 表增加 `seq`
  - [x] `seq` 为持久数字 ID
  - [x] 全局唯一
  - [x] 单调递增
  - [x] 允许空洞，不要求连续无缺口
  - [x] `LoopRecord` 增加 `seq: number`

- [x] 增加 migration
  - [x] 为历史 loops 回填 `seq`
  - [x] 新增 `counters` 表或等价计数器机制
  - [x] 初始化 `loop_seq` 为 `MAX(seq)`
  - [x] 为 `loops.seq` 建唯一索引
  - [x] 明确采用 SQLite table rebuild 方案

- [x] 扩展 storage 能力
  - [x] `loops.getBySeq(seq)`
  - [x] `loops.allocateSeq()` 或等价接口
  - [x] `seq` 分配在事务内完成

## Phase 2 - active run 聚合视图

- [x] 扩展 `/api/v1/runs/active` 返回结构
  - [x] 增加 `seq`
  - [x] 增加 `worktree` 摘要
  - [x] 保持 `loopId` / `runId` / `agent.executionId` 原样可见

- [x] 抽出统一 active-run descriptor builder
  - [x] `ps` 复用它
  - [x] 后续 `jump/logs/stop` 也复用它
  - [x] worktree 优先从 `checkpointJson` 解析，其次回退 `loop.metadataJson`

## Phase 3 - selector 解析

- [x] 统一支持数字 ID 解析
  - [x] 纯数字输入按 `loop.seq` 解析
  - [x] 非纯数字输入回退为真实 ID
  - [x] 现有 loop 路由支持 `seq`

- [x] 增加 active run detail 路由
  - [x] `GET /api/v1/runs/active/:id`
  - [x] `:id` 支持 `seq`

## Phase 4 - `looper ps` UX

- [x] 默认表格增加 `#` 列
  - [x] `#` 放在第一列
  - [x] 默认不再强调长 `runId`
  - [x] `age` / `target` / `step` 继续保留

- [x] `--json` 输出补齐数字 ID 与上下文
  - [x] `seq`
  - [x] `worktree`

## Phase 5 - 只读管理命令

- [x] 新增 `looper jump <id>`
  - [x] 默认输出可供 `eval` 执行的 shell 片段
  - [x] 提供官方 shell integration（zsh/bash/fish 至少一种）
  - [x] `--print-path` 输出 worktree path
  - [x] `--json` 输出 seq / path / branch / projectId

- [x] 新增 `looper logs <id>`
  - [x] 默认查看 latest run 的 latest agent execution
  - [x] 返回 logs metadata envelope，而不是仅裸 stdout/stderr
  - [x] 默认人类模式输出 stdout tail
  - [x] 支持 `--stderr`
  - [x] 支持 `--tail <n>`
  - [x] 支持 `--full`
  - [x] 无 active execution 时返回 `agent = null`
  - [x] 支持 `--json`

## Phase 6 - 可变更管理命令

- [x] 新增 `looper stop <id>`
  - [x] 定义 `RuntimeController.stopLoop(...)` 或等价 runtime bridge
  - [x] pause target loop
  - [x] kill active execution（如果存在）
  - [x] 更新当前 active run 终态
  - [x] 记录用户 stop event
  - [x] v1 为当前本地 CLI vendor 统一走 `SIGTERM -> SIGKILL` 退避策略
  - [x] 记录 vendor / pid 审计字段
  - [x] 为后续 vendor-native cancel 预留扩展接口

## Phase 7 - 测试

- [x] migration / storage 测试
  - [x] 历史 loop 回填顺序稳定
  - [x] `seq` 唯一
  - [x] `allocateSeq()` 单调递增
  - [x] `getBySeq()` 正确

- [x] API 测试
  - [x] `GET /api/v1/runs/active` 返回 `seq`
  - [x] loop 路由通过 `seq` 可命中
  - [x] `POST /api/v1/runs/active/:id/stop` 通过 `seq` 可命中
  - [x] `GET /api/v1/loops/:id/logs` 返回 envelope 结构正确
  - [x] `logs` 默认命中 latest run + latest execution
  - [x] `logs` 在无 agent step 时正确返回 `agent = null`
  - [x] `stop` 行为正确
  - [x] `stop` 的 vendor / pid 审计字段正确

- [x] CLI 测试
  - [x] `ps` 第一列为 `#`
  - [x] `loop pause <seq>` / `loop start <seq>` 工作正常
  - [x] `jump/logs/stop` 默认接受数字 ID
  - [x] 空态和错误态文案清晰
  - [x] `jump` shell integration 生成结果正确
  - [x] `logs` 默认输出 tail 视图
  - [x] `logs --stderr/--tail/--full/--json` 行为正确

## Out of scope for this spec

- [ ] 真正的双向交互式 PTY attach
- [ ] `logs --follow` / SSE streaming
- [ ] 新的持久化 session 领域模型
