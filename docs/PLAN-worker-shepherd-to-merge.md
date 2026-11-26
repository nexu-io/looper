# PLAN v3: worker 会话 shepherd 自己的 impl PR 到合并(looper:auto 一 label 到底)

Branch: `feat/looper-auto-flowchart-runtime`. Status: v3 已按两轮对抗评审重写,待第三轮 / 实施。
- v1 致命错:把 worker"跑一次-失败才 resume"的步骤机当成支持"事件驱动-无限重复"。
- v2 致命错:把控制流押在 `loop.Status=="shepherding"` 上——三条现有路径(失败/HITL答复/人类消息)会悄悄挪走它 → 回 prepareWork 重实现。
- **v3 核心修正:控制真相源 = 持久标记 `$.shepherd.active`(loop metadata)+ `checkpoint.PullRequest`,不是 `loop.Status`。** loop.Status 只作管理/展示;它被怎么挪都不影响控制流。

## 1. 目标
打 `looper:auto` 后一条链一张卡跑到合并:`… worker 开 impl PR → 同一 worker loop 自己 shepherd 该 PR(盯 CI/评审/冲突/合并队列 → 修 → auto-merge)→ 🎉 已合并`。

## 2. 硬约束
1. **绝不改 `internal/fixer/`、`internal/reviewer/`**(同事在用+升级冲突)。复用只经:包级稳定接口(`reviewer/automerge.Decide`)、共享 `internal/infra/github` gateway 纯加法、提示词(pr-autopilot recipe)。协调 fixer **靠它 discovery 已有的查锁(`hasActivePRLock`),不动它**。
2. 无 per-PR 常驻进程;codex 每事件一趟即退。
3. 崩溃/重启可恢复(状态全在 DB)。
4. **绝不自审自批,更绝不自己合并**(用户 2026-07-08 拍板):bot 只 fix + 盯,把 PR 盯到"可合并"(approve+green+无冲突+线程清)就**停在那等同事人工合并**。**永不 SubmitReview,永不 EnableAutoMerge / gh pr merge / enqueue**。合并这一步永远是人的动作;bot 检测到 MERGED 才置终态。
5. 对同事 looper(nettee)评审保守:默认照改;file:line 核实幻影才回 "cannot reproduce";绝不轻易 dismiss;拿不准 stop-and-ask。

---

## 3. 控制模型(v3 的根本重写)

### 3.1 真相源 = 持久标记,不是 loop.Status
- loop metadata 存 `$.shepherd = { active, phase, passCount, lastSignal, repliedThreads[], sessionLost, outcome }`。
- **`createRunContext` 的 start-step 解析器**(`worker/runner.go:2258`)加分支:**当 `$.shepherd.active==true` 且 `checkpoint.PullRequest != nil` → 强制 `startStep = stepShepherd`**,带 PR 检查点前进,绕过 prepareWork 默认与 `failed/interrupted` 门。**完全不看 `loop.Status`。**
- 于是三条会挪 loop.Status 的路径都不再致命:
  - 失败路径(`runner.go:1184`)retryable→`queued`:重被 claim → 解析器看标记 → 仍回 `stepShepherd`;non-retryable→`paused`:交人(合理)。
  - HITL 答复(`hitl_github_poll.go:208`)→`running`:下一趟解析器看标记 → `stepShepherd`。
  - 人类消息(`hitl_github_poll.go:175`)→`queued`:同上。
- `stepShepherd` 是序列末尾(`nextWorkerStep=""` → `CurrentStep` 置 nil,一趟一 enqueue);`validateWorkerResumeCheckpoint`(:3041)对 stepShepherd 走 `default→nil`,PR-only 检查点被接受(已核实)。

