---
id: 20260717-pre-spec-multirole-decision-grill
name: 技术方案前的多角色需求 GRILL 与决策分流
status: implemented
created: '2026-07-17'
execution_mode: hitl
---

# 技术方案前的多角色需求 GRILL 与决策分流

## 1. 结论

Looper 的 Plane `dispatch/plan` 流程新增一个位于技术 Spec 之前的“需求调研与决策收敛”阶段：

1. 先在隔离 worktree 中阅读代码、复现问题、查已有设计模式，并形成结构化《决策简报》。
2. 再由一个 fresh agent 对简报做对抗式 requirement GRILL，去掉能由代码事实或工程常识回答的问题。
3. 剩余阻塞问题按产品、设计、研发三种角色分流；Plane work item 评论是唯一回答事实源，飞书只负责通知和展示。
4. 产品先回答；产品边界明确后，设计与研发问题可以并行等待。
5. 所有阻塞问题均被正确角色明确回答后，再写中文技术 Spec，并继续现有的 fresh GRILL、独立 REVIEW、Looper owner 审批和 Worker 实现流程。

本次不把“问题数量”作为必须写产品 Spec 的硬门槛。数字只约束通知可读性：飞书摘要最多展示三个最关键问题，Plane 保留完整问题清单。

## 2. 背景与问题

现有流程在 Plane feature 上先执行 `productSpecGate`：没有 `looper:product-spec` 就在创建 worktree 前暂停。技术 Spec 写作过程中，主 planner agent 才可能通过 `productAsk` 发现产品问题。之后才执行技术 Spec 的 GRILL 和 REVIEW。

这会产生四类问题：

- Looper 尚未充分读代码、复现和查既有规范，就要求产品提供完整 Spec；产品需要承担本可由研发调研消除的上下文整理。
- 真正的产品、设计和研发决策在技术 Spec 写作中才暴露，技术方案可能建立在未经确认的产品假设上。
- 当前只有 `productOwner` 和本地 `owner`，没有设计负责人，也没有按 Plane 身份校验每类回答者。
- 飞书里的问题容易变成长文本；设计位置、布局或交互取舍仅靠文字不够直观。

期望结果是：小问题能由对应负责人在 Plane 评论中快速拍板；大需求才要求正式产品 Spec；存在真实界面取舍时，Looper 先给设计同学可视化选项，而不是让设计同学从空白上下文开始研究。

## 3. 已确认的产品决策

### 3.1 角色与权责

- 产品决策人：项目 `productOwner`。
- 设计决策人：项目 `designOwner`。
- 研发决策人：当前部署的 Looper `owner`。V1 不新增 `engineeringOwner`，因为初期只有研发同学在本地部署 Looper。
- 最终技术 Spec 审批人：当前 Looper `owner`。
- V1 每种角色只配置一位负责人，不做 delegate、组织架构或值班动态路由。
- 不允许任意群成员的评论解除决策阻塞或批准技术 Spec。

### 3.2 渠道

- Plane work item 评论：需求决策问题、回答、Decision Log 的事实源。
- Plane page：正式产品 Spec和技术 Spec 的事实源。
- 飞书 thread：单向通知、@对应负责人、展示设计截图和跳转 Plane；不读取飞书回复，不通过卡片按钮改变决策状态。
- 问题必须假设接收者完全不知道任务背景：用用户语言描述背景、现状、待决定事项、选项、推荐和影响，不要求先读代码或技术 Spec。

### 3.3 产品 Spec 的判定

不按问题数量机械判定。下列任一情况成立时，要求正式产品 Spec：

- 新增或显著改变一项完整用户能力。
- 涉及多个页面、角色或一条端到端用户流程。
- 涉及权限、付费、数据删除、兼容性承诺或其他高风险规则。
- 多个产品决策相互依赖，无法逐条独立回答。
- 第一轮产品评论回答后仍持续出现新的阻塞产品决策，说明需求边界没有形成。

否则，允许产品负责人直接在 Plane work item 评论中按问题编号快速回答；即使独立小问题超过三个，也不因此强制写 Spec。

### 3.4 设计决策的可视化

- Looper 必须先查现有页面、组件、设计系统和相似交互。能够由一致性原则唯一确定的 bug 修复，不升级给设计同学。
- 存在真实界面取舍时，Looper 给出 2–3 个最小 HTML 方案，并渲染成截图；每个方案使用稳定编号，如 `DESIGN-001-A`。
- 飞书卡片直接展示截图、推荐方案和一行取舍；按钮只跳转到 Plane work item。
- 设计负责人必须在 Plane 评论中选择编号或给出明确修改意见。飞书回复不会被读取。
- 新页面、多步骤流程或信息架构改变不能只靠临时 HTML，必须要求设计稿或设计文档链接。

### 3.5 收敛规则

