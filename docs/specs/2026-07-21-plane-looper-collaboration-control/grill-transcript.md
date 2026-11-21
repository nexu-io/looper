# Grill Transcript — Plane 内派活与 Looper 多角色协作面板

- Spec bundle：`product.md` / `design.md` / `technical.md` / `validation.md`
- Work item：OPEND-1645 · `a6eb4e1e-67d7-43f5-8f83-c023faa446cd`
- Grilled at：2026-07-21 11:13 CST
- Grill agent：fresh adversarial `/root/plane_looper_grill`（未参与 AUTHOR）
- Grounding 概况：🟢 已核对 Plane/Looper 源码 6 轮；🟡 未核对源码 0 轮
- 最终状态：`GO-to-review`，不是 approved

## 拷问记录

### Q1 [镜头：边界与失败模式] 🟢 · argument

**问：** Spec 把 `trigger label + 唯一 Looper owner assignee + exact target label` 定义成 durable Authority，但 Plane 现有通用 work-item PATCH 对 Project Member/Admin 开放，可直接整组替换 assignees/labels，不走新 dispatch 路由的锁/幂等，也不写 marker。成员手工改属性与另一成员点派发并发时，系统凭什么区分合法 dispatch、保证唯一 owner/node？

Grounding：

- `apps/api/plane/app/permissions/project.py:85-116`
- `apps/api/plane/api/views/issue.py:740-795`
- `apps/api/plane/api/serializers/issue.py:234-283`
- `internal/forge/plane.go:181-217`

**答（作者）：** 接受 blocking。新增 Plane 服务端 `LooperDispatch` 作为唯一 Authority；每 work item active states partial unique，并带 revision/idempotency。labels/assignee/target 只做投影与 discovery hint。claim、新 loop、新 run 和 Looper-managed side effect 都必须核对活动 dispatch；通用 PATCH 造成投影不一致时标 drifted 并 fail closed。

**判：** 解决。持久化 Authority 覆盖所有写路径；新增 T4b/E2 专门验证原生 UI/API 绕过。

### Q2 [镜头：风险 · 安全 · 数据] 🟢 · argument

**问：** Node 自报 `ownerPlaneMemberId` 没有身份意义。现有 loopernet enrollment 只接受 join key + node_name/GitHub identity，heartbeat 的 capabilities 又由持 node token 的客户端任意上报。任一 Node 都能冒充另一 Plane member。哪个不可由 Node 自报的 durable binding 才是 `node_id ↔ Plane member` Authority？

Grounding：

- `internal/network/protocol/protocol.go:60-67,87-93`
- `internal/network/cloud/service.go:133-188,205-215`

**答（作者）：** 接受 blocking。新增 Plane `LooperNodeBinding`：owner 用自己的 Plane personal API key 发起，owner 取 `request.user`；Node 同时用 loopernet node token 获取一次性 signed challenge，证明控制 node_id。Project Admin 只启用项目/角色，不能替代 owner 身份证明。Plane 保存 Node public key，claim/transition 同时验证 owner Plane 身份、Node 签名和 active binding；支持 revoke/rotate/member removal。

**判：** 解决。targets、dispatch、claim、revoke 均消费同一个双向证明 binding；heartbeat 自报 owner 不再参与身份判断。

### Q3 [镜头：风险 · 安全 · 数据] 🟢 · argument

**问：** 产品/设计/研发/QA 的 Plane IDs 目前来自每台 Node 的本地 project config。两个 Node 可以配置完全不同的决策人；一旦转派，就可能悄悄换一套回答/审批 Authority。Plane 端的单一、带 revision 的 project-role mapping 是什么？

Grounding：

- `internal/config/types.go:632-677`
- `internal/runtime/awaiting_role_decisions.go:309-319`
- `internal/runtime/blocked_conditions.go:230-247`
- `internal/runtime/spec_approval.go:112-120`

**答（作者）：** 接受 blocking。新增 Plane `LooperProjectRolePolicy`：每 project 单一 revision，product/design/QA member IDs，engineering policy 固定为 dispatch owner。创建 dispatch 时冻结 role-policy revision 和 member snapshot；本地角色配置在 strict Plane 集成模式只做 bootstrap/迁移提示，冲突时 fail closed。项目策略更新默认只影响新任务；在途刷新必须显式带原因，未解决请求 supersede/reissue，已解决决定保留 actor/revision。

