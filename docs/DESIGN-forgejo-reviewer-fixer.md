# Forgejo reviewer / fixer 改造与验收

目标是让原生 Forgejo review → fixer → 再次 review 持续运行。远端没有 resolve 接口时，修复、验证、推送和回评继续执行；评论仍然保留未解决状态。可见消息使用与 GitHub 相同的正文与 disclosure 模板，不展示 Reviewer Summary / Looper Forgejo Fixer Summary 专用标题或轮次协议。

本文是本次改造的实施记录。既有 hold、作者过滤、自审开关和自动合并授权规则继续生效。

## 行为与验证

| 编号 | 预期行为 | 主要验证 |
| --- | --- | --- |
| F1 | 手动创建 reviewer / fixer 按项目 provider 读取 PR 和 hold，包括动态注册的项目；不误调用 gh | API provider 合约 |
| F2 | Forgejo 的真实冲突、当前 head 的失败 CI 能进入公共 reviewer / fixer 数据结构 | REST / runtime 合约与 sandbox |
| F3 | 原生 inline finding 可自动触发 fixer，同一账号的 Looper reviewer finding 不被当成 fixer 自言自语过滤 | discovery 与跨步骤集成 |
| F4 | 无 resolve 接口不阻止代码修复；agent 明确 fixed、验证与推送完成后记录本地处理结果，不伪造远端 resolved | fixer 生命周期集成 |
| F5 | 未改变且已处理的 finding 不因轮询或重启反复修复；新评论、被编辑的 finding 和发生分歧的 head 重新评估 | 持久状态 / 漂移集成 |
| F6 | 修复后的 head 可以再次 review；repair deferred / declined 不被记录成 fixed | reviewer / fixer 交接与 sandbox |
| F7 | 原生和兼容评论模式都复用 GitHub 署名，协议元数据不可见，修复反馈说明实际处理结果 | 渲染合约与远端正文检查 |
| F8 | agent 获取正确的 Forgejo PR URL、API 与 CI 日志上下文 | prompt / runtime 合约 |
| F9 | 显式启用自动合并后沿用现有范围、策略与分支保护规则，并绑定已审核 head | config / provider 合约与隔离分支验收 |

## 实施顺序

1. 补齐 provider 数据和手动入口。保留原有公共结构，提供真实 mergeability、commit status、Actions 和 review 数据；修正遗漏的 provider dispatch。
2. 打通原生 review / fix 状态流转。移除修复前的 resolve 能力阻断，将代码处理结果与远端评论状态分开，接入已有重试、回评和消息发布路径。
3. 运行完整 Go 检查和隔离 sandbox 验收，更新配置说明、迁移步骤和实际限制。

每个阶段在相关测试通过后进行独立代码审查。源码修改在独立 worktree 分支完成，不更改当前运行的 looperd 配置或进程。

## 权威与成本

对 finding 的处理决策以 agent 的结构化 `repair_results` 为权威；推送、head 和评论指纹只用于检测执行结果或输入漂移，不能从“发生了 commit”推断“所有评论已修复”。

Forgejo 拒绝在自己创建的 PR 上提交 REQUEST_CHANGES；已获准的自审因此保留结构化 blocking outcome，以 COMMENT 作为传输形式。marker 验证和历史恢复共用一个非持久化的显式兼容参数，其成本是两处事件匹配调用需要保持一致；直接放宽所有 COMMENT 会改变其他作者及 GitHub 的既有策略，因此不采用。自审是否允许仍由现有配置与手动入口决定。

