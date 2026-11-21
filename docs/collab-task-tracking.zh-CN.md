# Plane → Looper 任务追踪

默认 Feishu Bridge 按真实回调记录每轮执行；传统 Worker 按已有阶段记录。两者都以原任务 ID 关联续跑，不替 Agent 预选仓库。

Looper 已有本地 SQLite 与日志落盘。本改动复用 `event_logs` 补充关键过程事件，不再创建一套任务／运行表。

| Friday 追踪中需要回答的问题 | Looper 数据 |
| --- | --- |
| 哪条 Plane 任务、哪次派发 | `task.received`／Bridge 的 `plane.dispatch.received` 关联 dispatchId、issueNumber、本地 projectId；任务主体为 `loops.id`。workspace、Plane project、node 当前依赖配置，尚未固化为每次执行快照 |
| 哪次执行／续跑、哪轮 Agent | `runs` → `agent_executions`，事件含 loopId/runId；统一 correlationId=loopId |
| 卡在哪个阶段、各阶段起止 | Worker 记录 `run.started`、`step.started/completed`、`run.ended` 和恢复策略；Bridge 记录真实 turn 起止。耗时由各自运行起止计算，不虚构 Bridge 内部阶段 |
| 等谁回复、消息是否接收 | Worker 记录人工问答与 `feishu.receipt`；Bridge 记录人工等待及后续执行，SDK 消息收据尚未统一采集。事件不复制问题／回复正文 |
| 哪些仓库、分支、PR、验证 | `repository.reported`：Agent 在完成标记的 repositories 数组上报；source=agent_report，与独立验证、人工验收区分 |
| 交付产物与原始日志 | `artifacts.reported`：Worker 计完成标记中的 artifacts，Bridge 计 PR 引用；两种数量不能直接比较。Go executor 有本地捕获输出；Bridge 尚未导出 SDK 原生会话日志 |
| Plane 状态是否回写成功 | `plane.state.synced` 只在远端状态转换成功后记录；原生 Bridge 用 `diagnostic.plane.sync.failed` 记录状态回写及带时间线事件的上下文写入失败，用 `diagnostic.plane.context.synced` 记录上下文事件得到确认 |
| 任务状态何时变化 | `task.state_changed`；completed 表示当前执行流程结束，不等于验收、合并或发布 |

## 查看

```sh
looper --config <配置> collab trace 3 --state-dir <协作状态目录>
looper --config <配置> collab trace <loopId或runId> --state-dir <协作状态目录> --json
```

命令直接以只读方式读取状态目录中的 `looper.sqlite`，无需启动服务，不执行迁移，也不创建缺失的库。JSON 含任务关联、全部运行／Agent 执行与时间线；已有 Go executor 输出记录时附日志路径，Bridge 日志路径为空；按 runId 查询时只保留该轮及任务级事件。

新增事件使用稳定 ID 防止重复观察写出重复事实；已有事件不可被后来的同 ID 载荷覆盖。新增追踪字段不包含任务正文、回复正文、环境变量、完整命令或模型思考内容。旧版 `agent.invoked` 原始事件含 argv，trace 导出仅取允许的元数据字段，原始私有库／日志仍应按内部数据保管。

## 导出单个 Plane 任务的排障包

```sh
looper collab export OPEND-123 --output ./OPEND-123-diagnostics.zip
```

有独立协作状态目录时，传入原来的 `--state-dir`。同一任务编号对应多个 Plane 任务，或者旧派发只存了 UUID 时，用完整任务链接代替编号。命令离线读取同一个 SQLite 快照，按保存的 Plane 任务链接匹配本地派发，包含本机保留的多次派发、重试和续跑。输出路径须指向一个新文件。

- `summary.md` 列出每次派发的状态、最后确认的步骤，以及之后尚未观察到成功确认的 Plane 回写错误。
- `trace.json` 包含所有匹配派发、运行轮次和已采集事件及其覆盖情况。
- `diagnostics.json` 包含可安全分享的结构化错误，例如 Agent 中断、结论缺失及 Plane 写入的 HTTP 状态。

其他设备的历史、缺少任务链接的旧派发和此前未采集的事件可能缺失。原始会话、stdout、stderr、任务和聊天正文保留在原执行设备。结构化错误只提供已记录的状态和分类。

## 边界与代价

- 复用已有状态和事件表；增加一个按 task/time 查询的索引。新增关键事件写入增加少量本地写负担，没有外部服务依赖。
- 事件是对已观察状态的追踪，不参与任务授权或完成裁决；Agent 的结构化仓库上报是其决策与交付声明，不添加另一层推断门禁。
- 追踪写入失败记录诊断，不回放已接收任务或远端动作。状态变更与追踪写入之间若崩溃，可能缺少相应事件；这不是一个事务性审计账本，当前状态仍以执行表／远端事实为准。
- `tracking_enabled` 表示存在接单追踪起点；`partial_legacy` 表示缺少起点，可能是旧任务或写入缺口，不承诺全部事件完整。历史阶段不伪造补齐。
- Bridge 缺少启动回调时不虚构一次执行；进程恢复时，未收到结束回调的旧执行标记为 interrupted。旧版预构建 Bridge 缺少 turnId，无法保证回调重放去重。
- 当前范围为本机落盘与查询，没有自动接入 Friday 的服务端采集、PG／对象存储、跨设备看板或归档保留期。模型 token／费用尚无统一采集，返回 null，不能当作零消耗。
- 未实现业务验收事实或多 PR 合并／发布事实；飞书 Get 表情只表示收到消息，Agent 报告 passed 也不能替代人工验收。

## 本地只读看板

```sh
looper --config <配置> collab dashboard --state-dir <协作状态目录> --port 18090
```

浏览 `http://127.0.0.1:18090`：最近 500 个 Worker 业务任务、状态、运行／Agent 轮次、有起止记录的 Agent 累计耗时、追踪覆盖及可下载的任务时间线。页面每 5 秒刷新；按上海时区展示日期。可以离线查看已有数据库，不要求 collab 服务运行。

只绑定本机回环地址，拒绝非本机 Host 和写请求，不执行数据库迁移，不开放原始日志／配置／提示词。未知耗时保留 null；累计 Agent 时长只统计已有完整起止的执行，不等于任务墙钟时间或计费时间。该命令须使用本机已有且已迁移的状态库。
