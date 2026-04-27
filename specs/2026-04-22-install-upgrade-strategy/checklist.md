# Install / Upgrade Strategy Checklist

## Phase 0 - 锁定用户模型与支持边界

- [ ] 明确 Looper 的用户安装入口是 `looper`，不是分别安装 CLI / daemon
- [ ] 明确 release 渠道只面向用户直接分发 `looper`
- [ ] 明确 `looperd` 继续由 CLI 负责安装与升级编排
- [ ] 明确当前主支持矩阵仍是 macOS `darwin-arm64`
- [ ] 明确 `go install` 仅为开发者路径，不作为普通用户推荐安装方式
- [ ] 明确 `foreground` 仍是当前受支持默认 daemon 模式
- [ ] 明确 `launchd` 作为下一阶段正式托管模式演进方向
- [ ] 明确最低支持的 macOS 版本
- [ ] 明确安装脚本的长期稳定 URL 策略
- [ ] 明确 manifest 的发现地址与托管方式
- [ ] 明确签名方案采用 `minisign` 还是 `cosign`
- [ ] 明确更新 channel 是一次性 flag 还是持久化配置
- [ ] 明确 telemetry 策略（推荐无 telemetry）
- [ ] 明确 Product Version / API Version / Schema Version / Build Metadata 的分层模型
- [ ] 明确 GitHub Release 是正式产品版本发布载体
- [ ] 明确 pre-1.0 阶段的 SemVer 规则（MINOR 可 break，PATCH 不可伪装 break）
- [ ] 明确 `--channel` 在 v1 中默认是一次性 flag，不持久化
- [ ] 明确源码默认版本切换为 `0.0.0-dev` 的迁移方案

## Phase 1 - 补齐用户可达的安装入口

- [ ] 新增 `scripts/install.sh`
- [ ] 安装脚本支持架构检测
- [ ] 安装脚本支持下载匹配的 `looper` artifact
- [ ] 安装脚本支持 checksum 校验
- [ ] 安装脚本默认安装到用户可写 PATH（且保证用户安装后可直接执行）
- [ ] 若目标 PATH 不可见，安装脚本经确认后更新 shell profile 或给出替代路径
- [ ] 安装脚本安装完成后输出清晰 next steps
- [ ] 新增 `scripts/uninstall.sh`
- [ ] README 将安装脚本设为默认推荐路径
- [ ] README 保留 GitHub Releases 手动安装 fallback

## Phase 2 - 新增 first-run bootstrap

- [ ] 设计 `looper bootstrap`
- [ ] `bootstrap` 支持 preflight 检查（平台、架构、`git`、`gh`）
- [ ] `bootstrap` 支持初始化 `~/.looper/` 目录结构
- [ ] `bootstrap` 在 config 缺失时支持生成最小 `~/.looper/config.json`
- [ ] `bootstrap` 支持最小 project 注册或跳过逻辑
- [ ] `bootstrap` 支持交互式选择 `agent.vendor`
- [ ] `bootstrap` 支持配置 `notifications.osascript.enabled`
- [ ] `bootstrap` 支持可选启用 `server.authMode=local-token`
- [ ] `bootstrap` 自动执行 `looper daemon install`
- [ ] `bootstrap` 自动执行 `looper daemon start`
- [ ] `bootstrap` 自动做 health check 并输出安装摘要
- [ ] `bootstrap` 支持 `--yes` 非交互模式
- [ ] `bootstrap` 具备幂等行为
- [ ] `bootstrap` 在依赖缺失时快速失败并输出人工安装指引
- [ ] `bootstrap` 不预创建 daemon 专属 runtime 目录的未来可变布局
- [ ] `bootstrap` 明确部分失败后的重入语义（已装未启 / 已配未装 / 已装启动失败）

## Phase 3 - 正式定义 Release Contract

