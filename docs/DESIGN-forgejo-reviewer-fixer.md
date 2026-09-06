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

优先删除“无法 resolve 就不允许修复”的能力门槛。已有 checkpoint 与 fixer evidence 可承载本地处理结果，不引入第二套 receipt / ledger 或数据库迁移。新增本地处理结果如果需要状态值，必须明确表示“代码已处理、远端仍未关闭”，并在发现、重试与展示路径保持同一含义；这是避免无 resolve 平台重复修复的必要成本。不能仅忽略远端未解决评论，否则新评论、修改后的评论和 deferred 结果会丢失。

## 验收命令

```sh
gofmt -l .
go vet ./...
go test ./...
go build ./...
```

Sandbox 使用 `core/looper-sandbox`、独立配置 / 数据库 / worktree / 端口及专属分支和标签。测试结束关闭本轮 PR 并删除本轮测试分支、标签；不合并到 sandbox 默认分支，不操作现有 looperd。远端请求同时校验 HTTP 状态，不能只依赖 tea 退出码。

实际验证结果与最终配置步骤在验收阶段补齐。
