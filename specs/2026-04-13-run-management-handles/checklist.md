# Run Management Numeric ID Checklist

## Phase 1 - 持久 loop 数字 ID 基础设施

- [ ] 为 `loops` 表增加 `seq`
  - [ ] `seq` 为持久数字 ID
  - [ ] 全局唯一
  - [ ] 单调递增
  - [ ] 允许空洞，不要求连续无缺口
  - [ ] `LoopRecord` 增加 `seq: number`

- [ ] 增加 migration
  - [ ] 为历史 loops 回填 `seq`
  - [ ] 新增 `counters` 表或等价计数器机制
  - [ ] 初始化 `loop_seq` 为 `MAX(seq)`
  - [ ] 为 `loops.seq` 建唯一索引
  - [ ] 明确采用 SQLite table rebuild 方案

- [ ] 扩展 storage 能力
  - [ ] `loops.getBySeq(seq)`
  - [ ] `loops.allocateSeq()` 或等价接口
  - [ ] `seq` 分配在事务内完成

## Phase 2 - active run 聚合视图

- [ ] 扩展 `/api/v1/runs/active` 返回结构
  - [ ] 增加 `seq`
  - [ ] 增加 `worktree` 摘要
  - [ ] 保持 `loopId` / `runId` / `agent.executionId` 原样可见

- [ ] 抽出统一 active-run descriptor builder
  - [ ] `ps` 复用它
  - [ ] 后续 `jump/logs/stop` 也复用它
  - [ ] worktree 优先从 `checkpointJson` 解析，其次回退 `loop.metadataJson`

## Phase 3 - selector 解析

- [ ] 统一支持数字 ID 解析
  - [ ] 纯数字输入按 `loop.seq` 解析
  - [ ] 非纯数字输入回退为真实 ID
  - [ ] 现有 loop 路由支持 `seq`

- [ ] 增加 active run detail 路由
  - [ ] `GET /api/v1/runs/active/:id`
  - [ ] `:id` 支持 `seq`

## Phase 4 - `looper ps` UX

- [ ] 默认表格增加 `#` 列
  - [ ] `#` 放在第一列
  - [ ] 默认不再强调长 `runId`
  - [ ] `age` / `target` / `step` 继续保留

- [ ] `--json` 输出补齐数字 ID 与上下文
  - [ ] `seq`
  - [ ] `worktree`

## Phase 5 - 只读管理命令

- [ ] 新增 `looper jump <id>`
  - [ ] 默认输出可供 `eval` 执行的 shell 片段
  - [ ] 提供官方 shell integration（zsh/bash/fish 至少一种）
  - [ ] `--print-path` 输出 worktree path
  - [ ] `--json` 输出 seq / path / branch / projectId

- [ ] 新增 `looper logs <id>`
  - [ ] 默认查看 latest run 的 latest agent execution
  - [ ] 返回 logs metadata envelope，而不是仅裸 stdout/stderr
  - [ ] 默认人类模式输出 stdout tail
  - [ ] 支持 `--stderr`
  - [ ] 支持 `--tail <n>`
  - [ ] 支持 `--full`
  - [ ] 无 active execution 时返回 `agent = null`
  - [ ] 支持 `--json`

## Phase 6 - 可变更管理命令

- [ ] 新增 `looper stop <id>`
  - [ ] 定义 `RuntimeController.stopLoop(...)` 或等价 runtime bridge
  - [ ] pause target loop
  - [ ] kill active execution（如果存在）
  - [ ] 更新当前 active run 终态
  - [ ] 记录用户 stop event
  - [ ] v1 为当前本地 CLI vendor 统一走 `SIGTERM -> SIGKILL` 退避策略
  - [ ] 记录 vendor / pid 审计字段
  - [ ] 为后续 vendor-native cancel 预留扩展接口

## Phase 7 - 测试

- [ ] migration / storage 测试
  - [ ] 历史 loop 回填顺序稳定
  - [ ] `seq` 唯一
  - [ ] `allocateSeq()` 单调递增
  - [ ] `getBySeq()` 正确

- [ ] API 测试
  - [ ] `GET /api/v1/runs/active` 返回 `seq`
  - [ ] loop 路由通过 `seq` 可命中
  - [ ] `POST /api/v1/runs/active/:id/stop` 通过 `seq` 可命中
  - [ ] `GET /api/v1/loops/:id/logs` 返回 envelope 结构正确
  - [ ] `logs` 默认命中 latest run + latest execution
  - [ ] `logs` 在无 agent step 时正确返回 `agent = null`
  - [ ] `stop` 行为正确
  - [ ] `stop` 的 vendor / pid 审计字段正确

- [ ] CLI 测试
  - [ ] `ps` 第一列为 `#`
  - [ ] `loop pause <seq>` / `loop start <seq>` 工作正常
  - [ ] `jump/logs/stop` 默认接受数字 ID
  - [ ] 空态和错误态文案清晰
  - [ ] `jump` shell integration 生成结果正确
  - [ ] `logs` 默认输出 tail 视图
  - [ ] `logs --stderr/--tail/--full/--json` 行为正确

## Out of scope for this spec

- [ ] 真正的双向交互式 PTY attach
- [ ] `logs --follow` / SSE streaming
- [ ] 新的持久化 session 领域模型
