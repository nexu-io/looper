# OPEND-1645 · Plane × Looper 协作入口 · 验证计划

## 目标

证明按钮权限、唯一执行归属、离线恢复、多角色展示和跨服务降级都符合产品 AC，而不仅是“接口返回 200”。

## Sources

- 产品 AC 真源：`product.md` AC-1～AC-18。
- 技术测试映射：`technical.md` T1～T12。
- Looper live sandbox 术语与隔离要求：`CONTEXT.md` 的 `Live sandbox`。
- 现有 Plane live E2E：`internal/forge/plane_live_e2e_test.go:24-41`。

## 测试层级

1. **Plane API 单元/集成**：权限、事务、幂等、Node directory 降级。
2. **Looper Go 测试**：协议兼容、Plane exact target、非目标 fail closed、状态摘要。
3. **Plane Web 组件/Playwright**：视觉状态、响应式、键盘和埋点。
4. **隔离 E2E**：真实自建 Plane sandbox project + loopernet + 两个独立 `HOME`，复用 `internal/e2e/harness.TempHome`，使用专用测试 work item/repo，不碰生产任务。

## 隔离 E2E 场景

### 环境

- Plane：专用测试 project，不使用 OPEND 生产 work item 列表。
- Node A：owner A、node `amber`、`HOME=<tmp>/amber-home`。
- Node B：owner B、node `cyan`、`HOME=<tmp>/cyan-home`。
- 两个进程分别拥有独立 `.looper` config/SQLite、network credential、端口、worktree root 和日志目录；测试禁止继承真实用户的 `~/.looper`。
- Upgrade-gate Node C：先固定到 strict 功能引入前的真实 Looper commit，证明未升级 daemon 会阻止项目开启 strict；随后停止旧进程/启动项并升级同一 Node。fixture 只可用于组件测试，不能替代准入 E2E。
- loopernet：临时 DB 与短 heartbeat interval。
- Code repo：Looper live sandbox 或等价专用 repo；禁止写 Open Design 正式仓。

### 场景 E1 · 精确派发

1. owner A/B 分别用自己的 Plane 身份与 Node credential 完成绑定，Project Admin 启用 Planner；两个 Node 在线。
2. owner B 登录 Plane，点击“交给我的 Looper”；普通 Member/Admin 尝试派给 B 均没有该能力。
3. 断言 Plane durable state 只有一个 strict dispatch，owner/node 分别是 B/`cyan`，且 work item 没有 legacy planner trigger。
4. 运行两个 scheduler tick。
5. 断言 B 创建唯一 loop；A 为 skipped，工作目录无新 worktree。

### 场景 E2 · 并发与重试

1. owner A/B 两个浏览器身份同时对同一 work item 发起 self-dispatch。
2. 断言只有一方成功；另一方收到 409 和已生效 owner。
3. 对成功请求重放同一 idempotency key，断言无第二条审计/loop。
4. 通过 Plane 原生属性 API 改普通 assignee/非 trigger label，断言 Authority 不变、health 进入 projection_drift；A/B strict inbox 仍只有获胜 Node 可见，显式 restore 后投影恢复。
5. 分别经单条 PATCH、bulk、import、automation 和内部 ORM 尝试添加 protocol policy 中的真实 legacy trigger，断言 domain/DB guard 拒绝；运行已升级 Node C 后仍无额外 loop/worktree/comment。
6. strict epoch 后新建 work item，断言创建事务已写 strict_v1；在派发前抢先添加 legacy trigger 仍被拒绝，不存在生产 unclassified→legacy 路径。

### 场景 E3 · 离线恢复

1. 关闭 Node B，等待 UI 显示离线。
2. 保持 B 的 offline queue 默认关闭，owner B self-dispatch 被拒绝；owner A/普通 Member/Admin 更不能派给 B。
3. owner B 开启 offline queue 后由 B 自己派发，确认出现“离线排队”。
4. Node A 运行多个 tick，断言不领取。
5. 启动 B，断言自动领取且无需再次点击。

### 场景 E4 · 多角色协作

