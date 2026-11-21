# 将各台 Looper 的执行数据汇总到 Plane

## 结论与代码依据

可行。让 Plane 承担团队范围的接收、权限控制、查询与展示，Looper 保留本地事实和离线看板。当前尚未实现自动上传或 Plane 集中看板；本文是后续实施设计。

核对的 Plane 版本为 `core/plane` 的 `main`，提交 `3af1125180628eed78b43e55f498a4f516e66f12`。这证明代码能力，不代表生产部署状态。

| 已有代码 | 可以复用 | 还缺什么 |
| --- | --- | --- |
| `apps/api/plane/db/models/looper_binding.py`：NodeBinding、NodeKey、NodeSession | 项目、成员、设备归属和签名密钥 | 上传任务历史的授权规则，不依赖仍在执行的 session／lease |
| `apps/api/plane/app/views/looper/dispatch.py`：签名校验、成员检查；`integrations/looper/replay.py`：nonce 消费 | 验证真正有权限的设备请求、防止请求重放 | 独立的遥测批量接收入口、限额、按事件幂等 |
| `apps/api/plane/db/models/looper.py`：Dispatch、Artifact、CollaborationEvent、CollaborationSnapshot | 关联 Plane 任务、派发和交付 | 本地 run／Agent 执行明细、各设备事件、时间与覆盖信息 |
| `apps/api/plane/integrations/looper/dispatch.py`：`append_dispatch_event` | 保留现有业务协作事件 | 该函数同时更新业务阶段快照，不能拿来逐条吞运行埋点 |
| `apps/api/plane/app/views/issue/looper.py` 与 `apps/web/core/components/issues/issue-detail/looper-collaboration/` | 单任务入口、协作摘要、下钻入口 | 项目／工作空间范围聚合接口及集中看板；现有 recent_events 只取最近 3 条协作事件 |
| Looper `runs`、`agent_executions`、`event_logs` 与 `collab trace` | 本机任务、轮次、Agent、时间线和白名单元数据 | 可靠上传游标、队列、重试、服务端确认和覆盖诊断 |

不把遥测写入业务协作事件，也不让迟到的历史事件改变 Plane 的派发状态、等待对象或当前阶段。业务结论仍由 Agent 的结构化结果和现有业务协议负责；指标只记录事实和缺口。

## 最小实现

```mermaid
flowchart LR
    A[各设备本地 SQLite] --> B[独立异步上传器]
    B --> C[Plane 批量接收与去重]
    C --> D[遥测事实表]
    D --> E[项目与工作空间聚合查询]
    E --> F[Looper 效能看板]
    F --> G[原 Plane 任务与执行时间线]
```

第一期采用 Plane 自身 PostgreSQL 保存元数据和事实；按项目／日期索引，设置单批条数及大小限制。已有服务足以承载试点，不另建汇总服务。量增大后，可从事实表投递分析仓库，保持本地事件 ID 与服务端 ID 的映射。

建议新增两类持久数据：追加的遥测事件，以及从事实投影出的任务／轮次摘要。前者解决补传和复算，后者避免看板反复扫描全部 JSON。接收事务内写事件并更新可重算投影；任一步失败整批回滚。先用实时 SQL 聚合验证规模，确有瓶颈再增加每日汇总表。

建议新增的 API（均尚未实现）：

- `POST /api/workspaces/{slug}/projects/{project}/looper/telemetry/batches/`：设备签名提交批次，返回已确认事件与连续游标。
- `GET .../looper/analytics/`：按时间、设备、成员、Agent、仓库筛选的项目指标。
- 工作空间级 analytics 聚合：只汇总当前访问者有权查看的项目。
- 单任务 telemetry 查询：从 Plane 任务下钻到派发、各设备执行轮次及按时间排序的事件。

### 关联、离线与幂等

服务端从已验证的绑定确定 workspace、project、member、node，拒绝客户端伪造租户归属。事件携带 schemaVersion、localEventId、localLoopId、localRunId、executionId、dispatchId、occurredAt；服务端另记 receivedAt、Looper 版本和本地库实例标识。Plane issue 从有权限的 dispatch 反查，不能只凭本机 issueNumber 关联。

