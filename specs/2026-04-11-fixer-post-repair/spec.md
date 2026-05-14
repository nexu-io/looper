# Fixer Post-Repair Reconciliation 方案

## 1. 背景

当前 Fixer 的主流程是：

1. `discover-pr`
2. `claim-pr`
3. `collect-fixes`
4. `repair`
5. `validate`
6. `push`
7. `recheck`

其中 `repair` 会调用 Coding Agent 进入 worktree 修改代码。

问题在于：**commit / push / resolve review comments** 这类动作到底应由 agent 自己完成，还是由 `looperd` 程序化执行。

本方案结论：

- Agent 只负责**理解问题并修改代码**
- `looperd` 负责**commit / push / resolve comments / 状态对齐**
- Fixer 必须在**独立临时 worktree** 中执行，不直接写用户主工作区
- 不新增独立的 `reconciler` loop type

---

## 2. 设计结论

### 2.1 不引入新的 `reconciler` 角色

不将“修复后的收尾工作”抽成与 `reviewer / fixer / worker` 并列的新角色。

原因：

- 这类工作没有独立的发现/调度生命周期
- 主要是确定性副作用，不需要 LLM 推理
- 它天然依赖 Fixer 的 `repair` 输出，属于 **Fixer 内部 phase**，不是新的 loop type

因此应把这部分能力做成 **Fixer 的后处理 steps**。

### 2.2 Agent 与 Orchestrator 的职责边界

#### Agent 负责

- 阅读 `FixItem[]`
- 在 worktree 中做代码修改
- 输出执行摘要

#### looperd 负责

- 为 Fixer 创建和回收独立 worktree
- 检查 agent 退出后的 git 状态
- 必要时创建 commit
- push 分支
- resolve 已处理的 review threads
- 记录结构化执行结果、幂等状态和事件

---

## 3. 新的 Fixer 流程

建议将流程调整为：

1. `discover-pr`
2. `claim-pr`
3. `collect-fixes`
4. `prepare-worktree`
5. `repair`
6. `reconcile-commits`
7. `validate`
8. `push`
9. `resolve-comments`
10. `recheck`

约束：

- `prepare-worktree` 负责 worktree 的创建 / 复用 / reset / head 校验
- `repair` 仍然是 **Fixer 中唯一允许调用 agent 的 step**
- `reconcile-commits`、`push`、`resolve-comments` 都必须是程序化步骤
- `resolve-comments` 只能在 `validate` 与 `push` 成功之后执行
- 所有 mutating steps 都只允许作用于 Fixer 专属 worktree，不允许直接修改 `project.repoPath`

---

## 4. 用户手动工作流隔离

Fixer 的设计必须默认假设：用户可能同时在主 checkout 中手动修改代码。

因此第一原则是：

**Looper 不能直接在用户主工作区上执行 Fixer。**

### 4.1 隔离原则

- `project.repoPath` 视为用户工作区
- Fixer 必须为每个目标 PR 创建独立 worktree
- agent、validate、commit、push、resolve-comments 全部在该 worktree 中完成
- 主工作区中的未提交修改默认视为用户拥有，Fixer 不应试图消费、整理或提交

### 4.2 推荐 worktree 形态

建议为 Fixer 引入与 Worker 类似的 worktree 机制。

示例命名：

```text
<worktreeRoot>/looper-fix-<projectId>-pr-<prNumber>
```

branch 策略：

- Fixer 针对现有 PR 工作，因此通常仍跟踪该 PR 的 `headRefName`
- 但执行位置应是 Fixer 自己的临时 worktree，而不是用户当前 checkout

worktree 复用策略：

- 若该 PR 的 Fixer worktree 已存在，可以复用其目录
- 但每次进入新一轮执行前，必须先同步远端并校验目标 branch/head
- 不允许直接沿用未知来源的旧 worktree 状态进入 `repair`

推荐行为：

