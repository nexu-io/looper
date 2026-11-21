# OPEND-1645 · Plane 内派活与 Looper 多角色协作面板 · 产品 Spec

## 标题

让研发成员在 Plane work item 内把需求交给自己绑定的 Looper，并让团队直观看到多角色决策、规划、实现与验收进度。

## Why · 为什么要做

- **用例**：团队正在把 Looper 部署到各研发同学本地，并以自建 Plane 作为需求事实源。当前派活主要依赖手工改 assignee、标签和飞书通知，用户无法在一个地方确认“是否已派发、由谁的 Looper 执行、卡在哪个角色、产物在哪里”。
- **已观察到的痛点**：近期真实协作中出现过重复通知、产品问题错 @ 研发、技术 Spec 审批错 @ 产品、任务已开 PR 但 Plane 状态未同步，以及看见“修复中”却无法判断是哪台 Looper 在运行等问题。
- **为什么现在做**：单人部署时这些问题还能靠口头补救；开始向同事分发后，同一个 Plane 项目会连接多个本地 Looper，身份不清和重复领取会直接造成并行返工或错误决策。
- **不做的机会成本**：每新增一名 Looper 使用者，派单、排障和验收成本都会线性增加；团队仍需在 Plane、飞书、本地 dashboard 和 PR 之间人工拼接状态。

## Sources · 事实源

- **Plane repo**：`/Users/elian/Documents/nexu/plane`，branch `preview`，base `90ae8457d07c9b5e5e0514735170f55304506cc9`。
  - `apps/web/core/components/issues/issue-detail/root.tsx:61-269`：work item 详情页根布局；`ADMIN` / `MEMBER` 已被视为可编辑角色。
  - `apps/web/core/components/issues/issue-detail/main-content.tsx:95-192`：标题、描述、widgets 与 activity 的主内容层级，是协作面板的主展示位置。
  - `apps/web/core/components/issues/issue-detail/sidebar.tsx:59-220`：右侧属性栏，可放折叠后的 Looper 摘要，但不承载完整协作流程。
- **Looper repo**：`/Users/elian/Documents/looper`，branch `feat/looper-auto-flowchart-runtime`，base `5c26cd1d0fe5f7839868024a7a833b5083eb8680`。
  - `internal/forge/plane.go:181-217`：Plane work item 已支持按 label 与 assignee UUID 发现任务。
  - `internal/forge/plane.go:233-328`：Looper 已能幂等合并 Plane labels 与 assignees。
  - `internal/planner/runner.go:690-752`：Planner 按当前 Plane 身份与触发标签认领需求。
  - `internal/worker/runner.go:994-1083`：Worker 可按 `PlaneAssigneeID` 与触发标签筛选 Plane work item。
  - `internal/network/protocol/protocol.go:36-46,76-98,137-155`：loopernet 已有节点成员、能力与心跳模型，可扩展 Plane 身份和项目能力，但它不是工作意图 Authority。
  - `internal/api/handler.go:1963-1997,6178-6219`：本地 Looper API 已输出 loop 状态、Plane/PR/action 链接和 phase，可作为实时状态字段的语义来源。
- **Plane work item**：`OPEND-1645`，UUID `a6eb4e1e-67d7-43f5-8f83-c023faa446cd`。
- **相关讨论**：本轮用户要求重点拍死“谁可以点击、点击后谁的 Looper 运行”，并要求界面围绕 Looper + 多角色协作，尽量直观美观。
- **交互原型**：同目录 `prototype.html`，可直接用浏览器打开；桌面截图 `prototype-desktop.png`；完整视觉说明见 `[spec:design]`。
- **访问前提**：本机可读两个 repo；Plane Pages 与 work item 发布使用本机 `plane` CLI 的既有认证配置。

## 目标用户与成功标准

### 目标用户

1. 在 Plane 创建、编辑、推进需求的产品、设计、研发、QA 成员。
2. 在自己电脑运行 Looper 的研发同学（Looper owner）。
3. 负责项目配置和误操作处理的 Plane Project Admin。

### 成功标准

- 派发者不用理解标签协议，也能在一次确认内把需求派给明确的 Looper owner。
- 任意成员打开 work item，5 秒内能回答：谁的 Looper 在处理、处于什么阶段、在等谁、最近发生了什么、下一步是什么。
- 同一个 work item 不因双击、多人同时点击或轮询延迟产生两个活跃执行归属。
- 产品、设计、研发决策分别展示正确负责人，不再把“产品决策”和“技术方案审批”混为一人。

## Goals / Non-goals

### Goals