- 产品问题先收敛，之后才最终生成设计和研发问题；设计与研发问题可以并行等待。
- 每个阻塞问题必须有稳定 ID：`PROD-###`、`DESIGN-###`、`ENG-###`。
- 只有问题所绑定角色的 Plane 负责人回答才有效。
- 非阻塞假设可以由 Looper 采用推荐方案，但必须写入 Decision Log；阻塞问题超时不能自动采用推荐方案。
- 收敛分成三道 barrier：初始 GRILL 只冻结产品边界；产品回答或正式 Spec 出现后立即 fresh GRILL，生成新 revision 的设计与研发问题；设计和研发阻塞项全部回答后再运行最终 fresh GRILL。只有最终输出 `blocking=[]` 才能进入技术 Spec。
- 产品答案发生变化时，旧 revision 的下游问题、答案和截图全部标记为 superseded，不得复用。

## 4. 事实源

以下事实来自当前主干实现，实施前须再次核对：

- `internal/planner/runner.go`：当前步骤为 `discover-issues → prepare-worktree → write-spec → publish → grill → review → notify`，没有技术 Spec 之前的需求 GRILL。
- `internal/planner/runner.go`：`productSpecGate` 在 `prepare-worktree` 开头执行，Plane `kind/feature` 没有产品 Spec 时不会创建 worktree。
- `internal/planner/runner.go`：`plannerProductAskInstruction` 在写技术 Spec 时才分类产品问题；当前没有设计与研发决策角色。
- `internal/planner/runner.go`：现有 `grill` 和 `review` 是技术 Spec 发布后的两个 fresh pass，应保留。
- `internal/config/types.go`、`internal/config/project_roles.go`：项目已有 `productOwner`、`qa`、本地 `owner`，没有 `designOwner`；`owner` 当前只有飞书 open_id。
- `internal/loops/hitl.go`：通用 HITL metadata 一次只表达一个 `HITLAsk`，不适合作为多角色、多问题并行状态机。
- `internal/runtime/awaiting_product_spec.go`：当前只轮询正式产品 Spec 是否出现，然后恢复 paused planner。
- `internal/runtime/spec_approval.go`、`internal/infra/planedoc/gateway.go`：技术 Spec 审批目前从所有非 Looper page comment 中交给 LLM 判断，没有限制评论者必须是配置的 Looper owner。
- `docs/GUIDE-hitl-setup.md`：现有团队工作流仍包含飞书双向 HITL 描述；本改造的 Plane 决策链必须明确为 Plane 入站、飞书单向通知。

## 5. 范围

### 5.1 V1 范围

- 仅 Plane provider。
- 所有进入 planner 的 `dispatch/plan` 任务；不限制于 `kind/feature`。
- 产品、设计、研发三类阻塞决策。
- 产品评论快决策与正式产品 Spec 两条路径。
- 最小 HTML 设计选项、截图渲染、飞书图片通知。
- Plane 回答轮询、身份校验、Decision Log、重启恢复和幂等。
- 技术 Spec 审批改为只接受本地 Looper owner 的 Plane 身份。
- 保留现有技术 Spec GRILL、REVIEW 和后续 Worker 交接。

### 5.2 非目标

- GitHub / Forgejo 的多角色需求分流。
- 从飞书回复、表情或卡片按钮读取答案。
- 自动修改 Figma、读取设计稿语义或替代正式设计稿。
- 自动替产品撰写并批准正式产品 Spec。
- delegate、多人共同审批、组织架构或值班路由。
- 超时自动选择 Looper 推荐项。
- 通用可视化设计工具；V1 的 HTML 只用于低成本选项示意。

## 6. 目标状态机

Plane planner 的目标顺序：

```text
discover-issues
  → prepare-worktree
  → author-decision-brief
  → requirement-grill
  → route-product-decisions
      ├─ 无产品阻塞 ─────────────────────────────┐
      ├─ 正式产品 Spec → await-product-spec ─────┤
      └─ 评论拍板 → await-product-decisions ─────┤
                                                  ↓ 产品边界已冻结
                                      requirement-grill (fresh rerun)
                                                  ↓
                                      route-design-engineering-decisions
                                      ├─ 无下游阻塞 ──────────────┐
                                      └─ await design + eng ─────┤
                                                                   ↓
                                                    requirement-grill (final fresh)
                                                                   ↓ blocking=[]
                                             write-spec
                                               → publish
                                               → tech-grill
                                               → review
                                               → await owner approval
                                               → worker
```

关键约束：

- `prepare-worktree` 不再用产品 Spec gate 阻塞，从而允许在隔离目录做调研和 HTML 原型。
- `write-spec` 之前必须有已收敛的 Decision Log；技术 Spec 不再负责首次发现产品问题。
- 如果写技术 Spec 或第二轮技术 GRILL 又发现新的产品/设计阻塞问题，必须 fail closed，回退到 `author-decision-brief`，不能在技术 Spec 阶段临时向错误角色提问。
- GitHub / Forgejo 保持当前步骤序列，避免扩大 V1 影响面。
- 等待不是 failure，也不是 `SkipReason`。planner runner 新增 first-class `await` step outcome：它先持久化 `pipelineVersion/phase/resumeStep` 和等待条件，再成功结束当前 run、完成当前 queue item并把 loop 置为 `paused`。reconciler 满足条件后新建 queue item；`createRunContext` 对 V2 paused loop 必须优先从 checkpoint 的 `resumeStep` 恢复并原子清除本次 wait，而不是按 latest run status 或 `LastCompletedStep` 猜测。
- 每个 await 节点必须在状态表中定义进入条件、唯一 `resumeStep`、唤醒条件和清理动作；不得通过伪造 `FailureManualIntervention` 或普通失败来推进正常人类等待。

