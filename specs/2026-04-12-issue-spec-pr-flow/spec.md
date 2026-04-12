# Issue → Spec PR → Review → Worker Flow 改造方案

## 1. 目标

将当前 Looper 从“手动 `looper work` 启动 worker”升级为下面这条完整自动化链路：

1. 创建 GitHub issue，并分配给用户。
2. 用户本地 Planner 自动认领 issue，创建新 worktree，写 Spec，推送 spec PR。
3. 系统自动给 spec PR 添加 `looper:spec-reviewing`，提醒用户 review spec，并自动/手动补充 reviewer。
4. Reviewer 自动评审，Reviewer / Fixer 持续往返，直到问题清空。
5. 问题清空后移除 `looper:spec-reviewing`，添加 `looper:spec-ready`。
6. Worker 扫描到 `looper:spec-ready` 后，直接在该 PR 上继续实现并推送代码。

---

## 2. 当前实现现状

结合代码与现有 specs，可确认当前系统已经具备以下能力：

- **Reviewer loop**：`apps/looperd/src/reviewer/index.ts`
  - 已支持 PR discover / snapshot / agent review / publish。
- **Fixer loop**：`apps/looperd/src/fixer/index.ts`
  - 已支持 PR discover / collect fixes / worktree repair / validate / push / resolve comments。
- **Worker loop**：`apps/looperd/src/worker/index.ts`
  - 已支持 project 级 work 输入、创建 worktree、跑 agent、validate、自动 open PR。
- **GitHub 集成**：`apps/looperd/src/infra/github.ts`
  - 目前只覆盖 PR 查看、diff、review、create PR、resolve review thread。
- **Git / worktree 集成**：`apps/looperd/src/infra/git.ts`
  - 已有可靠的 worktree 创建、准备、push、cleanup 能力。
- **调度 / 状态机 / 存储**：`scheduler` / `storage` / `domain`
  - 已有 queue、dedupe、business lock、checkpoint、恢复、事件记录等基础设施。

当前系统主路径已经是：

- reviewer：围绕 **PR** 运转
- fixer：围绕 **PR** 运转
- worker：围绕 **project/work request** 运转

其中，`apps/looperd/src/domain/index.ts` 当前只允许：

- loop type：`reviewer | worker | fixer`
- loop target：`project | pull_request`

这比更早期的 task 方案已经前进了一步，但仍未进入“issue 驱动 + spec PR 驱动”的完整闭环。

---

## 3. 与目标流程的差距

### 3.1 Issue intake 还不存在

目标流程的第一步是“创建 issue 并分配用户”，随后由 Planner 自动认领。

当前缺口：

- `infra/github.ts` 没有 issue list / issue view / issue metadata API。
- 没有任何 loop 以 **issue** 为 target。
- 没有“assigned to me”或“label 命中”类型的 issue discovery。

结论：

> 当前系统没有 issue → planner 的入口链路。

### 3.2 Planner 角色还不存在

目标流程要求本地 Planner：

- 自动认领 issue
- 基于 issue 创建 worktree
- 写 spec
- 推送 spec PR
- 给 PR 打标并发起 review

当前缺口：

- `domain/index.ts` 没有 `planner` loop type。
- `worker/index.ts` 的语义是“实现工作并最终 open PR”，不是“根据 issue 先产出 spec PR”。
- CLI / API 没有 planner 入口。

结论：

> Planner 不是现有 worker 的轻微变体，而是一个新的 phase / loop type。

### 3.3 GitHub label / reviewer 编排还不存在

目标流程强依赖这些 GitHub 动作：

- 给 PR 添加 `looper:spec-reviewing`
- 移除 `looper:spec-reviewing`
- 添加 `looper:spec-ready`
- 自动或手动补 reviewer

当前缺口：

- `infra/github.ts` 没有 add/remove label 能力。
- `infra/github.ts` 没有 add reviewer 能力。
- Reviewer / Worker / Fixer 都没有 label 驱动发现逻辑。

结论：

> 当前系统还没有把 GitHub label 当作 phase handoff 信号来使用。

### 3.4 Reviewer 还不是 spec-reviewing label 驱动

当前 Reviewer 的 discover 逻辑位于 `apps/looperd/src/reviewer/index.ts`，核心是：

- 列出 open PR
- 过滤 draft / 非 open
- 仅处理“当前 gh 用户被 request review”的 PR

与目标流程的差距：

- spec review 阶段应该能直接由 `looper:spec-reviewing` 驱动。
- 不能只依赖“当前用户被 request review”。

