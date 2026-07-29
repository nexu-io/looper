# OPEND-1645 · Plane × Looper 派发与协作状态 · 技术 Spec

## 引用映射

- 权限与身份：产品 AC-1～AC-5、AC-9、AC-13、AC-15。
- 唯一派发与离线恢复：产品 AC-6～AC-8、AC-14。
- 多角色和产物可视化：产品 AC-10～AC-12、AC-16～AC-17。
- 埋点：产品 AC-18。

## Sources

- Plane `preview@90ae8457d07c9b5e5e0514735170f55304506cc9`。
  - `apps/web/core/components/issues/issue-detail/root.tsx:61-269`：权限和详情页插槽。
  - `apps/web/core/components/issues/issue-detail/main-content.tsx:95-192`：完整面板挂载点。
  - `apps/web/core/services/issue/issue.service.ts:445`：现有 work item API service 约定。
  - `packages/constants/src/user.ts:29-70`：项目权限级别。
  - `apps/api/plane/app/permissions/project.py:85-116`：现有通用 work-item mutation 对 Project Member/Admin 开放。
  - `apps/api/plane/api/views/issue.py:740-795`、`apps/api/plane/api/serializers/issue.py:234-283`：通用 PATCH 可直接整组修改 assignees/labels，因此不能充当不可绕过的 dispatch gate。
- Looper `feat/looper-auto-flowchart-runtime@5c26cd1d0fe5f7839868024a7a833b5083eb8680`。
  - `internal/forge/plane.go:181-217,233-369`：Plane work item 发现、标签、assignee、评论。
  - `internal/planner/runner.go:690-752`：Planner 领取。
  - `internal/worker/runner.go:994-1083`：Worker 的 Plane owner 过滤与领取。
  - `internal/network/protocol/protocol.go:36-98,137-159`：节点注册、能力、心跳、target label。
  - `internal/network/cloud/service.go:59,215,371-479`：成员与 capabilities JSON 持久化/查询。
  - `internal/config/validate.go:483-505`：当前 routed 模式仍含 GitHub 专属约束，不能直接假设 Plane Planner 已支持 Network 路由。
  - `internal/api/handler.go:1963-1997,2280-2329,6178-6219`：本地 loop/relationships 语义。
  - `internal/runtime/awaiting_role_decisions.go:309-319`、`internal/runtime/blocked_conditions.go:230-247`、`internal/runtime/spec_approval.go:112-120`：当前多角色资格读取每台 Node 的本地项目配置，多 Node 下需要 Plane 单一策略。
  - `internal/planner/runner.go:2438-2442`、`internal/worker/runner.go:2228-2241`、`internal/agent/executor.go:232-235`：agent 是 Start/Wait/Kill 黑盒进程，没有逐 tool side-effect fencing boundary。
- Work item：`OPEND-1645` / `a6eb4e1e-67d7-43f5-8f83-c023faa446cd`。

## Authority 声明

**Plane 服务端持久化的活动 `LooperDispatch` 记录是本次执行归属的 Authority。** work item 上的 owner assignee 与 `looper:strict-dispatch` 只是可见投影，不构成发现或领取条件。Looper 只从按稳定 `node_id` 索引的 strict inbox 获取任务，并在创建 loop、每次开始新 run和受控 publish 前核对 dispatch revision/state version/attempt/fencing token；没有合法记录时 fail closed，投影漂移只告警而不反向改变 Authority。

为什么必须新增 `LooperDispatch`：Plane 现有通用 work-item PATCH 对 Project Member/Admin 开放，成员可以绕过新按钮直接改 labels/assignees；仅靠 UI 约定或 dispatch 路由的 row lock 无法覆盖这些写路径。持久化记录防止“手工拼字段就启动 agent”和并发覆盖归属。成本是新增表、revision 生命周期、漂移状态和兼容路径；相比拦截所有通用 PATCH、区分人类与 Looper API key，这个 invariant 更局部且可审计。

loopernet 的成员目录、在线状态、负载和实时 loop 摘要仍只用于资格校验、唤醒与观测；它们失联不会自动取消或改派 Plane 上的 durable dispatch。`LooperDispatch` 也不是第二套任务队列：任务内容和团队状态仍在 work item，记录只保存一次执行的归属与 revision。

## 总体架构

```text
Plane Web
  ├─ GET  work-item looper summary ──┐
  └─ POST dispatch/stop/release ─────┼─ Plane API Looper integration
                                     │    ├─ 权限/事务/幂等
                                     │    ├─ 更新 assignee + trigger + target
                                     │    └─ 查询 loopernet directory（观测）
                                     │
                                     └── loopernet
                                           ├─ Node membership + signed challenge
                                           └─ heartbeat presence + dynamic load

local looperd
  ├─ heartbeat 身份/能力/摘要 → loopernet
  ├─ poll/webhook 读取 Plane durable intent
  ├─ exact owner + target 校验后领取
  └─ 阶段、链接、决策状态写回 Plane
```

## 数据与协议

### 1. Plane 项目角色策略（多 Node 的单一 Authority）

新增 `LooperProjectRolePolicy`，作为 Plane 集成项目中产品/设计/研发/QA 身份的唯一 Authority：

- 字段：`project_id`、单调递增 `revision`、`product_member_id`、`design_member_id`、`qa_member_id`、`engineering_policy=dispatch_owner`、`updated_by`、`updated_at`。
- Project Admin 可在项目集成设置中修改映射并填写变更原因；修改权限不等于获得回答/审批权，只有被映射的成员能执行对应专业动作。
- `engineering_policy=dispatch_owner` 固化当前团队决策：研发问题和待审批技术 Spec 路由给该 dispatch 的 Looper owner。
- 创建 `LooperDispatch` 时把 `role_policy_revision` 和 product/design/QA member IDs 快照入 dispatch；Node 只消费该快照，不从自己的本地 config 决定角色。
- 项目策略更新默认只影响新 dispatch。要修改在途任务，Admin 必须显式执行 `更新在途角色` 并填写原因，生成新的 dispatch role revision；未解决的旧 revision 决策请求被 supersede 后重新发给新负责人，已解决的决定保留 actor/revision 审计。
- 成员离开 project/workspace 时，引用该成员的策略与活动 dispatch 设置 `health=role_drift`，停止新问题/审批/side-effect，直到 Admin 重映射。
- 技术 Spec 已由旧 owner 针对相同 content hash 批准后释放，随后新 owner self-dispatch 时批准仍有效；若内容改变或批准尚未发生，则新 dispatch owner 才能批准。

现有每台 Node 的 `ProductOwner/DesignOwner/QA/Owner.PlaneID` 在启用 Plane 集成严格模式后只用于 bootstrap/迁移提示，不再是运行时 Authority。Node 发现本地值与 dispatch 快照冲突时必须 fail closed 并报告 config drift，不能用本地配置覆盖 Plane。成本是新增项目策略表、revision/snapshot 与角色变更迁移，但它防止新 owner 接手时悄悄更换决策人。

### 2. Plane ↔ Node 身份绑定（不信任 Node 自报 owner）

新增 Plane `LooperNodeBinding`，作为 `node_id ↔ Plane workspace/project/member` 的身份 Authority：