1. fetch 目标远端分支
2. 校验当前期望的 `headSha`
3. 必要时将 worktree reset 到 `origin/<headRefName>`
4. 再进入后续步骤

### 4.3 最小检测策略

即使已使用 worktree，仍建议加以下保护：

1. `repair` 前记录并校验目标 branch 的预期 `headSha`
2. worktree 中启动 agent 前，检查工作区是否干净
3. `push` 前再次比较远端 head 是否已变化
4. 若远端被用户或其他外部流程推进，Fixer 应中止当前轮次，并以可重试失败结束，由下一轮重新评估

### 4.4 可区分性原则

是否“某个文件内容由用户改还是 agent 改”并不总能可靠判断。

因此第一阶段不以内容归因为核心，而以**执行空间隔离**为核心：

- 主 checkout 中的改动：默认视为用户改动
- Fixer worktree 中的改动：默认视为本轮 Fixer 改动

这比事后根据 diff 猜测来源更可靠。

---

## 5. 为什么不让 agent 负责 commit / push / resolve

## 5.1 commit

如果 commit 交给 agent：

- `allowAutoCommit` 形同虚设
- 很难区分“agent 改了文件但没 commit”和“agent 已 commit”
- commit message、事件记录、恢复语义都不稳定

更合理的做法：

- 允许 agent 专注于改代码
- agent 退出后由 `looperd` 检查 git 状态
- 如果 agent 没有生成 commit，则由 `looperd` commit
- 如果 agent 已经生成 commit，则记录并接管后续步骤

也就是说：**commit 的最终权威在 `looperd`，但对 agent 自发 commit 保持兼容**。

## 5.2 push

push 属于明显的外部副作用，必须由 `looperd` 控制：

- 便于受 `allowAutoPush` 控制
- 便于保护分支策略统一生效
- 便于失败重试和事件审计

## 5.3 resolve comments

resolve comment 本质上是在声明“这个 review thread 已经被正确处理”。

这个动作不应由 agent 自证，应由 `looperd` 在以下条件成立后执行：

1. `repair` 已完成
2. `validate` 已通过
3. `push` 已成功

这样可以避免：

- agent 在修复未验证时过早 resolve
- 只 resolve 了一部分 thread 后流程失败，导致 GitHub 状态错乱
- 重试时无法准确判断哪些 comment 已经被处理

---

## 6. 详细步骤设计

## 6.1 `prepare-worktree`

这是新增 step，用于为后续 `repair` 提供一个可恢复、可验证、可隔离的执行空间。

职责：

- 创建或恢复该 PR 对应的 Fixer worktree
- fetch 目标远端分支
- 校验 `headRefName` 与预期 `headSha`
- 必要时 reset 到远端最新目标 head
- 记录 worktree 路径、branch、base head 等信息到 checkpoint
- 在进入 `repair` 前确保 worktree 是干净的

输出：

- `worktree.path`
- `worktree.branch`
- `worktree.headSha`
- `worktree.preparedAt`

失败策略：

- 若 worktree 无法恢复到预期 head，则中止当前轮次
- 若发现不可解释的脏状态，直接要求人工介入，不自动清理

## 6.2 `repair`

输入：

- PR 基本信息
- `headSha`
- 结构化 `FixItem[]`

职责：

- 仅修改代码
- 不要求 agent commit / push / resolve comments
- 允许 agent 为了判断某条 review feedback 是否已被当前分支真实覆盖，而重新读取相关 comment / thread 上下文
- 禁止 agent 修改 GitHub review state（包括 reply / resolve thread / submit review / 改 PR 元数据）
- 允许 agent 输出 completion marker summary
- 只允许在 Fixer worktree 中执行，不直接使用 `project.repoPath`

输出：

- agent summary
- agent stdout/stderr
- parse status

## 6.3 `reconcile-commits`

这是新增 step，用于把 agent 修改后的工作区状态收敛成可恢复、可观测的 git 状态。