`mergeable=false` 不能直接表示冲突：Forgejo 还用它表示计算中、计算失败及 WIP。provider 保留原始三态；运行时只在明确为 false 时，使用 `git merge-tree --write-tree` 检查准确的 base/head commit。代价是一次本地 Git 计算以及必要时获取缺失对象，不新增持久化状态，不变更分支、index、工作区或 FETCH_HEAD；简单映射布尔值会把正常 PR 错判为冲突，因此不足以满足 F2。该路径要求 Git 支持 `merge-tree --write-tree`（2.38+）。依据：[Forgejo 16.0.3 Mergeable 实现](https://codeberg.org/forgejo/forgejo/src/tag/v16.0.3/models/issues/pull.go#L881)。

优先删除“无法 resolve 就不允许修复”的能力门槛。复用已有 fixer evidence 的 `ResolveState=fixed_unresolved`，明确表示“代码已处理、远端仍未关闭”，不引入第二套 receipt / ledger 或数据库迁移。repair checkpoint 增加 agent 当时看到的评论指纹，防止 resolve 重试时把旧决定套给已编辑评论；缺少指纹的旧 checkpoint 重新采集，不从新快照补猜。成本是保留新状态值与旧 checkpoint 的兼容路径，并让发现、重试与展示保持同一含义。仅忽略所有远端未解决评论会丢失新评论、编辑和 deferred 结果；仅删除能力门槛则会在重启后重复修复。

真实验收发现，普通 push 后 Forgejo 会改变评论的 `updated_at`，而正文和其他返回字段完全不变。因此删除以该时间戳代表内容版本的假设，已有指纹改为 comment ID 加原始正文的 SHA-256，不新增持久化字段。定位投影和时间戳不参与计算；实际正文编辑即使没有更晚的时间戳也能被发现。成本是旧时间戳格式的 checkpoint / evidence 不再匹配，需要安全地重新评估一次，以及正文变化会触发重新处理。直接去掉漂移判断会让旧 agent 决定作用于新内容；直接把全文重复塞入指纹则增加 prompt 与 checkpoint 体积，均不采用。哈希只检测输入变化，fixed 的权威仍是 agent 的结构化结果。

原生评论没有远端 resolve/reply 结果时，公共修复说明是该轮唯一的远端处理反馈，不能沿用 GitHub 补充总结的 best-effort 语义。先在已有 run checkpoint 保存 agent 决定，说明发布成功或按同一 marker 查回后，再写入用于 discovery 去重的 evidence；发布失败仅重放已有步骤，不重复 agent 或推送。这个顺序避免增加“等待消息”的队列状态和额外 discovery 门禁。HTTP 结果只表示消息是否送达，不改变 agent 对代码是否 fixed 的决定。

混合旧 summary 与原生评论时，同一条公共回评先保存本轮成功结果，再重新采集新增或编辑的反馈。复用 checkpoint 中 agent 已观察的旧 reviewer round，避免新 round 借用旧决定；发布时仍刷新 fixer 评论以恢复响应丢失后的同一条消息。发现与 recheck 直接读取最新 reviewer round，因此手动循环关闭 autoDiscovery 后也能看到新反馈。没有增加快照字段或消息层；旧协议仍以 round 版本为边界，不扩展支持人工在同一 round 内改写隐藏协议内容。

Forgejo Compare API 只返回 commits/files/total_commits，没有 GitHub 的 status/ahead_by/behind_by。已处理 finding 是否仍包含在当前 head，使用准确 commit 的 Git ancestry 判断，复用冲突检查的对象获取逻辑。代价是本地 Git 查询和必要的对象获取；不能从比较结果的 commit 数量推断祖先关系。浅克隆中如果两个方向都查不到祖先，明确报错，避免把截断历史误判为分叉；先补全本地历史后可继续。

自动合并复用现有策略和 provider dispatch，不新增授权状态或重试账本。授权来自既有 clean review 结果、显式配置及 Looper 范围策略，提交 SHA 仅约束该决定不能作用于漂移后的代码。独立审查发现 Forgejo 16.0.3 的 scheduled merge 会在 `head_commit_id` 校验前返回，后台任务也不保存该 SHA，因此不能靠携带这个字段声称排队合并受保护。删除对服务器合并队列的依赖，使用 `merge_when_checks_succeed=false` 的即时合并与既有 Reviewer publish checkpoint 重试；CI 拒绝后复用已完成的评审，不重新调用 agent。成本是维护 settings / protection 的字段映射，并保留 review request 被审批消费后的同一 checkpoint 恢复路径。默认关闭，不 force merge、不自动删分支，不放宽标签、linked issue / criteria 或自审 COMMENT 限制。依据：[API 的排队与即时分支](https://codeberg.org/forgejo/forgejo/src/tag/v16.0.3/routers/api/v1/repo/pull.go#L1004)、[后台合并执行](https://codeberg.org/forgejo/forgejo/src/tag/v16.0.3/services/automerge/automerge.go#L231)。

## 验收命令

在本次实现分支的工作区构建可执行文件：

```sh
mkdir -p dist
go build -o dist/looper ./cmd/looper
go build -o dist/looperd ./cmd/looperd
```

既有 Forgejo 项目不需要迁移数据库。原生流程使用 `roles.reviewer.behavior.publishMode=single_review`，并启用 reviewer/fixer 的 autoDiscovery。同一账号同时创建和评审 PR 时，需要显式允许 self-review；可以使用配置的 reviewer label 触发，不依赖自请求评审。保留 `threadResolution.enabled=false`。这些设置均可放在目标项目的 `projects[].roles` 下；具体结构见 [配置说明](configuration.md#provider-support)。兼容 `summary_comment` 的旧项目可以继续运行，也使用相同的可见模板。

例如，对已在配置文件中声明的 Forgejo 项目，将以下 `roles` 合并到对应 `projects[]` 条目，可通过 `needs-review` 标签运行同账号 reviewer / fixer。先在仓库创建该标签，并添加到要处理的 PR；保留项目已有的 provider、repo、repoPath 和其他角色设置。不要为 CLI/API 管理的项目另建同 ID 配置条目；项目归属规则见 [项目配置来源](configuration.md#project-authority-and-import)。

```json
{
  "roles": {
    "reviewer": {
      "discovery": {
        "autoDiscovery": true,
        "triggers": {
          "enableSelfReview": true,
          "requireReviewRequest": false,
          "labels": ["needs-review"]
        }
      },
      "behavior": {
        "publishMode": "single_review",
        "loop": {"enabledByDefault": true},
        "threadResolution": {"enabled": false}
      },
      "autoMerge": {"enabled": false}
    },
    "fixer": {
      "autoDiscovery": true,
      "triggers": {"labels": ["needs-review"]}
    }
  }
}
```

修改目标配置后，用新构建的 CLI 做离线校验，再按既有服务部署方式切换到新 `looperd` 二进制。配置若使用 TOML，请替换下面路径；本次实现没有安装二进制或重启日常 daemon。

```sh
./dist/looper config validate --config "$HOME/.looper/config.json"
```

自动合并单独显式开启，仍要求既有 Looper scope / linked issue / acceptance criteria / 分支保护。等待 CI 使用现有有界重试，达到队列重试或连续失败上限后，需要在 CI 就绪后继续或重新检查；不是无限期后台排队。自审降级为 COMMENT 不授予合并权限。

完整源码检查：

```sh
gofmt -l .
go vet ./...
go test ./...
go build ./...
```

Sandbox 使用 `core/looper-sandbox`、独立配置 / 数据库 / worktree / 端口及专属分支和标签。测试结束关闭本轮 PR 并删除本轮测试分支、标签；不合并到 sandbox 默认分支，不操作现有 looperd。远端请求同时校验 HTTP 状态，不能只依赖 tea 退出码。

## 实际验收结果（2026-09-06）

实例版本为 `16.0.3+gitea-1.22.0`，使用 `core/looper-sandbox` 和已有 tea 登录 `powerformer-code`。

| 验收 | 结果 |
| --- | --- |
| [PR #28](https://code.powerformer.net/core/looper-sandbox/pulls/28)：真实 Codex 原生闭环 | Reviewer 找到折扣计算错误 → 自动 Fixer 推送 `151738696dce05c524c861e335db3b8f345d6034` 并回评 → 新 head Reviewer 发布 clean；共 3 次 agent 执行、2 条 native review、1 条 fixer 回评 |
| 同一数据库重启 | 执行数仍为 3，无新增 run、push、review 或修复消息；队列为空闲状态 |
| 修复与消息 | 远端提交通过最初的 3 个 unittest（另存于 agent 工作区外）；comment `2446` 保持未解决，fixer 回评明确写明 `code fixed; comment remains open`；reviewer/fixer 均使用公共 Powered by Looper 署名，无专用 Summary 标题 |
| [PR #26](https://code.powerformer.net/core/looper-sandbox/pulls/26) / [#27](https://code.powerformer.net/core/looper-sandbox/pulls/27)：真实 provider 合约 | 两个角色正确区分真实冲突与干净 PR；当前提交 status failure → success 去重、URL/描述映射通过；脏工作区、refs、index、FETCH_HEAD 保持不变 |
| 受保护的临时 base | CI 失败拒绝合并（405），错误 SHA 和推送后过期 SHA 拒绝合并（409）；正确新 SHA 只合入临时 base，真实 Git 祖先关系验证通过 |
| 清理 | PR #24 / #25 / #26 / #28 已关闭未合并，#27 仅合入临时 base；本轮分支、标签、保护规则全部删除，自有测试进程停止。main 前后均为 `ef6a1f22b225a7ae990d2773b825ffeb072b0dc1` |
| Go 检查 | gofmt、vet、build 与完整 test 通过。并行检查时一条未修改的 worker E2E 触发既有 2 秒本机 HTTP 超时，单独重跑完整测试通过；没有修改阈值或断言 |

验收期间发现并修复了生产代码的时间戳指纹问题。另修正了测试采集器对不分页评论接口的假设，统一按实际 `X-Total-Pages` / `Link` 读取；最终重启校验复用了已成功的三次真实执行，没有额外调用 agent。原始执行器因采集器修正而中断，续验通过后由其原有 finally 路径完成清理；不将该执行器的退出码冒充整段脚本零退出。

本机证据分别保存在 `dist/forgejo-acceptance-20260906-native-v3/`（远端快照、SQLite、原始测试、续验代码/日志、result.json、cleanup.json）和 `dist/forgejo-completion-20260906/`（provider 日志与 Go 检查）。之前的 native 尝试保留在相邻目录，便于核对已修复问题。

真实验收覆盖原生主闭环、commit status、冲突和即时合并接口。503 / 响应丢失 / SQLite 重放、编辑与新 finding、force-push、混合旧协议、外部 reviewer、HITL 和合并授权策略由自动化合约测试覆盖。未声称真实运行 Forgejo Actions workflow；其读取、映射和失败状态通过 provider 合约验证。

仍不支持远端 review-thread resolve、Forgejo coordinator、routed network/webhook 和 GitHub 的同 head decline adjudication。自动合并继续默认关闭、有界重试；不会使用 Forgejo 未保留审核 SHA 的 scheduled merge。

已有 `tea` 登录和 Codex 登录时，可执行隔离的真实 agent 验收：

```sh
python3 scripts/forgejo-review-fix-smoke.py \
  --base-url https://code.powerformer.net \
  --repo core/looper-sandbox \
  --tea-login powerformer-code
```

脚本从当前源码构建二进制，手动发起第一轮原生 reviewer（不使用 force），随后自动执行 fixer 与新 head 的 reviewer，并重启同一隔离数据库验证不会重复推送或发布 review。最后运行远端修复 commit 的 fixture 测试、检查公共署名与可见消息格式，并关闭自身 PR、删除自身分支和标签。真实 Codex 调用使用当前账号，会消耗模型额度。证据保存在 `dist/looper-smoke-<run>/`，包括远端快照、run/checkpoint、fixture 测试结果与 cleanup 结果。

不调用 agent 的真实 provider 合约验收：

```sh
LOOPER_FORGEJO_LIVE_CONTRACTS=1 \
LOOPER_FORGEJO_LIVE_BASE_URL=https://code.powerformer.net \
LOOPER_FORGEJO_LIVE_REPO=core/looper-sandbox \
LOOPER_FORGEJO_LIVE_TEA_LOGIN=powerformer-code \
LOOPER_FORGEJO_LIVE_MERGE=1 \
go test ./internal/runtime -run '^TestForgejoLiveProviderContracts$' -v -count=1 -timeout=15m
```

该测试建立自己的 base 和两个 head 分支，验证真实冲突、当前提交的 status 映射、脏工作区不被改变，以及受保护分支上 CI / SHA 的合并约束。第二个开关才允许修改自有 base 的保护并合并到该分支；省略它则只检查冲突和状态。它验证传输与 adapter 行为，reviewer 的 clean / criteria / scope 授权另由生命周期合约覆盖。清理结果与默认分支前后 SHA 输出到测试日志。