- 字段：`workspace_id`、`project_id`、`member_id`、`node_id`、`node_name_snapshot`、`allowed_roles`、`allow_offline_queue`（默认 false）、`state(active/revocation_pending/revoked；pending 仅兼容旧数据)`、`created_at/revoked_at`、`revision`。Admin pause 是独立的 project-integration health，不是 binding state。
- `LooperConnectionSession(member_id,connect_code,status,expires_at,binding_id,node_id,error,completed_at)` 承载 Plane-first 的 10 分钟连接向导；只允许 session owner 从 Plane 读取或取消，CLI 只持高熵一次性 code。V1 同一 `(project_id,member_id)` 只允许一个 active binding，已有设备时明确阻止替换。
- `LooperNodeKey(binding_id,key_revision,public_key,state,created_at,revoked_at,stop_ack_attempt_allowlist)` 保存每个 revision 的历史公钥；旧 key 只有 attempt ID 在不可扩大的 allowlist 中时才能验证 recovery/stop_ack，不能 claim、新 run 或普通 event。
- V1 对 `(project_id, member_id)` 建 active unique constraint：一个 owner 在一个项目最多一个 active Node，不设计多 Node 选择或 actor allowlist。
- 每个 binding 还有唯一 `LooperNodeSession(session_id, binding_id, instance_nonce, lease_expires_at, last_renewed_at, state)`。daemon 启动时先在 `<HOME>/.looper/runtime/<binding_id>.lock` 取得 OS 跨进程独占锁，再用签名请求建立/恢复 session；复制到另一 HOME 的 credential 也必须经过服务端 session CAS。binding 存在 holding dispatch 时，新 instance 不能抢占过期 session，只有原 journal 持有的 `session_id + claim_idempotency_key + attempt/fencing token` 可恢复；缺失正向记录一律等待。
- Node **不能提交 ownerPlaneMemberId**。owner 来自创建绑定请求时 Plane 已认证的 `request.user/member`。
- 本地执行 Plane 页面给出的 `looper plane connect <origin> --code <one-time-code>` 时同时完成两份证明：
  1. 一次性 code 对应创建它的 Plane 登录用户、workspace 和 project，CLI 不能提交或覆盖 owner ID，也无需读取用户的 Plane personal API key；
  2. Node 本地产生 Ed25519 keypair，用 loopernet node token 申请一次性短时 challenge。challenge 固定编码 `version + network_id + node_id + public_key_sha256 + audience=plane:<workspace_id> + challenge_id + nonce + issued_at + expires_at`，由 loopernet 服务端签名；Node 再用私钥对完整 binding request 签 possession proof。Plane 使用显式配置的 loopernet trust-root 公钥验 challenge，校验 audience/expiry/单次消费，再用请求中的公钥验 possession proof。
- 两份证明通过后 binding 直接以 `planner/worker` 角色进入 active；无需 Project Admin 审批成员自己的电脑。Project Admin 仍只负责项目级角色策略、strict rollout 与暂停集成，不能替代 owner 身份证明或替别人派发。
- dispatch 强制 `request.user == binding.member_id == dispatch.owner_member_id`；Admin/Member 都不能替另一 owner 创建 dispatch。
- claim/transition 不依赖长期保存 owner personal API key；使用 active binding 的 Node 私钥签名，并在 claim、每次新 run、受控 publish 前由 Plane 实时确认 owner 仍是 project/workspace 成员。成员移除 hook 只负责加速收敛，不能替代该检查。
- owner 可 revoke；Project Admin 只能暂停项目集成。Admin pause 仅把 project/dispatch health 设为 `binding_suspended`，阻止新 self-dispatch/claim/run/publish，但不改变 lifecycle、不释放任何 owner 的 holding dispatch。pause/revocation_pending 期间统一 recovery-only allowlist：只接受原 session/attempt 的 reconcile、termination summary 和 stop_ack；其余 Node mutation 全拒绝。
- owner 主动 revoke queued binding时，在同一事务执行 `queued→released`、写 owner/原因审计后进入 revoked。claimed 但尚未启动时，必须由原 session 按正向 journal 证据先 reconcile 回 queued，再由 owner revoke；证据缺失就等待。awaiting_human 已有已验 termination summary，owner stop 可直接 CAS 为 confirmed_stopped，再显式 release。running 则进入 `stop_requested + revocation_pending`，原 attempt stop_ack 后 confirmed_stopped，仍需 owner release。Node 永久离线时保持等待，V1 无 break-glass。
- active/holding execution 存在时拒绝 rotate；owner 必须先按上面流程 stop/release。没有 holding dispatch 时 rotate 创建新 key revision并立即禁用旧 key；这避免 rotation 中出现两个可发普通请求的公钥。
- node rename 以稳定 `node_id` 为准；显示名可更新，已有 dispatch 保存 snapshot。任何显示标签都不替代 node_id。

签名协议必须使用 RFC 8949 deterministic CBOR，禁止用字符串拼接。三个 domain-separated profile 均先编码 canonical CBOR map，再签 `SHA-256(profile_utf8 || 0x00 || cbor_bytes)`：

- `LOOPER-LINK-CHALLENGE-V1`：loopernet 签 `{network_id,node_id,public_key_sha256,audience,challenge_id,nonce,issued_at_ms,expires_at_ms}`；所有 ID/nonce 为固定字节串，不用可变分隔文本。
- `LOOPER-LINK-PROOF-V1`：Node 签 `{challenge_sha256,plane_workspace_id,plane_project_id,member_id_from_plane_session,public_key,key_algorithm="Ed25519"}`，证明 challenge、公钥和当前 Plane 身份绑定在同一请求。
- `LOOPER-NODE-REQUEST-V1`：Node 签 `{method,path,query,body_sha256,binding_id,key_revision,dispatch_id,dispatch_revision,state_version,execution_attempt_id,fencing_token,timestamp_ms,nonce}`。method 必须大写；path 是解码后再按 RFC 3986 重新 percent-encode 的绝对路径；query 对解码后的 key/value 以字节序排序、逐项 percent-encode，必须包含 inbox 的 `node_id/cursor`；body hash 基于收到的 raw bytes。不存在的 dispatch/attempt 字段使用 CBOR `null`，不得省略。

规范性 CDDL（map key 使用整数，下面名字仅为说明；所有字段都必须存在）：

```cddl
uuid = bstr .size 16
sha256 = bstr .size 32
nonce = bstr .size 16
opaque-id = tstr .size (1..128) ; 现有 network_id/node_id，如 node_<token>，不假装 UUID
link-challenge = {1: 1, 2: opaque-id, 3: opaque-id, 4: sha256, 5: tstr,
                  6: uuid, 7: nonce, 8: int, 9: int}
link-proof = {1: 1, 2: sha256, 3: uuid, 4: uuid, 5: uuid,
              6: bstr .size 32, 7: "Ed25519"}
node-request = {1: 1, 2: tstr, 3: tstr, 4: tstr, 5: sha256,
                6: uuid, 7: uint, 8: uuid / null, 9: uint / null,
                10: uint / null, 11: uuid / null, 12: uint / null,
                13: int, 14: nonce}
signed-envelope = {1: uint, 2: "Ed25519", 3: bstr, 4: bstr .size 64}
; 1=trust/key revision, 3=exact deterministic-CBOR payload bytes, 4=signature
link-request = {1: signed-envelope, 2: signed-envelope}
; 1=loopernet-signed challenge envelope, 2=Node-signed link-proof envelope
```