- 在 work item 主内容区提供统一的 `Looper 协作` 面板和上下文主按钮。
- 明确定义 self-dispatch、停止/释放、查看、审批等动作的角色权限。
- 明确定义 Plane 用户、Looper owner 和本地 Node 的身份映射与默认选择规则。
- 把需求澄清、多角色决策、技术方案、实现、PR、QA 验收串成一条可视化进度。
- Plane 服务端的 Looper 派发记录保持执行归属 Authority；work item 属性是可见投影，loopernet/心跳只提供“谁在线、支持什么”的可观测信息。
- 飞书继续只做通知和跳转，不成为控制或回答入口。

### Non-goals

- 第一版不在 Plane 内嵌完整终端日志或本地 dashboard。
- 第一版不做自动负载均衡、随机派发或“系统替人决定派给谁”。
- 第一版不允许 Plane 直接访问同事电脑的 `localhost`。
- 第一版不提供跨机器 `pause` / `kill` / `takeover`；这些动作仍在 Looper owner 本地执行，后续再经安全设计开放。
- 不重写现有 Planner 的多角色决策流程，也不让飞书回复成为事实源。
- 不把在线心跳当作执行 Authority；loopernet 暂时不可用时，已写入 Plane 的派发仍然有效。

## 核心概念与权限

### 三种身份

- **Plane actor**：点击按钮的人；V1 中必须与 Looper owner 是同一 Plane 用户。
- **Looper owner**：被选择的研发同学；以 Plane member UUID 标识，对应其本地 Looper 配置。
- **Looper Node**：owner 运行的具体 `looperd` 实例；以稳定 `node_id` 和可读 `node_name` 标识。界面显示“来自 杨瑾龙的 Looper · MacBook-Pro”，但业务归属以 owner 为主。

### 谁可以做什么

| 动作 | Guest | Project Member | Project Admin | 当前 Looper owner |
|---|---:|---:|---:|---:|
| 查看协作状态与产物 | ✅ | ✅ | ✅ | ✅ |
| 首次派发 | ❌ | 仅能派给自己的 Looper | 仅能派给自己的 Looper | ✅（派给自己） |
| 请求停止 / 释放归属 | ❌ | 仅自己的 dispatch | 不能替其他 owner 操作 | ✅ |
| 审批技术 Spec | ❌ | ❌（除非本人是当前 dispatch owner） | ❌（除非同左） | ✅ |
| 回答产品 / 设计决策 | 仅配置的对应角色负责人 | 同左 | 同左 | 仅当同时拥有该角色 |
| 查看本地实时详情 | ❌ | 仅自己的 Node | 仅自己的 Node | ✅ |

权限原则：Plane 现有项目角色决定“能否改 work item”；Plane 端 Looper project role policy 决定“能否代表产品、设计、研发作决定”。管理员不能因为是管理员就替专业角色审批内容。

角色映射在 Plane 项目集成设置中只有一份：产品、设计、QA 显式指定负责人；研发负责人固定为当前 dispatch 的 Looper owner。Project Admin 可以配置/更换映射，但不因拥有配置权自动获得回答或审批权。派发时冻结角色策略版本；旧 dispatch 释放、新 owner self-dispatch 后，产品/设计/QA 不变，研发负责人变为新 Looper owner。

### V1 派发信任模型（已由 maintainer 确认）

V1 采用 **owner-only self-dispatch**：当前 Plane 登录用户只能启动绑定在自己名下的 Looper，不能向同事电脑派活；Project Admin 也没有代派权限。这样不需要 actor allowlist、授权管理或“管理员是否天然可信”的第二套策略。

- 点击者必须是 Node binding 的 owner，同时具备当前 work item 编辑权。
- 一个 Plane owner 在同一项目最多只有一个 active Node；V1 不提供 Node 选择器。
- 产品、设计、QA 可以回答各自问题和查看协作状态，但不能启动研发 owner 的本地 agent。
- owner 可选择是否允许自己的离线 Node 排队，默认关闭；关闭时按钮不可派发并解释“Looper 离线”。
- V1 不提供管理员强制转派、远程强杀或其他 Node 接管。原 Node 丢失时任务保持“等待 Looper 恢复”，不释放执行归属。
- 若确需更换研发 owner，原 owner 必须恢复 Node、停止本地 execution 并释放 dispatch；随后新 owner 自己点击“交给我的 Looper”。

## 派发规则

### 主按钮

- 未派发且本人有可用 Node：`交给我的 Looper`
- 未派发且本人无 Node：只读提示 `尚未绑定你的 Looper`
- 已排队：`已派给 <owner> · 等待领取`
- 执行中：`<owner> 的 Looper 处理中`
- 等人决策：`等待 <产品/设计/研发负责人>`
- 已出 PR：`查看 PR`
- 待验收：`开始验收`
- 完成：`已完成`

