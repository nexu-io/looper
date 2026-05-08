# Looper 安装与版本升级完整方案

## 1. 背景

当前仓库已经切到 Go-first 的 CLI + daemon 形态：

- `looper` 作为用户入口 CLI
- `looperd` 作为本地 daemon
- GitHub Releases 发布 `looper-*` / `looperd-*` 二进制与 `.sha256`
- `looper daemon install` 可以把 managed daemon 安装到 `~/.looper/bin/looperd`
- `looper upgrade --check` 与 `looper upgrade --daemon` 已存在，但完整统一升级路径尚未完成

现状虽然“可用”，但还不满足“把产品发给另一个用户，他可以低摩擦安装并持续升级”的要求。

主要问题：

1. CLI 安装仍偏手动，缺少一条真正推荐的一键路径。
2. 首次使用需要用户自己拼装 config、agent env、project 注册流程。
3. `looper upgrade` 还不是完整的 CLI + daemon 统一升级入口。
4. release 对机器消费者的契约还不够完整，只有 asset + checksum，没有稳定 manifest / channel / rollback 语义。
5. 当前 macOS 二进制未签名，Gatekeeper 体验不稳定。

本 spec 的目标，是给 Looper 定义一套完整、可分期落地的安装与升级方案，并与当前 Go 版本的现实约束保持一致。

---

## 2. 目标

目标：

1. 为新 macOS 用户提供低摩擦安装路径。
2. 明确“安装什么、谁负责安装 daemon、谁负责升级”的职责边界。
3. 提供统一的 `looper upgrade` 体验，覆盖 CLI、daemon、检查、回滚与版本 pin。
4. 定义 release contract，让 CLI / 安装脚本 / Homebrew 等机器消费者都可稳定依赖。
5. 明确 CLI / daemon 的兼容与版本策略，避免静默错配。
6. 将 rollback、DB 备份、失败恢复纳入正式设计，而不是靠人工排障。

非目标：

1. 本次不把 Linux / Windows 一起拉进当前支持矩阵。
2. 本次不要求立刻提供 `.pkg` 图形安装器。
3. 本次不要求立即完成 Apple notarization；但必须为其预留演进路径。
4. 本次不改动现有 `~/.looper/` runtime layout 的核心目录约定。

## 2.1 执行分期约定

本文描述的是目标方案与演进路线；具体实施顺序与完成状态以同目录下 `checklist.md` 为准。

即：

> **spec 负责设计与约束，checklist 负责执行 truth。**

本文中的 “Phase 1 / 2 / 3” 只是产品路线分组，不与 `checklist.md` 中更细的执行 phase 编号做一一对应。

## 2.2 支持平台范围

本 spec 当前仅覆盖：

- macOS 13+（arm64）为正式支持范围
- macOS 12 为 best-effort

以下平台不在本 spec 的执行范围内：

- macOS <= 11
- Linux
- Windows

---

## 3. 结论

## 3.1 推荐安装模型

Looper 的推荐安装模型调整为：

> **统一由 `looper` CLI 承担用户入口、bootstrap、daemon 安装与升级编排职责。**

推荐用户路径：

```bash
curl -fsSL https://raw.githubusercontent.com/nexu-io/looper/main/scripts/install.sh | sh
looper bootstrap
```

或：

```bash
brew install powerformer/tap/looper
looper bootstrap
```

其中：

- 分发渠道面向用户只分发 `looper`
- `looperd` 不要求用户独立理解与手动管理
- `looper bootstrap` 负责初始化 `~/.looper/`、生成默认配置、安装 daemon、启动并验证
- `looper upgrade` 负责统一升级 CLI + daemon

换言之：

> **用户安装的是 Looper，不是分别安装 CLI 和 daemon。**

## 3.2 推荐升级模型

统一升级入口固定为：

```bash
looper upgrade
```

同时保留显式子路径：

```bash
looper upgrade --check
looper upgrade --cli
looper upgrade --daemon
looper upgrade --to 0.3.0
looper upgrade --rollback
looper upgrade --channel beta
```

升级逻辑由 CLI 编排，daemon 不承担自更新职责。

## 3.3 推荐分发渠道

分发渠道按成熟度分层：

### A. GitHub Releases（source of truth）

必须继续保留，并作为所有其他渠道的底层来源。

