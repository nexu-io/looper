# Plane → Looper 任务追踪

默认 Feishu Bridge 按真实回调记录每轮执行；传统 Worker 按已有阶段记录。两者都以原任务 ID 关联续跑，不替 Agent 预选仓库。

Looper 已有本地 SQLite 与日志落盘。本改动复用 `event_logs` 补充关键过程事件，不再创建一套任务／运行表。

| Friday 追踪中需要回答的问题 | Looper 数据 |
| --- | --- |
| 哪条 Plane 任务、哪次派发 | `task.received`、`plane.dispatch.received`：Plane issue、dispatch、workspace/project、node；任务主体为 `loops.id` |
| 哪次执行／续跑、哪轮 Agent | `runs` → `agent_executions`，事件含 loopId/runId；统一 correlationId=loopId |
| 卡在哪个阶段、各阶段起止 | `run.started`、`step.started/completed`、`run.ended`；结束事件含执行状态、时长与恢复策略 |
| 等谁回复、消息是否接收 | `human.requested`、`human.answered`、`feishu.receipt`；只记录消息标识与状态，不复制问题／回复正文 |
| 哪些仓库、分支、PR、验证 | `repository.reported`：Agent 在完成标记的 repositories 数组上报；source=agent_report，与独立验证、人工验收区分 |
| 交付产物与原始日志 | `artifacts.reported` 记录产物数量；原始输出及引用保留在对应 execution 的日志文件中 |
| Plane 状态是否回写成功 | `plane.state.synced` 只在远端状态转换成功后记录；失败保留运行错误和服务诊断 |
| 任务状态何时变化 | `task.state_changed`；completed 表示当前执行流程结束，不等于验收、合并或发布 |

## 查看

```sh
looper --config <配置> collab trace 3 --state-dir <协作状态目录>
looper --config <配置> collab trace <loopId或runId> --state-dir <协作状态目录> --json
```

命令直接以只读方式读取状态目录中的 `looper.sqlite`，无需启动服务，不执行迁移，也不创建缺失的库。JSON 含任务关联、全部运行／Agent 执行、时间线和原始日志路径；按 runId 查询时只保留该轮及任务级事件。

新增事件使用稳定 ID 防止重复观察写出重复事实；已有事件不可被后来的同 ID 载荷覆盖。新增追踪字段不包含任务正文、回复正文、环境变量、完整命令或模型思考内容。旧版 `agent.invoked` 原始事件含 argv，trace 导出仅取允许的元数据字段，原始私有库／日志仍应按内部数据保管。

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