- [ ] 冻结 `looper-<os>-<arch>` / `looperd-<os>-<arch>` 命名约定
- [ ] 冻结 `<asset>.sha256` sidecar 命名约定
- [ ] 冻结 managed daemon 安装路径 `~/.looper/bin/looperd`
- [ ] 冻结 daemon lookup 顺序 `~/.looper/bin/looperd` → `$PATH`
- [ ] 为每个 release 生成 `manifest.json`
- [ ] 为每个 release 生成 `manifest.json.minisig`
- [ ] 在 manifest 中声明 `manifestVersion`
- [ ] 在 manifest 中声明 `version` 与 `tag`
- [ ] 在 manifest 中声明 `channel`
- [ ] 在 manifest 中声明 `apiVersion`
- [ ] 在 manifest 中声明 `schemaVersion`
- [ ] 在 manifest 中声明 `minCliForDaemon` / `minDaemonForCli`
- [ ] 在 manifest 中声明完整 artifact URL / sha256 / size
- [ ] 为 GitHub Release body 补齐固定的 Compatibility / Upgrade Notes 区块
- [ ] 明确 manifest 是机器权威真相，Release body 是人类可读投影
- [ ] release workflow 的 asset 校验步骤拒绝缺少 `manifest.json` / 签名文件的 release
- [ ] 为 frozen asset naming / install path 增加 contract test

## Phase 4 - 完成统一升级入口

- [ ] 完成顶层 `looper upgrade`
- [ ] 支持 `looper upgrade --check`
- [ ] 支持 `looper upgrade --cli`
- [ ] 支持 `looper upgrade --daemon`
- [ ] 支持 `looper upgrade --to <version>`
- [ ] 支持 `looper upgrade --channel <stable|beta>`
- [ ] 支持 `looper upgrade --rollback`
- [ ] 支持 `looper upgrade --yes`
- [ ] 升级流程引入 upgrade lock，避免并发升级
- [ ] 明确 upgrade 离线 / 超时 / manifest 缓存策略
- [ ] 升级前先校验 manifest 签名
- [ ] 升级前先计算 current/latest plan
- [ ] current/latest 判断改为真正的 semver compare
- [ ] major 升级要求显式确认
- [ ] 明确 `--to <older-version>` 的 downgrade 语义与边界
- [ ] 明确 `upgrade --check` 的退出码契约
- [ ] 明确完整 flag × install-source 行为矩阵
- [ ] 增加 upgrade lock stale-lock 检测与 `--force-unlock`
- [ ] 增加 `~/.looper/state/upgrade.json` 状态文件

## Phase 5 - CLI 自升级与 daemon 升级安全性

- [ ] CLI 自升级支持 staging 下载
- [ ] CLI 自升级支持原子替换
- [ ] CLI 自升级保留上一版 `looper.prev`
- [ ] CLI 路径不可写时给出明确人工替代步骤
- [ ] Homebrew 安装来源默认拒绝 CLI 自升级并给出 `brew upgrade` 指引
- [ ] `go install` 路径默认拒绝 CLI 自升级并给出对应指引
- [ ] 明确 install-source 检测机制（build-time + path heuristic）
- [ ] daemon 升级支持 staging 下载
- [ ] daemon 升级支持 checksum + manifest 校验
- [ ] daemon 升级支持原子替换
- [ ] daemon 升级保留上一版 `looperd.prev`
- [ ] 新增或补齐安全停止 daemon 的受支持能力
- [ ] 用正式 `daemon stop` / 等价内部原语替换当前 “Phase 1 minimal process management” 提示
- [ ] 明确 daemon 升级时有活跃 loops 的处理策略（拒绝 / drain / force-stop）
- [ ] daemon 升级失败时自动回滚二进制
- [ ] daemon 升级成功后执行健康检查
- [ ] 升级状态写入专用 state 文件
- [ ] 明确升级被中断（如 Ctrl+C）时的恢复语义

## Phase 6 - DB 备份与回滚边界

