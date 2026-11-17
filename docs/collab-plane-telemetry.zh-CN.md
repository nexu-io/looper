# 上传协作任务元数据到 Plane

Plane 需同时部署 `core/plane` 的 Looper telemetry 接收端及迁移 0159；旧版本不会接受新接口。

## 使用

先用现有 `looper plane connect` 建立项目与设备绑定，运行新版 `looper collab start`，使独立 SQLite 数据库完成迁移 0023。

一次同步（不需要停止协作服务，也不会重新执行任务）：

```sh
looper collab sync --json
# 自定义状态目录时，增加 --state-dir <原目录>
```

持续同步：将对应 Plane provider 的 `strictDispatch.telemetryEnabled` 设为 `true`，重启 collab。默认关闭；开启后启动时及每分钟尝试一次。原生 Bridge 与旧 worker 两种独立 collab 服务均使用同一上传器。

每个 provider 只上传其配置项目中有 `strictDispatchId` 的任务。未关联 Plane 的个人本地任务不自动上传；不会因仓库相同扩大项目范围。身份及设备归属由 Plane 验证原有节点绑定，不接受客户端自报成员。

## 数据和重试

- 上传任务名称、状态、派发 ID、时间、运行和 Agent 次数、Agent 类型、白名单事件与仓库引用；不上传 prompt、argv、原始日志、聊天正文、环境变量或本地日志路径。
- SQLite 保存稳定 database ID，以及每个目标／任务的版本、待上传快照和确认状态。先保存后发请求；重试使用同一版本原文，收到匹配 ACK 才推进。目标 origin、workspace、project、binding 变化时使用独立确认记录。
- Plane 按 binding + database ID + local task ID 去重；旧快照不回退任务状态，重复事件不重复统计，冲突返回 409。上传失败仅记警告／命令错误，既不重新派发也不重跑 Agent。
- 使用已有 Ed25519 节点签名和 nonce 防重放，不申请执行 session／lease。有效绑定可补传已完成任务；撤销绑定或移除成员后拒绝上传。
- 单任务快照最大 1 MiB。超限保留本地并报告错误，不截断事件后宣称成功；需要更大历史量时再增加分批事件协议。

## 人的协作

一期只保存来源、用途分类、参与者来源 ID、原消息 ID、时间和定位链接。普通消息默认 `discussion`；协议还接受 `information`、`decision`、`review`、`acceptance`，分类不驱动任务状态／验收权限。

原生飞书 Bridge 在既有去重、@策略和访问校验通过后，发送独立的无正文观察回调；只有本机已有任务的话题可以落盘。真实来源时间与发送者来自 SDK。可定位链接使用飞书返回的消息位置；缺少位置时显示未提供链接，不编造 URL。回调／查询失败只记录错误，不阻塞人工命令的执行或延迟原有收到回执。

Plane 页面直接读取现有评论的 actor、ID 和时间，不复制评论正文。评论所属任务按原 Plane issue 去重。外部来源 ID 暂不自动映射为 Plane 成员，不算到设备主人名下。

当前自动来源为 Plane 评论与原生飞书 Bridge 已接收的任务消息。旧 worker 只有历史 reply ID 时不补猜参与者；GitHub 全量评论抓取、自动语义分类和跨平台账号映射尚未实现。

## 看板口径

Plane 工作空间侧栏的 Looper 页面分别展示团队／本人设备、执行任务、运行、Agent 累计观测用时、人工协作和单任务时间线。个人执行按绑定所有者统计，个人协作只按已确认的 Plane actor 统计。

原始 Plane work item、派发、Looper 执行任务、run、Agent 与消息使用不同分母；评论不会增加执行次数。设备按成员 + node ID 去重，不从上传时间猜测在线。`completed` 是执行状态，不代表人工验收、PR 合并或发布。Token、成本和未观测到的时间保持未知；不把缺失数据填成零。

页面遵守现有 WorkspaceMember + ProjectMember 的读取权限。遥测数据只是执行观察，不改变 Plane 派发、角色、协作快照或验收的业务权威。

配套 Plane 基线为 `feat/looper-task-context`（0158）；本轮遥测迁移为 0159。上游 Agent 工作摘要回写使用独立业务接口，遥测上传器不读取 outcome 的 Markdown 或原评论正文。