发布资产：

- `looper-darwin-arm64`
- `looper-darwin-arm64.sha256`
- `looperd-darwin-arm64`
- `looperd-darwin-arm64.sha256`

### B. 安装脚本（默认推荐）

新增 `scripts/install.sh` 作为默认推荐安装路径：

- 检测 macOS 架构
- 下载匹配的 `looper` binary
- 校验 `sha256`
- 安装到用户可写 PATH（优先已在 PATH 中的用户目录；若 `~/.local/bin` 不在 PATH，则需显式提示并经用户确认后追加 shell profile，或回退到 `/usr/local/bin`）
- 安装完成后提示运行 `looper bootstrap`

该脚本只安装 CLI，不直接安装 daemon。

### C. Homebrew tap（Phase 2）

新增 `powerformer/tap`：

- formula 只安装 `looper`
- post-install 不做 daemon 安装，只提示 `looper bootstrap`
- 避免为 `looperd` 再维护第二套面向用户的显式安装路径

### D. `go install`（开发者路径）

保留为开发者/贡献者路径，但明确不是普通用户推荐方案。

### E. uninstall

需要提供显式卸载路径：

- `scripts/uninstall.sh`
- 或后续 `looper uninstall`

至少要支持删除：

- CLI binary
- managed daemon binary
- updater state

并对是否删除 `~/.looper/config.json` / DB / logs / worktrees 给出显式确认。

---

## 4. Release Contract

当前 release 资产命名与 managed daemon 路径已经形成兼容边界；本方案要求将其正式冻结，并在此基础上增加 manifest 契约。

同时，本节明确：

> **GitHub Release 不只是二进制下载容器，而是 Looper 产品版本的正式发布载体。**

## 4.1 已冻结的兼容锚点

以下约定继续保留：

1. CLI release 资产命名保留 `looper-<os>-<arch>`。
2. daemon release 资产命名保留 `looperd-<os>-<arch>`。
3. checksum sidecar 保留 `<asset>.sha256`。
4. managed daemon 安装路径保留 `~/.looper/bin/looperd`。
5. daemon 查找顺序保留 `~/.looper/bin/looperd` → `$PATH`。

## 4.1.1 GitHub Release 作为正式产品版本载体

Looper 的正式产品版本，必须通过 GitHub tag + GitHub Release 对外表达。

规则：

1. 每个正式版本都必须对应一个 git tag。
2. 每个对外发布版本都必须有对应的 GitHub Release。
3. CLI 与 daemon 的对外发布版本以同一个 GitHub Release 为准。
4. 用户可见的“当前版本 / 最新版本 / 可升级版本”判断，以 GitHub Release 元数据为主，而不是以源码中的硬编码字符串为主。

这意味着：

> **Git tag / GitHub Release 是版本 source of truth；二进制中的版本号由构建时注入。**

## 4.1.2 Git tag 规则

tag 采用标准 SemVer 形态：

- `v0.3.0`
- `v0.3.1`
- `v0.4.0-rc.1`
- `v1.0.0-beta.2`

约束：

1. tag 一律带前缀 `v`。
2. release title 采用 `Looper vX.Y.Z`。
3. prerelease tag 必须在 GitHub Release 中标记为 prerelease。
4. draft release 不能被 `upgrade --check` 视为可升级目标。

## 4.2 新增 manifest

每个 release 必须新增：

- `manifest.json`
- `manifest.json.minisig`

`manifest.json` 是 CLI updater、安装脚本、Homebrew formula 生成脚本的稳定机器输入。

对机器消费者：

> **manifest 是权威机器真相；GitHub Release body 是从同一输入派生的人类可读投影。**

建议结构：

```json
{
  "manifestVersion": 1,
  "version": "0.3.0",
  "tag": "v0.3.0",
  "released": "2026-04-22T12:00:00Z",
  "channel": "stable",
  "apiVersion": "v1",
  "schemaVersion": 12,
  "minCliForDaemon": "0.2.0",
  "minDaemonForCli": "0.2.0",
  "artifacts": {
    "looper-darwin-arm64": {
      "url": "https://github.com/...",
      "sha256": "...",
      "size": 123
    },
    "looperd-darwin-arm64": {
      "url": "https://github.com/...",
      "sha256": "...",
      "size": 456
    }
  }
}
```