## 7. 决策简报协议

### 7.1 Agent 输出

`author-decision-brief` 主 agent 在 worktree 内充分调研后输出版本化 JSON；fresh `requirement-grill` 可以修改同一个简报文件，但不能写技术 Spec。

建议运行目录位置（明确位于业务 worktree 外）：

```text
<runtime-artifact-root>/<loop-id>/<revision>/decision-brief.json
<runtime-artifact-root>/<loop-id>/<revision>/<question-id>/<option-id>.html
<runtime-artifact-root>/<loop-id>/<revision>/<question-id>/<option-id>.png
```

这些是运行产物，不提交到业务仓库；最终结论进入 Plane Decision Log 和技术 Spec。Looper 为 agent 显式传入唯一 allowlisted artifact root；agent 返回 worktree 内路径、symlink、越界路径或业务 worktree 出现 `.looper/decision-*` 时立即 fail closed。进入 `write-spec` 前再次检查业务 worktree，防止 fallback commit 把调研 JSON、HTML 或 PNG 提交进去。

最小协议：

```json
{
  "version": 1,
  "summary": "用户是谁、遇到了什么、为什么重要",
  "facts": [
    {"text": "当前行为", "evidence": "文件、测试、复现或已有文档"}
  ],
  "formalProductSpec": {"required": false, "reason": ""},
  "questions": [
    {
      "id": "DESIGN-001",
      "role": "design",
      "blocking": true,
      "question": "用户在什么场景下需要看到哪个按钮？",
      "context": "不依赖代码知识的完整背景",
      "options": [
        {"id": "DESIGN-001-A", "label": "方案 A", "impact": "影响", "htmlPath": "...", "pngPath": "...", "manifestPath": "..."}
      ],
      "recommendedOption": "DESIGN-001-A",
      "recommendationReason": "为什么",
      "evidence": ["现有相似页面或复现证据"]
    }
  ]
}
```

### 7.2 Authority

- 代码、测试、复现、已有产品/设计文档是 `facts` 的 authority。
- 用户已确认的复杂度规则是 `formalProductSpec.required` 分类的 policy；该值由 fresh GRILL 后的结构化 agent 输出执行，不再用基础设施做第二套启发式推断。
- Plane 中由配置角色本人发布的评论或正式 Spec 是最终决策 authority。
- Agent 推荐、截图和 LLM 对评论的语义解析都不是审批 authority。

## 8. Plane 评论与回答协议

### 8.1 请求评论

每个角色、每个 revision 发布一条 append-only 的 Looper 签名评论，不为每个问题单独发评论。V1 不假设 Plane 支持可靠地 PATCH work item comment；新 revision 创建新评论，并用一条签名审计评论声明旧 revision 已 superseded。评论包含隐藏 marker：

```html
<!-- looper:decision-request v=1 loop=<loop-id> role=design revision=2 -->
```

可见正文包含：背景、已确认内容、完整问题列表、每题选项与推荐，并明确要求按以下格式回复：

```text
DESIGN-001: DESIGN-001-B
DESIGN-002: 自定义: 保持现状，但文案改为……
```

同一个 `loop + role + revision` 只能创建一次请求评论。fresh GRILL 修订问题时递增 `revision` 并创建新请求；reconciler 只读取最新有效 revision。飞书摘要卡可以原地 patch，但 Plane 历史保持 append-only。

### 8.2 有效回答

有效回答同时满足：

- 评论时间晚于当前 revision 的请求评论；V1 不消费对旧评论的编辑，负责人改变答案时必须发一条新评论。
- `actor` 等于该角色配置的 Plane member UUID。
- 能明确映射到当前 revision 中尚未回答的一个或多个问题 ID。
- 不来自 Looper 签名评论。

V1 不再用 LLM 猜测回答归属：必须显式引用问题 ID。带选项的问题优先回答合法 option ID；如果所有建议选项都不合适，正确角色必须使用 `问题ID: 自定义: 清晰决定`。`待定`、稍后讨论、拼错 option ID、把多个 option ID 混在同一答案、缺少问题 ID 或同一评论互相冲突都会清除该题的旧答案并保持 blocked，不能猜测。`designDocumentRequired=true` 的问题只接受单一 HTTP(S) 设计稿/设计文档 URL。