其中 UUID 字段只用于 Plane/binding/dispatch/attempt；现有 loopernet `network_id/node_id` 按 UTF-8 opaque text 编码。path 必须以 `/` 开头、移除 dot segments、拒绝反斜线/控制字符/percent-encoded slash，并以 uppercase hex percent encoding输出。query 按原始 RFC 3986 规则解析：`+` 永远是加号而不是空格；无 `=` 参数拒绝；空值编码 `key=`；重复 key 保留；解码后按 `(key_bytes,value_bytes)` 排序，再对非 unreserved bytes 用 uppercase percent encoding，`%2F` 保持数据而不是路径。无 query 编码为空 tstr。header 必须恰有 `v,key,ts,nonce,sig` 且不得重复；`key=<binding UUID>:<uint revision>`，base64url 无 padding。

HTTP 使用 `Looper-Signature: v=1; key=<binding-id>:<key-revision>; ts=<int64-ms>; nonce=<base64url-128bit>; sig=<base64url-ed25519>`。Plane 允许最大 ±120 秒时钟偏差，并以 `(binding_id,key_revision,nonce)` 保存 10 分钟 replay record。nonce 插入与对应业务 mutation/claim/transition 在同一数据库事务提交；只读 inbox 也先事务化消费 nonce再返回。重复 nonce、过期时间、query/body/path/空字段或 header/CBOR 字段不一致均拒绝。

规范性签名 transcript：`digest = SHA-256(UTF8(profile-name) || 0x00 || exact_payload_cbor_bytes)`；signature 固定为 **pure Ed25519 对这 32-byte digest 签名**，不是 Ed25519ph，也不再内部预哈希。`link-proof.challenge_sha256` 就是 `LOOPER-LINK-CHALLENGE-V1` 上述 digest，不是 envelope hash或 payload 单独 hash。loopernet challenge 以 `base64url(no-padding, deterministic-CBOR(signed-envelope))` 传输；Plane 先按 `LoopernetTrustKey(key_revision,algorithm,public_key,not_before,not_after,state)` 找 trust root，验证 envelope algorithm/key revision/signature，再验证 payload。

link request 使用 `Content-Type: application/vnd.looper.link-request+cbor;v=1`，HTTP raw body是 deterministic-CBOR `link-request`，同时携带两份不可省略的 envelope：

1. `challenge_envelope`：loopernet trust-key revision + exact link-challenge payload + loopernet pure-Ed25519 signature；
2. `proof_envelope`：字段 1 固定为 `0`（binding key revision 尚未分配），字段 2 为 `Ed25519`，字段 3 是 exact link-proof payload，字段 4 是新 Node key 的 pure-Ed25519 signature。

Plane 先从 challenge envelope key revision 找 trust root并验证签名/algorithm，再解出 challenge，校验 audience/expiry/network/node/public-key hash；计算规范 challenge digest并与 proof payload 的 `challenge_sha256` 逐字节比较，再用 challenge 中绑定的 public key 验 proof envelope。challenge ID/nonce 单次消费、binding 创建和 key revision 1 分配在同一事务提交；任一步失败都不消费、不建 binding。Node request header 中的 sig 使用相同 transcript 规则，header 的 ts/nonce/key 必须与 decoded node-request 字段逐字节相等。

密钥轮换要求 owner 重新完成 Plane 身份 + loopernet challenge：旧 key 不再允许新 claim；若存在 active execution，只在其结束前接受绑定该 attempt/fencing token 的 `stop_ack`，不设通用 grace。S2a 的首个可合并产物必须是 Plane/Python 与 Looper/Go 同时消费的规范 golden-vector fixture；覆盖 challenge envelope、link proof、GET query、JSON raw body、null、opaque node ID、Unicode、重复 query、篡改、错误 key revision 和 pure-Ed25519/Ed25519ph 混淆。vectors 未跨语言通过不得实现业务 endpoint。

新增绑定的成本：需要 Plane 表/管理 UI、Node keypair、loopernet challenge endpoint、trust-root 配置、nonce store、吊销/轮换与时钟偏差处理。它防止已入网 Node 在 heartbeat 里冒充其他 Plane 成员；仅把第一次自报永久化不构成身份认证。

### 3. loopernet V1 边界（删除重复状态机）

- V1 不增加 `planeProjects[]` 或 `activeLoops[]` digest。项目/角色资格已由 Plane `LooperNodeBinding` 表达，运行阶段已由 `LooperDispatch` claim/transition 与 Plane marker 表达；再写一份 capabilities JSON 只会产生双写、陈旧合并和隐私过滤问题。
- loopernet 只提供现有 Node membership、最后 heartbeat、通用 roles/dynamic load，并新增绑定时所需的短时 signed node challenge。
- 在线定义：最后心跳不超过 `2 × heartbeat interval + grace`；超时只影响展示。Plane summary 用 Node ID 连接 binding 与 membership；phase 不从 loopernet 合并。
- 若将来需要秒级日志/进度流，另设只读流式 observation 协议，不复用 heartbeat capabilities 承担生命周期持久化。

### 3.5 Work item 协议分类（阻断旧 Looper 绕过）

新增不可变 `LooperWorkItemProtocol(work_item_id, protocol=legacy|strict_v1, classified_at, classified_by, project_strict_epoch)`。它不是展示 label，而是 work item 的一对一协议记录。

`LooperProjectProtocolPolicy` 保存 `strict_epoch`、全部已升级 binding 签名上报的 effective legacy trigger label ID 并集、Node capability revisions 与 activation checklist revision。它只服务纵深防御和启用门禁，不允许旧二进制共存。

- 项目首次开启 strict 功能时写入 `project_strict_epoch`。epoch 之前的已有任务一律批量分类为 legacy，不做原地升级；epoch 之后创建的新 work item 必须在创建事务内同时写 `strict_v1` 分类，不存在生产 `unclassified` 或“先加 trigger 抢注 legacy”的状态。
- 同一 work item 的分类一经写入不可修改；strict 和 legacy 可以存在于同一项目，但同一任务永不互转。回滚功能也不能删除或降级分类。
- strict dispatch 只接受已分类 `strict_v1` 的 work item；legacy trigger mutation 只接受 epoch 前已分类 legacy 的 work item。post-epoch 新任务、strict 分类和 legacy trigger 不存在竞态或转换入口。
- V1 **不支持旧版 daemon 与 strict-enabled 项目共存**。启用前所有连接该 Plane project 的 Looper 必须升级并上报 `strict_dispatch_v1` capability；Project Admin/Looper owner 完成进程清单、停止旧 daemon、移除其启动项，并轮换仅供旧 Looper 使用的凭据。任何 membership heartbeat 缺 capability、已知 Node 版本回退或人工清单未签收时，项目只能 read-only，不能 self-dispatch。
- legacy work item 可以保留，并由已升级 Node 的兼容路径处理；已升级 Node 即使本地 legacy trigger 热更新，也必须先查询不可变 protocol，遇到 strict_v1 一律跳过。对 strict work item 的任何 legacy label 都只是普通投影，无法绕过新版 daemon 的 protocol gate。
- 纵深防御仍在 API domain guard + 数据库 IssueLabel join trigger 两层拒绝把 policy 中任一 effective legacy trigger label ID 加到 strict work item；单条 PATCH、bulk、import、automation、内部 ORM 都适用。已升级 Node 配置热更新 trigger 时先签名上报新的 capability revision，Plane 更新并集前扫描 strict work item；若已有冲突 label 则配置更新/新派发 fail closed。即使测试绕过 guard，新版 daemon 仍必须靠 protocol gate 跳过。
- 安全边界明确排除“持有有效 Plane credential、却不遵守 strict protocol 的未知旧/恶意客户端”；V1 rollout 通过升级、停进程和凭据治理移除它，而不是声称新服务端能约束任意旧二进制。若团队需要长期 mixed-binary 共存，必须另做受控 launcher/attestation 项目，不在 V1 内。
- 对 `legacy` work item，strict dispatch endpoint 返回 409 `legacy_work_item_not_upgradeable`。如需改造，必须新建 strict work item 并显式关联原任务，不能复用旧 ID。