## 4.2.1 manifest 发现地址

manifest 不能依赖“枚举所有 GitHub release 后自己猜最新版本”。

必须提供稳定发现地址，例如：

- `https://github.com/nexu-io/looper/releases/latest/download/manifest.json`
- `https://github.com/nexu-io/looper/releases/latest/download/manifest.json.minisig`

若后续引入 `stable` / `beta` channel，则需要为每个 channel 提供独立稳定地址，例如：

- `.../channels/stable/manifest.json`
- `.../channels/beta/manifest.json`

在没有独立 channel endpoint 之前，CLI 只把 `stable` 作为默认受支持自动发现路径；`beta` 可通过显式 `--to` 或预发布 channel URL 实现。

### 为什么需要 manifest

因为仅靠 GitHub Releases asset 枚举不够：

1. 无法表达 channel（stable / beta）。
2. 无法表达兼容窗口（`minDaemonForCli` / `minCliForDaemon`）。
3. 无法表达 API version 与升级策略。
4. 无法表达 schema 兼容边界。
5. 无法为未来签名校验提供稳定入口。

## 4.2.2 GitHub Release body 约定

GitHub Release body 不应只依赖自动生成 notes，还必须包含稳定的兼容信息块。

至少应包含：

1. Summary
2. Compatibility
   - product version
   - API version
   - schema version
   - `minCliForDaemon`
   - `minDaemonForCli`
3. Upgrade notes
   - 是否有 migration
   - 是否有手动 restart / config 迁移要求
4. Artifacts

即使未来机器读取以 `manifest.json` 为主，人类阅读的 GitHub Release 页面也必须能直接看出“这版会不会 break、怎么升、需要注意什么”。

权威关系要求：

- 机器消费者以 `manifest.json` 为准
- 人类阅读以 Release body 为准
- 二者若不一致，属于 release workflow defect，不能视为允许存在的双真相

## 4.2.3 Manifest forward compatibility

CLI 必须拒绝解析 `manifestVersion` 高于自己所支持上限的 manifest。

此时应：

1. 给出结构化错误
2. 明确提示用户先升级 CLI
3. 仅在必要时回退到最小化 release asset 枚举，用于告诉用户“哪个 CLI 版本理解这个 manifest”

时间戳格式统一采用 RFC3339 UTC。

## 4.3 完整性与真实性校验

下载链路必须区分两类校验：

1. **完整性**：`sha256`
2. **真实性**：`manifest.json.minisig`

仅靠 `.sha256` 只能防损坏，不能防替换。

因此：

> **CLI 与安装脚本最终都必须以签名过的 manifest 为信任根。**

在 Apple codesign / notarization 完成前，`minisign` 是当前成本最低且足够实用的真实性方案。

补充说明：

- 初次安装的第一跳信任仍然来自 HTTPS + GitHub release/脚本分发地址
- manifest 签名主要保护后续 upgrade 与 machine-consumer 下载链路
- 如后续评估认为 GitHub OIDC + `cosign` 更适合 workflow，则允许在实现前替换签名方案，但必须保持“有签名 manifest”这一能力目标不变

---

## 5. 首次安装与 bootstrap 体验

## 5.1 新用户目标路径

理想路径：

```bash
curl -fsSL https://raw.githubusercontent.com/nexu-io/looper/main/scripts/install.sh | sh
looper bootstrap
```

完成后用户应具备：

- 已安装 `looper`
- 已安装 managed `looperd`
- 已创建最小 `~/.looper/config.json`
- 已启动 daemon
- 已通过 `looper status` 验证

## 5.2 `looper bootstrap`

新增命令：

```bash
looper bootstrap
```

职责：

1. preflight 检查：
   - 当前平台/架构是否支持
   - `git` / `gh` 是否可用
   - 如启用 `gh` 集成，可提示 `gh auth status`
2. 初始化目录：
   - `~/.looper/bin`
   - `~/.looper/backups`
   - `~/.looper/logs`
3. 若配置不存在，则交互式生成最小 `config.json`
4. 提示用户选择或填写：
   - `agent.vendor`
   - 是否启用 `notifications.osascript.enabled`
   - 是否启用 `server.authMode=local-token`
   - 默认 project（可跳过）
