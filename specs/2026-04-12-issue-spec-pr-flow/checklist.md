# Issue → Spec PR → Review → Worker Flow Checklist

## Phase 0 - 范围与约束先定死

- [x] 明确 v1 固定采用 `same_pr` 策略
- [x] 明确双 PR (`separate_pr`) 仅作为 Phase 2 可配置方向，不进入当前实现范围
- [x] 明确 Phase 1 不实现 Ralph Loop 单 run 内部循环，直接复用现有外部循环
- [x] 明确 Planner discover 规则：`looper:plan` + assign 给当前 GitHub 用户
- [x] 明确 repo → project 映射规则：一个 repo 只能命中一个 active project，否则拒绝认领
- [x] 明确 spec 文件约定：`specs/<issue-number>-<slug>/spec.md`
- [x] 明确 Worker 读取 spec 的来源优先级：PR body / metadata 中的显式 `specPath`
- [x] 明确 v1 implementation 阶段状态机：Worker 开始时移除 `looper:spec-ready`
- [x] 明确 v1 implementation 阶段不新增 implementation label，直接复用现有 reviewer / review-request 机制
- [x] 明确 Worker push 后如需 Reviewer 自动介入，由 Worker 主动补 reviewer
- [x] 明确 v1 是否实现 `looper:needs-human` label（建议实现，作为兜底）
- [x] 若 v1 不加 `looper:needs-human` label，确认内部 `manual_intervention` / `paused` 路径可直接复用

## Phase 1 - GitHub Gateway 基础能力

- [x] 在 `apps/looperd/src/infra/github.ts` 增加 issue list 能力
- [x] `listIssues` 必须支持 `labels` + `assignee` 过滤参数（spec discovery 依赖）
- [x] 在 `apps/looperd/src/infra/github.ts` 增加 issue detail/view 能力（建议）
- [x] 在 `apps/looperd/src/infra/github.ts` 增加 PR add label 能力
- [x] 在 `apps/looperd/src/infra/github.ts` 增加 PR remove label 能力
- [x] 在 `apps/looperd/src/infra/github.ts` 增加 PR add reviewer 能力
- [x] 在 `apps/looperd/src/infra/github.ts` 增加按 label 过滤 open PR 的能力
- [x] 为新增 GitHub gateway 能力补齐单测

## Phase 2 - Domain / Scheduler / Runtime 基础改造

- [x] 在 `apps/looperd/src/domain/index.ts` 增加 `planner` loop type
- [x] 在 `apps/looperd/src/domain/index.ts` 增加 `issue` target type
- [x] 在 domain 中新增 `IssueLoopTarget` 接口（含 `repo` + `issueNumber`），并扩展 `LoopTarget` union
- [x] 在 domain 中新增 `PLANNER_STEPS` 定义，并将其加入 `LOOP_STEPS_BY_TYPE` 和 `LoopStep` union
- [x] 调整 `assertLoopTypeMatchesTarget`：允许 `worker + pull_request`，允许 `planner + issue`
- [x] 检查并更新 loop target key / unique active loop 相关约束测试
- [x] 在 `scheduler/index.ts` 的 `QUEUE_LOOP_PRIORITIES` 中增加 `planner` 及其优先级值
- [x] 确定 `planner` 在 `QUEUE_LOOP_PRIORITIES` 中的优先级数值（建议高于 reviewer）
- [x] 确保 project 的 `repo` 字段在 startup 时被解析并持久化，供 Planner discover 做 repo → project 反向查找
- [x] 在 runtime `processScheduledWork` 中增加 `planner` 分支（与 reviewer/fixer/worker 并列）
- [x] 在 runtime 中新增 `discoverIssues` 调用 Planner discover
- [x] 为 domain / scheduler / runtime 基础改造补齐单测

## Phase 3 - Planner Loop

- [x] 新增 `apps/looperd/src/planner/index.ts`
- [x] 实现 `discover-issues`
- [x] 在 discover 中接入 `looper:plan` + assignee + repo/project 唯一映射校验
- [x] 实现 repo → projectId 反向查找：基于 project metadata 中缓存的 `repo` 字段或 startup 解析结果
- [x] 实现 `prepare-worktree`
- [x] 复用现有 worktree / git 基础设施创建 planner branch
- [x] 实现 `write-spec`
- [x] 定义 planner agent prompt 模板，包含：issue 标题、issue body、repo 上下文、spec 文件路径约定、AGENTS.md 内容（如存在）
- [x] 让 planner 将 spec 写到 `specs/<issue-number>-<slug>/spec.md`
- [x] 实现 `publish`
- [x] `publish` 内串行完成 push / open PR / add `looper:spec-reviewing` / add reviewers
- [x] `publish` 中每个子操作（push / open PR / add label / add reviewers）需独立幂等，支持中途失败后 resume
- [x] 在 PR body 或 metadata 中写入显式 `specPath`
- [x] 明确 Planner 完成后如何触发 Worker：创建新的 worker loop with `pull_request` target
- [x] 实现 `notify`
- [x] 为 Planner loop 补齐 checkpoint / resume / failure 路径测试