因此同一项目可同时保留 legacy/strict work item，但只能由已升级 Looper 二进制运行；不能把“任务协议共存”误解为“旧、新 daemon 共存”。

### 4. Plane durable dispatch 表达

- 新表 `LooperDispatch`（具体 app 命名按 Plane 约定）：
  - `id` UUID、`work_item_id`、单调递增 `revision`、单调递增 `state_version`；
  - `requested_mode`（auto/worker）、`active_role`（planner/worker）、`owner_member_id`、`node_id`、`node_name_snapshot`；
  - `role_policy_revision`、`product_member_id_snapshot`、`design_member_id_snapshot`、`qa_member_id_snapshot`、`engineering_member_id_snapshot=owner_member_id`；
  - `node_binding_revision`、`dispatched_by_member_id=owner_member_id`；
  - `state`（`queued/claimed/running/awaiting_human/stop_requested/confirmed_stopped/released/completed/failed`）；
  - `wait_kind`（null/role_decision/technical_spec_approval/qa）与 `active_role_revision`；
  - `health`（`ok/projection_drift/role_drift/binding_suspended/node_unreachable`），不与 lifecycle state 混用；
  - `execution_attempt_id`、`claim_idempotency_key`、单调递增 `fencing_token`、`claim_lease_expires_at`、`last_node_ack_at`；
  - `created_by`、`created_at`、`updated_at`、`idempotency_key`、`release_reason`；
  - 对 holding states 建立每个 work item 最多一条的 partial unique constraint；`work_item_id + idempotency_key` 唯一。
- strict Planner/Worker **不写** legacy `looper:plan` / `looper:worker-ready` trigger；Node 通过按稳定 `node_id` 索引、带签名的 strict dispatch inbox 发现任务。
- work item 可显示 `looper:strict-dispatch` 与 owner assignee 作为人类可见投影，但旧 Looper 不把它识别为可领取 trigger。
- owner：work item assignees 中恰好一个属于注册 Looper owner；其他非 Looper 角色 assignee 保留。
- 审计：新增一条带稳定 HTML marker 的 Plane comment：

```html
<!-- looper:dispatch v=1 id=<uuid> owner=<member_id> requested=auto active=planner -->
<p>已由 <actor> 派给 <owner> 的 Looper（<node_name>）…</p>
```

`dispatch_id` 由服务端生成；重复 `Idempotency-Key` 返回同一结果。评论是审计记录，labels/assignees 是投影，二者都不是领取 Authority。

### 5. Durable invariant、状态机与恢复

对启用此功能的 project，任意时刻都必须满足：

```text
可领取 = 存在唯一 active LooperDispatch D
      AND work item 未 archived/completed
      AND strict inbox 的认证 node_id == D.node_id
      AND claimant reports D.id + D.revision + D.state_version
      AND active LooperNodeBinding matches D.owner_member_id + D.node_id
      AND request signature proves bound Node private key
      AND D.owner_member_id 当前仍是 project/workspace member
      AND D.role_policy_revision is valid and has no role drift
      AND D.dispatched_by_member_id == D.owner_member_id
      AND offline dispatch was allowed when D was created
```

- holding states = `queued/claimed/running/awaiting_human/stop_requested/confirmed_stopped`，都占用每个 work item 唯一活动归属；terminal states = `released/completed/failed`。`health` 不参与 unique constraint。
- 合法边：`queued→claimed|released`；`claimed→running|queued|failed|stop_requested`；`running→awaiting_human|completed|failed|stop_requested`；`awaiting_human(role_decision)→claimed(planner)`；`awaiting_human(technical_spec_approval)→queued(worker)`；`awaiting_human→confirmed_stopped`；`stop_requested→confirmed_stopped`；`confirmed_stopped→released`；terminal 不可回退。每次 transition 使用 `(dispatch_id, expected_state_version)` CAS，成功后 `state_version+1`。
- `requested_mode=auto` 初始 `active_role=planner`。Planner process termination summary 已被接受、technical-spec artifact 的当前服务端 revision/hash 已由 owner 批准后，Plane 在单事务执行 strict handoff：校验 approval→`active_role=worker`→`active_role_revision+1`→`state=queued`→`state_version+1`→追加 handoff event。owner/node/dispatch ID 和唯一归属不变；不写 `looper:worker-ready`。Worker 从同一 strict inbox 领取时创建全新 attempt/fencing token。
- `requested_mode=worker` 直接以 `active_role=worker,state=queued` 创建，但仍受 approved spec/waiver 服务端门禁。handoff 条件不满足时保持 awaiting_human，不允许 Planner 自行承担 Worker，也不允许客户端直接改 active_role。
- claim 生成 `execution_attempt_id` 与新的单调 fencing token；同一 `claim_idempotency_key` 在响应丢失时返回相同 attempt/token，不创建第二次领取。所有 transition、角色事件和 Looper-managed publish 都必须携带 attempt + fencing token，旧 token 返回 409。
- `claimed` lease 过期时，只有原 binding session 提交持久化的 `session_id + claim_idempotency_key + execution_attempt_id + fencing_token`，并由 AttemptJournal/pidfile/process containment 共同证明从未启动进程，才能把它 CAS 回 queued；空 journal、复制 credential、新 session或只有“没看到 PID”的否定证据都进入 `health=node_unreachable` 等待，不能猜测重跑。
- `running` 心跳过期只设置 `health=node_unreachable`，不改变 lifecycle、不释放 unique constraint。Node 恢复后先上报本地 PID/attempt 状态：进程仍在则恢复 heartbeat；已退出则按 exit result transition；无法证明则继续等待。
- Plane 暂时不可用时，Node 不开始新 run、不发布评论/分支/PR；已启动黑盒 agent 可能继续产生本地或直接外部副作用，恢复后以 fencing token reconcile。旧 token 永远不能覆盖新 state。
- Node 必须使用本地 `AttemptJournal` 做 write-ahead，所有阶段用 SQLite transaction + WAL checkpoint/fsync 持久化：
  1. 发 claim 前先写 `claim_pending(dispatch_id, revision, claim_idempotency_key)` 并 fsync；
  2. 收到响应后先写 `claimed(attempt_id, fencing_token, state_version)` 并 fsync，之后才允许创建 worktree；响应丢失/重启后只用持久化的 claim key重试，服务端返回同一 attempt/token；
  3. 创建 worktree 后写 `worktree_ready(path, content_hash)` 并 fsync；
  4. 启动统一 executor wrapper。wrapper 在 exec agent 前原子写 attempt 专属 pidfile（PID/process-group/start-time）并 fsync，再写 journal `process_started`；父进程崩溃时恢复逻辑同时检查 journal、pidfile 和 OS process start-time，任一证据不一致就等待人工恢复，绝不当作“未启动”；
  5. wrapper wait 到子进程/process-group 全部退出后，写并签名 `termination_summary(exit_status, exited_at, process_group_empty, worktree_hash)`；只有该摘要持久化并被 Plane 接受，`running` 才可转 `awaiting_human/completed/failed/stop_requested→confirmed_stopped`。