### 3.2 loop.Status 与域层(只加一个状态,终态复用 completed)
- 加 `LoopStatusShepherding = "shepherding"` 到 `internal/domain/domain.go`:
  - `activeLoopStatuses` + `conflictingActiveLoopStatuses`(防同 target 起重复 loop)。
  - `loopStatusTransitions`:`running→shepherding`;`shepherding→{shepherding, queued, paused, awaiting_human, completed, stopped, terminated}`(管理路径能 pause/stop/close;worker/reconciler 自身写走 raw Upsert 绕校验,但列全保正确)。
  - `AssertKnownLoopStatus` 列表 + `IsActiveLoopStatus`/`IsConflictingActiveLoopStatus`。
- **稳态**:一趟 pass 后 `running→shepherding`(展示"盯着")。
- **终态**:检测 MERGED → `completed` + `$.shepherd.outcome="merged"`;CLOSED → `completed` + `outcome="abandoned"`。**不发明 merged/abandoned 状态**(免得每处 terminal switch 都要学)。`shouldRequeueLoop`(`runtime.go:3014`)终态集已含 `completed` → 不会 requeue 已合并 PR。
- `WorkerSteps`(domain.go:69)加 `"shepherd"`。

### 3.3 loop 状态面完整审计(逐处,含二轮评审新发现)
必须让 `shepherding` 被当"活的、非终态、非卡死",且**别有路径把它当已完成**:
- `worktreecleanup/service.go:269` `protectsLoopStatus` + `runtime/worktree_cleanup.go:452` `worktreeCleanupActiveLoopStatus`:加 `shepherding`(M2)。
- `worker/runner.go:2604` `pausedOrCompleted`(discovery dedupe)+ `:934` 跳过集:加 `shepherding`(M5),防被打回 queued 从 prepareWork 重跑。
- `domain.go` `activeLoopStatuses`/`conflictingActiveLoopStatuses`/`loopStatusTransitions`/`AssertKnownLoopStatus`/`IsActiveLoopStatus`(域层 blocker)。
- `runtime.go:3014` `shouldRequeueLoop`:干净 park 的 shepherding loop(上一趟 success)在这里返回 false → **靠 reconciler 复活**(3.4),不靠它。
- `notify` `feishuLoopStatusTerminal`:确认 `shepherding` 不在终态集(卡片继续动)。
- `statusForCheckpoint`(worker):shepherd 趟不产出会误判的终态。
- CLI/API(`loops/service.go`、`api/handler.go`):加了转换即可 pause/stop/close。

### 3.4 恢复 + 唤醒(如实:reconcile 周期,非严格事件驱动)
- **无 per-PR-event→loop 入队缝**:webhook 转发事件(`webhook.go:28`:pull_request/issue_comment/pull_request_review/pull_request_review_comment/push/check_run,**无 merge_group/check_suite**)只 `requestWake`→claim pass。
- 新 `reconcileWorkerShepherd`:
  - **接进 `runRecoveryPipeline`(`runtime.go:683`,startup)**——重扫 shepherding loop、**第一时间重认领 `pr:repo:N` 锁**(压缩被 fixer 抢窗口)。
  - **接进 `runWakeReconcile`(`runtime.go:1155`)+ 一条 shepherd 专用 ticker**(合理周期,如 30-60s)——不假装实时;文档标"reconcile 周期,不是逐事件"。
  - 每 loop:续 `shepherd:<loopID>` 锁 → 算稳定信号(去 updatedAt)→ 变则 `shepherding→queued`+NextRunAt=now(唤一趟)→ MERGED/CLOSED 置 `completed`+outcome、释锁。
- **恢复自洽**:状态全在 DB(loop status + `$.shepherd` + worker native session id + checkpoint.PullRequest);重启后 reconciler 重扫、重认领锁、resume 同一会话。

---

## 4. 会话续接(B2,HOLDS)
- 每趟 agent Start **必传** `NativeSessionID = r.latestNativeSessionID(loop.ID)`(`hitl.go:136`,取 loop 最近执行 session;`CapturePullRequestSnapshot` 写的是快照表不是执行表,reviewer 是另一 loop,不污染)+ `NativeResumePrompt`。走 executor 显式分支(`executor.go:172`,不要求 pending)。
- **capture 失败(返回空)**:置 `$.shepherd.sessionLost=true`,提示词显式声明"上一会话上下文丢失,先从 PR diff+评审重建理解",事件/卡片标注;**绝不静默开新会话**。
- 断言:worker 一条会话贯穿 plan→execute→openPR→shepherd 各趟(`GetLatestByLoopID` 返回上一趟 session)。