职责：

1. 检查当前 `HEAD` 是否相对 `repair` 前发生变化
2. 检查工作区是否仍存在未提交改动
3. 如存在未提交改动且允许 auto-commit，则由 `looperd` 执行 commit
4. 记录本轮最终 commit SHA 列表
5. 记录这些 commit 是 agent 产生的，还是 looperd 补交的

建议采集的信息：

- `baseHeadSha`: 进入 `repair` 前的 head
- `finalHeadSha`: `reconcile-commits` 结束时的 head
- `newCommitShas`: 本轮新增 commits
- `committedByAgent`: boolean
- `committedByLooperd`: boolean
- `workingTreeClean`: boolean

行为约束：

- 若没有文件变化且没有新 commit，step 成功结束，但标记为 no-op
- 若存在未提交变化且 `allowAutoCommit = false`，step 失败并给出明确原因
- commit message 由 `looperd` 统一生成，不依赖 agent 文本
- 若 agent 产生了多个 commit，第一阶段不做 squash，直接记录全部新增 commit SHA

推荐 commit message：

```text
fixer: address PR #<number> follow-up items
```

如后续需要，可把 fix item 摘要附在 body 或 trailer 中，但第一阶段不强制。

## 6.4 `validate`

在 `reconcile-commits` 之后执行。

职责：

- 跑验证命令
- 检查 git 状态是否仍可接受
- 检查冲突是否消失

建议新增约束：

- 验证通过后，工作区应保持干净；若验证步骤产生额外变更，必须再次失败或显式纳入后续 reconcile 策略

第一阶段建议：

- `reconcile-commits` 后应尽量让工作区干净
- 若 `validate` 产生新的文件修改，可允许一次额外的 `reconcile-commits -> validate` 收敛机会
- 若经过一次补充收敛后仍继续产生新修改，则判定失败，避免进入无限循环

## 6.5 `push`

继续由程序化步骤完成。

前置条件：

- `validate.passed === true`
- 至少存在一个本轮可追踪的 commit 需要同步到远端，或远端分支落后于本地

职责：

- push 当前分支
- 记录 push 成功时间、remote、branch

并发变化处理：

- 若 push 前发现远端 head 已变化，则当前 run 以可重试瞬时失败结束
- 不做 force push
- 下一轮由 `discover-pr / collect-fixes / prepare-worktree` 基于新状态重新收敛

## 6.6 `resolve-comments`

这是新增 step。

职责：

- 仅对本轮 `FixItem[]` 中 `type === 'comment'` 的项尝试 resolve
- 只 resolve 当前仍处于 unresolved 状态的 thread
- 逐条记录 resolve 结果，支持重试跳过已完成项

前置条件：

- `validate` 已通过
- `push` 已成功
- 对于每条准备 resolve 的 comment thread，必须已有该轮 agent 明确回传的 per-thread confirmation（例如 `review_thread_replies` / 持久化 explanation），否则只记录未完成状态，不自动 resolve

失败策略：

- 某条 thread resolve 失败时，记录失败原因并允许 step 重试
- 不要因为单个 thread 已被他人提前 resolve 就报错；应视为幂等成功

---

## 7. Checkpoint 扩展建议

建议为 Fixer checkpoint 增加明确的后处理状态块。

示意：

```ts
type FixerCheckpoint = {
  // existing fields...
  worktree?: {
    path?: string
    branch?: string
    headSha?: string
    baseHeadSha?: string
    preparedAt?: string
  }
  repair?: {
    agentExecutionId?: string
    summary?: string
    parseStatus?: 'parsed' | 'missing' | 'invalid_json'
    completedAt?: string
  }
  reconcileCommits?: {
    baseHeadSha?: string
    finalHeadSha?: string
    newCommitShas: string[]
    committedByAgent: boolean
    committedByLooperd: boolean
    workingTreeClean: boolean
    completedAt?: string
  }
  push?: {
    branch?: string
    remote?: string
    pushedAt?: string
  }
  resolvedComments?: {
    items: Array<{
      fixItemId: string
      threadId?: string
      status:
        | 'resolved'
        | 'already_resolved'
        | 'failed'
        | 'skipped_no_evidence'
        | 'skipped_no_confirmation'
        | 'reply_failed'
        | 'stale_state'
      message?: string
      updatedAt: string
    }>
  }
}
```

