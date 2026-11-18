# Plane 任务记忆

本功能依赖 Plane 的 `0160_looper_memory` 和记忆 API。记忆目录由 Plane project、module 和来源 item 组织；飞书保存可编辑原文，Plane 保存人工发布时的不可变正文快照。Agent 每次开始或续跑时获得适用目录，再自行选择原文；Looper 不替 Agent 预选仓库或记忆。

## 开启

在已完成设备绑定的 Plane provider 中设置 `strictDispatch.memoryEnabled: true`，重启 `looper collab`。默认关闭，和 `telemetryEnabled` 分开控制。开启后会上传记忆取用观察，并为新完成的任务运行独立回溯 Agent，产生额外模型用量。

```json
{
  "strictDispatch": {
    "enabled": true,
    "memoryEnabled": true,
    "baseURL": "https://plane.example.com",
    "nodeId": "existing-node-id",
    "bindingId": "existing-binding-id",
    "keyRevision": 1,
    "privateKeyFile": "/absolute/path/to/existing-private-key"
  }
}
```

保留已有连接字段；示例仅展示相关配置。Plane 服务端还需配置飞书应用和项目专用文件夹，详见配套 Plane 的 `docs/integrations/looper-memory.md`。不开启记忆不影响原有任务执行。

## Agent 如何使用

原生 Codex Bridge 注册 `looper_memory` stdio MCP，只开放 `list_for_item`、`get`、`feedback`、`propose` 四个工具。任务目录包含 task 标识和用法；工具通过本机私有 Unix socket 访问宿主。Agent 不获得 Plane 签名密钥或飞书凭据，也没有发布、归档和修改业务任务的工具。仅这四个工具配置逐工具批准，不修改全局沙箱或全局批准策略。

- `list_for_item`：当前 item module 和项目共用索引；用 cursor 翻页，用 `include_project_modules: true` 查同项目其他 module。
- `get`：按 memory ID、发布 revision、cursor 读取快照；同次分段读取可共用 `logical_read_id`。
- `feedback`：声明采用、帮助、不适用或错误，写明用途及依据来自目录还是正文。声明采用不等于质量提升已被证实。
- `propose`：提交候选经验，人工发布后才进入后续任务目录。

目录中的资料不具备系统指令权限，不能覆盖当前规范。每次续跑重新获取目录，避免沿用旧权限或旧发布状态。目录分页和正文读取都会检查当前项目绑定及原文访问能力；服务不可用明确报告，不推断为“没有记忆”。

兼容命令随目录给出：`python3 <workspace>/memory.py --socket <socket> --task <task> <action> --input <json-file>`。原生 Codex 的 MCP 路径经过真实模型验收；其他执行器仍需验证其工具或本地命令接入，不能据此声称全部执行器自动支持。

## 回溯与维护

仅为开启功能后、已观测记忆目录且明确完成的新任务排队；旧的已完成任务不会批量补跑。回溯使用独立、临时 Codex HOME，复制现有模型登录态后删除，禁用 shell、插件和网络搜索，不继承业务工具、飞书/Plane 密钥或任务写权限。输入包含有界的任务摘要、产物引用和可访问记忆，输出候选经验及维护建议，不自动将其发布成规范。

维护扫描覆盖原文漂移、到期复核及负反馈，独立于业务任务。每台设备串行处理回溯；Plane 负责领取、过期重领、尝试上限及完成幂等。真实 token 来自模型结果，未知费用保持未知。已知密钥值在回溯证据上传前脱敏；任务内容本身会发送给现有 Plane 服务和配置的模型供应商，开启前应确认项目范围。

## 数据与排障

SQLite 迁移 `0024_memory_operations` 保存取用完成、索引提供和回溯结果的待上传队列；失败重传只补观察，不重跑业务。事件 ID 幂等，服务端 ACK 后保留上传状态。数据位于现有 Looper 数据库，沿用其备份和访问权限。

本地协作看板 `/api/memory` 展示最近 500 条元数据事件、待上传数量和错误，不展示正文；任务数是本机 loop 数。集中看板 `/:workspace/looper/memories` 按原始 Plane item 去重业务任务，保留设备/dispatch/run 作为追踪维度，两者口径不同。

排障顺序：确认 `memoryEnabled`、Plane 配套版本、绑定有效、项目文件夹授权；查看本地 pending/error，再查 Plane 记忆事件和维护任务。关闭功能停止后续上传/回溯，不删除已保存的数据。