## 5. 锁(M1,单 owner + Refresh + stepShepherd 绕过 claim-lock)
- **修 M1a 自死锁**:`ProcessClaimedItem`(`runner.go:1082`)`if resumedRun.StartStep != stepPrepareWork` 再加 **`&& resumedRun.StartStep != stepShepherd`** → shepherd 趟 `claimedLockKey=""`,**跳过整个 reacquire/release/retarget 块**(否则 checkpoint.ClaimedLockKey 被重定向成 `pr:repo:N`、reacquire 撞 `shepherd:<loopID>` owner 报"lock already held")。
- shepherd 自管 `pr:<repo>:<N>` 锁,**固定 owner `shepherd:<loopID>`**:reconciler 每 tick + pass 起始 `Acquire`(空/过期时)或 `Refresh`(自己持有时);**pass 间不 release**;只终态/stop-ask 才 release。**tick 周期 << TTL**(tick 30-60s,TTL ≥ 10min)。
- 效果:fixer discovery `hasActivePRLock`(`fixer/runner.go:1401`,owner 无关)看到锁被持有+无运行中 fixer loop → **跳过、不建 loop、不冒卡**。全靠 fixer 现有行为,零改动。
- 崩溃后锁过期 → 重启 recovery pipeline 第一时间 `Acquire`(过期可抢)重认领。窗口内被同账号 fixer 抢仅"配置了重叠"时才发生,per-pass 仍防并发。

## 6. HITL stop-and-ask(M4,端口 + 持久标记自愈唤回)
- ask 机制现只在 `runExecuteStep`(prompt 注入 + `pendingHumanAnswer` `runner.go:1733`、哨兵 `.looper/ask.json`→`awaitingHumanError` `:1789`、`markHumanAnswerConsumed` `:1813`)。**stepShepherd 要把这套端口过来**(复用同一 `.looper/ask.json` 契约 + `hitlEnabled`)。
- 挂起=`awaiting_human`;人答复走 `deliverHITLAnswerToLoop`(`hitl_github_poll.go:208`)设 `running`——**但有持久标记 3.1,下一趟解析器仍回 stepShepherd,自愈**,不需要它显式拨回 shepherding。
- 自由消息(`enqueueHumanMessageToLoop` `:175`)翻 `queued`——同样被标记自愈。

## 7. 盯到"可合并"就停(用户更正:bot 不合并,等同事)
- **删掉整个 auto-merge**:不 `EnableAutoMerge`、不 `gh pr merge`、不 enqueue、不 SubmitReview。原 Stage F 的"接口扩展 + 自建合并 gate + `automerge.Decide`"**全部去掉**。
- bot 的职责止于"把 PR 盯到健康、可合并":修评审/CI/冲突、re-request reviewer、保持无冲突。
- **可合并态(reviewDecision==APPROVED + required-green + mergeable + 线程清)→ 卡片相位置 `✅ 待合并(等同事合并)`,继续盯,什么都不合。**
- **合并由同事人工完成**。bot 每趟检测 PR `state`:`MERGED` → 终态 `completed`+outcome=merged、卡片 🎉已合并、释锁、停;`CLOSED`(未合)→ completed+abandoned。
- 判"可合并态"只为**卡片相位 + 决定不再唤 agent**,不为合并——所以只需轻量读 `reviewDecision/mergeStateStatus/state` + CI 状态,**不需要**扩 worker 接口去拿 reviews[commit]/required-check 那套精细字段(那是为合并 gate 的,已删)。

