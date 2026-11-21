# Plane × Looper 本地部署与派活

这套流程让每位研发只把任务派给自己电脑上的 Looper。Plane 保存唯一执行归属；飞书只负责通知，不读取飞书回复。

## 研发首次接入

1. 安装同一版本的 Looper，登录团队指定的 coding agent、`gh` 和 `plane` CLI，并加入团队 loopernet。
2. 在本机打开该项目的 GitHub checkout；若命令不在仓库内执行，准备好 `--project-path` 和 `--code-repo owner/repo`。
3. 打开任一 Plane work item 的 Looper 面板，点击“连接我的 Looper”，复制页面给出的一次性命令并在本机执行：

   ```bash
   looper plane connect <plane-url> --code <10-minute-code>
   ```

   如果当前配置还没有这个 Plane 项目，命令会从一次性会话读取 workspace/project，从当前 Git origin 识别代码仓库，并幂等补齐 provider、project 与项目级触发规则。也可以显式传入：

   ```bash
   looper plane connect <plane-url> --code <code> \
     --project-path /path/to/repo --code-repo owner/repo
   ```

   命令会核对当前 Plane 登录身份和项目、本机 Node 与 loopernet challenge，在本机生成 `0600` Ed25519 私钥，自动启用 strict dispatch、重启 daemon，并等待签名 inbox 建连。私钥不会上传。
4. 执行 `looper plane doctor`。它一次检查本地仓库、Git/gh、coding agent、Plane CLI/API key、loopernet、私钥、签名 inbox 和 daemon；全部为 `✓` 才算接入完成。
5. Plane 页面显示六项连接检查全部通过后完成。无需 Project Admin 审批个人电脑；V1 每个 Plane 用户在一个项目只允许一台电脑，已有设备时不会自动替换。

## Project Admin 首次启用

进入 `项目设置 → Looper`，依次选择产品、设计、QA 负责人并保存。研发负责人不单独配置，始终等于该 work item 的 Looper owner。至少一名研发完成自助连接且 Node 在线后，在同一页点击“启用项目协作”。

只有 Project Admin 能看到这个设置入口；普通成员从具体 work item 连接自己的电脑。Project Admin 只负责项目级角色和集成开关，不审批成员自己的电脑。任一已连接 Node 离线、未上报 strict capability 或角色成员不在项目中，激活会 fail closed。

旧服务器仍可用 `looper plane setup ... --checklist-revision 1` 兼容接入，但它不再是推荐路径。

## 日常派活

打开 Plane work item 的 Looper 面板，确认卡片底部显示正确的 owner、Node 和在线状态，然后点击“派给我的 Looper”。V1 不提供跨 owner 目标选择，也不允许管理员替别人派活。

自动模式依次执行：需求调研 → 多角色决策 → 中文技术方案 → owner 审批 → Worker 实现 → PR/QA。产品与设计问题在 Plane 对应入口回答；飞书仅发通知。较大的产品需求会要求正式 product spec，小问题允许一句快速决策。

## 停止与恢复

Plane 的“请求停止”不是远程强杀。运行中的 agent 仍由原 owner 本地停止并由原 Node 确认进程已经退出；Node 丢失时保持等待，不自动改派。确认停止后，owner 填写原因并 release，其他研发才可把同一 work item 派给自己的 Looper。

loopernet 暂时不可用时，Plane 仍展示持久化的 owner、阶段和产物，只把实时在线状态降级为 `unavailable`。