结论：

> Reviewer 需要新增 label-based discovery，而不是只看 review request。

### 3.5 Worker 还不能“接管已有 spec PR”

这是当前最大的结构性缺口。

目标流程要求：

- Worker 扫描到 `looper:spec-ready`
- 直接在该 **已有 PR** 上继续实现
- 不新开第二个实现 PR

当前 Worker 语义：

- target 是 `project`
- 输入来自 `/api/v1/workers`
- `prepare-worktree` 创建新 branch
- `open-pr` 为本轮工作创建新的 PR

这与目标流程冲突，因为目标流程要求：

- Worker target 必须允许是 `pull_request`
- Worker worktree 必须直接跟随现有 PR branch
- Worker 结束动作应是 `push` 到现有 PR，而不是 `open-pr`

结论：

> Worker 必须从“创建 PR 的执行器”升级为“既能开 PR，也能在已有 PR 上继续工作”的执行器。

### 3.6 现阶段不引入 Ralph Loop 内循环

目标流程里提到“通过 Ralph Loop 持续完成工作”，但当前代码里没有这个概念。

本方案明确收口为：

- **Phase 1 不实现 Worker 单 run 内部多轮 agent loop**
- 持续推进能力直接复用现有外部循环：
  - worker push
  - reviewer review
  - fixer repair
  - 必要时 worker 再次运行

结论：

> Ralph Loop 作为单 run 内部循环机制，不纳入本次改造范围。

---

## 4. 推荐的目标架构

## 4.1 phase 边界

建议把完整流程拆成四个 phase：

1. **Issue intake / planning**
2. **Spec review**
3. **Spec ready handoff**
4. **Implementation on same PR**

每个 phase 的“执行状态”继续由 SQLite 里的 loop / run / queue 管理；
每个 phase 的“跨阶段信号”用 GitHub label 表达。

即：

- **内部 source of truth**：loop / run / checkpoint / queue / locks
- **外部 handoff signal**：PR labels

这是最小改造、且与现有架构最兼容的方案。

## 4.2 新 loop / target 建议

### 新增 loop type

- `planner`

### 新增 target type

- `issue`

### Worker target 放宽

当前 `worker` 只应对 `project`；建议改为允许：

- `project`
- `pull_request`

对应语义：

- `worker + project`：从 work request 启动，必要时创建 PR
- `worker + pull_request`：接管现有 spec PR，在同一 PR 上继续实现

### 4.3 issue discovery 与 project 映射

Planner 不能扫描“所有 assigned issues”，否则 scope 太宽、误触发风险高。

本方案建议直接定义 v1 discovery 规则：

- issue 必须带 `looper:plan` label
- issue 必须 assign 给当前 GitHub 用户
- issue 所属 repo 必须能唯一映射到本地某个 `projectId`

即只有满足以上三个条件的 issue 才会进入 Planner discover 范围。

关于 repo → project 映射，v1 建议要求：

- 一个 repo 只能对应一个 active looper project
- 若同一 repo 命中多个 project，Planner 应拒绝认领并报错，而不是猜测归属

这样可以避免 issue intake 阶段引入隐式歧义。

### 4.4 spec 文件约定

Planner 写出的 spec 必须有稳定路径，供后续 Worker 直接读取。

建议约定：

```txt
specs/<issue-number>-<slug>/spec.md
```

例如：

```txt
specs/123-worker-pr-target/spec.md
```

同时约定：

- Planner 必须把最终 spec 路径写入 PR body 或 PR metadata
- Worker 不依赖模糊搜索，而是优先读取该显式 specPath
- v1 Worker 读取优先级：loop / queue metadata 中的显式 `specPath` > PR body 中的 `Spec: <path>` 显式记录

这能减少“PR 里到底哪份 spec 才是本轮执行依据”的歧义。

---

## 5. 建议状态机

### 5.1 GitHub 外部状态

建议明确以下 label：

- `looper:spec-reviewing`
- `looper:spec-ready`

### 5.2 implementation 阶段最小状态机

v1 implementation 阶段保持最小状态机：

1. Worker discover 命中 `looper:spec-ready`
2. Worker 开始执行时立即移除 `looper:spec-ready`，避免重复认领
3. Worker 在同一 PR 上继续实现、validate、push
4. v1 不新增 implementation label，直接复用现有 reviewer / review-request 机制
5. Worker push 后主动补 reviewer，使 Reviewer 自动重新介入