需要正式产品 Spec 时不沿用普通问题评论格式。Looper 明确要求产品负责人在当前 work item 的 Links 中关联标题为 `looper:product-spec` 的非空 Plane 原生页面；外部链接、空白占位页、普通 work item 评论和飞书回复都不会解除正式 Spec 门禁。resolved 阶段仍重读页面；链接删除或正文变化会撤销旧下游 snapshot，先 park/cancel queue，再等待一轮稳定读取。

错误角色的回答保留在 Plane 中但不改变状态；V1 不为每次无效输入追加审计评论，避免轮询刷屏，正确回答格式由原角色请求评论持续展示。

### 8.3 Decision Log

每次有效回答（包括只回答了部分角色）后，Looper 立即持久化 checkpoint，并追加一条不可变 Decision Log 快照。相同快照 hash 幂等，新快照保留旧快照作为替换历史：

```html
<!-- looper:decision-log v=1 loop=<loop-id> revision=<n> snapshot=<hash> -->
```

每条记录至少包含：问题 ID、最终回答、回答者 display name（如 API 可得）/ actor、来源 comment ID、时间、采用的 option，以及是否替换旧答案。技术 Spec 必须引用最新有效 Decision Log。

### 8.4 Plane API contract spike（实施 Step 0）

编码前必须在隔离 Plane project 验证并留存真实响应样例：

- work item comment GET 的 UUID、actor UUID、created_at、可选 display_name / updated_at 字段。
- comment create 返回值。
- 是否存在可用的 PATCH-by-UUID；不存在时按上述 append-only 协议实施。
- page comment 的 actor UUID、comment UUID、时间字段。

实现独立 `WorkItemComment` model，全程保留 UUID；不得继续使用当前会把 Plane UUID 丢成 `Comment.ID=0` 的通用模型作为决策 authority。actor UUID 缺失或不稳定时，identity gate fail closed，不能按 display name 猜测。

## 9. 角色配置

项目配置扩展为：

```jsonc
{
  "projects": [
    {
      "id": "open-design",
      "productOwner": {
        "feishuOpenId": "ou_...",
        "planeId": "<member-uuid>"
      },
      "designOwner": {
        "feishuOpenId": "ou_...",
        "planeId": "<member-uuid>"
      },
      "owner": {
        "feishuOpenId": "ou_...",
        "planeId": "<member-uuid>"
      }
    }
  ]
}
```

实施建议：

- 给现有 `FeishuActorConfig` 增加可选 `PlaneID`，并将注释改为项目 actor 身份配置；暂不做破坏性重命名。
- 新增 `DesignOwner *FeishuActorConfig` 与 `ProjectDesignOwner` resolver。
- `owner.planeId` 是研发决策和技术 Spec 审批的唯一允许身份。
- 对启用了 Plane planner 的项目，缺少某个实际被提问角色的 `planeId` 时 fail closed，并在本地 owner 飞书 thread 提示配置；不允许退化为“任何人都能回答”。

## 10. HTML 原型与截图通知

### 10.1 生成约束

- 只为 `role=design` 且确有视觉/交互取舍的问题生成。
- 每题 2–3 个选项；共享同一 viewport、种子数据、字体和基线样式，确保截图差异只来自待决策点。
- Agent 产出的原始 HTML 不是可直接执行的 authority。Looper 必须先解析成受限静态 HTML：白名单 tag/attribute，删除 `script`、事件属性、iframe/object/embed、表单、meta refresh 和所有非允许 URL；默认禁用 JavaScript。
- CSS 中的 `url()`、`@import` 等外部加载也必须解析和拒绝，不能只清洗 HTML 属性。
- V1 不允许任何本地图片/字体/外部资源；这些 tag、URL 属性和 CSS 加载原语一律拒绝，浏览器不会获得业务 worktree 或用户目录路径。
- 截图固定 viewport，并把 viewport、renderer、生成时间、PNG byte size、SHA-256 和完成方式写入每个选项的持久化 manifest。
- 生成失败时不能伪装成有截图：保留设计问题 blocked，向 Looper owner 报告 renderer 配置错误。

### 10.2 渲染器

V1 由 Looper 控制浏览器进程，不允许运行任意 `renderCommand`：

- 默认在 macOS 探测 Chrome/Chromium；配置只允许覆盖 browser executable path。
- Looper 把清洗后的静态页面和复制后的 allowlisted 资源放进新的临时根，通过只监听 loopback 的临时 server 提供，不使用 `file://`。
- 清洗后的页面使用 `script-src 'none'` CSP，Chrome 同时带禁用 JavaScript 参数；所有非 loopback HTTP(S) 流量强制走本机关闭代理，host resolver 也只排除 `127.0.0.1`。Chrome 使用临时 user-data-dir、无扩展、超时和最小权限参数；任一层失败都停止渲染。
- 不下载浏览器、不在运行时执行 `npx` 安装，避免同事部署时引入隐式网络和磁盘开销。
- renderer threat model 和清洗器需要单独安全测试；不能用“agent 承诺不联网”作为隔离证据。

### 10.3 飞书发送

现有 notify gateway 没有图片上传能力。新增 app-bot 图片上传与 thread 图片消息：

