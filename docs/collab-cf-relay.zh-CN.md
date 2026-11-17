# 独立协作：Plane 配对 + CF 心跳与飞书 inbox

独立协作不再要求加入 loopernet。保留原有 loopernet，供其他 Looper 网络功能和旧配对使用。

同事仍从 Plane「个人设置 → 我的 Looper」复制一次性连接命令，执行后运行 `looper collab start`。连接命令登记本机公钥，不再领取网络邀请，也不要求用 GitHub 身份入网。私钥留在本机。

## 数据流

- Plane 保存用户、设备公钥、项目权限、任务派发和协作记录。
- 本地 Looper 主动轮询 Plane 的签名任务接口，执行 Agent；CF 不存任务内容。
- 飞书回调仍进入现有 CF Worker + D1 inbox；Looper 仍按本机拥有的话题续跑原任务。
- Looper 每 15 秒向 CF 发一次设备签名心跳。Worker 只向部署配置中固定的 Plane 地址转发验证；Plane 检查公钥、签名、时间、一次性 nonce、账号和设备状态，通过后 CF 才记录收到时间。
- Plane 查询 CF 的设备记录，最近 90 秒内的有效心跳视为在线。查询凭证仅保存在 Plane 和 Worker，不下发给本机。
- CF 心跳故障不会中断原任务或阻止本机继续拉取 Plane。Plane 发起新派发仍使用实际在线状态；离线排队沿用原设置。

新配对复用原设备私钥与签名协议；不新增网络身份、入网码、签名根密钥或独立常驻服务。D1 新增的 `devices` 表只存设备 ID、node ID 和收到心跳的时间，身份和撤销权威仍是 Plane。

## 部署

Plane 新增两项服务端设置：

| 设置 | 含义 |
| --- | --- |
| `LOOPER_COLLAB_RELAY_URL` | 已有 CF Worker 的基地址，支持 `/collab` 前缀 |
| `LOOPER_COLLAB_RELAY_TOKEN` | 查询在线状态的服务端 secret，与 Worker 的 `PRESENCE_TOKEN` 相同 |

Worker 在原绑定、回调验证和 inbox 设置上新增：

| 设置 | 含义 |
| --- | --- |
| `PLANE_BASE_URL` | 固定的 Plane origin，不允许由客户端指定 |
| `PRESENCE_TOKEN` | 在线状态查询 secret；与给本地实例的 `POLL_TOKEN` 分开 |

先应用 `deploy/feishu-inbox-worker/migrations/0002_device_presence.sql`，再发布 Worker 和 Plane。它是增量表，不清空原 inbox。不要改动仍承载旧业务的飞书回调根路由。

新 CLI 向连接 exchange 声明 `pairing=device_v1`，支持该方式的 Plane 返回直接配对接口及 relay 地址。旧 CLI 的入网方式保留；新 CLI 对旧 Plane 也保留旧协议回退。新协议的配对无需任何 `LOOPERNET_*` 设置。

## 运行和验收

- `looper collab status` 显示 Plane 是否连接、CF 心跳是否成功；`--json` 的 `plane` 字段提供设备 ID 和两项连通状态。
- 未连接 Plane 时仍可以启动本地协作，状态明确提示到个人设置复制连接命令。首次连接成功后，运行中的 host 在下一次轮询自动加载，无需重启。
- 替换已连接的电脑或改动连接身份时，仍应重启原 host；不把运行任务切换到另一把设备私钥。
- 验收须清空测试环境的 `LOOPERNET_*` 设置，使用全新设备目录，确认没有创建 `network.json`，再验证配对、心跳、在线、派发、执行和 Plane 回写。
- 另检查错误签名、心跳重放、已撤销设备、Worker/Plane 不可达，以及在线时间过期；不得把失败的验证写成新心跳。

本次没有接通 SSO → 正式 Looper 应用的飞书身份映射，也没有改变飞书应用凭证的本地配置。现有 inbox 的共享读取 token 仍用于可信团队实例，它不是按设备隔离的消息权限。不要把这次网络简化描述成“全新同事只输入一个命令即可完成所有授权”。