## 8. looper:auto trigger 溯源(major,per-PR 钉死)
- worker 完成时手里没有"我在 looper:auto 链"的存量信号(`loopTriggerLabelHint` 是按角色推的)。**方案**:worker 开 PR 后 gating 时,用 `work.IssueNumber` **现读 issue 的 label**(Plane/GitHub 都有 label API),命中 `looper:auto` 才置 `$.shepherd.active`;否则今天的 `completed` 不变。
- 备选(更稳):coordinator/planner 在链上把 `looper:auto` 作为 loop metadata 传递到 worker loop(不依赖现读)。二次评审后择一。
- **gate = 开关 ON + trigger 命中 looper:auto + 我们自己 worker 开的 PR**;三者缺一 → 今天行为**逐字节不变**(非 auto 的 worker/fixer/reviewer 流零影响)。

## 9. 一趟 pass(`runShepherdStep`,幂等)
1. 取实时 PR detail + 算**稳定信号** `[state, reviewDecision, requiredCI_phase, mergeable, mergeStateStatus, headSha, unresolvedThreadCount]`(**去 updatedAt** 防自触发,D1);续 `shepherd:<loopID>` 锁;校验/重物化 worktree。
2. MERGED→`completed`+outcome=merged、释锁、停;CLOSED→completed+abandoned、释锁、停。
3. `passCount++`;超阈值(如 20 或无进展 N 趟,D2)→ stop-ask。
4. 拍 `CapturePullRequestSnapshot`(reviewer 关着,不拍卡片 §A 冻)。
5. 决策:
   - gate 全过(§7)→ 不唤 agent,`EnableAutoMerge`。
   - 有未解决线程 / required CI 失败 / 冲突 / 队列 drop → 唤 agent(§4 显式续接 + pr-autopilot 提示词 + 保守护栏 + merge_group drop via gh);回帖去重靠 `review_thread_replies` 幂等 + `$.shepherd.repliedThreads`。
   - 否则(CI pending/等复审)→ 回 `shepherding` 等 reconciler。
6. agent 后:re-request nettee;更新 `$.shepherd.{phase,passCount,lastSignal,repliedThreads,headSha}`;拍新快照;`running→shepherding`。

## 10. 卡片相位 / config —— 同 v2
- `loopShepherdPhase` 读 `$.shepherd.phase`;`feishuLoopFlowchartStyle` worker 分支 `shepherding`→`👀评审中`/`🔧修复中`/`✅待合并`;`shepherding` 加进 `feishuLoopAwaitingMerge`。进程 `🔨实现中→👀评审中/🔧修复中→✅待合并→🎉已合并`。
- config:worker-shepherd enable(默认关,opt-in per project)+ `workerAutoMerge`。**D3 核实:我们 looperd 不给 open-design 跑 reviewer 角色**(否则审自己 PR);写进 gating/文档。**与 `looper takeover` 互斥**(它会起 reviewer+fixer,重叠;同一 PR 二选一)。

## 11. fixer 能力零影响(专列)
- 不改 `internal/fixer/`、`internal/reviewer/` 任何文件。
- 协调靠 fixer **现有** discovery 查锁(`hasActivePRLock`);shepherd 只持一把 fixer 本就尊重的 `pr:repo:N` 锁,fixer 行为不变。
- 跨人:fixer `AuthorFilter=CurrentUserLogin` → 同事 fixer 只碰自己 PR。
- fixer 独立跑非 looper:auto 的 PR 完全不受影响(shepherd 只在开关+looper:auto+我们 worker 开的 PR 上起)。
- auto-merge 走 `reviewer/automerge` 包级 + 共享 gateway,不调 reviewer 方法。

## 12. 分阶段
1. 域层:`LoopStatusShepherding` 枚举/转换/active 集/AssertKnown + `WorkerSteps` 加 shepherd + `shouldRequeueLoop`/worktree 保护/discovery dedupe/notify 终态 全审计(3.2/3.3)。
2. worker `stepShepherd` + **持久标记驱动的 start-step 解析**(3.1,B1)+ **stepShepherd 绕 claim-lock**(5,M1a)+ 完成尾部 gating(8)。
3. `runShepherdStep` 骨架:显式会话续接(4,B2)+ 单 owner 锁 Refresh(5)+ worktree 校验 + 稳定信号(9)。
4. `reconcileWorkerShepherd`:recovery pipeline + runWakeReconcile + ticker;锁续期/重认领;cap(3.4/D2)。
5. HITL 端口进 stepShepherd(6,M4)。
6. auto-merge 自建 gate + 当前 head 独立批准(7,M6)。
7. 卡片相位 + 拍快照(10)。
8. config + looper:auto gating + D3 核实 + takeover 互斥(8/10)。
9. 全链 e2e:#5272 自动接续到真合并。

