# GRILL Transcript

## Round 1 — requirement / product decisions

Fresh reviewer verdict: `BLOCKED`。

它核对当前 planner、角色配置、HITL 和 Plane approval 后，要求明确：适用任务、正式产品 Spec 门槛、设计产物门槛、多角色顺序、问题打包、研发负责人、收敛标准和最终审批人。

最终决策：

- 覆盖所有 `dispatch/plan`，简单 `dispatch/implement` 不增加前置流程。
- 正式产品 Spec 按复杂度和风险判断，不按问题数硬切；数字只限制飞书摘要可读性。
- 产品先收敛，设计与研发随后并行。
- 研发决策和技术 Spec 审批均由本地 Looper owner 负责；V1 不增加 engineering owner / delegate。
- 局部设计取舍由 Looper 生成 2–3 个最小 HTML + 截图供 design owner 在 Plane 选择；新页面、多步骤或信息架构变化仍要求正式设计产物。
- Plane 是回答 authority，飞书只通知；每个 blocking ID 必须由对应角色明确回答，超时不自动选择。

## Round 2 — adversarial technical review

Fresh reviewer verdict: `BLOCKED`，发现九项实现缺口：

1. paused planner 没有唯一 resume step，可能从 discover 或 write-spec 错误恢复。
2. “所有问题回答后再 GRILL”与“产品回答后才生成设计/研发问题”形成死锁。
3. Plane work item comment 当前模型丢失 UUID，且 Spec 假设了未验证的 PATCH 能力。
4. worktree 内 `.looper/decision-*` 可能被 write-spec fallback commit 提交。
5. 任意 HTML + `file://` / Chrome 参数不足以形成安全隔离。
6. 仅靠本地 checkpoint 无法覆盖“飞书已发送、本地 receipt 未落盘”的 crash window。
7. 全局 step sequence + runtime flag 会让旧 loop 在升级或回滚时被新流程重新解释。
8. owner-only approval 没绑定技术 Spec revision，旧 approve 可能批准新内容。
9. EAG 使用了代码不识别的 `LOOPER_HOME` 和错误配置字段，可能污染真实 `~/.looper`。

修订结果：

- 增加 first-class await outcome、唯一 `pipelineVersion/phase/resumeStep` 和 V1/V2 独立 transition table。
- 把流程拆成产品 barrier、设计/研发 barrier、final GRILL 三道 barrier；上游改答使下游 revision 失效。
- 增加 Step 0 Plane/Feishu/renderer contract spike；Plane V1 协议默认 append-only revision，并使用独立 UUID comment model。
- 运行产物移到业务 worktree 外，增加路径和 commit guard。
- 把原始 HTML 改为受限静态 HTML：HTML/CSS 白名单、禁 JS、资源复制、loopback server、CDP 请求拦截。
- Feishu 可见消息去重必须有服务端幂等 UUID 或 remote lookup 证据，否则不发布图片功能。
- 技术 Spec approval 绑定 `specRevision + contentHash + reviewRequest`。
- EAG 改用临时 `HOME`、显式 config、真实 `storage.dbPath` / `maxConcurrentRuns` / 独立端口并断言全部路径隔离。

剩余的非产品阻塞：Step 0 必须取得真实 Plane/Feishu contract 证据。它已经被收口为实施的第一个 HITL spike，不需要新增产品决策。

### Round 2 re-review

Fresh reviewer 对修订稿复核后又发现两处残留矛盾：V1 仍需保留旧 `productSpecGate`；renderer 文档仍残留手动 command / `file://` 表述。两处均已修正：只有 V2 跳过旧 gate，V1 原样保留；renderer 只允许 browser executable path，并统一使用受控 loopback origin。

最终 verdict：`PASS`。

## Post-review safety decision

用户明确要求：所有飞书 spike / EAG 消息只能发送到“agent 通知群”，不得发送到有其他同事的“Looper 协作”群。Spec 已增加基于 chat ID 的 allow/deny preflight、fail-closed 行为和验收证据要求；群名不作为安全 authority。

补充：被 @ 的 open_id 不在“agent 通知群”时允许显示灰色 @，不阻塞发送或验收；群目标 chat ID 才是安全门禁。