- 使用配置已有的 Feishu app 凭据上传 PNG，拿到 `image_key` 后发到原任务 thread。
- 每个设计问题发一组图片和一张摘要卡；摘要卡最多展示三个关键问题，完整问题与回答入口指向 Plane work item。
- 使用 `loop-id + option-id + revision` 形成稳定可见消息 UUID；重新渲染后 PNG 字节变化也不会重复出卡。Feishu send API 的服务端幂等 UUID 是可见 exactly-once authority，每张图的 message receipt 发送后立即写 checkpoint；崩溃窗口内允许重复不可见的 upload，不允许重复可见消息。
- 若服务端不支持幂等 UUID，必须先证明能通过 thread message list 按稳定 marker 找回已发送消息，再允许发送；两者都不可用时图片通知功能不得发布，不能用“发送后写 checkpoint”宣称 exactly-once。
- 图片发送失败不影响 Plane 记录，但设计问题继续 blocked，并通知本地 owner 处理，不能降级成无图让设计师猜。

## 11. 持久化与恢复

不改 SQLite schema。把 `DecisionBrief` 的运行状态作为 planner checkpoint JSON 的一个版本化字段保存，至少包括：

- 创建 loop 时冻结的 `plannerPipelineVersion`（旧流程 V1 / 新流程 V2）、当前 `phase`、唯一 `resumeStep` 和 await reason。
- 当前 brief revision；问题本身另有稳定 question hash，回答必须绑定 request revision 与 question hash。brief 在补入 HTML/PNG/manifest 路径时会变化，因此不保存一个没有校验消费者的伪 authority hash。
- 当前阶段。
- 每个问题的 role、blocking、request comment ID / timestamp、answer comment ID、answer actor 和状态。
- 每个角色的通知 dedupe key。
- Decision Log comment ID。

引入该 checkpoint 字段防止以下具体故障：daemon 重启后重复提问、错误消费请求之前的旧评论、部分回答丢失、同一设计截图重复发送，以及升级/回滚时用当前 feature flag 重新解释历史 step。

成本是增加一个 authority-bearing 的持久状态与恢复分支。更简单的“每次从全部 Plane 评论重新推断”不足以满足幂等：评论可能被编辑、问题 revision 会变化、同一问题可能重答，且通知是否已发无法从 Plane 推导。实现 PR 必须按仓库规则请求 `@oracle` review。

## 12. 对现有流程的修改

### 12.1 Planner

- `internal/planner/runner.go`
  - 创建 loop 时冻结 `plannerPipelineVersion`；V1/V2 使用独立 transition table，不再让一张全局 `plannerStepSequence` 同时解释两种历史 checkpoint。
  - Plane V2 路径插入 `author-decision-brief`、三道 requirement barrier 和对应 route/await steps。
  - 仅在 V2 handler 中跳过旧 `productSpecGate`；V1 transition table 和 handler 原样保留 gate，确保 flag 关闭的新 loop 与历史 V1 loop 行为不变。
  - 把旧 `productAsk` 从正常写 Spec 合约中移除；若技术 Spec agent 仍发现阻塞需求问题，结构化返回并回退需求阶段。
  - 保留并重命名现有技术 `grill` 语义，避免与 requirement GRILL 混淆。
- 建议把多角色决策协议放到独立 `internal/planner/decisions` package，不继续扩张单个 `runner.go`。

### 12.2 Plane gateway 与 reconciler

- 新增 work item comment 的完整读取模型，必须保留 UUID comment ID、actor UUID、created time 和 HTML marker；display name / updated time 以 contract spike 的真实返回为准，不存在时协议不得依赖它们。
- 新增 `reconcileAwaitingRoleDecisions`，只扫描确实处于该阶段的 paused planner；按 actor 校验并聚合新评论。
- 改造 `reconcileAwaitingProductSpec`：只处理 brief 明确判定需要正式产品 Spec 的 loop，并在 Spec 出现后回到 fresh requirement GRILL，不直接进入写技术 Spec。
- `reconcileSpecApproval` 先过滤 `Actor == ProjectOwner(...).PlaneID`，再交 LLM 判断是否明确 approve。其他人的 approve 不产生 worker-ready。
- 每次发布技术 Spec review 时持久化 rendered Plane page hash，并在独立 REVIEW 完成后、打开审批门前再次读取；REVIEW 运行中页面被编辑时废弃本轮结果，重新发布本地收敛版本并 fresh REVIEW。打开审批后再持久化 `specRevision + specContentHash + reviewRequestCommentID/timestamp`，只接受当前 request 之后的 owner 评论；审批期间 Plane 页变化同样立即废弃当前审批门，旧 owner approve 不得批准新内容。