**判：** 解决。转派不会暗中更换产品/设计/QA；研发责任人变更有 dispatch revision 边界。

### Q4 [镜头：风险 · 安全 · 数据] 🟢 · human-decision

**问：** 任意 Project Member 若可向另一位同事的离线 Looper 派发并自动运行，就等于把普通 work-item 编辑权升级为在同事电脑上启动高权限 agent 的权限。第一版信任模型到底是全员可信、每次 owner 接受，还是 owner/Admin allowlist？

Grounding：

- `internal/agent/executor.go:2134-2135,2207-2211`：当前 agent 运行权限高，风险不止错误 assignee。

**答（作者）：** 推荐 C：owner-controlled actor allowlist。Node binding 默认 `owner_only`，owner 可选择 admins/selected/all_members；Project Admin 可暂停/revoke/缩小能力，不能替 owner 扩大派发人范围。dispatch actor 还必须有 work-item edit permission。offline queue owner opt-in、默认 false；收紧策略取消未 claim 且已失权任务。

**判：** 明确承接为 human-decision，但不记“已由 agent 解决”。推荐 C 已完整写入产品/技术/验证 Spec；maintainer 必须在 approve 前明确确认，未确认不得 `spec:approved`。

### Q5 [镜头：边界与失败模式] 🟢 · argument

**问：** Spec 承诺 revoke/reassign 后运行中 agent 会在“下一 side-effect boundary” fail closed，但真实 Planner/Worker 把 coding agent 作为黑盒子进程 Start 后 Wait，接口只有 Wait/Kill，没有逐 tool hook。若不异步 kill，就不存在所声称的 boundary。要选 process kill、mutation proxy，还是诚实降级？

Grounding：

- `internal/planner/runner.go:2438-2442`
- `internal/worker/runner.go:2228-2241`
- `internal/agent/executor.go:232-235,2134-2135,2207-2211`

**答（作者）：** 接受 blocking，诚实降级。V1 running dispatch 不可直接 supersede/reassign；先记 `stop_requested/revocation_pending`，由原 owner 本地停止，Node 回报 execution PID 已终止后旧 dispatch 才 terminal，随后才能创建新 dispatch。Looper 只能在自己掌控的 post-run publish gateway 前刷新 revision；不能追回黑盒 agent 已发生的本地/直接远程副作用。即时撤销需未来 credential fencing proxy 或 controlled sandbox，列为 non-goal。

**判：** 解决。产品文案、AC-13、T7、E6/E7/E8 均同步真实保证，不再虚构 side-effect boundary。

### Q6 [镜头：简单性 · YAGNI] 🟢 · argument

**问：** Plane 已持久化 binding、role policy、dispatch、claim/transition 和 markers，loopernet 也已有 membership/heartbeat/load。为什么还要把 `planeProjects[] + activeLoops[]` 塞进 loopernet capabilities，再合并第二份状态？若没有不可替代字段，应删除。

Grounding：

- `internal/network/protocol/protocol.go:36-46,76-98`
- `internal/network/cloud/service.go:371-413`
- `AGENTS.md` 的“新概念必须写清 trade-off”“优先删除一层”要求

**答（作者）：** 接受 blocking，删除两组 digest。V1 loopernet 只承担 membership、presence、dynamic load 和 binding signed challenge；项目/角色资格由 Plane binding 决定，phase 由 Plane dispatch transition/markers 决定。未来若要秒级流，另立只读 observation 协议。

**判：** 解决。去掉双写、payload 版本、陈旧合并和隐私过滤问题。

## Needs-spike 复核

初稿曾提出 SPIKE-1，在“扩展 `network.mode=routed`”和“独立 Plane exact-target policy”之间运行时选择。GRILL 后作者重新核对：`internal/config/validate.go:483-505` 已明确 routed Planner 被禁止且依赖 GitHub identity。该证据足以排除复用现有 GitHub Network 语义；最终直接选择独立 Plane strict-dispatch claim gate，不再是承重运行时未知。

fresh agent 复核结论：needs-spike 已合理消除；剩余风险由集成测试和隔离双 Node E2E 验证，不需要先赌一个架构 spike。

## Execution mode 决策（DECIDE）

