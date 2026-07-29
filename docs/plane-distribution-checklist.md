# Plane × Looper 同事分发检查表

每次准备把新版本分发给同事前，用一台没有 Looper 历史配置的 Mac 验证。不要用开发机现成的 `~/.looper` 代替此门禁。

## 发布方

- Looper 与 Plane PR 已合并，并发布同一个明确版本；Plane 连接页给出的 CLI 参数与该版本一致。
- `scripts/verify-plane-distribution.sh` 通过。
- Plane `项目设置 → Looper` 可保存产品、设计、QA 负责人；研发规则固定显示为当前任务 Looper owner。
- 至少一个 active Node 在线后可启用项目协作；缺角色、Node 离线或版本过旧时必须拒绝启用并给出明确原因。

## 新同事机器

1. 安装并登录 coding agent、Git、`gh` 与 `plane` CLI。
2. clone 代码仓库。
3. 安装 Looper，加入团队 loopernet。
4. 在 Plane work item 点击“连接我的 Looper”，在 checkout 内粘贴一次性命令。
5. 运行 `looper plane doctor`，确认 config、repository、GitHub auth、Plane auth、loopernet、Node binding、signed inbox、daemon 全部通过。
6. 给自己的一个测试 work item 派发任务，观察 `等待领取 → 队列中第 N 位 → 运行 → 等待决策/PR/QA → 完成`，并确认刷新页面后状态与产物链接仍在。
7. 停掉本机 daemon，确认 Plane 显示离线且任务保持原 owner 等待恢复；重新启动后继续，不被其他人的 Looper 领取。

## 仍需外部准备的凭据

浏览器连接已经自动补齐 Plane workspace/project/provider，但当前版本仍需要三类本机凭据：coding agent 登录、GitHub CLI 登录、Plane API key/CLI 登录。它们不写入项目配置，也不能由团队共享。`looper plane doctor` 会逐项指出缺失项。
