# 独立协作：Plane 配对、心跳与 CF 飞书 inbox

独立协作不再要求加入 loopernet。旧配对和其他 Looper 网络功能保留原路径。

同事从 Plane「个人设置 → 我的 Looper」复制一次性连接命令，执行后运行 `looper collab start`。连接登记本机公钥，私钥留在本机，无需额外入网码。

## 数据流

```text
本机 Looper → Plane：设备签名、心跳、领取任务、回写摘要和时间线
飞书 → CF inbox ← 本机 Looper：接收回调、轮询并续跑原话题
```

本机每 15 秒直接向已配对的 Plane 发一次签名心跳。Plane 验证设备公钥、请求签名、时间、一次性 nonce、账号及撤销状态后，在原设备记录保存服务端收到时间。最近 90 秒内的有效心跳视为在线；从未收到心跳的设备为离线。个人设置、连接检查、任务面板与派发读取同一份记录。

CF 只做飞书消息中转，不主动访问 Plane。心跳不经过 CF，也不需要给 CF 开通公司网络入口。心跳失败不会中断正在执行的任务；新派发沿用原在线检查和离线排队设置。在线只代表设备近期可达，不代表任务已经完成或通过验收。

直接配对的 node ID 由 Plane 分配为 `collab-<连接 UUID>`；旧 CLI 设备仍从 loopernet 读取在线状态。新协议复用原设备私钥与签名协议，不增加网络身份、共享心跳密钥或独立服务。

## 部署与升级

先部署 Plane 并应用 `0160_looper_device_heartbeat` 迁移，再升级本机 Looper、重启 `looper collab start`。无需重新配对；已有设备私钥与绑定继续使用。

CF Worker 删除了 `/devices/heartbeat` 与 `/devices/<id>`，保留飞书回调、`/events` 和 `/healthz`。先升级本机，再发布 Worker，避免旧版客户端继续调用已删除的 CF 心跳接口。不要改动仍承载旧业务的根路由。

原先的 `LOOPER_COLLAB_RELAY_URL`、`LOOPER_COLLAB_RELAY_TOKEN`、`PLANE_BASE_URL`、`PRESENCE_TOKEN` 均不再使用，可从各自部署配置中移除。历史 D1 `devices` 表无需迁移或清理，新版不再读写它；原 inbox 不受影响。新部署不需要应用 `0002_device_presence.sql`。

## 运行与验收

- `looper collab status` 显示 Plane 连接与设备心跳；`--json` 的 `plane.heartbeatReachable` 表示最近一次心跳是否得到确认，替代旧 `relayReachable`。
- `looper plane doctor` 检查同一状态。不连接 CF 心跳服务，不要求任何 `LOOPERNET_*` 设置。
- 未连接 Plane 也能启动本地协作。首次连接后运行中的 host 自动加载绑定；替换已有设备身份仍需重启，避免切换执行中的私钥。
- 验证错误签名、篡改、重放、停用账号及已撤销设备不能刷新心跳；断网超过有效期后显示离线，恢复后重新在线。
- 验证 CF 中断不影响设备心跳和领取任务，但飞书回复需要 inbox 恢复后再处理。

SSO → 正式 Looper 应用的飞书身份映射及同事的本地 bot 凭证配置仍需完善。现有 inbox 共享读取 token 面向可信团队实例，不是逐设备的消息权限；不能据此宣称全新同事输入一个命令便完成全部授权。