5. 若未安装或版本不符合目标版本，则执行 `looper daemon install`
6. 自动执行 `looper daemon start`
7. 轮询 `/api/v1/status` 或等价 health check
8. 输出 next steps（如 `looper project add /path/to/repo`）

### 幂等性要求

`bootstrap` 必须是幂等的：

- 已有 config 时不覆盖，除非用户显式要求
- 已安装 daemon 时不重复下载，除非版本不匹配或 `--force`
- 已启动 daemon 时优先做状态确认

### 部分失败后的重入语义

`bootstrap` 必须显式处理以下部分完成状态：

1. **无 config、无 daemon**
   - 按完整初始化路径执行
2. **有 config、无 daemon**
   - 跳过 config 初始化，继续安装 daemon
3. **有 config、有 daemon binary、daemon 未运行**
   - 不重复下载，直接做 daemon 启动与 health check
4. **有 config、有 daemon binary、daemon 已运行**
   - 做状态确认并输出摘要，不重复执行破坏性步骤
5. **daemon 安装成功但启动失败**
   - 下次 `bootstrap` 必须识别为“已安装但未运行”，优先执行诊断/启动，而不是回到全量重装路径

### 非交互模式

必须支持：

```bash
looper bootstrap --yes --project-path /path/to/repo --agent-vendor opencode
```

用于：

- CI smoke
- 文档脚本化验证
- 高级用户自动化安装

### 依赖缺失时的原则

对于 `git` / `gh` 等依赖：

- `bootstrap` 负责检测与清晰报错
- 不负责静默替用户安装系统工具
- 文档中提供 Homebrew 等人工安装提示

## 5.3 Gatekeeper 处理

在未完成 Apple 签名/公证前：

1. 文档中明确说明二进制可能被 Gatekeeper 拦截。
2. `bootstrap` / 安装脚本可在获得用户明确确认后执行 `xattr -d com.apple.quarantine`。
3. 如果用户拒绝，应输出清晰人工处理指引。

即：

> **可以帮助用户处理 quarantine，但不能静默越过用户同意。**

---

## 6. daemon 生命周期方案

## 6.1 当前阶段（Phase 1 / 2）

当前默认保留 `foreground` 模式作为受支持主路径：

- `looper daemon install`
- `looper daemon start`
- `looper daemon restart`
- `looper daemon status`
- `looper daemon logs`

CLI 是 daemon lifecycle 的唯一编排入口。

## 6.2 `launchd` 演进

`daemon.mode=launchd` 已在配置面存在，但尚未形成完整主路径。

本方案要求在 Phase 2 把它补成正式能力：

- `looper daemon install --launchd`
- 写入 `~/Library/LaunchAgents/...plist`
- 用 `launchctl bootstrap` / `kickstart` / `bootout` 编排
- 让 daemon 获得开机自启、崩溃恢复、用户态托管能力

原则：

> 不在 `foreground` 路径上硬补“伪后台托管”；完整托管能力应明确落在 `launchd` 模式。

---

## 7. 升级方案

## 7.1 命令面

统一升级命令面定义为：

```bash
looper upgrade
looper upgrade --check
looper upgrade --cli
looper upgrade --daemon
looper upgrade --to <version>
looper upgrade --channel <stable|beta>
looper upgrade --rollback
looper upgrade --yes
```

语义：

- `upgrade`：升级 CLI + daemon 到当前 channel 最新版本
- `--check`：只展示 current/latest 与兼容状态
- `--cli`：只升级 CLI
- `--daemon`：只升级 daemon
- `--to`：升级/安装到指定版本
- `--channel`：切换稳定/预发布通道
- `--rollback`：回滚到上一版已保留二进制

### 7.1.1 flag 与安装来源矩阵

| 场景 | release 二进制安装 | Homebrew 安装 | `go install` / dev 安装 |
|---|---|---|---|
| `looper upgrade` | 升级 CLI + daemon | 升级 daemon，并提示用 `brew upgrade` 升级 CLI | 升级 daemon，CLI 自升级默认拒绝 |
| `looper upgrade --cli` | 允许 | 拒绝，并提示 `brew upgrade` | 拒绝，并提示重新 `go install` / 重新构建 |
| `looper upgrade --daemon` | 允许 | 允许 | 允许 |
| `looper upgrade --to <version>` | 允许，但受 schema / compatibility 预检约束 | CLI 部分按 Homebrew 规则拒绝自升级 | CLI 部分按 dev 安装规则拒绝自升级 |
| `looper upgrade --rollback` | 允许 | 仅对 daemon 回滚；CLI 按渠道自管 | 仅对 daemon 回滚；CLI 不做自回滚 |