1. 在 Plane 配置并冻结 product/design/QA role-policy revision；让两个 Node 的本地角色配置故意互相冲突。
2. Planner 产出一个产品问题和一个设计问题。
3. 面板显示快照中的对应负责人、数量与 Plane action URL，不采用任一 Node 的冲突本地值。
4. 被映射负责人从角色卡进入普通 Plane comment composer，用显式 `role_request_id` 分别回答；飞书只验证收到通知，不在飞书回复。未映射成员/Admin 的自由评论与伪造 marker 都不满足请求。
5. 断言 Planner 感知回答并继续，技术 Spec 审批只面向 dispatch owner。
6. 更新项目 role policy，断言在途请求不会静默换人；显式刷新后未解决请求才重新路由并保留审计。
7. Planner 写 technical-spec 对象引用并以 termination summary 进入 awaiting approval；owner 通过 approval API 批准服务端当前 revision/hash。
8. 断言同一 dispatch 原子切换 `active_role=worker,state=queued`，state/role revision 递增且不出现 legacy worker trigger；Worker 以新 attempt/fencing token 领取、实现并创建经服务端核验的 PR artifact。

### 场景 E5 · 降级

1. 执行中停止 loopernet。
2. 断言 Plane 面板保留 owner/dispatch/Spec/PR durable 状态，在线信息标不可用。
3. 断言系统没有释放归属、创建第二个 dispatch 或第二个 loop。

### 场景 E6 · 停止、释放与新 owner 接手

1. 让 B 处于 running，Admin/A 尝试停止或接管，断言 403/409 且 B 仍是唯一 Authority。
2. owner B 在本地停止 execution，Node 回报 PID 已终止；检查旧 worktree 并记录处理结果。
3. owner B 填写原因 release；随后 owner A 自己点击派发，断言旧 dispatch terminal、新 dispatch 唯一、审计历史完整。
4. 断言 B 后续 tick 不创建新 run，A 才可领取。
5. 另让 B 处于 awaiting_human，owner stop 直接复用进入等待态时已验的 termination summary 转为 confirmed_stopped，不产生新的 stop_ack 等待；release 后 A 才可 self-dispatch。

### 场景 E7 · 身份冒充与吊销

1. Node A 在 heartbeat/capabilities 自报 owner B UUID，断言 targets 仍按 Plane binding 显示 owner A。
2. Node A 使用 owner B 的 Plane work item/dispatch 信息尝试 claim，断言 403。
3. link request 缺 challenge envelope、challenge/proof digest 不匹配、错误 trust-key revision、过期或已消费 challenge，均不得创建 binding；完整双 envelope golden vector 才成功。
4. 同 owner 的第二台 Node 尝试注册，断言 active unique constraint 拒绝；使用另一私钥 claim Node A 的任务同样 403。
5. Admin pause 只设置 binding_suspended 并保留 holding dispatch；owner revoke queued 才原子 released。running 时 owner revoke Node A binding，断言进入 revocation_pending、不可新 claim/普通 transition；当前 attempt/fencing token 仅可提交一次 stop_ack。Node 丢失时状态保持等待，不创建新 dispatch。

### 场景 E8 · owner-only 权限

1. Project Member、产品/设计/QA 负责人和 Project Admin 均尝试派给 owner B，断言服务端拒绝且 UI 只读。
2. owner B 有 work-item edit permission 时可以 self-dispatch；移除该权限后即使 binding active 也被拒绝。
3. 删除 owner B 的 project membership，在 claim、新 run 和 publish 三个边界均断言 fail closed；删除 hook 是否及时不影响结果。
4. 断言不存在 allowlist、管理员代派或跨 owner target API 字段。

### 场景 E9 · claim、崩溃与重放恢复

1. 在 claim_pending fsync 后丢弃首次 claim HTTP 响应；重启后从 journal 取同一 idempotency key 重试，断言返回同一个 attempt/fencing token。
2. 分别在 claimed、worktree_ready、wrapper pidfile、process_started、termination_summary 每个 fsync 边界 kill daemon；恢复时结合 journal/pidfile/OS process start-time 判断，无法证明则等待，任何点都不产生第二个 agent。
3. running 时杀掉 daemon但保留/移除 agent PID 两种情况：恢复后分别续接或上报退出；证明不了时进入 node_unreachable 并等待，不重跑。
4. 使用旧 state_version、旧 fencing token、重复 nonce 和乱序 transition，全部返回 409/拒绝且 snapshot 不回退。
5. 同一 HOME 启动两个 daemon，第二个拿不到 OS lock；复制 credential 到另一 HOME 再启动，服务端 NodeSession CAS 拒绝。holding attempt 的 session lease 过期后，空 journal/new session 仍不能接管；只有原持久化 session/claim key/attempt/token 可恢复。

### 场景 E10 · 版本启用门禁与 Plane 故障