- `running→awaiting_human/completed/failed` 与 `stop_ack` 使用同一终止证明规则；completed/failed 释放唯一约束前必须确认 process group 不存在。每个 write-ahead 边界都做逐点 kill 测试。
- `queued→claimed`、角色回答后的 `awaiting_human(role_decision)→claimed`，以及 Planner→Worker handoff 后的首次 Worker claim，每次都生成新的 `execution_attempt_id` 和更大的 fencing token；旧 attempt 只能读审计，不能恢复运行。进程终止判定复用现有 `internal/processcontainment` 的 confirmed-dead + PID start-time/reuse 校验；单看 PID 不存在或 exit code 不足以作为证明。
- Plane 原生属性 UI/API 仍可编辑普通 assignee/label，但它不能创建或改变 `LooperDispatch`。投影不一致只设置 `health=projection_drift` 并显示“恢复显示字段”动作；Node 直接查询 strict inbox，领取不依赖这些投影，因此普通 PATCH 既不能改派，也不能制造执行层 DoS。
- 只有 owner 的 `dispatch/stop/release` integration 路由可改变 Authority。V1 没有跨 owner reassign；旧 dispatch terminal 后，新 owner 才能 self-dispatch。
- 对已开始的 agent 子进程不存在逐 tool 的强制 boundary：Planner/Worker 当前 `Start` 后 `Wait`，只有 process-level `Kill`。因此第一版不允许 running dispatch 被 supersede 或接管；owner 先请求停止，Node 回报进程终止后变 `confirmed_stopped`，再显式 release。
- Looper 在自己掌控的 post-run 评论、状态更新、push/PR 等 gateway 边界必须刷新 dispatch revision；这只能阻止 Looper-managed publish，不能声称追回黑盒 agent 已做的本地 shell/file/直接远程副作用。UI 必须提示检查旧 worktree。

### 6. 多角色协作持久化契约

核心面板不得从各 Node 的本地 SQLite 临时拼状态。Plane 新增可重建、带版本的数据契约：

- `LooperCollaborationEvent`：`id`、`dispatch_id`、`event_version`、`source_event_key`（唯一幂等键）、`event_type`、`phase`、`role`、`role_request_id`、`actor_member_id`、`role_policy_revision`、`artifact_id`、`payload_schema_version`、`occurred_at`、`recorded_at`。只追加，不原地改历史。
- `LooperRoleRequest`：`id`、`dispatch_id`、`source_event_key`、`role(product/design/engineering/qa)`、`question_summary`、`eligible_member_id`、`policy_revision`、`status(open/answered/superseded/expired)`、`answer_comment_id`、`created_at/answered_at`；唯一 `(dispatch_id,source_event_key)`。四类 `eligible_member_id` 均由服务端从 dispatch role snapshot 派生，Node body 不可指定；研发永远等于 dispatch owner。
- `LooperArtifact`：`id`、`dispatch_id`、服务端派生的 `type/title/url/content_hash/source_revision_id`、`source_kind(plane_page/plane_comment/scm_pr)`、`source_object_id`、`source_event_key`、`created_at`，唯一 `(dispatch_id,source_event_key)`。Node 只能提交受限对象引用和 intent，不能提交可信 type/url/hash；Plane/SCM 服务端读取对象、验证其属于当前 workspace/project/work item 或配置仓库，再生成字段。只允许 HTTPS，origin 必须属于配置的 Plane base URL 或代码托管 allowlist，拒绝 localhost/private IP/file/data URL与重定向到非 allowlist origin。
- `LooperTechnicalApproval`：`id`、`work_item_id`、`dispatch_id`、`artifact_id`、`source_revision_id`、`content_hash`、`work_item_content_hash`、`approved_by_member_id=dispatch.owner`、`role_policy_revision`、`approved_at`、`state(active/superseded)`；只有 Plane-authenticated owner approval API 能创建。任何 source revision、服务端 hash、work-item content hash 或 role revision 改变都会 supersede。
- `work_item_content_hash = SHA-256(RFC8785-JCS({name, description_html, issue_type_id, priority_id, spec_links_sorted}))`。文本先校验 UTF-8、转 NFC、统一 LF；`description_html` 使用 Plane 服务端已 sanitize 的 stored bytes，spec links 按 `(title,url)` 字节序排序。approval/waiver 创建、Planner→Worker handoff 与 direct Worker dispatch 都重新计算；任一标题、正文、类型、优先级或 spec link 变化都使旧记录失效。
- `LooperCollaborationSnapshot`：由事件表事务内投影，包含 `phase`、`phase_started_at`、每个角色 open/answered count、`waiting_role/member`、最近事件、artifact 列表、`snapshot_version`、`events_sha256`。hash 算法为按 event_version 排序后的 deterministic CBOR event 数组做 SHA-256；任何时候可按 events 重建并校验。

Looper 写角色问题、回答感知、Spec/PR/QA 产物时使用稳定 `source_event_key` 幂等 upsert；同一 dispatch 内按 Plane 分配的 `event_version` 排序，迟到事件若不符合当前 lifecycle/role revision 则记录为 superseded，不能倒退 snapshot。原生 Plane comment/Page/PR 是正文事实源，协作表只保存引用、摘要和状态，不复制正文。

### 7. 状态聚合

Plane 面板 summary 按以下优先级聚合：

1. `LooperDispatch` 的 claim/transition、owner/node/revision（durable）。
2. `LooperCollaborationSnapshot` + RoleRequest/Event/Artifact（durable collaboration projection，可从事件重建）。
3. Plane work item 的 assignee、strict marker、state、Spec/PR links（durable display projection/artifacts）。
4. loopernet 最近 heartbeat 只提供 Node presence/dynamic load（ephemeral observation）；不可用时显示 `live_status=unavailable/stale`，不覆盖 Plane phase。

## Plane API

在现有 work item 权限体系下增加独立 integration 路由（命名可按上游 API 约定调整）：

- `GET /api/v1/workspaces/{ws}/projects/{pid}/work-items/{wid}/looper/`
  - 返回 `permissions`、`dispatch`、`owner/node`、`liveStatus`、`phases`、`roles`、`artifacts`、`availableActions`。