默认决策：

> 在 v1 中，`--channel` 只作为一次性命令参数，不持久化到配置。

### 7.1.2 退出码约定

`looper upgrade --check` 需要明确退出码语义：

- `0`：检查成功，且当前已是最新稳定版本
- `1`：检查成功，且存在可升级版本
- `>1`：检查失败（网络、签名、解析等错误）

其他 upgrade 子命令默认沿用“成功 0 / 失败非 0”的常规 CLI 约定。

## 7.2 推荐默认流程

执行 `looper upgrade` 时：

1. 拉取当前 channel 的 `manifest.json`
2. 校验 manifest 签名
3. 获取 current CLI / daemon 版本
4. 计算升级 plan（含 version、schema、install-source、channel 预检）
5. 获取 upgrade lock，避免并发升级
6. 若 daemon 正在执行 in-flight loops，则默认拒绝升级 daemon，并提示用户稍后重试或显式选择强制策略
7. 如涉及 major 升级，要求显式确认
8. 下载目标 CLI / daemon 到 staging 目录
9. 校验 `sha256`
10. 执行 CLI 自升级
11. 执行 daemon 升级
12. 做健康检查
13. 成功后写入 upgrade state；失败则自动 rollback

这里的 latest/current 判断，应基于真正的 SemVer 比较，而不是字符串比较。

### 7.2.1 Upgrade lock 与中断恢复

upgrade lock 推荐落到：

- `~/.looper/run/upgrade.lock`

要求：

1. lock 必须记录 PID 与启动时间等最小状态
2. stale lock 必须可检测（进程不存在或启动时间不匹配）
3. 需要提供受控的 `--force-unlock` 逃生口
4. 升级过程中断时，状态写入 `~/.looper/state/upgrade.json`

`upgrade.json` 至少应记录：

- current version
- previous version
- target version
- startedAt
- install source
- daemon upgrade stage

下次执行 upgrade 时，CLI 必须先判断是继续、回滚还是安全放弃。

## 7.3 CLI 自升级

CLI 自升级由当前运行的 `looper` 进程完成。

要求：

1. 检测当前 CLI 路径是否可写。
2. 使用临时文件 + 原子替换。
3. 保留上一版到 `looper.prev`。
4. 若不可写，则打印明确的人类可执行替代步骤，而不是静默失败。
5. 若当前安装来源是 Homebrew 或 `go install` 路径，则默认拒绝自升级，并提示用户使用对应渠道升级。

### 7.3.1 安装来源识别

CLI 需要识别当前安装来源，推荐优先级：

1. build-time 注入的 `installSource`
2. 已知路径模式（如 Homebrew Cellar）
3. dev / `go install` 特征
4. 若仍无法确定，则标记为 `unknown`

安全默认值：

> 当安装来源无法可靠识别时，拒绝 CLI 自升级，并给出人工升级指引。

## 7.4 daemon 升级

daemon 升级要求：

1. 升级目标固定为 managed daemon 路径 `~/.looper/bin/looperd`
2. 使用 staging 下载
3. checksum 与 manifest 校验通过后再切换
4. 升级前必要时停止 daemon
5. 原子替换并保留 `looperd.prev`
6. 启动新 daemon 并轮询健康检查
7. 如失败，自动回滚到 `looperd.prev`

这里要求明确补齐 daemon 停止能力：

> **要么新增 `looper daemon stop`，要么把“安全停止并替换 daemon”实现为受支持内部原语；不能把它留成隐式假设。**

该能力是 unified upgrade 的前置能力，不依赖 `launchd` 正式化完成后才可落地。

当前实现只提供 `upgrade --daemon` 的薄包装，不满足这里的 rollback / health-check / manifest-gated 要求；在这些要求全部落地前，不应把现有行为误认为“完整 daemon upgrade 方案已存在”。

## 7.5 升级提示策略

所有命令不应被更新检查阻塞。

允许增加“轻提示”机制：