`reviewPlaneContentHash` 的 authority 是 GRILL 后由 Looper 把收敛的本地 Spec 发布到 Plane、再从 Plane 服务端读回的 rendered page bytes；不是 REVIEW agent 自报的结构化结果。它防止的具体失败是“REVIEW 读旧本地文件时，Plane 页面被人工改成新内容，随后未经 REVIEW 的页面却开放审批”。代价是 checkpoint 多一个 receipt、旧 checkpoint 需安全地重新发布并 REVIEW、远端读取失败会暂停流程。更简单的“只信 READY”无法证明 READY 对应 owner 实际看到的页面，而删除 Plane 审批页又不符合人工在 Plane 审批的既定产品流程，因此保留这一窄的 drift guard。

### 12.3 Notify gateway

- 新增图片上传、thread 图片消息和设计决策摘要卡。
- 产品、设计、研发分别 @对应 Feishu open_id；技术 Spec 审批只 @本地 owner。
- 任务根卡、实时进度卡、HITL 卡底部统一显示小字 `来自 @owner 的 Looper`，复用当前部署的 `owner.feishuOpenId`。owner 不在通知群也照常发送灰色 @；不做成员校验。未配置时显式显示 `未配置 owner`，避免多台本地 Looper 的卡片来源匿名。
- 所有通知带 Plane 深链；飞书不作为答案 transport。

### 12.4 文档

- 更新 `docs/GUIDE-hitl-setup.md`、配置示例和 Looper skill：明确需求与技术 Spec 的正式回答都在 Plane，飞书仅通知。
- 说明如何获得 `productOwner/designOwner/owner` 的 Plane member UUID 和飞书 open_id。
- 说明 browser executable path、受控 renderer 探测结果、安全诊断和失败处理；不暴露任意 shell render command。

## 13. 验收标准

### 13.1 流程

- [x] 启用 V2 的 Plane `dispatch/plan` 任务会先创建隔离 worktree 并调研，不会因缺少产品 Spec 在调研前暂停；V1 仍保留原 gate。
- [x] 技术 Spec 文件或 Plane page 在所有阻塞需求问题收敛前不存在。
- [x] 第一轮 brief 由主 agent 生成，再由 fresh agent GRILL；回答后至少再执行一次 fresh requirement GRILL。
- [x] 小型独立产品问题可由 product owner 在 Plane 评论回答后继续，无需正式产品 Spec。
- [x] 复杂需求会说明触发了哪条规则并等待正式产品 Spec；产品 Spec 出现后自动感知并重新 GRILL。
- [x] 产品先收敛；其后设计与研发问题可在同一等待阶段并行存在。

### 13.2 身份与权限

- [x] product owner 的评论只能回答 `PROD-*`。
- [x] design owner 的评论只能回答 `DESIGN-*`。
- [x] Looper owner 的评论只能回答 `ENG-*` 并批准最终技术 Spec。
- [x] 错误角色、普通群成员和 Looper 自己的评论都不能解除阻塞。
- [x] 缺失所需 Plane identity 时 fail closed，并给本地 owner 可执行的配置提示。
- [x] owner 对旧技术 Spec revision 的 approve 不能批准任何后续被修改的 Spec。
- [x] approval judge 运行后、添加 `worker-ready` 前再次读取并校验 Plane page hash；判断期间被修改的页面立即作废并要求重新 GRILL + REVIEW。
- [x] 独立 REVIEW 运行期间 Plane page 被修改时，未经 REVIEW 的页面不能打开 owner 审批门；必须重新发布本地收敛版本并 fresh REVIEW。
- [x] Plane 审批请求缺少远端 comment ID 或 `created_at` 时 fail closed，不使用本机时间补造 authority boundary。
- [x] 共享群中的任务根卡、实时进度卡和 HITL 卡均显示当前部署的 Looper owner；owner 不在群不阻断灰色 @，身份缺失时明确诊断。

### 13.3 设计选项

- [x] 能由已有规范唯一确定的 UI bug 不打扰设计负责人，brief 中记录采用依据。
- [x] 真实设计取舍生成 2–3 个可打开的 HTML 和一致 viewport 的 PNG。
- [x] 飞书 thread 中能直观看到各选项截图、推荐与 Plane 回答链接，并正确 @design owner。
- [x] 飞书回复不会被消费；Plane 中的有效选择才能继续。
- [x] 重启、重试或重复 reconcile 不会重复发送相同可见图片消息或同 revision 的 Plane 请求；该保证有 Feishu 服务端幂等 UUID 或可验证 remote lookup 作为证据。

### 13.4 技术 Spec 与后续流程

- [x] 中文技术 Spec 引用最终 Decision Log，不重新发明已经拍板的产品/设计行为。
- [x] 现有技术 Spec fresh GRILL 和独立 REVIEW 继续运行。
- [x] 只有 Looper owner 的明确 Plane page approve 才添加 `looper:worker-ready`。
- [x] 需求决策、技术审批和 Worker 交接均保留可审计 actor、comment 和时间。
- [x] final requirement GRILL 必须返回 `questions=[]`；non-blocking question 也不能作为未记录假设混入技术 Spec。
- [x] `RETURN_TO_REQUIREMENTS` 持久化进 checkpoint；重试不会丢失，reopen 会清空旧答案、请求、截图 receipt、Decision Log 与后续派生产物。