## 13. 测试计划(关键)
- 解析器:`$.shepherd.active`+PR 检查点 → 新 run **从 stepShepherd 起**,即使 loop.Status 是 queued/paused/running(打脸 v2 的三条挪状态路径)。
- claim-lock:stepShepherd 趟不进 reacquire/release,不自死锁(M1a);fixer discovery 因锁跳过。
- 会话:每趟 Start 收到显式 NativeSessionID;capture 失败走 sessionLost 不静默(B2)。
- 域层:CLI/API 能 pause/stop/close 一个 shepherding loop;merged/abandoned 落 completed+outcome、不被 requeue。
- worktree:shepherding 受保护,pass 间不被回收。
- HITL:shepherd ask 挂 awaiting_human,人答后下一趟仍回 stepShepherd(标记自愈)。
- auto-merge:仅"nettee 在当前 head 上 APPROVED + required-green + mergeable + 线程0"才开;**永不 SubmitReview**;push 后换头需 nettee 重批。
- 恢复:restart 后从 DB 恢复、重认领锁、续同一会话。
- 全局 cap;一张卡(planner→worker→shepherd 同 taskKey);向后兼容(非 auto 逐字节不变)。

## 14. 已完成前置
- draft 修复 commit `eb4a3e4`(worker prompt 强制 ready-for-review 禁 --draft)。

## 15. 仍需第三轮确认
1. **持久标记驱动的 start-step 解析**是否和 `stopObsoleteResumedIssueRun`(`runner.go:1114`)/`ShouldRestartFromDiscover`/其它 resume 早退逻辑相容?会不会有路径在解析器命中标记前就把 shepherd loop 当普通 issue loop 处理掉?
2. `shepherding` 加进 `conflictingActiveLoopStatuses` 会不会误伤(比如 reviewer/fixer 对同 PR 的正当 loop 判定)?
3. §8 trigger 溯源:现读 issue label vs coordinator 传标记,哪个更稳且不碰 reviewer/fixer?
4. stepShepherd 端口 HITL:`runExecuteStep` 里那套 pendingHumanAnswer/consume 有没有隐藏的对"execute step 专属状态"的依赖?
5. 一趟 pass 内让 agent 跑完整 pr-autopilot sweep vs looper 侧编排多小步——哪个更可控(尤其 push 后换头 + re-request 的时序)?

---

## 16. 第三轮评审结论 + v3.1 必修(收敛:可建,非根本缺陷)

三审确认核心赌注(持久标记当控制真相源)成立;剩下都是**具体的包内编辑清单遗漏**,均不碰 fixer/reviewer。

### 两个 blocker(编辑清单遗漏,非设计错)
- **#1 `stepShepherd` 必须进 `workerStepSequence`(runner.go:70)+ `executeStep` 派发(runner.go:1272)**。否则 `stepsFrom` 找不到 → 默认 index 0 → 返回整条序列 → v1 复活。修:append 为序列末元素 + 加 dispatch case + 测 `stepsFrom(stepShepherd)==[stepShepherd]`、`nextWorkerStep(stepShepherd)==""`。
- **#2 完成尾部(ProcessClaimedItem:1215-1242)要 shepherd-aware**。它无条件 `completeRun("success")`+`Queue.Complete`+`updateLoop("completed")`+notify → (a) 置 completed 而非 shepherding (b) **完成 queue item → 破坏 #3 的唤醒** (c) 每 30-60s 一趟都发"已交付"通知。修:分支——park→`shepherding`+cancel/recreate queue item+抑制 notify;终态→`completed`+`$.shepherd.outcome`+只通知一次;**非 shepherd 分支逐字节不变**(向后兼容)。