### 5.3 spec review clean 条件与 label 切换

spec review clean 的判定与现有 fixer 语义保持一致：

- `unresolvedThreadCount === 0`
- 且不存在 `CHANGES_REQUESTED` / `REQUEST_CHANGES` review 决议

满足条件时，在现有 `pr:<repo>:<prNumber>` 锁内执行幂等切换：

- 移除 `looper:spec-reviewing`
- 添加 `looper:spec-ready`

### 5.4 manual intervention 兜底

v1 不引入额外 implementation label，也不强制引入 `looper:needs-human` label 作为状态机节点。
需要人工介入时，直接复用现有内部 `manual_intervention` / `paused` 路径。

### 5.5 Planner 触发 Worker 的方式

Planner 在 publish 阶段不会直接启动 Worker run。
v1 的 handoff 方式是：

- Planner 打开 spec PR 并添加 `looper:spec-reviewing`
- Reviewer / Fixer 在 clean 后切换到 `looper:spec-ready`
- Runtime 中的 Worker discover 扫描 `looper:spec-ready`，并按需创建 `worker + pull_request` loop

### 5.6 手动触发与默认行为

Phase 4 暴露最小手动触发入口：

- API: `POST /api/v1/planners`
- CLI: `looper plan --project <projectId> --issue <number>`

在完整链路验收完成前，Planner 的自动 issue discover 默认关闭；需要显式启用 runtime planner discovery。

### 5.7 不收敛阈值与降级策略

v1 复用现有 queue retry / pause 语义，不新增独立收敛状态机。

- retryable failure：继续走已有重试与 checkpoint resume
- manual intervention：直接进入现有 `paused` / `manual_intervention` 路径
- review / fix 长期不收敛：以现有 `retryMaxAttempts` 为阈值，超过后降级为人工介入

这保证 planner / reviewer / fixer / worker 都沿用一致的恢复路径。
- `looper:needs-human`（建议新增，兜底异常）

推荐流转：

1. Planner 创建 spec PR
2. 添加 `looper:spec-reviewing`
3. Reviewer / Fixer 循环处理
4. 当 review comments / failing checks / unresolved threads 清空后：
   - 移除 `looper:spec-reviewing`
   - 添加 `looper:spec-ready`
5. Worker 发现 `looper:spec-ready`
6. Worker 在同一个 PR 上实现并 push

### 5.2 内部执行状态

建议 phase 与 loop 对应如下：

| phase | loop type | target |
| --- | --- | --- |
| issue → spec PR | planner | issue |
| spec review | reviewer | pull_request |
| spec fixing | fixer | pull_request |
| spec-ready → implementation | worker | pull_request |

这样可以复用现有的：

- queue dedupe
- business locks
- run checkpoints
- active run 观测
- agent execution 审计

---

## 6. 最小必要改造清单

在进入 feature 实现前，建议先单独完成一轮**domain + scheduler 基础改造**，否则后续 Planner / Worker 改造会同时踩到运行时、调度、约束校验三层逻辑。

这一基础改造建议单独成 PR。

## 6.1 GitHub Gateway 扩展

文件：`apps/looperd/src/infra/github.ts`

至少补齐：

- `listIssues()`
- `viewIssue()`（可选但建议有）
- `addLabelsToPullRequest()`
- `removeLabelsFromPullRequest()`
- `addReviewersToPullRequest()`
- 支持按 label 过滤 `listOpenPullRequests()` 或新增 `listOpenPullRequestsByLabel()`

这是整个目标流程的基础设施前置条件。

## 6.2 新增 Planner loop

建议新增：

- `apps/looperd/src/planner/index.ts`

推荐 v1 steps：

1. `discover-issues`
2. `prepare-worktree`
3. `write-spec`
4. `publish`
5. `notify`

其中：

- `write-spec` 是唯一 agent-heavy step
- `publish` 负责串行完成：
  - push branch
  - open PR
  - add `looper:spec-reviewing`
  - add reviewers

不建议 v1 再单独拆 `claim-issue` / `validate-spec` / `label-pr` / `request-review` 等细步，否则会让新 loop 在首版就过度复杂。

## 6.3 Reviewer 新增 label-based discovery

文件：`apps/looperd/src/reviewer/index.ts`

需要支持：

- 发现带 `looper:spec-reviewing` 的 PR
- 可与“当前用户被 request review”并存，而不是二选一

建议保留原 discover 能力，新增一个基于 label 的分支，避免破坏现有 reviewer 主流程。