- `GET .../looper/targets/?requested_mode=auto|worker`
  - V1 只返回当前登录用户自己的唯一 active binding；再与 loopernet membership/presence、Plane project membership、当前 actor 的 edit permission 取交集。不返回同事 Node，不信 heartbeat 自报 owner，也不把 admin token 下发浏览器。
- `POST .../looper/dispatch/`
  - body：`requested_mode(auto|worker)`、`idempotency_key`；target 从当前用户唯一 active binding 得出，不接受任意 `target_node_id/owner_member_id`。
  - 在 DB transaction 内 `select_for_update` work item 和 active dispatch，二次校验 `request.user == binding.member_id`、实时 project membership、权限、Node capability、archive/done；先创建唯一 dispatch revision，再更新人类可见投影并写审计 comment。
  - `requested_mode=worker` 不能只信前端置灰：服务端要求二选一——存在 owner 对最新 technical-spec artifact 当前 revision/content hash 的有效批准记录；或存在同一 owner 创建的 `LooperDirectImplementationWaiver(work_item_id,work_item_content_hash,reason,approved_by,created_at)`，且 work item 类型为 Bug、没有 open 产品/设计 request。work item/spec 内容改变即使批准/waiver 失效。否则返回 422 `planner_required`。
  - 冲突返回 `409` 和当前 dispatch 摘要；重复 idempotency key 返回原结果。
- `POST .../looper/dispatch/{dispatch_id}/stop|release`
  - 仅 dispatch owner。`stop` 把 claimed/running CAS 为 stop_requested；若 awaiting_human 已有服务端接受的 termination summary，则直接 CAS 为 confirmed_stopped，不等待不存在的进程再 stop_ack。`release` 仅接受 queued/confirmed_stopped，写原因后转为 released。没有 reassign API；confirmed_stopped 在 owner 显式 release 前仍占用唯一归属。
- `GET .../looper/dispatch/inbox?node_id=<stable-id>&cursor=...`
  - 给签名后的 Node 返回只属于该稳定 `node_id` 的 strict dispatch，数据库按 `node_id + active state` 建索引；不先扫描全部 work items，不依赖 legacy labels/assignees。
- `POST .../looper/dispatch/{dispatch_id}/claim|transition`
  - 必须携带 expected revision/state_version、claim idempotency key 或 execution attempt/fencing token，以及 Node request signature；revision/owner/node/binding/实时 membership 不匹配返回 409/403。transition 只接受状态表中的合法边。
- `POST .../looper/dispatch/{dispatch_id}/stop-ack`
  - 只接受当前 execution attempt/fencing token；即使 binding 为 revocation_pending 仍允许一次，body 必须包含 PID/process-group、退出时间、exit status 与签名 termination summary。成功后进入 confirmed_stopped；若 binding 正在 revocation_pending 则同时完成 revoke，否则 binding 保持 active。重复 ack 幂等，dispatch 仍等待 owner 显式 release。
- `POST .../looper/connections/` + `GET|DELETE .../looper/connections/{id}`
  - Plane 登录用户创建/轮询/取消自己的短时连接会话；返回可复制命令、到期时间、现有设备和逐项检查结果，不暴露其他成员的会话。
- `POST /api/looper/connections/exchange|link|complete`
  - CLI 用一次性 code 换取绑定上下文，提交 loopernet challenge + 本机 possession proof；验证成功即 self-activate。`complete` 只有在该 binding 已建立未过期的签名 Node session 后才成功，避免页面把“写了配置”误报为“inbox 已连通”。
- `POST .../looper/bindings/link` + `approve`
  - 仅保留旧 CLI/旧服务兼容；新版服务 link 会直接 active，approve 对 active binding 返回冲突。revoke/rotate 仍由 owner 控制。
- `POST .../looper/nodes/{binding_id}/sessions/open|renew|recover`
  - Node-signed；open 对 binding 做 session CAS，holding dispatch 存在时拒绝不同 instance_nonce。recover 只接受已绑定原 session 且与服务端 current attempt 一致的 journal 正向字段；renew 不改变 execution Authority。所有 response 含 lease/state version并写审计。
- `PATCH .../looper/nodes/{binding_id}`
  - 只有 binding owner 能切换自己的 `allow_offline_queue` 或请求 revoke；Admin 只能暂停项目集成。expected binding revision 防止并发覆盖。
- `POST .../looper/direct-implementation-waiver/`
  - 仅当前 owner，要求 Bug、reason、expected work-item content hash、无 open 产品/设计 request；返回可审计 waiver，供 worker dispatch gate 使用。
- `GET|PATCH .../looper/role-policy/`
  - GET 对项目成员可见；PATCH 仅 Project Admin，要求 expected revision + 变更原因。对在途任务的映射更新走单独显式动作，不能静默覆盖 snapshot。
- 不提供 Node 可任意指定 `event_type/actor/phase/answered` 的通用 events endpoint。`LooperCollaborationEvent` 只能由受控服务端动作产生，authority matrix 如下：dispatch transition 生成 phase/state event；Node-signed `role-requests` 只能创建 open request；Plane-authenticated answer 事务只能生成 answered；Project Admin role-policy 事务只能生成 superseded；Node-signed artifacts 只能生成 artifact-added。所有 actor、eligible member、phase、role revision 和 event type 均由服务端根据动作与 dispatch snapshot 派生。
- `POST .../looper/dispatch/{dispatch_id}/role-requests|artifacts`
  - Node-signed ingest；必须校验当前 session/attempt/fencing token、合法 lifecycle、dispatch role revision 和唯一 `source_event_key`。服务端对 dispatch row 加锁分配 `event_version`，数据库唯一约束为 `(dispatch_id,event_version)` 与 `(dispatch_id,source_event_key)`。
  - role request body 只接受 `source_event_key`、role 和精简 question summary；action comment/Page URL 由服务端创建并绑定当前 request，`eligible_member_id/policy_revision/status` 全由服务端填充。
  - artifact body 只接受 `source_event_key/source_kind/source_object_id/intent`。服务端解析 Plane Page/comment 或 SCM PR，验证项目/work item/repo 归属、redirect 与当前 revision，并自行派生 type/title/url/source_revision/content hash。Plane Page hash 取服务端 canonical stored content（UTF-8、LF、NFC）SHA-256；SCM PR 用 repo ID + PR number + head SHA 作为 revision并由 SCM API核验。
- `POST .../looper/role-requests/{role_request_id}/answer`
  - 使用 Plane 登录身份，不用 Node 签名；服务端强制 `request.user == eligible_member_id`、request 仍 open、policy revision 匹配。body 可以是一句快速决策或关联 Spec Page，不强制产品写完整 spec。
  - UI 使用普通 Plane comment composer，但提交时显式携带 `role_request_id`；服务端在同一事务创建正常可见 comment、绑定 `answer_comment_id`、追加 answered event 并更新 snapshot。普通自由评论或只含文本 marker 的评论不会被猜测成答案。