- 最多每 24 小时检查一次最新版本
- 若存在新版本，仅输出一行提示
- 支持通过 config / env 关闭

更新检查必须有超时与缓存策略：

- 离线时快速失败
- 不阻塞主命令执行
- 可缓存上一次 manifest 摘要

---

## 8. 版本与兼容策略

本节将版本概念拆成 4 层，避免“只有一个模糊 version 字段”的状态。

## 8.0 版本分层模型

Looper 的版本体系分为：

1. **Product Version**
   - 用户可见版本
   - 通过 git tag / GitHub Release 表达
   - CLI 与 daemon 共享

2. **API Version**
   - CLI 与 daemon 管理协议版本
   - 体现在 `/api/v1/*`
   - 只在协议破坏性变更时升级

3. **Schema Version**
   - SQLite schema 版本
   - 由 daemon migration 系统维护
   - 用于 upgrade / downgrade 安全边界

4. **Build Metadata**
   - git sha
   - build timestamp
   - dev build 标识
   - 用于排障，不构成产品版本本身

原则：

> 产品版本、API 版本、Schema 版本必须明确区分，不能继续用单一字符串承担全部语义。

## 8.1 版本号策略

继续保持：

1. CLI 与 daemon 共享同一版本号
2. 同一 git tag 发布二者
3. 构建出的 CLI / daemon `--version` 必须与 tag 对应版本一致

当前阶段不建议将 CLI 与 daemon 版本解耦。

同时要求：

1. 源码中的默认版本必须是 dev version，而不是手工维护的正式 release version。
2. 正式版本号由 release build 注入。
3. PR / 本地未打 tag 构建可显示为 `0.0.0-dev` 或等价 dev semver。

`minDaemonForCli` / `minCliForDaemon` 主要用于：

- 只升级了一侧
- rollback 后短暂错位
- 指定版本安装/降级
- Homebrew / 手动替换导致的版本偏差

## 8.1.1 Product Version 的 source of truth

Product Version 的唯一 source of truth 是：

1. git tag
2. GitHub Release

不是：

- 手工改源码常量后再让 release 去适配它

推荐模型：

1. 源码默认 `0.0.0-dev`
2. release workflow 从 tag 解析出 `X.Y.Z`
3. 通过 build flags 注入到 CLI 与 daemon
4. `version --json` 输出最终注入值

这样可以避免：

- 忘记 bump 硬编码版本
- tag 与二进制版本漂移
- PR 专门为了改版本号而制造噪音提交

### 8.1.1a 版本注入迁移步骤

从“源码硬编码正式版本”切换到“tag 驱动版本注入”是一次明确迁移，不应被视为自然发生。

迁移要求：

1. 将源码默认版本切换为 `0.0.0-dev`
2. 同一个 PR / 变更批次中，把 release workflow 改为从 `github.ref_name` 解析正式版本
3. 同时更新版本比较逻辑，明确 dev build 的比较语义

默认规则：

- `0.0.0-dev` 本地/PR 构建默认视为开发构建
- `upgrade --check` 对 dev build 可以提示“存在正式 release”，但不能把这种状态误报为普通 patch drift

在这一步迁移完成前，tag-driven version injection 仍属于目标设计，而不是既有事实。

## 8.1.2 SemVer 规则

在当前 pre-1.0 阶段，采用：

- `0.MINOR.PATCH`

规则：

1. `MINOR` 可包含 breaking changes
2. `PATCH` 只用于 bugfix / 文档 / 非破坏性小改动
3. 任何会影响 CLI surface、API、config、schema 的明显变更，都不应伪装成 patch

在 1.0 以后，再切换到标准语义：

- MAJOR：breaking
- MINOR：backward-compatible feature
- PATCH：bugfix

## 8.1.3 Prerelease 规则

允许的 prerelease 形态：

- `-alpha.N`
- `-beta.N`
- `-rc.N`

规则：

1. prerelease 必须发布到 GitHub Release，并标记为 prerelease
2. stable channel 默认不消费 prerelease
3. 只有显式 `--channel beta` 或等价 prerelease channel 时，CLI 才会把它视为候选升级目标
4. draft release 永远不是升级目标

## 8.2 兼容原则

本方案定义：