- Proposed：`HITL-needs-engineer`
- AFK-readiness 对抗结论：**AFK 被驳倒；HITL-needs-engineer 成立。**
- 依据：
  1. Q4 仍需 maintainer 产品/安全决策；
  2. S2 包含 Plane 认证绑定、Node keypair、signed challenge、防重放、角色/actor policy revision 与吊销；
  3. S3/S4 含数据库迁移、partial unique、并发 Authority、跨 Node claim fencing；
  4. 真实双 Node E2E、设计截图、权限矩阵必须人工签收。
- Phase gates：
  1. 开工前：maintainer 确认 Q4 推荐 C。
  2. S1：agent 可辅助只读 UI/API scaffold，但不可自行合并或启用。
  3. S2～S4：engineer-led；auth/crypto、迁移、Authority/并发不得无人值守合并。
  4. S5：agent 可实现；设计关键截图、权限矩阵、隔离双 Node E2E 人工签收。
  5. feature flag、生产迁移、token/密钥、灰度、回滚只由人执行。

## 最终 verdict：GO-to-review

- 工程 blocking：0。
- 未运行的承重 spike：0。
- human-decision：1 条，已明确指派 maintainer 在 Plane approve 前确认推荐 C。
- 这不是 `spec:approved`；只允许发布到 `spec:reviewing`。

## 未解 / punt 清单

- **[human-decision · approval gate] Q4：** maintainer 明确确认是否采用推荐 C（owner-controlled actor allowlist；默认 owner-only；offline queue opt-in）。若不同意，需回到产品/技术/验证 Spec 重写权限矩阵，不能直接 approve。
- **[future non-goal] 即时撤销 running agent：** 需要受控 sandbox 或所有外部 mutation 经过 fencing proxy；不属于 V1，当前保证是“确认旧进程终止后再转派”。

---

## 第二阶段 · maintainer 决策、独立复审与返工（2026-07-21）

上一版发布后，maintainer 重新拍板并要求 fresh agent 独立评审。以下记录覆盖从首个 `REQUEST CHANGES` 到最终 signoff 的完整收敛链；旧 Q4 的 allowlist 推荐已被新的 owner-only 决策替代。

### Q7 [human-decision · 产品/权限] 🟢

**问：** V1 谁能派活、研发决策人是谁、Node 丢失/多 Node/远程接管如何处理？

**答（maintainer）：** V1 仅 owner self-dispatch；研发决策人始终等于 dispatch owner；Node 丢失就等待恢复；不做 remote kill/takeover；每 owner/项目最多一个 active Node，多 Node 后续再做。

**判：** 解决并锁定。删除 actor allowlist、管理员代派、跨 owner target 和直接 reassign；新 owner 只能在旧 owner 停止并 release 后自行派发。

### Q8 [fresh review 1 · Authority/恢复/认证/投影] 🟢

**问：** 第一位 fresh reviewer 给出 `REQUEST CHANGES`：旧 Looper 可通过 legacy trigger 绕过、dispatch 状态机缺 attempt/fencing、revoke 后 stop_ack 死锁、签名不足以防重放、多角色面板没有持久化契约。

**答（作者）：** 改为按稳定 node_id 的 strict inbox；补 state_version、attempt、claim idempotency、lease/fencing、合法转移与故障恢复；增加 revocation_pending 的受限 stop_ack；定义 Ed25519 challenge/request 签名；增加 Event/RoleRequest/Artifact/Snapshot 持久化模型和真实独立 HOME E2E。

**判：** 方向解决，但第二位 reviewer 发现实现级细节仍不完整，继续返工。

### Q9 [fresh review 2 · mixed version/WAL/CBOR/角色 API] 🟢

**问：** 第二位 reviewer 仍判 `REQUEST CHANGES`：普通 PATCH 可补回 legacy trigger；本地 claim 缺 write-ahead 顺序；Admin pause 与 owner-only 冲突；签名无规范编码；角色回答没有显式 request/授权事务。

**答（作者）：** 增加不可变 work-item protocol；AttemptJournal 按 claim_pending→claimed→worktree→pidfile→termination summary 逐步 fsync；Admin pause只设 health、不释放归属；签名改 deterministic CBOR + CDDL + nonce transaction；删除通用语义 event ingest，回答必须绑定 role_request_id，并通过 durable outbox重建 snapshot。