### 本人 Looper 资格

V1 不展示跨 owner 的目标下拉框。当前登录用户自己的 Looper 只有同时满足以下条件才可派发：

1. owner 是当前 Plane project 的有效成员；
2. owner 已用自己的 Plane 身份把 Node 绑定到本人，Node 也已证明自己持有注册 credential；不能只靠 Node 自报 owner UUID；
3. Project Admin 已在 Plane binding 中启用该 Node 在本项目可执行的角色；
4. Node 的通用能力满足本次执行角色（Planner 或 Worker）；
5. binding 没有 revoke/drift/冲突；离线但仍在保留期内的 Node 可选，但必须显示“离线，联网后领取”。
6. 当前点击者就是 binding owner，且拥有 work-item 编辑权；
7. 同一项目中该 owner 没有第二个 active Node binding。

活跃派发存在时，任何人都不能创建第二份派发。其他研发如果想接手，只能等待原 owner 释放后，再由新 owner 自己点击派发。

### 执行方式

- `自动判断（推荐）`：进入 Planner；Planner 先调研并把问题按产品、设计、研发分流，再产出技术 Spec。
- `直接实现`：只在 work item 已有人工批准的技术 Spec，或被明确标记为无需产品/设计决策的小型 Bug 时开放；否则置灰并解释原因。

点击确认后，弹窗必须用一句完整的人话复述结果，例如：

> 将 OPEND-1645 以“自动判断”交给你自己的 Looper（MacBook-Pro）。它当前离线；你已开启离线排队，联网后会自动领取。

## 多角色协作流程

```text
需求已创建
  → Looper 调研
  → 需要谁决策？
       ├─ 产品：范围、规则、优先级
       ├─ 设计：界面、交互、视觉方案（优先给 HTML 示意 + 截图选项）
       └─ 研发：架构、风险、实现取舍
  → 信息收齐
  → 技术方案 + GRILL + REVIEW
  → Looper owner 审批技术方案
  → 实现 → PR → Review/Fix
  → QA 验收 → Done
```

面板不要求每个需求都经过所有节点；不适用的阶段显示“已跳过（原因）”，不能直接消失，以免用户误以为漏做。

## 交互界面改动

- 完整面板放在 work item 描述下方、activity 上方，宽度足以表达流程；sidebar 仅显示折叠摘要。
- 顶部是 Looper 品牌区：机器人图标、`Looper 协作`、owner/Node 小字、在线状态和上下文主按钮。
- 中间是横向阶段轨迹，在窄屏转为纵向；当前阶段使用品牌青绿色，阻塞使用琥珀色，失败使用红色，完成使用绿色。
- 当前阶段下方只突出“现在需要做什么”；历史事件折叠，避免把日志墙搬进 Plane。
- 产品、设计、研发、QA 各用独立角色卡；卡片显示负责人、问题数、状态和唯一行动入口。
- 设计决策若有多个选项，优先显示 HTML 原型截图缩略图而不是纯文本 A/B/C。
- 原型与视觉细节见同 bundle 的 `[spec:design]`。

## 空态、异常与边界

- 未配置 Looper 集成：面板显示配置说明；普通成员无按钮，Admin 可进入配置。
- 没有可用 Node：显示“暂无已注册 Looper”，不展示空下拉框。
- Node 离线：仅当 owner 已开启自己的离线队列时允许排队，并明确说明；默认禁止派发。
- loopernet 不可达：仍展示 Plane 中的 durable 派发状态，实时在线信息标“暂不可用”；禁止因此自动转派。
- 多人并发点击：只允许一个成功；后来者看到“任务已由 X 派给 Y”。
- 双击/网络重试：使用同一个 idempotency key，不产生重复派发或重复评论。
- owner 从项目移除或 Node 身份 drift：停止新领取，面板进入“等待原 Looper 恢复/联系 owner”；已经执行的本地进程不被远程强杀，也不允许新 Node 接管。
- owner 可撤销自己的 Node；Project Admin 只能暂停项目集成。若已有运行中 agent，binding 进入 `revocation_pending`，只允许原 execution 提交一次受限 `stop_ack`；在进程终止并确认前 binding 不完成撤销，也不得创建新 dispatch。Node 永久丢失时 V1 保持等待，不提供 break-glass。
- 项目角色映射更新默认只影响新任务；修改在途任务必须显式说明原因。未解决问题重新路由，已完成决定保留原负责人和策略版本。
- 更换研发 owner：V1 没有直接“转派”。原 owner 先在本地停止并释放；Plane 确认旧 execution terminal 后，新 owner才可以自己的身份重新派发。运行中、停止待确认或 Node 丢失时都不能抢占。
- archived/done work item：只读，不允许派发；重新打开后才可重新派发。

