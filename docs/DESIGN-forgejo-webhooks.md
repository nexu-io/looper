# Forgejo 原生 webhook：实现与验收

## 范围与依据

本次需求是为 Forgejo 增加完整的 webhook 触发支持，替代旧 MVP 的 polling-only 限制。实现复用 Looper 的 `tunnel` 模式、hook 生命周期、异步 discovery 队列及四个角色的现有执行入口。GitHub 的 `gh-forward` 与已有 tunnel URL 保持兼容；Forgejo-only 配置不需要 `gh`。启用步骤见 [配置说明](configuration.md#forgejo-webhook-setup)。

核对的官方稳定版为 Forgejo **14.0.2、15.0.8、16.0.4**。它们在本次所需的事件、签名、payload 和 hook 管理合同上相同：

- 原生签名是 `X-Forgejo-Signature` / `X-Gitea-Signature` 中的 HMAC-SHA256 十六进制字符串，没有 `sha256=` 前缀。精确事件在 `Event-Type` header；兼容的 GitHub header 也会同时出现。[签名与 header 实现](https://codeberg.org/forgejo/forgejo/src/tag/v16.0.4/services/webhook/shared/payloader.go)
- Issue、PR、评论、评审与同步事件使用 Forgejo 原生 payload。Actions 使用 `run.repository`，原始触发器位于 JSON 字符串 `run.event_payload`；不从显示文本 `prettyref` 推断 PR。[通知实现](https://codeberg.org/forgejo/forgejo/src/tag/v16.0.4/services/webhook/notifier.go)、[Actions 结构](https://codeberg.org/forgejo/forgejo/src/tag/v16.0.4/modules/structs/action.go)
- CREATE 支持原生 `forgejo` hook。GET 不返回 secret；PATCH 不更新 secret，也不更新 Actions 订阅。`issues` / `pull_request` 会展开为子事件；包含 push 与三个 Actions 事件时回读共 19 项，因此按展开集合比较，避免每次 reconcile 修改同一 hook。[14.0.2 API](https://codeberg.org/forgejo/forgejo/src/tag/v14.0.2/routers/api/v1/utils/hook.go)、[16.0.4 API](https://codeberg.org/forgejo/forgejo/src/tag/v16.0.4/routers/api/v1/utils/hook.go)、[事件回读](https://codeberg.org/forgejo/forgejo/src/tag/v16.0.4/models/webhook/webhook.go)

## 行为与取舍

Webhook 是 discovery 的唤醒信号。自动运行的权威仍是用户配置的角色策略、仓库当前标签/分配/评审状态及既有角色输出；签名只认证事件来源，没有增加对 agent 结构化输出的推断或审批层。

| 事件 | 立即执行的现有 discovery |
| --- | --- |
| Issue 生命周期、标签、分配、里程碑、评论 | 按 Issue 编号查询，进入 Planner / Worker 的原有筛选与入队 |
| PR 生命周期、标签、分配、同步、评审请求、评论与评审 | 按 PR 编号进入 Reviewer / Fixer |
| 非删除的分支 push | Fixer 的 base-branch-update discovery |
| Actions success / failure / recover | 原始触发器有 PR 时定向查询，否则合并一次仓库 PR discovery |

标签、当前用户分配、hold、归档项目、角色开关、预算和队列去重继续生效。HTTP 202 只表示异步接受；队列满和 admission 拒绝返回 503。队列自身保留既有重试和取消语义。稳定版 Forgejo 没有 `check_run` / commit-status webhook；外部 CI、没有 PR 引用的 Actions、有界列表未覆盖的项目和停机期间漏掉的事件仍依赖 polling fallback。Coordinator、远端 thread resolve 和 routed network 不在此次范围内。

实现先尝试消除重复层：没有新增 webhook daemon、持久化 delivery 表、迁移、重试账本或角色授权状态，而是给已有 tunnel 和 forwarder 增加原生协议适配。需要保留的差异如下：

- **仓库地址作为已有 record 的 key。** Forgejo 用完整仓库 URL，防止多个实例的同名仓库共用 hook ID 或 secret；GitHub 沿用 `owner/repo`。成本是 CLI rotate/delete 对 Forgejo 要使用完整 URL。只用 slug 无法区分实例。Secret 文件名使用 URL 的 SHA-256，避免简单字符替换碰撞与长 URL 超过文件名限制；没有额外持久化 hash 字段。
- **从已认证目标传递仓库身份。** tunnel 路由匹配配置项目，验签后携带它的完整 identity。scheduler 将该 identity 与同一份 runner 配置快照比较，防止旧事件在项目改绑后触发另一实例。成本是 discovery 参数多带已有 identity；不建立第二个项目目录或生命周期。header 和 payload 的 repo 名都无法独立表达可信实例来源。
- **按已记录 ID 管理 hook。** 复用现有 ownership 记录、URL drift 检查、连续禁用熔断和孤儿记录。Hook 管理的权威是本地记录的 remote ID 与用户的 tunnel 配置，跟 agent 输出无关。存储更新/删除以原 ID 为条件，防止 CLI 轮换时被旧 reconcile 覆盖；成本是并发冲突要重试，已创建但未发布的 hook 要回滚。复用一个 SQLite 条件写入即可，无需增加锁、版本字段或恢复层。
- **替换实现轮换与订阅修复。** Forgejo PATCH 无法完成这两件事，因此创建替代 hook → 发布已有记录 → 删除旧 hook。订阅修复复用 secret；轮换使用新 secret 文件。成本是短暂存在两个 hook，轮换时切换窗口内旧 secret 的在途事件可能验签失败；失败/重复依靠现有 discovery 去重与 polling 补偿。如果清理旧 hook 失败，明确报告其 ID，不引入清理账本。SQLite 发布失败会删除新 hook，保留旧记录与密钥。

Hook 管理使用项目默认 identity；未配置时使用 provider token 或显式 tea login。角色身份不参与仓库管理，角色 discovery 仍各自绑定原身份。项目移除后的孤儿清理可使用仍配置的 provider 凭据；没有可用凭据时可显式 `--forget`。直接 HTTP 管理禁用重定向，避免把创建请求的 secret 转发到其他地址。tea 仍由已安装 CLI 执行，其重定向行为不受 Looper 控制，应配置 canonical base URL；Looper 不读取 tea 的密钥文件。tea 的 HTTP 非成功状态即使退出码为 0 也必须报告失败，缺少状态的 mutation 不视为成功。[tea API 实现](https://gitea.com/gitea/tea/src/commit/9c12138d6273ba8feb6e7578833cc330c0cd6461/cmd/api.go)、[tea HTTP client](https://gitea.com/gitea/tea/src/commit/9c12138d6273ba8feb6e7578833cc330c0cd6461/modules/api/client.go)

## 验收与验证

验收负责人：本次实现者。官方源码用于确认协议；自动化测试使用本地 HTTP 合同服务，不把模拟服务描述为真实 Forgejo 联调，也不运行付费 agent。

| 要求 | 主要证据 |
| --- | --- |
| 签名 webhook 不等轮询即可创建四个角色的持久化任务并唤醒 claimer | `TestForgejoWebhookIngressCreatesRoleQueuesWithoutPollingOrGH`：真实 tunnel → forwarder → scheduler → 四个 runner → SQLite，未执行 scheduler tick、未调用 `gh` |
| 原生 header / payload、PR 评论与评审、Actions 原始触发器正确 | `TestForgejoNativeEventRouting`、`TestForgejoWebhookLifecycleAndSignedIngress` |
| Issue 定向读取绕过列表上限，hold、未分配、关闭与 PR 伪装不创建任务 | 四角色 ingress 集成测试；fixture 拒绝 Issue 列表请求 |
| 多实例、重复 UUID、归档与项目改绑隔离 | `TestForgejoForwardIsolationDedupeAndEligibility`、`TestForgejoWebhookCatalogDoesNotRetargetQueuedDelivery` |
| 管理 hook 重启复用、回读收敛、禁用重启、订阅修复、移除后成为孤儿 | `TestForgejoWebhookLifecycleAndSignedIngress`、`TestForgejoWebhookSubscriptionRepairRespectsDisableLatch` |
| 密钥轮换、删除与并发冲突不覆盖新的记录 | `TestForgejoWebhookCommandsRotateAndDeleteByRecordedID`、`TestForgejoWebhookReconcileCannotOverwriteConcurrentRotation` |
| 动态 provider 配置、兼容 Gitea header、停机 admission / 队列满返回 503 | `TestForgejoWebhookIngressRetryAndGiteaHeaders` |
| 管理身份、错误处理、签名 secret 不被错误输出/重定向泄漏 | `TestForgejoWebhookClientUsesProjectIdentityAndProviderAuthForOrphans`、`TestForgejoWebhookClientRejectsRedirectAndRedactsSecret`、`TestForgejoWebhookTeaRequiresHTTPStatusForMutations` |
| GitHub 路径与完整仓库回归 | 既有 webhook 测试及仓库根检查 |

2026-09-14 验证结果：`go vet ./...`、`go build ./...`、`go test -p 1 ./...` 均通过；四个 webhook 相关包的 race 检查通过，最后改动过的 runtime 和 CLI 路径也分别复跑通过。630 个当前仓库 Go 文件使用与 CI 一致的 Go 1.22.12 完成 gofmt 检查，`git diff --check` 通过。本机 Go 1.27 对既有配置测试的对齐格式与 CI 不同，已恢复该文件原样。根目录 `gofmt -l .` 还遍历了忽略的 `.slim/worktrees/`，因此本地格式检查按 Git 当前仓库文件清单执行；没有修改其他 worktree。另修正了 GitHub webhook 测试夹具在清理时直接改写进程存活状态的 data race。

默认并行 `go test ./...` 首轮通过，后续全量复跑的 `TestHumanAttentionContract_PostClaimNotifyCanceledOnShutdown` 和 `TestHumanAttentionContract_NotifyDrainTimeoutRetainsStorage` 出现假 osascript 在三秒内未启动的超时；相关通知实现与测试未修改。两项单独复跑、runtime 整包 `-count=1`、串行全量均通过。保留这个并行测试限制，没有为取得通过而延长断言时限。本次未改动用户运行配置、注册线上 hook 或执行真实 Forgejo 联调。