## 14. 测试策略

### 14.1 单元与契约测试

- DecisionBrief schema：未知版本、重复 ID、跨角色 option、无 evidence、无推荐、非法 artifact path。
- 三道 barrier：初始只发产品、产品回答后生成新 revision 的设计/研发、下游全部回答后 final GRILL；产品改答会废弃旧下游 revision。
- resolved checkpoint 仍观察正确角色的新 Plane 评论；queue 尚未执行时发生冲突改答会取消 active queue、恢复 paused barrier，不能带旧答案继续 final GRILL。
- 产品 Spec 分类：每条复杂度规则的正反例；证明问题数量本身不触发正式 Spec。
- actor 校验：正确角色、错误角色、空 identity、Looper 签名评论。
- 回答解析：显式 option、自由文本、部分回答、条件式回答、冲突回答、旧 revision 回答。
- Decision Log：create/update、答案替换、comment cursor、重启幂等。
- 技术 Spec 审批：owner 对当前 revision 的 approve 通过；旧 revision owner approve、product/design/普通成员 approve 均不通过。
- renderer：浏览器探测/路径覆盖、固定 viewport、超时、静态清洗+CSP+关闭代理/host resolver 禁网、输出路径和失败不降级；PNG 必须通过 chunk/CRC/IEND/完整 decode，并在活跃 Chrome 下连续稳定采样后才能主动结束进程。
- artifact guard：agent 返回 worktree 内路径、symlink 越界和进入 write-spec 前残留均 fail closed；fallback commit 不包含运行产物。
- Feishu image：upload/send/dedupe、API 失败、send 成功但本地 receipt 未落盘时的 crash recovery。
- pipeline version：V1 loop 升级后仍按 V1 续跑，V2 wait 从唯一 resumeStep 恢复，flag 改变不改写旧 loop。

### 14.2 集成测试

使用 fake agent + fake Plane + fake Feishu 覆盖：

1. 无问题：brief → fresh GRILL → write-spec。
2. 产品评论快决策：产品回答后重新 GRILL → write-spec。
3. 正式产品 Spec：等待 → 自动发现 → 重新 GRILL。
4. 产品回答后同时等待设计与研发；先回答一方仍保持 blocked，全部回答后继续。
5. 错误角色先回答、正确角色后回答。
6. daemon 在请求后、部分回答后、图片发送后分别重启。
7. 技术 Spec 阶段发现新需求阻塞时回退，不继续 publish。
8. 旧 V1 paused/failed planner 在启用和关闭 V2 flag 后均沿 V1 transition table 恢复。

### 14.3 隔离 Looper 端到端验收门（EAG）

在专用 Plane sandbox project、飞书“agent 通知群”和临时本地 Looper home 中运行真实 agent。严禁把任何 spike / EAG 测试消息发到“Looper 协作”群，避免打扰正在协作的产品、设计和研发同事：

```text
HOME=<temp>/home
--config <temp>/home/.looper/config.json
storage.dbPath=<temp>/home/.looper/looper.sqlite
storage.backupDir=<temp>/home/.looper/backups
project.repoPath=<sandbox clone>
project.worktreeRoot=<temp>/worktrees
scheduler.maxConcurrentRuns=1
server.port=<dedicated free port>
notifications.webhook.chatId=<agent 通知群 chat ID>
```

使用仓库现有 temp-home harness 的隔离方式，不使用代码并不识别的 `LOOPER_HOME`。同时配置独立 daemon 端口、artifact root 和浏览器临时目录；启动后必须从 runtime 状态/日志断言 DB、backup、log、worktree、artifact、config 全部位于 temp root，任何真实 `~/.looper` 路径都使 EAG 立即失败。

飞书发送前增加独立 preflight：读取实际 `chatId`，必须等于显式配置的“agent 通知群”测试 chat ID，并且不得等于“Looper 协作”群 chat ID。群名只用于日志展示，不能作为安全判断；测试配置必须保存允许和禁止的 chat ID。校验失败时 fail closed，一条飞书消息也不能发送。

负责人是否已经加入“agent 通知群”不属于发送门禁：允许使用正确 open_id @未入群成员，飞书显示灰色 @可以接受。E2E 校验卡片/消息携带了预期 open_id，但不要求 mention 可点击、可通知或该用户是群成员。

准备一个同时包含以下不确定性的 sandbox work item：产品范围选择、按钮位置的两个 HTML 方案、一个研发取舍。验收顺序：

1. 启动隔离 daemon，确认先调研和 requirement GRILL，不写技术 Spec。
2. 用错误 Plane 账号回答并确认不继续。
3. product owner 正确回答，确认出现设计截图和研发问题。
4. design owner 选择截图方案，Looper owner 回答研发问题；故意先只完成一方，确认仍等待。
5. 全部回答后确认 fresh requirement GRILL 再运行且 `blocking=[]`，随后生成中文技术 Spec。
6. 用非 owner 点赞/approve，确认不派发 Worker；再由 owner approve，确认添加 worker-ready、Plane 状态进入 In Progress。
7. 在关键节点重启 daemon，确认无重复 Plane 评论、飞书卡片或图片。
8. 保存 work item、comment IDs、thread 消息、agent run IDs、生成截图 hash、日志和最终 Decision Log 作为验收证据；清理 sandbox 分支、worktree 和临时 home。