设计原则：

- 每个 step 都能从 checkpoint 判断“是否已完成”
- 每个外部副作用都要留下结构化结果
- 对重试要友好：已成功的 comment resolve 不能重复报错

---

## 8. Gateway / Infra 变更

## 8.1 Git gateway

Fixer 现有 git gateway 需要从“只 push”扩展为同时支持 commit 对齐。

建议新增能力：

```ts
interface FixerGitGateway {
  createWorktree(input: {
    projectId: string
    repoPath: string
    worktreeRoot: string
    branch: string
    baseBranch?: string
    prNumber: number
    protectedBranches?: string[]
  }): Promise<{
    path: string
    branch: string
    headSha?: string
  }>

  prepareWorktree(input: {
    worktreePath: string
    branch: string
    expectedHeadSha?: string
    remote?: string
  }): Promise<{
    headSha?: string
    clean: boolean
  }>

  inspectHead(input: { worktreePath: string; baseRef?: string }): Promise<{
    headSha?: string
    newCommitShas: string[]
    hasUncommittedChanges: boolean
    changedFiles: string[]
  }>

  commit(input: {
    worktreePath: string
    message: string
  }): Promise<{ commitSha: string }>

  push(input: {
    worktreePath: string
    branch: string
    remote?: string
    protectedBranches?: string[]
  }): Promise<void>
}
```

注意：

- `createWorktree` 应尽量复用已有 worktree 时做状态校验，而不是盲目重建
- `prepareWorktree` 应明确承担 fetch / head 校验 / reset / clean-check 职责
- `inspectHead` 应尽量返回确定性 git 视图，而不是依赖 agent 上报
- `commit` 内部应只提交当前 worktree 改动，不改动全局配置

## 8.2 GitHub gateway

建议新增 resolve review thread 能力，例如：

```ts
interface FixerGitHubGateway {
  // existing methods...
  resolveReviewThread(input: {
    repo: string
    threadId: string
    cwd?: string
  }): Promise<void>
}
```

`FixItem` 的 `comment` 类型当前只有 `id + summary`。若该 `id` 不是可直接 resolve 的 thread id，则需要在 `collect-fixes` 阶段保存足够的信息，使 `resolve-comments` 阶段能够确定唯一线程。

这意味着需要二选一：

1. 扩展 `viewPullRequest` 返回 review thread 级信息
2. 或在 `collect-fixes` 阶段追加一次 thread 级查询

第一阶段只要能稳定拿到 `threadId` 即可，不强制限定来源实现。

因此建议把 comment 型 FixItem 扩展为可稳定映射到 review thread：

```ts
type FixItem =
  | { type: 'comment'; id: string; threadId: string; summary: string }
  | { type: 'check'; name: string; summary: string }
  | { type: 'conflict'; files: string[] }
```

如果当前 GitHub 数据源还拿不到 `threadId`，则第一阶段可以先在 collect-fixes 时保存 comment id -> thread id 的映射快照。

---

## 9. 幂等与恢复策略

## 9.1 `prepare-worktree` 重试

- 若 worktree 已存在，可重复进入并重新执行 prepare
- 若 prepare 过程中发现远端 head 已变化，则当前 run 以可重试失败结束
- 若 worktree 脏且无法确认来源，则要求人工介入

## 9.2 `repair` 重试