1. 启动真实旧版 Node C并让它 heartbeat，断言项目 strict activation/dispatch 保持 read-only；记录进程清单后停止旧进程、移除启动项、升级 C，使全部 Node 报告 `strict_dispatch_v1`，此后 activation 才成功。
2. 同项目保留一个 legacy control work item，由**已升级的** Node C 兼容路径成功领取。生产 API 添加 legacy trigger 到 strict item 必须被 guard 拒绝；另在隔离 DB fixture 中绕过生产 guard 注入同一关联，仅用于证明新版 C 仍先读 protocol并跳过 strict item，而目标 Node 从 inbox 领取。版本回退、capability revision 缺失或 trigger 配置与 policy 冲突时立即阻止新派发。
3. running 中断开 Plane：禁止新 run和受控 publish；恢复后当前 Node 用 fencing token reconcile，旧 token 不能覆盖新状态。
4. 注入 comment/投影写失败，断言 dispatch transaction 按定义回滚或 projection_drift 可恢复，不产生第二执行。
5. 注入角色 answer comment 已创建但 snapshot worker 崩溃，断言 durable outbox 重放后只产生一个 answered event；并行多个 request、重复/乱序 event 均按 server event_version 重建。

### 场景 E11 · 直接实现服务端门禁

1. 绕过 UI 直接 POST `requested_mode=worker`，没有批准 spec/waiver 时断言 422 planner_required。
2. owner 批准 technical-spec revision/content hash + 当前 work-item content hash 后可 worker dispatch；修改 spec，或修改 work item 标题/正文/类型/优先级/spec links 后旧批准立即失效。
3. 对 Bug 创建带原因的 direct waiver 且无 open 产品/设计 request 时可 dispatch；普通需求、非 owner、存在 open request 或 work item 内容改变时均拒绝。
4. 旧 owner 已对相同 source revision/content hash 和 work-item hash 做出的批准，在其正常 release、新 owner self-dispatch 后按既定规则仍有效；审计明确显示原批准人，内容任一变化则要求新 owner 重审。

## 视觉验收

- 截图基线：未派发、本人无 Node/无权限、在线运行、离线排队、状态过期、projection/role drift、产品阻塞、设计方案、Spec 待审批、stop requested、revocation pending、Node 丢失等待、409 冲突、PR、待 QA、失败、完成、集成未配置。
- 三个 viewport：1440×900、834×1194、390×844。
- 人工责任人：设计负责人确认信息层级与视觉；QA 确认角色动作、链接与异常文案；Looper owner 确认身份和技术状态准确。

## 退出条件

- T1～T12 全部通过。
- 隔离 E1～E11 无残留 work item labels、binding、临时 HOME、credential、端口、worktree、daemon PID 或后台 agent；显式扫描真实 `~/.looper` 未被测试修改。
- Plane/Looper/loopernet CI 全绿，且设计关键截图人工通过。

## Execution mode

`HITL-needs-engineer`：自动化覆盖主要行为，但真实双 owner/双 Node、升级门禁、Planner→Worker handoff、安全故障注入、视觉与角色权限必须人工见证。

## 2026-07-21 实现验证记录

已自动验证：

- Looper `go test ./...`（包含现有隔离 E2E harness）、`go vet ./...`、`go build ./...` 全部通过。
- Plane Looper contract/unit suite 通过；本轮连接/协作相关 17 条 contract 覆盖自助会话、10 分钟过期/取消、唯一设备、无需 Admin 审批、signed inbox 完成门禁，并继续覆盖双 owner/双 Node inbox 隔离、wrong-node claim、签名/replay/fencing、角色路由、owner-only 回答、实时目录降级、Planner→Worker 原子 handoff、旧 attempt 失效、termination summary 门禁和 awaiting-human stop。
- CLI 在独立临时 HOME + fake Plane + fake loopernet 中通过 `Plane one-time code → identity exchange → signed link → daemon restart → signed inbox ready → complete`；无需 Admin 设备审批，私钥权限为 `0600`，并覆盖 inbox 尚未 ready 时的重试。
- Plane Web TypeScript、Looper 面板 oxlint、19 个 locale key sync 通过；连接向导由 Claude Code 产出。因验收机磁盘已触发 97% hard-stop，本轮未重复生成大型 production build 缓存，最终浏览器视觉验收仍保留。
- 在全新 PostgreSQL 临时数据库从零迁移至 `db.0126` 成功，新增 connection session schema 与旧 pending binding 自激活迁移均可应用；临时数据库在测试退出时删除。

最终人工验收仍保留：真实 Plane/loopernet 上两个真实 owner 的浏览器点击见证、三个 viewport 的实际页面截图/键盘检查、真实 PR 产物链，以及 production feature flag/密钥/回滚演练。自动化不会替代这些上线 gate。
