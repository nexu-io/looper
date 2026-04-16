# CLI / Daemon Distribution Checklist

## Phase 0 - 方向先定死

- [x] 明确主分发策略采用 `npm CLI + compiled daemon`
- [x] 明确 `@powerformer/looper` 只承载 `looper` CLI
- [x] 明确 `looperd` 不再作为 npm 包内默认安装的 daemon 形态
- [x] 明确 `looper` single binary 仅作为可选发布方向，不作为主路径
- [x] 明确 Phase 1 不实现 Homebrew / 自更新 / Windows service
- [x] 明确顶层升级入口采用 `looper upgrade`
- [x] 明确 `looper daemon upgrade` 不作为主 UX
- [x] 明确 CLI 与 daemon 共享同一版本号，并从同一 git tag 发布
- [x] 明确当前一期只支持 macOS，不支持 Linux

## Phase 1 - 收回 npm 包职责边界

- [x] 从 `apps/cli/package.json` 的 `bin` 中移除 `looperd`
- [x] 从 `apps/cli/package.json` 的 `files` 中移除 `dist/looperd.js` 与 daemon 相关产物
- [x] 将 `apps/cli` 的构建脚本恢复为只构建 `looper`
- [x] 从 `apps/cli/scripts/build.ts` 中移除 looperd build 与 migrations copy 逻辑
- [x] 确认 `npm pack --dry-run` 产物只包含 CLI 所需文件

## Phase 2 - 为 `looperd` 建立 compile 构建链路

> 当前约定：compile 仍是 release 架构的一部分，但不进入本地 dev/build/test 主流程；
> 本地已知坏环境可以 fail-fast，release workflow 再在 macOS runners 上做最终 smoke validation。

- [x] 在 `apps/looperd/package.json` 增加 `compile` 脚本
- [x] 增加按平台 target 的 compile 脚本
- [x] 第一批支持 `darwin-arm64`
- [x] 第一批支持 `darwin-x64`
- [x] 明确 Linux 不进入当前范围
- [x] 验证 compile 产物至少可执行 `--version`（完整运行依赖 Phase 3）
- [x] 验证 compiled binary 体积是否在可接受范围，并记录预期大小（当前实测：darwin-arm64 ≈ 58.9 MiB，darwin-x64 ≈ 64.1 MiB；Phase 1 先以单产物 < 70 MiB 为可接受范围）

## Phase 3 - SQLite migrations 内嵌化

- [x] 设计 migration 内嵌表示结构
- [x] 将现有 `.sql` 文件转换为内嵌资源或生成模块
- [x] 改造 `migrate.ts`，从内存资源读取 migration 列表
- [x] 删除对运行时 migrations 目录扫描的主路径依赖
- [x] 保留测试场景下可注入 migrations 的能力（如需要）
- [x] 验证源码运行 / build 运行 / compile 运行三种模式行为一致

## Phase 4 - `looperd` binary 运行与元数据

- [x] 明确 compiled binary 的输出命名规则
- [x] 明确 daemon version / build metadata 暴露方式
- [x] 明确版本 single source of truth，并让 CLI/daemon 构建都读取它
- [x] 在状态接口中返回 daemon 版本信息
- [x] 验证 CLI 可读取并展示 daemon 版本
- [x] 明确 binary 安装目录约定（建议 `~/.looper/bin/`）
- [x] 支持 `looperd --version`
- [x] `looperd --version` 必须在 bootstrap 前短路
- [x] 支持 CLI 获取当前 daemon 版本（运行中 / 未运行两种路径）
- [x] 替换 `apps/looperd/src/server/index.ts` 中硬编码的 daemon 版本字符串

## Phase 5 - Release Workflow

> release workflow 是 compile 可用性的权威验证点；本地 compile 失败不能单独作为否定该分发模型的依据。

- [x] 增加 tag 驱动的 release workflow
- [x] matrix 构建 macOS 双架构 `looperd` binary
- [x] 第一批 workflow 至少覆盖 `darwin-arm64`
- [x] 第一批 workflow 至少覆盖 `darwin-x64`
- [x] macOS targets 使用 macOS runners 构建
- [x] 明确当前 workflow 不发布 Linux artifacts
- [x] 上传 GitHub Release artifacts
- [x] 生成 SHA-256 校验信息
- [x] Phase 1 先只依赖稳定 artifact 命名 + GitHub Releases API
- [x] 如后续确有需要，再补 manifest
- [x] 在同一 release 流程中发布 npm CLI
- [x] 验证 release 产物命名与下载地址可预测
- [x] 验证 tag 与 npm package version 一致
- [x] 验证 compiled `looperd` artifact 可执行 `--version`
- [x] 验证 checksum 与 artifact 内容一致
- [x] 验证 CLI 可根据 release 元数据定位正确下载目标

## Phase 6 - CLI 的 daemon 安装助手