- 协作 event/comment/artifact 与 snapshot 若不能同一事务，必须先写 durable outbox；worker 按 outbox id 幂等投影，crash 后重放。snapshot 更新使用 expected snapshot version，乱序事件按 server event_version 重建，不能按客户端时间排序。
- `POST .../looper/technical-approvals/`
  - Plane-authenticated dispatch owner 提交 `artifact_id + expected_source_revision_id + expected_content_hash + expected_work_item_content_hash`；服务端重新读取源对象与 work item并重算当前 revision/hash，确认它是当前 work item 的 technical-spec artifact 后才创建 approval。Planner→Worker handoff 和 direct worker dispatch 再次读取并比较，旧 approval 不可复用。

第一版不提供 remote pause/retry/kill/takeover API。Plane 可以记录 `stop_requested` 并通知 owner，但这不是远程 kill；只有原 Node 回报目标 execution PID 已终止后，状态才能成为 `confirmed_stopped`。原 Node 丢失时保持等待，不允许 Admin 跳过确认。

## Looper 领取链路

### Planner

- 为 Plane provider 增加 strict dispatch inbox consumer；Planner 不再通过 legacy label discovery 找 V1 派发。
- 创建 loop 前验证 dispatch id/revision/state_version/attempt/fencing token/owner/node/mode；projection drift 仅上报告警，不改变 inbox Authority。
- 产品/设计/研发问题的 eligible actor 从 dispatch role snapshot 读取；decision request 写入 `role_policy_revision`，现有 `ProjectProductOwner/DesignOwner/Owner` 本地解析在严格模式下不得决定资格。

### Worker

- Worker 与 Planner 共用 strict dispatch inbox client，不复用只面向 GitHub routed 的 target/lease 语义。
- 与 Planner 相同，`ensureLoopForDiscoveredIssue` 前和新 run side-effect 前核对 dispatch revision；现有本地 ensure-loop 只负责本机幂等，不能替代跨写路径 Authority。
- 技术 Spec approval actor 使用 dispatch owner + content hash + dispatch revision；本地 `Owner.PlaneID` 冲突时 fail closed。

### Plane strict-dispatch 与现有 Network 的边界

- 明确选择独立的 Plane strict-dispatch claim gate，不扩展或复用当前 `network.mode=routed`：`internal/config/validate.go:483-505` 明确禁止 routed Planner 并强制 GitHub identity，修改这套语义会把本需求不必要地扩到既有 GitHub Network。
- Plane 项目是否启用 strict gate 由 Plane integration capability + Node 协议 capability 共同决定；启用后 Planner/Worker 只从 strict inbox 读取 `LooperDispatch`，不再使用 legacy trigger 作为候选集。
- 现有 GitHub `looper:target:*` / Coordinator / Lease 行为保持不变；Plane 人工派发不消费 GitHub Network Lease。
- strict work item 不写 generic trigger；服务端在开放派发前要求项目已完成升级清单，且所有可见 membership/binding heartbeat 报告最低协议 capability。缺 capability 时项目 read-only。一个项目可以同时存在“升级前创建的 legacy 任务”和“升级后创建的 strict dispatch”，但运行它们的 daemon 必须全部是新版，同一个 work item 永不在两种协议间转换。

## Plane Web

- 新目录：`apps/web/core/components/issues/looper-collaboration/`，包含 summary、timeline、role cards、dispatch modal、empty/error states。
- 在 `IssueMainContent` description 后、widgets/activity 前挂完整面板；在 sidebar 只挂 compact summary。
- 权限沿用 `IssueDetailRoot` 的 `allowPermissions([ADMIN, MEMBER], PROJECT, ...)`，但服务端返回的 `availableActions` 是最终准则。
- 使用 Plane 现有 UI primitives、tokens、toast 与 modal，不引入独立设计系统。
- 所有文案进入 i18n namespace；首发至少 `zh-CN` / `en`，不硬编码中文。

## 并发、失败与安全

- 同 work item 派发用 DB row lock；标签与 assignee 的更新、审计 comment 必须同事务提交或回滚。
- partial unique constraint 是最终并发兜底；row lock 不是唯一保障。
- 原生 work-item PATCH 造成投影 drift 时必须可观测，但不改变 strict inbox Authority；只有显式“恢复显示字段”能按当前 dispatch 重建投影，不允许反向用投影改写归属。
- target 列表结果只能作为展示缓存；POST 时必须重新验证 active Plane binding、project membership、允许角色和 loopernet presence/capability。
- dispatch POST 必须验证 actor 的 work-item edit permission、实时 project membership，且 `request.user == binding.member_id`；不能把 `ADMIN/MEMBER` 当充分条件。
- loopernet admin/service token 只在 Plane API 服务端；浏览器和 work item comment 不出现 token。
- Node heartbeat 不接受 owner Plane UUID 作为身份依据；Plane owner 来自 Plane 登录会话 + Node credential 双向证明后自激活的 binding，项目是否开放派发仍由 Admin 的集成开关控制。
- binding challenge、request nonce 与 fencing token 按本 Spec 的固定消息格式防重放；Node 私钥仅存本机，Plane 只存 public key，loopernet node token 不进入 Plane Page/comment/log。
- Admin suspend 只设置 `health=binding_suspended` 并继续占用归属，不得取消/release。owner 主动 revoke queued binding 时才允许事务化 `queued→released`；运行中 owner revoke 先进入 `stop_requested/revocation_pending`，阻止 Looper-managed post-run publish，但不能保证阻止黑盒 agent 的当前本地/直接远程动作。原 Node 通过受限 stop_ack、owner 再 release 前禁止新 dispatch；Node 丢失就等待恢复。
- role-policy revision 与每条 decision/approval 一同记录；旧 revision 的未解决回答不能被新 revision 误读。
- `(project_id, owner_member_id)` active binding 唯一；V1 拒绝第二个 active Node，避免未定义的多 Node 路由。
- Plane API/loopernet 超时分别设置短 timeout 与 circuit breaker；summary 可降级，dispatch 在不能确认 target 资格时 fail closed。
- PII：只显示项目成员已可见的 display name/avatar；active loop digest 不含内容。

## 可观测与告警

- Plane API 指标：dispatch 成功/冲突/权限拒绝/directory 超时/事务失败、targets 延迟、summary 降级率。
- loopernet 指标：active Node 数、身份 drift、过期心跳、binding challenge 成功/拒绝/重放。
- Looper 指标：strict inbox 拉取数/延迟、非目标 inbox 返回错误、claim conflict、旧 fencing token 拒绝、状态写回失败。
- 连续 Node directory 失败只告警，不改派；Plane durable mutation 部分成功必须视为 P0 并事务回滚。

## Rollout / migration / rollback

- 先部署 loopernet signed-challenge/trust-root，再部署 Plane binding/inbox；随后停止该项目全部旧 daemon、移除启动项、升级并签收进程清单。任一 Node 未报告最低 strict protocol capability 时 Plane 不开放派发按钮。
- 第一个 UI 版本先 `read_only=true` 对比现有 dashboard/Plane 状态；验证一周后开放 dispatch。
- 不迁移历史 work item；只有 durable dispatch 的任务进入新面板完整态，旧任务显示 legacy summary。
- 回滚先关闭新派发；在途 strict dispatch 仍由原已升级 Node 收敛或等待恢复，不能降级成 labels/assignees 任务，也不能恢复旧 daemon。
- 若 strict inbox 有问题，关闭新派发并保留 Authority；禁止管理员用现有 Plane labels 让旧 Node 接管。恢复服务或由原 owner确认停止后再 release。