1. 同一 major 内，CLI 与 daemon 应保持兼容。
2. 同一 major 内允许短暂错位，但必须在 manifest 中声明最小兼容版本。
3. 当 daemon 低于 `minDaemonForCli` 时，CLI 必须拒绝继续执行需要 daemon 的操作，并明确提示升级。
4. 当 CLI 低于 `minCliForDaemon` 时，也必须给出结构化升级提示。
5. `upgrade --check` 对 current/latest 的判断必须使用真正的 semver compare，而不是简单字符串 compare。

例外：以下命令不应因 daemon 版本过旧而被阻断，因为它们本身就是恢复路径的一部分：

- `looper upgrade*`
- `looper version`
- `looper daemon install|start|restart|status|logs`

## 8.3 API 版本原则

管理 API 的兼容边界继续以 `/api/v1/*` 为基础。

规则：

1. `v1` 内只允许非破坏性新增。
2. 破坏性变更必须通过 `/api/v2/*` 引入。
3. 新旧 API 的重叠期不能少于两个 minor release。

即：

> **不允许通过“看起来还是 v1，实际上悄悄 break”来推进升级。**

另外，manifest 中声明的最小兼容版本只有在目标 release 正式提供该字段后才强制执行；旧 release 未声明时，CLI 采用保守兼容提示，而不是伪精确 hard-fail。

## 8.4 Schema 版本原则

Schema Version 是 daemon 与 SQLite 的兼容边界，不应再隐含在“某个版本升级可能有 migration”这种模糊表达里。

规则：

1. daemon 启动时必须知道自己支持到哪个 schema version
2. 若磁盘上的 schema version 高于当前 daemon 所支持版本，则 daemon 必须拒绝启动
3. 若磁盘上的 schema version 低于当前 daemon 所支持版本，则执行 forward migration
4. rollback / downgrade 不自动承诺 schema 回退

因此：

> **Product Version 不等于 Schema Version；升级判断必须同时考虑二者。**

因此，`upgrade --to <older-version>` 或 `--rollback` 在真正替换二进制前，必须先做 schema 兼容性预检；若目标版本不支持当前 schema，则操作必须在替换前就被拒绝。

## 8.5 `version` 命令与状态接口

为让人和机器都能看懂版本状态，最终应补齐：

- `looper version --json`
- `/api/v1/status` 中的服务版本字段

至少输出：

- product version
- api version
- schema version
- git sha
- build timestamp
- channel

版本来源判定顺序也需要冻结：

1. daemon 正在运行时，以 `/api/v1/status` 中 daemon 自报版本为准
2. daemon 未运行时，以 managed daemon binary 的 `--version` 为准
3. 若 managed daemon 不存在，则回退到 `$PATH` daemon binary
4. latest 版本一律以 GitHub Release / manifest 为准

`version --json` 的输出在同一 major 内应保持稳定，仅允许新增字段，不允许静默删字段或改字段含义。

---

## 9. 数据安全、回滚与失败恢复

## 9.1 二进制回滚

每次成功升级后都要保留：

- `looper.prev`
- `looperd.prev`

并支持：

```bash
looper upgrade --rollback
```

## 9.2 数据库备份

daemon 升级可能伴随 migration。

因此要求：

1. 升级前判断目标版本是否包含未应用 migration
2. 若包含 migration，则在升级前自动备份 DB 到 `~/.looper/backups/`
3. 备份文件命名应包含版本与时间戳
4. `package.requireBackupBeforeMigrate` 在后续 minor 中应默认切为 `true`

建议切换策略：

- 新增该能力的下一个 minor release 先支持但默认不变
- 再下一个 minor release 将默认值切到 `true`
- README / release notes 必须提前声明

## 9.3 回滚边界

回滚能力必须明确边界：

1. 二进制回滚始终支持
2. 跨 migration 的 DB 语义回滚不承诺自动完成
3. 若 schema 已升级且旧 binary 无法兼容，则 `--rollback` 必须拒绝直接切换，并提示用户从 pre-upgrade backup 恢复

同样地：

- `looper upgrade --to <older-version>` 视为 downgrade
- downgrade 适用与 rollback 相同的 migration 安全边界
- 不能把 `--to` 作为绕过数据兼容检查的后门

---

## 10. Release Workflow 变更

现有 `.github/workflows/release.yml` 已具备基础构建与发布能力；本 spec 要求在其上继续补齐：