- [x] 设计 `looper daemon install` 命令
- [x] 自动识别当前平台和架构
- [x] 自动拼出正确的 release artifact 下载地址
- [x] 下载 binary 到本地安装目录
- [x] 设置可执行权限
- [x] 已安装时提供幂等行为或覆盖策略
- [x] 安装失败时输出清晰错误信息

## Phase 7 - CLI 的 daemon 管理命令

- [x] 设计 `looper daemon start`
- [x] 明确启动前如何定位已安装的 `looperd`
- [x] daemon 查找顺序固定为 `~/.looper/bin/looperd` → `$PATH` → 报错
- [x] `looper daemon status` 兼容 binary 安装模式
- [x] 设计 `looper daemon restart`
- [x] 明确 Phase 1 是否只支持手动运行 daemon，而不承诺完整后台托管

## Phase 8 - 顶层 `looper upgrade`

- [x] 设计 `looper upgrade`
- [x] 支持 `looper upgrade --check`
- [x] 支持 `looper upgrade --daemon`
- [x] Phase 1 先实现 `--check`
- [x] Phase 1 再实现 `--daemon`
- [x] 完整 `looper upgrade`（CLI + daemon）后置
- [x] 从 npm registry 获取最新 CLI 版本
- [x] 从 GitHub Releases REST API 获取最新 daemon 版本
- [x] daemon 未安装时，`looper upgrade` 自动走 install 路径
- [x] daemon 下载采用临时文件 + 校验 + 原子替换
- [x] CLI 自升级设计单独后置，不在当前 phase 强行落地
- [x] 明确 daemon 下载失败时的提示与恢复策略
- [x] 升级完成后默认只提示 `looper daemon restart`，不自动重启 daemon

## Phase 9 - README / docs 更新

- [x] README 安装说明改成双通道模型
- [x] README 写清楚 CLI 通过 npm 安装
- [x] README 写清楚 daemon 通过 `looper daemon install` 或手动下载安装
- [x] README 写清楚 daemon 启动与验证流程
- [x] README 写清楚 `looper upgrade` 是统一升级入口
- [x] README 写清楚升级后需手动 `looper daemon restart`
- [x] `docs/configuration.md` 检查是否需要补充 binary 安装路径说明
- [x] 在相关文档中移除“daemon 需从源码启动”的过时描述

## Phase 10 - 兼容性与版本策略

- [x] 明确 CLI / daemon 的 API 兼容原则
- [x] 明确 `/api/v1` 在当前阶段不做破坏性变更
- [x] 明确 CLI 与 daemon 轻度版本错位时的支持策略
- [x] 明确 daemon 过旧时 CLI 是否给出升级提示
- [x] 明确 current/latest 版本信息的数据来源与降级策略
- [x] 明确 major version 升级是否需要 `--force` 或确认

## Phase 11 - 验证与验收

- [x] 单测覆盖 migration 内嵌化逻辑
- [x] 单测覆盖 daemon install 下载与平台识别逻辑
- [x] 单测覆盖 daemon start / restart 的主要分支
- [x] 单测覆盖 `looper upgrade --check` 的主要分支
- [x] 单测覆盖 `looper upgrade --daemon` 的主要分支
- [x] 单测覆盖升级失败后的可重试行为
- [x] 验证 `npm install -g @powerformer/looper` / 本地 tarball 全局安装后 CLI 正常工作
- [x] 验证手动安装的 `looperd` binary 可启动
- [x] 验证 `looper status` / `looper daemon status` 在新安装模型下工作正常
- [x] 验证当前 Phase 1 支持的升级路径（`looper upgrade --check` / `looper upgrade --daemon`）
- [x] 验证 GitHub Release 元数据解析与下载链路逻辑（真实 release smoke 待首个 release 执行）

验证记录（2026-04-15）：

- `bun test apps/looperd/src/storage/sqlite/migrate.test.ts apps/cli/src/index.test.ts` 通过，补齐 Phase 11 缺失单测
- `bun run typecheck` 与 `bun run build` 通过
- 本地 tarball 全局安装 smoke：`npm install -g --prefix <tmp> apps/cli/powerformer-looper-0.2.0.tgz` 后 `looper --help` 正常
- 手动安装 compiled `looperd-darwin-x64` 到 `~/.looper/bin/looperd` 后，`looper daemon start`、`looper daemon status --json`、`looper status --json` 均正常
- GitHub Release download 逻辑由 `apps/cli/src/daemon-release.test.ts` 与 `apps/cli/src/daemon-install.test.ts` 覆盖；仓库当前尚无公开 release 可做真实 smoke

## Out of scope for this spec

- 默认把 `looper` 也切换为 single binary 主分发方式
- 完整的 CLI / daemon 自更新框架
- Homebrew / apt / yum / winget 等多渠道分发
- 将 `looperd` 全量迁移到 Node 兼容运行时
