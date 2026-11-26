# ADR-001 · Plane → loopernet 信任与部署边界

- 状态：**Accepted for implementation（2026-07-21）— 生产启用仍需最终人工验收**
- 关联：OPEND-1645
- 范围：S2a/S2b 开始前的承载模块、网络、凭据、nonce、迁移与轮换决定

## 决策

### 1. 承载模块与调用方向

Looper integration 放在 Plane API 的独立 domain 模块中，由 Plane Web 只访问同源 Plane API。浏览器不直接访问 loopernet，也不持有 loopernet service credential、Node public key 管理凭据或签名验证配置。

调用方向固定为：

```text
Plane Web → Plane API → loopernet HTTPS API
local looperd → Plane signed integration API
local looperd ↔ loopernet challenge / heartbeat API
```

loopernet 只提供短时 signed challenge 与在线观测，不回写 `LooperDispatch`，也不成为执行归属 Authority。Plane 到 loopernet 的网络故障只能让在线状态降级或阻止新绑定/新派发，不能释放、改派或覆盖已有 dispatch。

### 2. 网络与服务身份

- Plane server 通过每个环境独立配置的 HTTPS base URL 访问 loopernet；生产环境必须限制 DNS/egress，并验证标准 TLS hostname。
- Plane → loopernet 使用独立的只读/挑战用途 service credential；credential 不能调用 Plane，也不能代表 Node 签名。
- 禁止浏览器传入任意 loopernet URL；base URL 只能来自服务端配置。
- 请求设置短超时、有限重试与熔断；读在线状态失败返回 `unavailable`，创建 binding 或 dispatch 的资格校验失败则 fail closed。
- loopernet 不得主动连接 Plane 数据库。若未来需要 webhook，另立 ADR，不复用本协议的信任假设。

### 3. Trust root 与 secret 注入

- 每个环境维护独立的 loopernet trust-root key ring，条目至少包含 `key_revision`、算法、public key、`not_before`、`not_after` 和状态。
- key ring 与 Plane → loopernet service credential 由部署环境的 secret manager 以只读文件或等价 secret mount 注入；不写入数据库迁移、镜像、前端环境变量或仓库。
- Plane 启动时严格解析配置；重复 revision、未知算法、失效 key、空 key ring 均禁止开启 binding/dispatch feature flag，但不阻止 S1 只读面板启动。
- Plane 只接受技术 Spec 规定的 pure Ed25519 envelope；Ed25519ph、算法降级和没有 key revision 的签名全部拒绝。

### 4. Nonce / replay store

签名请求的 replay record 使用 Plane 主 PostgreSQL，而不是 Redis。原因是 nonce 消费必须与 claim、transition、binding 创建等业务 mutation 在同一数据库事务提交；Redis 无法提供这条跨存储原子性。

- 唯一键：`(binding_id, key_revision, nonce)`；link challenge 使用等价的 challenge ID/nonce 唯一键。
- 保存 `seen_at` 与 `expires_at`，默认保留 10 分钟；后台清理只删除已过期记录。
- 写操作先验证时间窗和签名，再在业务事务内插入 nonce；唯一冲突即 replay，整笔事务失败。
- signed inbox 虽为读取，也必须在短事务中消费 nonce 后返回；不得用缓存命中绕过 replay 检查。
- 数据库不可用时所有 signed endpoint fail closed，不能降级到内存或 Redis nonce。

### 5. 迁移与启用顺序

1. 部署 S1 additive schema 与只读面板；不创建 dispatch，不改变 legacy discovery。
2. 提交 Python/Go 共用的 golden vectors，并让 Plane、Looper、loopernet 三方测试同时通过。
3. 部署 nonce、trust-root、Node binding、project role policy 与 protocol classification 的 additive schema；所有 mutation feature flag 保持关闭。
4. 为目标环境注入独立 trust roots/service credential，验证 Plane → loopernet 网络和时钟监控。
5. 升级项目内全部 Looper daemon，停掉旧进程/启动项并完成凭据治理；未完成时项目保持 read-only。
6. owner 从 Plane 页面自助完成单台 Node binding；Project Admin 只签收项目角色策略、Node 清单和 activation checklist，不审批个人设备。
7. 小范围开启 strict epoch 与 self-dispatch；epoch 前任务永久为 legacy，epoch 后新任务在创建事务内分类 `strict_v1`。
8. 通过真实双 Node、旧 daemon 阳性对照、断网/重放/进程崩溃 E2E 后再扩大灰度。

所有迁移均为 additive；关闭开关只停止新派发，不删除 strict 分类，不把在途 dispatch 降级成 legacy。

### 6. 多环境密钥轮换

- dev、staging、production 使用互不相同的 trust roots、service credential 与 Node binding key，不允许跨环境复用。
- loopernet 先发布新 trust-root revision；Plane key ring 同时加入新 revision，确认可验证后，loopernet 才切换为只用新 revision 签发 challenge。
- 旧 trust root 只在其 `not_after` 前验证切换前已签发且仍未过期的 challenge，不继续签发，也不提供无期限 grace。
- Node binding key 轮换必须重新走 Plane owner 身份 + loopernet challenge。旧 key 不得创建新 claim；仅可按技术 Spec 对原 active attempt 提交 recovery-only `stop_ack`。
- 每次轮换记录环境、旧/新 revision、操作者、开始/结束时间与回滚条件；回滚只能恢复验证旧的短时 challenge，不能恢复旧 key 的新执行权限。

## 结果与限制

- 获得了可事务化的 replay 防护和清晰的单一 Authority；代价是 signed read 也依赖 PostgreSQL 写可用性。
- Plane Web 不感知 loopernet credential，降低了浏览器泄露与任意目标访问风险。
- 本 ADR 接受前，S2 只能编写 golden-vector fixture 和测试脚手架；不得实现或开启 binding、dispatch、claim、transition mutation。

## 最终生产验收项

- [ ] Plane integration domain 的最终包路径与维护人
- [ ] 各环境 Plane → loopernet base URL、egress 与 TLS 方案
- [ ] secret manager 注入形式及 credential 最小权限
- [ ] PostgreSQL nonce 容量、清理任务与告警阈值
- [ ] staging/production trust-root 轮换 runbook 与负责人
- [x] 已获项目 owner 授权继续实现并由 agent 承担中间验证（2026-07-21）
- [ ] 最终验收人确认后才允许打开 production feature flag