**判：** 解决；验证补齐逐 fsync kill、未授权回答、乱序/outbox、真实 HOME 与直接 API 绕过。

### Q10 [fresh review 3 · 动态 trigger/双 daemon/签名类型/事件 Authority] 🟢

**问：** 第三位 reviewer 发现自定义热更新 trigger、复制 credential 的双 daemon、opaque node ID 与 UUID 混淆、suspend/revoke/rotate 恢复白名单、Node 伪造 event/artifact 语义、direct worker UI-only 门禁。

**答（作者）：** 增加 OS lock + 服务端 NodeSession CAS，空 journal/new session 不可接管；明确 opaque node ID 与 UUID 的 typed CDDL、query/path/header规范和历史公钥 allowlist；统一 recovery-only 请求；所有角色负责人/actor/event type 服务端派生；direct worker 必须有最新批准或 owner 的 Bug waiver。

**判：** 解决。第三轮 verdict 仍 `REQUEST CHANGES` 是因为后续发现 Planner→Worker handoff 和 mixed-binary 前提尚未锁死。

### Q11 [fresh acceptance gate · strict handoff/旧二进制/批准事实链] 🟢

**问：** “自动判断”如何从 Planner 原子进入 Worker？真实旧二进制无法被新协议约束，是否仍声称 mixed-binary 共存？technical-spec artifact/hash/approval 是否仍信 Node？CBOR challenge envelope 是否完整？

**答（作者）：** dispatch 拆为 requested_mode 与 active_role；有效 termination summary + owner 对服务端当前 Spec revision/hash 的批准触发 `awaiting_human(planner)→queued(worker)`，新 attempt/token 且不写 worker-ready。V1 明确禁止旧、新 daemon 混跑，只允许 legacy/strict work item 由新版二进制共存处理。Artifact 只收对象引用，服务端核验归属、revision、hash；新增 owner-authenticated approval。签名补 pure Ed25519 transcript、trust-key revision、signed envelope和跨语言 golden vectors。

**判：** 核心方向解决；release gate 剩 3 个文档一致性 Major。

### Q12 [fresh release gate · 状态/纵深防御/work-item hash] 🟢

**问：** awaiting_human stop 的 API 与状态图冲突；生产 guard 与 test-only legacy 注入混写；approval 缺 work_item_content_hash。

**答（作者）：** awaiting_human 复用已验 termination summary 直接 confirmed_stopped；生产 domain/DB guard 拒绝 effective legacy trigger，协议 gate 的双保险只通过隔离 DB fixture 验证；approval 保存 RFC8785-JCS work-item content hash，并在批准、handoff、direct worker 时重算。

**判：** 三项 Major 解决；release reviewer确认其余恢复、安全、角色和 handoff 无新 Blocker。

### Q13 [fresh signoff · post-epoch 抢注/link carrier] 🟢

**问：** strict epoch 后的新任务是否还能先加 trigger 被分类 legacy？link request 是否同时携带 Plane 验证所需的 loopernet challenge 与 Node proof？

**答（作者）：** post-epoch work item 在创建事务中强制写 immutable strict_v1，生产不存在 unclassified→legacy；link-request CDDL 外层同时携带 challenge envelope 与 proof envelope，Plane 在一个事务验证 trust root、digest、公钥、audience/expiry、单次消费并创建 binding。

**判（同一 fresh reviewer 复核）：** 两项均 `RESOLVED`；未发现 Blocker/Major；最终 verdict `APPROVE`。

## 第二阶段 Execution mode 复核

- 最终建议仍为 `HITL-needs-engineer`。
- 产品/权限 open decision：0；Q7 已由 maintainer拍板。
- needs-spike：0；运行时未知均转化为明确协议和可证伪故障注入/E2E。
- 人工 gate：安全协议 golden vectors、Plane migration、daemon 全量升级与旧进程清单、真实双 HOME、Planner→Worker→PR、视觉风险态与上线/回滚必须由工程师/维护者签收。

## 最终 verdict：APPROVE（fresh signoff）

这是独立 reviewer 对 spec 质量的结论，不等于 Plane `spec:approved`。Plane 仍保持 `spec:reviewing`，最终批准与 execution-mode label 由 maintainer 人工执行。