## 6.4 Worker 支持 pull_request target

文件：`apps/looperd/src/worker/index.ts`

至少要改造：

- `prepare-work`：识别当前输入来自 project 还是 PR
- `prepare-worktree`：当 target 是 PR 时，直接 checkout / prepare 现有 PR branch
- `execute`：基于 spec PR 中已有 spec 继续实现
- `validate`
- `open-pr`：拆成 target-sensitive 逻辑
  - `project` 模式：保留 `open-pr`
  - `pull_request` 模式：改为 `push-existing-pr` 或复用 fixer 的 push 思路

推荐不要把 project / PR 两种模式写成大量布尔分支散落在每个 step 中；
更合适的方式是尽早区分两条 execution path，或把 PR 模式抽成清晰的 helper。

同时需要强调：

- 这不是一个“放宽字段校验”的小改动
- 它会影响 `createLoop()` 的 domain 校验、相关测试、唯一性约束与运行时分发

因此它应被视为一个单独的基础模型改造，而不是附带修改。

## 6.5 Label transition orchestration

v1 不建议先抽中心化 orchestration 层。

首版直接采用最小策略：

- Planner 完成后：打 `looper:spec-reviewing`
- Spec review fully clean 后：切换为 `looper:spec-ready`

- Planner 在 `publish` step 内直接处理首个 label
- Reviewer / Fixer 在确认 clean 后直接处理 `spec-reviewing -> spec-ready`

只需要补充两条约束：

- label 变更必须幂等
- label 变更前先获取独立 lock，例如 `label-transition:<repo>#<pr>`

如果后续 phase 继续增多，再考虑抽象成统一 orchestration 层。

## 6.6 runtime / scheduler 基础改造

新增 `planner` 之前，必须先补齐运行时与调度层的基础改造。

至少包括：

- 扩展 scheduler 的 loop type / priority 定义
- 让 runtime dispatcher 支持 `planner`
- 评估并整理当前 runtime 中硬编码的多分支分发逻辑

建议在实现 planner 前，先把 runtime dispatcher 重构为更易扩展的注册式结构；否则继续往 `if / else if` 链条里塞第四种 loop，会让后续维护成本明显上升。

---

## 7. 建议保留与替换范围

## 7.1 建议直接复用

- `scheduler` 的 queue / dedupe / retry / lock 体系
- `fixer` 的 worktree + push + reconcile 经验
- `reviewer` / `fixer` 的 PR-centric 主模型
- `agent executor`
- `notification` 体系
- `store` / `sqlite` / `run checkpoint`

## 7.2 建议新增，不建议硬塞进现有概念

- 新增 `planner`，不要把“写 spec”硬塞成 worker 的特殊模式
- 新增 `issue` target，不要把 issue 塞进 project metadata 或 queue payload 的隐式字段里

## 7.3 建议重点重构

- `worker`：从“创建 PR”升级为“既可创建 PR，也可接管已有 PR”
- `github gateway`：从 PR-only 升级为 issue / label / reviewer-aware
- `runtime` / `scheduler`：从三类 loop 的硬编码分发，升级为可安全接纳第四类 loop 的结构

## 7.4 关于“同一个 PR 继续实现”的决定

Oracle review 提醒这是高风险点，但当前用户期望已明确要求：

- Worker 扫描到 `looper:spec-ready`
- **直接开始在该 PR 上面完成工作**

因此本方案保留“同一个 PR 继续实现”作为明确范围，不改成双 PR 方案。

这意味着：

- 首版必须接受 Worker dual-mode 改造的复杂度
- Reviewer prompt 必须具备 phase awareness，能区分当前审查的是 spec 还是 implementation
- label 生命周期后续还需继续补全（例如 implementation 进行中 / 完成后如何标记）

### 为什么当前版本仍然选择 same PR

虽然从抽象建模上看，“spec PR” 与 “implementation PR” 拆开会更干净，但对当前 Looper 早期版本来说，same PR 反而更简单。

原因：

- 当前 `fixer` 已经证明“在现有 PR branch 上创建 worktree、修改、validate、push”这条路径是可行的
- 如果改成双 PR，除了 Planner 和 Worker 之外，还要额外引入“spec PR 完成后如何触发 implementation PR”的跨 PR 编排
- 当前 Looper 是本地单用户 daemon + agent 驱动工作流，不是多人协作平台，拆成双 PR 的收益还不够高
- same PR 可以让 spec 与 implementation 共址，减少 Worker 查找上下文与 spec 来源的复杂度