- [ ] 升级前判断目标版本是否触发 migration
- [ ] 触发 migration 时自动创建 pre-upgrade DB backup
- [ ] backup 文件命名包含版本与时间戳
- [ ] `package.requireBackupBeforeMigrate` 默认值切换策略明确
- [ ] `looper upgrade --rollback` 明确只承诺二进制回滚
- [ ] schema 已升级且旧 binary 不兼容时，阻止直接 rollback 并给出恢复指引
- [ ] daemon 启动时增加 schema-version downgrade 保护
- [ ] downgrade / rollback 在替换二进制前执行 schema 兼容性预检

## Phase 7 - 兼容与协议策略

- [ ] 明确 CLI 与 daemon 继续共享同一版本号
- [ ] 明确 CLI / daemon 从同一 git tag 发布
- [ ] 源码默认版本切换为 dev version，由 build/release 注入正式版本
- [ ] 明确同一 major 内的兼容原则
- [ ] 明确 `minDaemonForCli` / `minCliForDaemon` 的执行语义
- [ ] 明确恢复路径命令（`upgrade*` / `version` / `daemon install|start|restart|status|logs`）不受 daemon-version gating 阻断
- [ ] `/api/v1` 的非破坏性演进规则写入用户可见文档
- [ ] 破坏性接口变更必须通过 `/api/v2` 引入
- [ ] 建立 N / N-1 兼容 smoke 验证
- [ ] 明确 version/source 判定顺序（binary / API / manifest / release）
- [ ] 补齐 prerelease channel 规则（draft 不算、prerelease 不进 stable）
- [ ] 补齐 `looper version --json` 输出（product/api/schema/build/channel）
- [ ] `version --json` 在同一 major 内保持稳定，仅允许新增字段

## Phase 8 - Gatekeeper / 信任链 / 平台体验

- [ ] 安装文档写清当前 unsigned binary 限制
- [ ] 安装脚本对 quarantine 处理要求显式用户确认
- [ ] `bootstrap` 对 quarantine 处理要求显式用户确认
- [ ] 输出清晰的 Gatekeeper 人工放行指引
- [ ] release workflow 预留 Apple codesign / notarization 扩展点
- [ ] 明确 install 首跳信任模型与文档表述

## Phase 9 - daemon 托管模式演进

- [ ] 正式设计 `looper daemon install --launchd`
- [ ] 生成 `LaunchAgents` plist
- [ ] 用 `launchctl` 接管 start / stop / restart 语义
- [ ] 明确 `foreground` 与 `launchd` 两条路径的文档边界
- [ ] 将“开机自启/崩溃恢复”能力只归属到 `launchd` 模式

## Phase 10 - 分发渠道扩展

- [ ] 新建 Homebrew tap
- [ ] formula 只分发 `looper`
- [ ] formula 与 release manifest / asset 命名保持一致
- [ ] 文档补齐 GitHub Releases / install script / Homebrew 三条路径

## Phase 11 - Workflow 与验证

- [ ] release workflow 改为 tag 驱动版本注入
- [ ] release workflow 生成 manifest
- [ ] release workflow 对 manifest 进行签名
- [ ] signed manifest + 对应 workflow 支持完成前，不宣称 unified upgrade 已 GA
- [ ] release workflow 校验完整 asset 集
- [ ] release workflow 支持 stable / beta channel
- [ ] prerelease tag 正确映射到 beta channel
- [ ] 验证 `manifest.json.version`、二进制 `--version`、Git tag 三者一致
- [ ] release workflow 增加 CLI + daemon 组合 smoke
- [ ] 验证二进制 `--version` 与 Git tag / GitHub Release 一致
- [ ] 验证干净 macOS 机器一键安装成功
- [ ] 验证 `bootstrap` 幂等
- [ ] 验证 `upgrade` 成功路径
- [ ] 验证 `upgrade` 失败后的自动 rollback
- [ ] 验证 migration 前 backup 正常生成
- [ ] 验证旧版本/新版本兼容提示正确
- [ ] 验证文档中的安装路径与实际 workflow 一致

## Out of scope for this checklist

- Linux / Windows 正式支持
- GUI `.pkg` 安装器
- 将 `looperd` 变成独立面向用户的主安装入口