## 数据埋点

| 事件名 | 触发时机 | 关键属性 |
|---|---|---|
| `looper_panel_viewed` | 面板进入可视区域 | workspace_id, project_id, work_item_id, phase, has_dispatch, viewer_role |
| `looper_dispatch_opened` | 打开派发弹窗 | eligible_owner_count, default_reason, requested_mode |
| `looper_dispatch_submitted` | owner 确认派发 | actor_pseudonym, node_pseudonym, mode, node_online |
| `looper_dispatch_result` | 派发返回 | success, error_code, latency_ms |
| `looper_dispatch_released` | 原 owner 停止并释放 | phase, stop_reason |
| `looper_role_action_clicked` | 点击产品/设计/研发/QA 卡片 | role, question_count, action_type |
| `looper_artifact_opened` | 打开 Spec/PR/验收/日志 | artifact_type, phase, owner_id |

敏感内容、问题正文、日志正文不进入埋点。

## Alternatives considered

1. **Plane 按钮直接请求 `localhost:7788`**：拒绝。共享 Plane 不知道哪个 localhost 属于谁，且有 CORS、认证、离线和越权风险。
2. **浏览器插件 / userscript 注入按钮**：拒绝作为正式方案。升级脆弱、权限难审计，也无法统一多人状态。
3. **只靠手工 label + assignee**：拒绝作为 Authority。Plane 原生 PATCH 可被普通 Member 调用，无法覆盖绕过按钮的并发写；它们只作为可见投影和 discovery hint。
4. **loopernet 直接成为任务队列 Authority**：拒绝。Plane 才是团队可见的事实源；注册目录和心跳只用于资格校验、唤醒与观测。
5. **自动按负载派给最闲 Node**：第一版不做。归属涉及代码权限与责任人，必须让人明确确认。

## Risks & mitigations

- **误派 / 越权**：服务端重复校验 Plane role、work item 状态和 eligible Node，不信任前端禁用态。
- **编辑权升级为远程执行权**：服务端强制 `request.user == binding.member_id == dispatch.owner_member_id`；普通编辑权和 Admin 身份都不能替别人启动 Looper，离线自动领取默认关闭。
- **运行中无法瞬时撤权**：第一版不允许运行中直接转派；旧进程未确认终止前不创建新 dispatch。界面明确提示本地副作用可能已发生，需 owner 检查 worktree。
- **身份映射漂移**：注册目录同时保存 Plane member UUID、Node ID、项目能力与最后心跳；drift 时禁止新派发。
- **Node 冒充 owner**：owner 必须用 Plane 登录身份发起绑定，Node 必须证明 credential，Project Admin 再启用角色；targets/claim 不接受 heartbeat 自报 owner。
- **本地角色配置分裂**：Plane project role policy 是唯一 Authority 并随 dispatch 快照；本地配置冲突时 fail closed，不能因更换研发 owner 暗中换产品/设计/QA。
- **重复领取**：Plane durable dispatch record 是 Authority；服务端事务 + partial unique constraint + idempotency key；labels/owner/target 漂移时 fail closed，Looper 本地仍保留 ensure-loop 幂等。
- **实时状态与事实源不一致**：UI 明确区分“Plane 派发状态（事实）”和“Node 最近心跳（观测）”；观测失联不改归属。
- **上游 Plane 升级冲突**：新 UI 封装在独立 `looper-collaboration` feature 目录，通过少量稳定插槽接入详情页。
- **信息太多**：默认只展开当前阶段与下一步，历史折叠；sidebar 只放摘要。

## Rollout / rollback

1. 以 workspace/project feature flag 灰度，仅在 `open-design` workspace 的试点项目开启。
2. 第一阶段只读展示当前 Plane/Looper 状态；确认映射正确后开放派发。
3. 再开放 owner 自己的离线排队；V1 不开放跨 owner 转派。
4. strict dispatch 使用专用 inbox，不写 `looper:plan` / `looper:worker-ready` 等 legacy trigger。开启前要求连接该项目的 Looper daemon 全部升级、旧进程和启动项已停止；V1 不支持新旧二进制混跑。旧 work item 可由新版 Looper 的兼容路径继续处理。
5. 回滚时先关闭新派发；已创建 strict dispatch 继续由已升级的目标 Node 收敛或等待恢复，绝不降级成 legacy label 任务。旧任务仍走旧协议，两类任务不互相转换。