EAG 不得使用真实生产 work item 验证错误身份、重复通知或重启恢复。
EAG 证据必须记录实际接收群的 chat ID，并证明所有测试消息都落在“agent 通知群”、没有落入“Looper 协作”群。

## 15. 发布、观测与回滚

### 15.1 Feature flag

新增项目级开关，V1 默认关闭：

```jsonc
"roles": {
  "planner": {
    "preSpecDecisionGrill": true
  }
}
```

仅对一个测试项目和一个本地 Looper owner 灰度；通过 EAG 后再扩到团队。

### 15.2 观测指标

- 每个任务被调研消除的问题数、实际升级到人的问题数。
- 评论快决策与正式产品 Spec 的比例。
- 按角色的等待时长、澄清轮次和无效回答次数。
- 每个 brief 的阻塞问题数量与 fresh GRILL 后减少量。
- 图片渲染/上传失败率、通知 dedupe 命中。
- 技术 Spec 阶段回退到需求阶段的次数；该值高说明前置 GRILL 质量不足。

运行一到两周后再用真实数据调整提问打包和正式 Spec 判定，不预设“最多三个问题”的硬规则。

### 15.3 回滚

- flag 只决定新建 loop 冻结为 V1 还是 V2；已经存在的 loop 永远按自身 `plannerPipelineVersion` 解释，不能随当前 flag 改道。
- 关闭 `preSpecDecisionGrill` 后，新任务恢复旧 planner 序列，已有 V2 loop 继续使用 V2；若二进制要回滚到不认识 V2 的版本，必须先用兼容 CLI 列出并暂停/完成所有 V2 loop，禁止旧二进制直接启动。
- 为升级前已经 paused/failed 的 V1 planner 做显式兼容测试，确保升级后仍走 V1 transition table；不自动迁移。
- 回滚不删除 Plane 的 Decision Request / Log；追加“流程已回滚”审计评论。
- 新增配置字段保持向后兼容，关闭开关时不参与旧流程。

## 16. 实施拆分

### Step 0（HITL）：Plane / Feishu / renderer contract spike

在隔离环境验证 Plane work item/page comment 字段与 mutation 能力、Feishu 消息服务端幂等或 remote lookup、图片 upload/thread send，以及受控 Chrome 渲染链。把脱敏响应样例和结论写入本目录的 `research.md`。任一 authority 或幂等前提不成立时先修订协议，不进入编码。

### Step 1（AFK）：协议、配置和 identity authority

实现 DecisionBrief / Decision Log 协议、`designOwner`、`owner.planeId`、配置校验、Plane 完整 comment model；先修复技术 Spec 只允许 owner 审批。请求 `@oracle` review。

### Step 2（AFK）：版本化 pipeline、前置 author + fresh requirement GRILL

建立 V1/V2 独立 transition table和 first-class await/resume 语义，插入 Plane-only planner steps，移动旧产品 Spec gate，生成并持久化 brief，保证三道 barrier、write-spec 前置门禁和回退语义。

### Step 3（AFK）：Plane 多角色请求、回答与恢复

实现 role request comment、answer reconciler、actor 校验、Decision Log、部分回答、revision 和重启幂等。

### Step 4（AFK）：HTML renderer 与飞书图片通知

实现安全本地渲染、Feishu image upload/thread send、摘要卡和 dedupe；补部署诊断。

### Step 5（AFK）：技术 Spec 接线和文档

让中文技术 Spec 消费最终 Decision Log，保留 tech GRILL/REVIEW；同步配置、HITL 渠道和部署文档。

### Step 6（HITL）：隔离 EAG

由真实产品、设计和 Looper owner 的测试身份完成 Plane 回答与审批；保存证据。此步骤不能 AFK，因为身份权限和实际可读性是验收对象。

## 17. 实施前复核清单

- 新 steps 与现有 interrupted/follow-up native resume 的交互是否会恢复到错误 agent session。
- planner checkpoint 增量字段是否足够，还是应删除更多现有 `awaitingProduct*` metadata，避免双状态源；pipeline phase 必须是唯一恢复 authority。
- Step 0 是否证明 Plane 稳定返回 actor/comment UUID；若不能，identity gate 必须阻塞而非按 display name 猜。
- 受限静态 HTML 清洗、loopback server、CSP+浏览器参数禁用脚本、关闭代理+host resolver 禁网、固定 viewport 在团队 macOS 环境是否可复现。
- 飞书图片上传 token 生命周期、消息线程参数和 dedupe 是否能复用现有 app client。
- 旧 paused planner 的兼容/迁移是否明确。