事件唯一键为 `(binding_id, database_instance_id, local_event_id)`；一个 Node 会有多个项目绑定，同一库的备份恢复也必须明确是否继续原实例。跨设备的同一派发通过 Plane dispatchId 汇合，运行通过各自设备和本地 runId 区分。任务数按 Plane issue 去重，派发次数另计，不按设备事件数计单。

本地上传队列需要稳定的递增序号和持久确认状态。现有 `created_at` 或临时查询的 rowid 不是正式同步协议，不能直接当可靠游标。可在首次上线时，将符合权限范围的白名单事件幂等入队，周期扫描补漏；业务执行不等待入队或上传成功。发送采用至少一次，服务端唯一键去重；同 ID 不同载荷返回冲突并保留诊断，不能覆盖原事实。

断网或重启从已确认位置续传；失败采用有限退避，拒收与暂时失败分开显示。老任务可以补传已有事实，但不能补造缺失起止或把 tracking_enabled 当成完整率。完整任务仍可能缺事件，需展示采集版本、可用阶段和缺口。

上传复用有效绑定和当前项目成员身份校验、签名和 nonce 协议，但独立于执行租约：任务结束或执行 session 过期后，仍允许具备有效项目权限的设备补传历史。绑定被撤销或成员退出必须拒收。不能复用要求 active session 的派发状态转换接口来传历史。

未绑定 Plane 的本地任务先保留本机；没有明确项目关联与授权时不自动上传。跨仓任务仍由执行 Agent 选择仓库，遥测接收其结构化上报，不能反向变成 Looper 的预选仓路由。

### 数据范围

第一期只传任务关联、状态、轮次起止、必要的人工等待事件、仓库角色与 PR 引用、验证声明、事件覆盖及上传健康。字段按 schema 校验、长度限额和白名单脱敏。

不上传整个 SQLite、宿主文件路径、完整命令／环境、提示词、聊天正文、模型思考或原始日志。需要证据下钻时先显示可用性和权限范围；原文／附件共享另行设计。Agent 上报的验证声明必须保留 source=agent_report，不能渲染为系统独立验证。

## 看板口径

| 维度 | 第一版展示与边界 |
| --- | --- |
| 业务量 | Plane 任务数、派发数、实际执行轮次；续跑与多设备参与不重复计单 |
| 当前分布 | 排队、执行、等待人工、跟进 PR、结束、失败／中断；区分当前状态与历史流入流出 |
| 时长 | 端到端墙钟、排队、单轮 Agent、人工等待；有成对事实才计算，标明样本数与覆盖率 |
| 稳定性 | 失败／中断轮次、后续恢复、需人工介入的任务；人工正常补充不算失败重试 |
| 交付 | 关联 PR 与仓库、Agent 验证声明；执行结束、合并、发布、人工验收分别展示 |
| 成本 | 各 Runtime 真实提供的 token／用量／费用，缺失显示未知；不能用执行耗时推算费用 |
| 设备健康 | 最后收到数据、待传队列量、拒收／版本不支持、迟到事件与采集覆盖；离线不等于任务失败 |

同任务并行 Agent 的累计时长可能大于墙钟时长。跨设备时钟偏差要单独标记；首期优先统计单机配对时长和服务端已知派发时间，不能把 receivedAt 当成实际执行时间。看板需能切换事件发生日期与接收日期以识别补传。

先用这些数据发现等待、返工、采集缺口；不直接做成员排名或声称因果提效。真实验收与统一模型用量尚未接通，应保持“未知”并展示覆盖。

## 顺序与验收

1. Plane：遥测 schema、权限与幂等接收、任务下钻查询。验证两台设备、重复批、乱序、迟到、跨项目拒收、已完成任务补传和撤销绑定拒收。
2. Looper：独立上传队列与游标。验证断网、重启、部分确认、同 ID 冲突、队列写入失败均不重跑原业务任务；主任务优先。
3. Plane：项目看板，再扩展到工作空间。验证同任务多轮／多设备不重复计数、缺数据不填零、筛选与权限一致。
4. 在本地 Plane + 两个隔离 Looper 实例试点，核对本机 trace、服务端事实和聚合结果，再决定部署与默认开启范围。

新增队列、事实表与投影会增加存储、保留期、迁移和对账成本；无法仅靠删除一层解决跨设备离线补传与可重算问题。先只做元数据，不引入对象存储、外部分析库或额外调度平台。