## Validation · 验收标准

- **AC-1**：Guest 能查看 Looper 面板但不能派发；Project Member/Admin 只有同时具备 work-item 编辑权且本人拥有 active Node 时，才能派给自己的 Looper；任何人都不能派给其他 owner。
- **AC-2**：角色审批权限与 Plane 端带 revision 的项目角色策略一致；Project Admin 不会天然获得产品、设计或技术审批权；不同 Node 的本地配置不能改变在途任务的角色资格。
- **AC-3**：按钮只使用当前登录用户经过 Plane 身份 + Node credential 双向绑定、项目匹配且支持所选角色的唯一 Looper，并显示 owner、Node、在线状态和当前负载；成员连接自己的电脑无需 Admin 审批，Project Admin 只启用项目级集成；Node 自报别人的 owner UUID无效。
- **AC-4**：同一项目每个 owner 最多一个 active Node；V1 没有跨 owner 目标下拉或多 Node 选择器。
- **AC-5**：派发确认文案明确说出 work item、执行方式、owner、Node 和离线影响；`直接实现` 只有最新技术 Spec 已由 owner批准，或 owner 对无待决策的小型 Bug 留下带原因 waiver 时才可用，绕过 UI 的请求同样被服务端拒绝。
- **AC-6**：成功派发后，Plane 中存在唯一、持久、可审计且不能靠手工拼 assignee/label 伪造的执行归属；对应 owner/Node 可领取，其他 Looper 不可领取。
- **AC-7**：双击、HTTP 重试、两个 owner 并发 self-dispatch，或有人通过单条/bulk/import/automation 改 assignee/label，都不能产生第二个活跃归属；普通投影漂移只告警，不改写 Authority。检测到未升级 daemon 时项目只读，不能开启 strict 派发。
- **AC-8**：owner 已显式开启离线队列时，可向自己已注册但离线的 Node 排队；默认关闭，任何其他人都无法投递。允许时面板显示离线排队，Node 恢复后无需再次点击即可领取。
- **AC-9**：面板始终显示“来自 <owner> 的 Looper · <node>”；多同事部署时不会只显示一个无主的“Looper 机器人”。
- **AC-10**：面板用需求澄清、角色决策、技术方案、实现、PR、QA、完成表达流程；当前阶段、阻塞角色和下一步一眼可见。
- **AC-11**：产品、设计、研发、QA 卡片分别展示 dispatch 冻结的角色策略负责人和待处理数量；释放后由新 owner self-dispatch 不会悄悄更换产品/设计/QA，研发负责人更新为新 Looper owner；每个问题绑定明确 request，只有对应负责人从 Plane 回答入口提交的评论会满足它，飞书仅通知且产品可只写一句快速决策。
- **AC-12**：不适用阶段显示“已跳过（原因）”，不会无痕消失。
- **AC-13**：活跃任务不能再次派发；运行中任务必须先由原 owner 本地停止并由 Node 回报终止、释放 dispatch，之后新 owner 才能 self-dispatch；停止待确认或 Node 丢失期间不创建新 dispatch，历史归属可追溯。
- **AC-14**：loopernet 不可用时，durable Plane 状态仍可展示且不会自动改派；实时状态明确降级。
- **AC-15**：未配置集成、无 Node、身份 drift、owner 被移除、work item archived/done 均有明确空态或错误态，不出现可点但必失败的按钮。
- **AC-16**：桌面主内容区和窄屏均可读；默认视图不展示日志墙，当前行动保持在首屏。
- **AC-17**：Spec、PR、决策、验收产物均从面板一跳可达，且链接标题可读。
- **AC-18**：埋点覆盖面板曝光、派发开启/提交/结果、释放、角色行动和产物打开，且不采集正文、日志、原始成员 ID、Node ID 或 idempotency key。

## Execution mode 自评

- **Proposed**：`HITL-needs-engineer`。
- **依据**：涉及 Plane 权限边界、跨服务身份、并发幂等、跨 owner 竞争和用户可见 UI；需要产品/设计确认界面，还必须做真实双 owner/双 Node 隔离 E2E。可以让 agent 完成切片实现，但不能无人值守合并或直接全量开启。

## Open questions

- 设计同学是否接受“主内容完整面板 + sidebar 摘要”及风险状态矩阵？需要在设计评审中确认，但不阻塞已确定的 owner-only 产品规则。
- 多 Node、跨 owner 授权、管理员 break-glass、远程强杀与人工接管全部明确留到后续版本，不作为 V1 隐含扩展点。