## Phase 4 - API / CLI 接入 Planner

- [x] 增加 Planner 启动入口（API）
- [x] 明确是自动 discover 驱动、手动触发，还是两者兼容
- [x] 明确 Phase 4 仅暴露手动触发入口；自动 discover 驱动在完整链路验收前不默认启用
- [x] 如需 CLI，增加对应 planner 命令或最小触发入口
- [x] 在状态接口 / 列表接口中暴露 planner loop 可见性
- [x] 为 API / CLI 入口补齐测试

## Phase 5 - Reviewer 支持 Spec Review 阶段

- [x] 在 reviewer `discoverPullRequests` 中新增 label-based 发现分支（`looper:spec-reviewing`），与现有 review-request 发现并存
- [x] 明确两个 discover 来源如何去重
- [x] Reviewer prompt 增加 phase awareness
- [x] 让 Reviewer 能识别当前审查对象是 spec 还是 implementation
- [x] 为 spec-reviewing discover 与 phase-aware review 补齐测试

## Phase 6 - Fixer / Review Clean 后的 Label 切换

- [x] 定义 spec review clean 条件：`unresolvedThreadCount === 0` 且无 `REQUEST_CHANGES` review（与 fixer 的 collectFixItems 逻辑对齐）
- [x] 明确由 Reviewer、Fixer，还是共享 helper 执行 label 切换
- [x] label 切换前复用现有 PR lock（`pr:<repo>:<prNumber>`），不新增独立 lock key
- [x] 切换逻辑保证幂等
- [x] 实现 `looper:spec-reviewing` -> `looper:spec-ready`
- [x] 为 label 切换成功、重复切换、竞态冲突补齐测试

## Phase 7 - Worker 支持接管已有 Spec PR

- [x] 在 Worker 或 runtime 中新增 `looper:spec-ready` PR 的 discover 逻辑
- [x] 在 runtime 中接入 Worker 对 `looper:spec-ready` 的 discover 调用
- [x] 在 `apps/looperd/src/worker/index.ts` 增加 `pull_request` target 模式
- [x] 在 `prepare-work` 中确定当前模式：`create-pr` vs `push-existing`
- [x] 在 `prepare-work` 中把 execution mode（`create-pr` | `push-existing`）写入 checkpoint，后续 step 通过 mode 分支
- [x] 在 `prepare-worktree` 中复用 Fixer 的现有 PR branch worktree 模式
- [x] 读取 PR body / metadata 中显式记录的 `specPath`
- [x] 确保 PR 模式下 `readSpecBlock` 从 worktree 路径读取 spec，而非 `projectRepoPath`
- [x] `execute` 基于 spec PR 中的 spec 继续实现
- [x] Worker agent prompt 中明确指示不要修改 spec 文件（v1 先做 soft protection）
- [x] `validate` 在 PR 模式下继续生效
- [x] 将 `open-pr` 逻辑改为 target-sensitive：
  - [x] `project` 模式保留 `open-pr`
  - [x] `pull_request` 模式改为 push existing PR
- [x] Worker 开始执行时移除 `looper:spec-ready`，避免重复认领
- [x] Worker push 完成后不新增 implementation label，直接依赖现有 reviewer / review-request 机制
- [x] 如需 Reviewer 自动介入，Worker push 后调用 `addReviewersToPullRequest`
- [x] 为 Worker dual-mode 补齐 checkpoint / resume / push 路径测试

## Phase 8 - Implementation 阶段 Review 行为补全

- [x] 让 implementation 阶段的 Reviewer 同样具备 phase awareness
- [x] 将 v1 的 implementation 阶段最小状态机回填到 spec 与实现说明

## Phase 9 - 兜底与失败恢复

- [x] 定义 review / fix 循环不收敛的判定阈值
- [x] 明确连续失败后的降级策略
- [x] 为 planner / reviewer / fixer / worker 的失败恢复路径补齐测试

## Phase 10 - 端到端验收

- [x] 准备一条真实或仿真的 issue → spec PR → spec-ready → implementation 场景
- [x] 验证 issue discover 只命中符合规则的 issue
- [x] 验证 Planner 能稳定创建 spec 文件与 spec PR
- [x] 验证 Reviewer / Fixer 能把 PR 从 `looper:spec-reviewing` 推进到 `looper:spec-ready`
- [x] 验证 Worker 能在同一 PR 上继续实现并 push
- [x] 验证 implementation 后仍能进入后续 review 流程
- [x] 将 acceptance flow 结果回填到 spec 或补充单独验收记录