- 若 `repair` 失败且没有新的 commit / 文件变化，可直接重试
- 若 `repair` 已产生新 commit，则后续重试前必须重新评估 head 是否变化

如果发现 worktree 已脏但不确定来源，应停止自动推进并要求人工介入，而不是尝试自动清理。

## 9.3 `reconcile-commits` 重试

- 若已经记录 `finalHeadSha` 且工作区干净，则视为完成
- 若之前 commit 成功但 checkpoint 未写入，可通过 `baseHeadSha..HEAD` 重新发现新增 commits

## 9.4 `push` 重试

- 允许因网络/临时远端错误重试
- 若远端已包含相同 commit，应视为幂等成功
- 若 push 前检测到远端 head 已变化，应中止并重新评估，不做 force push

## 9.5 `resolve-comments` 重试

- 对已 resolve 的 thread 视为幂等成功
- 对找不到的 thread 需要区分“已不存在 / 数据过期 / 权限问题”
- 若缺少已验证 evidence，则记录为 `skipped_no_evidence`，进入后续重试/回看流程
- 若缺少 agent 的 per-thread confirmation，则记录为 `skipped_no_confirmation`，进入后续重试/回看流程
- `recheck` 应再次读取 PR 状态，作为最终校正

---

## 10. Prompt 约束调整

Fixer prompt 应明确避免让 agent 承担 orchestrator 副作用。

建议补充约束：

- 只做代码修复，不要主动 push 分支
- 不要 resolve GitHub comments / reviews
- 如需要 commit，可尽量避免；即使 agent 产生 commit，looperd 也会在后续 step 做对齐

但这里要保持语气温和，避免与底层 agent 的自然工作流强冲突。

推荐方向：

```text
Focus on code changes needed for the listed fix items. Avoid pushing branches or changing remote review state; Looper will handle follow-up repository actions after your edits.
```

是否禁止 agent commit 可以先不写死；第一阶段以“兼容 agent 已 commit”更稳妥。

---

## 11. 第一阶段实施范围

必须实现：

- Fixer worktree 隔离
- 新 step：`prepare-worktree`
- 新 step：`reconcile-commits`
- 新 step：`resolve-comments`
- Fixer checkpoint 扩展
- Git gateway 扩展：createWorktree + prepareWorktree + inspect + commit
- GitHub gateway 扩展：resolve review thread
- 获取稳定 `threadId` 的数据链路
- Fixer prompt 追加“不要处理远端副作用”的说明
- Fixer worktree 在 loop terminal state 时清理

可暂缓：

- 基于 agent 上报和 git 实际状态做更复杂的合并策略
- comment resolve 的批量优化
- 更复杂的 commit message 模板/分类
- 根据 changedFiles 自动做更细粒度通知
- 基于 TTL 的 worktree reaper
- agent 多 commit 的 squash / 整理策略

---

## 12. 验收标准

至少满足：

1. Fixer 全流程在独立 worktree 中完成，不直接修改用户主工作区
2. `prepare-worktree` 能在复用已有 worktree 时完成 fetch / head 校验 / 必要 reset
3. Fixer 的 `repair` step 结束后，即使 agent 没有 commit，流程也能继续完成 commit
4. 如果 agent 已经生成 commit，Fixer 能识别并记录，而不是重复生成混乱提交
5. `allowAutoCommit = false` 时，存在未提交改动会在 `reconcile-commits` 明确失败
6. `push` 仍由 looperd 执行，并受现有配置与保护分支策略控制
7. 远端 head 变化时不会 force push，而是安全中止并等待下一轮重试
8. `resolve-comments` 只在验证和 push 成功后执行
9. 已被 resolve 的 comment 在线程重试时不会报错
10. 整个后处理过程具备 checkpoint 可恢复性和事件可审计性

---

## 13. 一句话原则

**Fixer 的 agent 负责在隔离 worktree 中“改代码”；Fixer 的 looperd 步骤负责“确认副作用并对外落账”。**