1. 生成 `manifest.json`
2. 对 manifest 做签名并上传
3. 校验完整 asset 集是否存在
4. 增加 N / N-1 兼容 smoke
5. 区分 stable / beta channel
6. 后续补 Apple codesign / notarization
7. 由 tag 驱动正式版本注入，而不是由源码常量反向校验 tag
8. release body 补齐 Compatibility / Upgrade Notes 区块

注意：signed manifest 是完整 `upgrade` GA 的前置条件。

- 在 signed manifest 完成前，新的 `upgrade` 流程只能视为 internal preview
- 对外 GA 不应建立在 unsigned manifest 或双真相 release body 之上

## 10.0.1 版本注入原则

release workflow 需要改成：

1. 读取 `github.ref_name`（如 `v0.3.0`）
2. 解析出产品版本 `0.3.0`
3. 作为构建参数注入 CLI / daemon
4. 再产出 release artifacts 与 `manifest.json`

而不是：

1. 先读二进制里的版本
2. 再检查是否碰巧等于 tag

前者是版本体系，后者只是发布前的一次一致性碰运气检查。

## 10.1 Channel 设计

建议 channel：

- `stable`
- `beta`

规则：

- 正式 tag 默认进入 `stable`
- `-rc.*` / `-beta.*` 进入 `beta`
- `looper upgrade --channel beta` 才会消费预发布资产

`--channel` 需要明确语义：

- 默认作为单次命令参数
- 若后续需要持久化，则新增明确 config 字段，而不是隐式写入现有配置

---

## 11. 分期路线

## Milestone A - 用户可安装、可自助初始化

交付：

- `scripts/install.sh`
- `scripts/uninstall.sh`
- `looper bootstrap`
- 完整 README / install doc
- release 补齐完整 CLI + daemon asset 集

目标：

> 干净 macOS 用户可以在几分钟内完成安装、初始化、启动、验证。

## Milestone B - 用户可稳定升级与托管运行

交付：

- 完整 `looper upgrade`
- rollback
- pre-migration backup
- `launchd` 模式正式化
- Homebrew tap

## Milestone C - 用户可无摩擦信任安装

交付：

- Apple codesign
- notarization
- 可选 `.pkg`
- 更完整的 channel / release contract 文档化

---

## 12. 关键设计决策

1. **CLI 是唯一用户入口。**
   - 用户不应该单独理解 `looperd` 的安装学。

2. **CLI 安装与 daemon 安装解耦，但体验上统一。**
   - 渠道只安装 `looper`，`bootstrap` 再安装 `looperd`。

3. **manifest + 签名是升级系统的信任根。**
   - 不能长期只靠 GitHub Release assets 枚举。

3a. **GitHub Release 是正式产品版本的发布面。**
   - 用户、CLI、文档、升级检查都应围绕 GitHub Release 构建一致心智。

4. **升级必须先设计 rollback。**
   - 否则“自动升级”只是自动制造损坏。

5. **API break 只能显式升级版本。**
   - 不能把 `/api/v1` 当成可随意破坏的内部接口。

5a. **版本必须分层。**
   - Product / API / Schema / Build Metadata 各自承担各自语义。

6. **不为了追求“自动后台运行”而污染 foreground 模式。**
   - 真正托管能力明确走 `launchd`。
7. **安装来源必须影响升级行为。**
   - Homebrew / `go install` / 手动下载三条路径不能伪装成同一种可自升级安装。

---

## 13. 验收标准

当以下条件满足时，本方案可视为基本落地：

1. 新用户仅通过安装脚本或 Homebrew，即可拿到 `looper`。
2. 新用户运行 `looper bootstrap` 后，可在不阅读源码的前提下完成 daemon 安装、启动与 `looper status` 验证。
3. `looper upgrade` 可以统一升级 CLI + daemon。
4. 升级失败时有自动 rollback，且不会静默破坏当前安装。
5. release 总是包含完整、可预测、可验证的 asset 集与 manifest。
6. README / docs 与实际支持路径一致，不再出现隐藏步骤。

---

## 14. Out of scope for this spec

- 立即支持 Linux / Windows 安装矩阵
- 将 `looperd` 暴露为独立面向用户的主要安装入口
- 在本 spec 内直接重做配置模型
- 在 Milestone A 就提供 GUI installer