### 三个 major
- **#3 reconciler 唤醒 与 completed queue item 不兼容**。worker loop 只能靠 **queue item** 被 claim,没有 NextRunAt 扫描器。§3.4 的"shepherding→queued+NextRunAt=now"**不创建 queue item**;正常 park 留的是 completed(非 cancelled)item → `RequeueLatestCancelledByLoop` 是 no-op → 静默卡死。修:park 时 **cancel queue item**(reconciler 再 requeue)**或** reconciler **新建** worker queue item(仿 `enqueueDiscoveredIssue`,含 LockKey+dedupe);并按 `status=="shepherding"` 过滤,避免 in-flight(running)被双派。
- **#4 `stopObsoleteResumedIssueRun`(runner.go:1114)会在失败/HITL resume 时中止 shepherd**。凡 `resumed==true`(每次 failed/interrupted resume)都进;若 issue 在 PR 仍开时被关(merge-closes-issue 竞态/手动关)→ 它释放 `checkpoint.ClaimedLockKey`(open-PR 后已重定向成 **`pr:repo:N`=shepherd 自己的协调锁**)+ 标 completed → shepherd 死+丢锁+fixer 趁虚。修:`$.shepherd.active`(或 `startStep==stepShepherd`)时**早退**。**这条护栏必须和 resolver 分支同一次改动落地。**
- **#5 §7 auto-merge gate 比"纯加法"大**:`worker.GitHubGateway`/`worker.PullRequestDetail`(runner.go:102/237)没有 Reviews/ReviewDecision/MergeStateStatus/checks——富字段在 gateway 自己的 `PullRequestDetail`(gateway.go:1412)。需扩 worker 接口 ~4 方法 + 扩 worker.PullRequestDetail + 接 runtime adapter + **worker 侧重写 head-match 解析**(reviewer 的 `hasApprovedReviewByAuthorForHead` runner.go:5381 不可 import,当参考)。数据都在,是真工作量,单独排期。

### 确认 HOLDS(核心积木都真)
持久标记 resolver(除 #4 短路)、checkpoint 跨趟保 PullRequest、claim-lock 绕过、域层 raw Upsert 绕校验、Q2 target-keyed 无冲突、会话续接跨趟、HITL 端口靠标记自愈、trigger `looper:auto` 从不被移除(可现读;**更推荐 discovery 时 stamp metadata**,race-free)。

### minor
#6 补 `shepherding→running` + 枚举串 + activeLoopStatuses;#7 discovery 两个跳过集必加;#8 worktree 两个保护集必加;#9 cost cap 只计"唤醒了 agent"的趟。

### §8 trigger 溯源定案:**discovery 时 stamp**
worker `ensureLoopForDiscoveredIssue`(runner.go:2592/2603 workerLoopTracksIssue)时读 issue label,命中 `looper:auto` 就把意图 stamp 进 loop metadata(一次读、race-free、免疫中途 label 改)。gating 用它,不现读。

### 🟢 最安全的第一阶段(零运行时行为变化,可独立先落)
**Stage A(inert surface)**:域枚举 `LoopStatusShepherding` + `running→shepherding`/`shepherding→{…}` 转换 + `AssertKnownLoopStatus`/`activeLoopStatuses`/`conflictingActiveLoopStatuses` + `WorkerSteps`/`workerStepSequence` 加 `stepShepherd` + `executeStep` 派发(暂返回 no-op park)+ 四个审计集(discovery 934/2604、worktree 269/452)。**因为没有任何地方 set `$.shepherd.active`,运行时行为零变化**、纯加法、可先测(`AssertLoopStatusTransition`/`stepsFrom`/`nextWorkerStep`)绿了再上控制流(resolver 分支 + shepherd-aware 尾部 + reconciler 唤醒 + #4 护栏同批)。