## 实现切片

1. **S1 · Read-only 协作投影**：先实现 versioned collaboration event/role/artifact/snapshot 与只读面板；验证 AC-9～12、14～17。可由 agent 辅助编码，但不可自行合并或开启 feature flag。
2. **S2a · loopernet trust**：Ed25519 challenge、Plane trust-root、nonce/expiry/replay 测试；没有 Plane UI，独立安全验收。
3. **S2b · Plane Node binding**：Plane-first 一次性连接、owner Plane 身份、唯一 active Node、自动 daemon 重启与 signed-inbox 完成门禁、offline 开关、suspend/revoke/rotate；验证 AC-3/4/8/15。
4. **S2c · 项目角色策略**：role-policy revision/snapshot、本地 config drift 与 member-removal gate；验证 AC-2/11/15。
5. **S3 · Durable dispatch 状态机**：迁移、owner-only 权限、state CAS、attempt/lease/fencing、幂等、stop/release、投影告警与审计；验证 AC-1/5～8/13/15。
6. **S4 · Strict inbox 与恢复**：按 node_id 索引的 Planner/Worker claim、Planner→Worker handoff、项目版本门禁、崩溃/离线/Plane outage reconcile；验证 AC-5～8/13/14。
7. **S5 · 风险态 UI、埋点和 E2E**：完成设计矩阵、响应式、无障碍与 analytics；验证 AC-10～18。

## 测试用例

- **T1（AC-1/2）自动化**：Guest/Member/Admin/owner/角色决策人矩阵；只有 `request.user == active binding owner` 且有编辑权时能 self-dispatch，Admin 不能代派。
- **T2（AC-3/4/15）自动化**：本人唯一 target、第二 active Node 被 unique constraint 拒绝、无 Node、drift、removed member、过期心跳。
- **T2b（AC-3/6/15）安全集成**：link request 缺任一 envelope、challenge/proof digest或 public-key hash 不一致、恶意 Node 自报另一 member UUID、重放 challenge/nonce、篡改 body/path、过期 timestamp、错误 audience/trust-root/key revision、旧 key/new key、binding revoked/member removed；均不得 link/claim/transition，完整跨语言 golden vector 才成功。
- **T2c（AC-2/11/13）角色集成**：两个 Node 本地配置互相冲突，断言都只接受 dispatch role snapshot；项目策略更新不静默影响在途任务；显式刷新角色 supersede 未解决请求；member removed 设置 `health=role_drift`；未映射成员/Admin 的普通评论或 answer API 均不能满足请求。并行 request、重复/乱序 event、comment/outbox 中断恢复后 snapshot 可重建且不倒退。
- **T3（AC-5）组件 + API**：确认文案完整包含 work item/mode/owner/node/offline；绕过 UI 请求 worker 时验证 approved spec content hash / direct Bug waiver，内容漂移、非 owner、非 Bug或 open role request 均返回 planner_required。
- **T3b（AC-5/11/17）集成**：Node 伪造 artifact type/hash、引用同域其他 project Page/无关 PR、同 URL Page 原地修改、redirect 到非 allowlist origin、复用旧 approval 均被拒；owner approval 后 Planner→Worker handoff 原子完成并生成新 attempt/token，全程没有 legacy worker trigger。
- **T4（AC-6/7）集成**：两个 owner 并发 self-dispatch + 重复 idempotency key，断言唯一活动 dispatch、单调 state_version、唯一 owner 与单条审计记录。
- **T4b（AC-6/7/13）集成**：strict epoch 后创建 work item 时已原子分类 strict_v1，任何生产路径都不能抢注 legacy；分别经单条 PATCH、bulk、import、automation 与内部 ORM 修改 labels/assignees，断言不能改写 Authority；新版 Node 即使 test-only fixture 让 legacy trigger 命中也先读 protocol 并跳过 strict item。普通投影修改只设置 projection_drift，strict inbox 仍只返回原 Node，显式 restore 后恢复。
- **T4c（AC-6/7/13/14）故障注入**：对 AttemptJournal 的 claim_pending、claimed、worktree_ready、wrapper pidfile、process_started、termination_summary 每个 fsync 边界逐点 kill；再覆盖 claim 响应丢失、同 credential 双 daemon、lease expiry、乱序/replayed transition、Plane outage、comment/outbox/projection 写失败。断言 token 可取回、旧 token fenced、无法证明时等待、无双跑且 snapshot 可重建。
- **T5（AC-8）隔离 E2E**：offline queue 关闭时 owner self-dispatch 被拒；owner 开启后可给自己排队，其他 owner 不能投递；目标恢复后自动领取且只有一个 loop。
- **T6（AC-9～12）组件/截图回归**：所有 lifecycle/角色/skip/block 状态和 owner 来源行。
- **T7（AC-13）集成**：非 owner stop/release 返回 403；Admin pause 只设置 binding_suspended 并保持 holding；owner revoke queued 原子 released；running stop_requested 期间无新 dispatch；revocation_pending 仅允许原 attempt 的 stop_ack；awaiting_human 复用既有 termination summary 直接 confirmed_stopped；owner release 后新 owner 才可 self-dispatch。lost Node 保持等待。
- **T8（AC-14）故障注入**：loopernet 超时/500/陈旧心跳，summary 降级且 Plane Authority 不变。
- **T9（AC-15）集成**：archived/done/Node removed/owner removed 状态 fail closed。
- **T10（AC-16/17）人工 + Playwright**：桌面/平板/手机、键盘路径、产物链接可达。
- **T11（AC-18）自动化**：事件触发与属性白名单，不包含正文/日志。
- **T12（AC-5～8/14）真实隔离 E2E**：在隔离 Plane project + 两个独立 HOME + loopernet 中完整验证双 owner 竞争、错 Node strict inbox 为空、未升级 Node 阻止启用、Planner→审批→Worker→PR、离线/崩溃恢复、角色事件、状态写回与无残留进程。

## Execution mode 自评

- **Proposed**：`HITL-needs-engineer`。
- **依据**：跨 Plane/Looper/loopernet，涉及 auth/crypto、迁移、并发、协议兼容与 UI。S1 只读 scaffold 可由 agent 辅助，但不可自行合并/启用；S2～S4 必须 engineer-led review，迁移、密钥、Authority/并发变更不得无人值守合并；S5 可由 agent 实现，设计截图、权限矩阵和真实双 Node E2E 必须人工签收；feature flag、生产迁移、密钥配置、灰度和回滚仅由人执行。

## 实现前 ADR（非产品未决项）

- S2 开始前由 Plane maintainer 记录承载模块与部署图 ADR：Plane→loopernet 网络路径、trust-root/service credential 注入、nonce store、迁移顺序和多环境密钥轮换。外部行为与安全协议不得由实现者自由改变。
- 即时 remote revoke/takeover、多 Node 与跨 owner allowlist 明确超出 V1；若未来建设，必须另写安全 Spec，不能用 prompt 约定冒充安全边界。