因此早期版本的明确策略是：

> **固定采用 same PR 流程，不在 v1 引入双 PR 分支。**

### 后续演进：支持可配置的 PR 策略

虽然 v1 固定 same PR，但建议在 spec 中预留后续演进方向：

- `same_pr`：Planner 写 spec，Worker 在同一 PR 上继续实现
- `separate_pr`：Planner 产出 spec PR，Worker 后续创建独立 implementation PR

也就是说：

- **v1：只实现 `same_pr`**
- **Phase 2：再考虑把 PR strategy 做成可配置项**

这样可以避免早期版本一次性承担两套工作流的实现与测试成本，同时不给后续产品演进封死路径。

---

## 8. 风险点

### 8.1 Worker-on-existing-PR 是最高风险改造

这是本方案里最容易把已有行为搞坏的地方。

原因：

- 当前 worker 假设自己拥有 branch 生命周期
- 当前 worker 的结束动作是 open PR，不是维护既有 PR
- 如果 project / pull_request 两种模式边界不清，会让 worker 文件继续失控膨胀

### 8.2 label 切换存在竞态

如果多个 loop 同时认为“spec 已 clean”，可能重复切换 label。

建议：

- 使用独立 lock key，例如 `label-transition:<repo>#<pr>`
- 所有 label 切换逻辑都要求幂等

### 8.3 review / fix 循环可能不收敛

建议补一个兜底：

- 连续 N 轮仍无法变 clean 时
- 添加 `looper:needs-human`
- 暂停相关 loop 或降级为人工接管

### 8.4 Spec 与代码 review 标准不同

spec PR 和 code PR 的审查标准不同。

建议：

- reviewer prompt 增加 phase awareness
- 至少能区分当前 review 的对象是“spec”还是“implementation”

### 8.5 Implementation 阶段 label 生命周期仍需补全

本 spec 已定义：

- `looper:spec-reviewing`
- `looper:spec-ready`

但 implementation 开始后，仍需明确：

- Worker 开始执行时是否移除 `looper:spec-ready`
- 是否新增 `looper:implementing`
- 代码完成后是否新增 `looper:implementation-reviewing` 或直接复用现有 reviewer 机制

本次改造至少要在实现前把这部分状态机补齐，否则会出现 label 与实际 phase 脱节。

---

## 9. 实施顺序建议

建议按下面顺序推进：

1. **先补 GitHub gateway 能力**
   - issues / labels / reviewers / label filter
2. **再做 domain + scheduler 基础改造**
   - `planner` loop type
   - `issue` target type
   - worker 支持 `pull_request` target 的模型前置条件
   - runtime / scheduler 对第四类 loop 的接纳能力
3. **再新增 Planner loop**
   - 先把 issue → spec PR 主入口打通
4. **再改 Reviewer discovery**
   - 支持 `looper:spec-reviewing`
5. **再改 Worker 以支持 pull_request target**
   - 在真实 spec-ready PR 上验证继续实现链路
6. **最后补 label transition logic**
   - spec-reviewing → spec-ready

原因：

- 新链路的真实入口是 Planner，而不是 Worker。
- 但 Planner 落地前，domain / scheduler / runtime 的基础模型必须先准备好。

---

## 10. 验收标准

本方案至少应以一条端到端 acceptance flow 作为完成标准：

1. 一个属于已注册 project repo 的 GitHub issue 被创建
2. issue 带 `looper:plan` label，且 assign 给当前 GitHub 用户
3. Planner 自动发现该 issue，并创建 `specs/<issue-number>-<slug>/spec.md`
4. Planner 推送 spec PR，并自动加上 `looper:spec-reviewing`
5. Reviewer / Fixer 循环后，该 PR 被切换为 `looper:spec-ready`
6. Worker 自动发现该 PR，并在同一 PR 上继续实现
7. 代码完成后，该 PR 进入后续代码 review 阶段

只要这条端到端链路不能稳定走通，就不能算“目标流程已跑通”。

---

## 11. 本次结论

当前项目并不是“只差几个 API”就能跑通目标流程，而是还缺少三块核心能力：

1. **Issue / label / reviewer 编排能力**
2. **Planner phase**
3. **Worker 接管既有 spec PR 的能力**

其中最关键的不是 Planner，而是：

> **让 Worker 从 project-driven/open-PR 模式，升级为能直接在 spec-ready PR 上继续实现。**

只要这块能力没有打通，issue → spec → review → implement 的整条链路就还不能闭环。
