# CLI / Daemon 分发与安装方案

## 1. 背景

当前 Looper 的发布与安装方式存在一个明显问题：

- `@powerformer/looper` 通过 npm 发布后，只能自然安装 `looper` CLI
- `looperd` 仍然要求用户从仓库源码启动
- 这导致 README 中的安装流程割裂：
  - 先 `npm install -g @powerformer/looper`
  - 再 `git clone && bun install && bun run dev`

这种体验不适合作为正式产品安装路径。

同时，`looperd` 本身深度依赖 Bun 运行时能力：

- `bun:sqlite`
- `Bun.serve()`
- `Bun.which()`
- `Bun.argv`

因此“继续把 daemon 当作普通 npm JS 脚本发布”并不是最自然的分发模型。

---

## 2. 目标

本方案目标是定义 Looper 的推荐发布模型，使用户安装路径清晰、运行时要求明确、后续 release 过程可自动化。

目标：

1. `looper` CLI 有简单、标准的安装与升级方式。
2. `looperd` daemon 不再要求用户从源码启动。
3. `looperd` 安装过程不再要求用户预先理解 Bun workspace / repo 结构。
4. 当前阶段先支持 macOS release 与自动下载。
5. 避免把平台相关的 daemon 发布复杂度强行塞进 npm CLI 包。

非目标：

1. 本次不把 `looperd` 全量迁移到 Node 兼容运行时。
2. 本次不要求 `looper` CLI 也必须变成 single binary。
3. **Phase 1 明确不实现** auto-update、Homebrew、Windows service 等完整安装生态。

## 2.1 执行分期约定

本文中的高层 phase 用于描述整体路线；实际实施顺序与任务拆分以同目录下的 `checklist.md` 为准。

换言之：

> **checklist 是执行层 source of truth；spec 提供架构与决策说明。**

---

## 3. 结论

## 3.1 推荐分发模型

推荐采用：

> **`looper` 继续通过 npm 发布；`looperd` 通过 Bun `--compile` 产出的平台二进制发布。**

即：

- CLI：`npm install -g @powerformer/looper`
- daemon：GitHub Releases 二进制 / 安装脚本 / 后续由 CLI 自动下载

这是当前最平衡的方案。

## 3.2 为什么不是“一个 npm 包同时安装 CLI + daemon”

虽然技术上可以让 npm 包同时带 `looper` 与 `looperd`，但不推荐作为长期方案，原因如下：

1. `looperd` 是 Bun-native 程序，不是普通 Node CLI。
2. 如果继续走 npm 包，用户仍需在机器上安装 Bun 才能运行 daemon。
3. 如果想在 npm 包中直接塞 compiled daemon，就会变成 per-platform 包管理问题：
   - `darwin-arm64`
   - `darwin-x64`
4. 这会显著增加 npm publish 与安装逻辑复杂度。

所以：

> **npm 负责分发 CLI；daemon 改用独立的二进制发布通道。**

## 3.3 为什么不建议 CLI 也默认编译成 single binary

`looper` CLI 技术上也可以用 Bun `--compile` 变成单文件可执行程序，但当前不建议把它作为主发布形态。

原因：

1. `looper` 当前本质上是轻量 HTTP client。
2. CLI 已经是 Node 兼容实现，不依赖 Bun 特性。
3. npm 安装与升级体验更好：
   - `npm install -g`
   - `npm update -g`
4. 如果改成 binary，需要同时维护 CLI 的 macOS 构建矩阵，收益有限。

结论：

- **CLI 可以编译，但不作为默认主分发方式。**
- **daemon 很适合编译，并应优先走 binary 分发。**

## 3.4 版本策略

CLI 与 daemon 在当前阶段应**共享同一个版本号**，并从**同一个 git tag** 发布。

例如：

- git tag: `v0.2.0`
- npm CLI: `0.2.0`
- GitHub Release daemon binaries: `0.2.0`

这样可以显著降低：

- 版本错位理解成本
- upgrade 逻辑复杂度
- 兼容矩阵维护成本

在项目规模仍较小时，不建议将 CLI 与 daemon 版本解耦。

当前建议把版本 single source of truth 固定在**发布 tag / CLI package version**，并由 daemon 构建过程读取同一版本值注入产物。

---

## 4. 候选方案比较

## 4.1 方案 A：npm CLI + compiled daemon（二进制）

### 结论

**推荐。**

### 形态

- `@powerformer/looper`：仅发布 `looper`
- `looperd`：按平台单独发布 binary

### 优点

1. CLI 安装/升级沿用 npm 生态。
2. daemon 不再要求用户额外安装 Bun。
3. 平台差异仅集中在 daemon 发布链路。
4. 用户模型清晰：
   - CLI 用包管理器安装
   - daemon 用 release 安装

### 缺点

1. CLI 与 daemon 成为两个分发通道。
2. 需要处理版本匹配和安装引导。
3. 需要建设 release workflow。

## 4.2 方案 B：CLI 与 daemon 都走 compiled binary

### 结论

**可行，但不推荐作为主方案。**

### 优点

1. 用户机器无需 Node / Bun。
2. “下载即运行”体验统一。

### 缺点

1. CLI 与 daemon 都需要平台构建矩阵。
2. 升级与 PATH 安装流程都要自建。
3. release 产物数翻倍。
4. CLI 侧收益不明显。

## 4.3 方案 C：npm 包同时承载 JS CLI + daemon artifact

### 结论

**不推荐。**

### 原因

1. npm 包天然不适合承载一组 per-platform daemon 二进制。
2. 需要额外的 postinstall / optionalDependencies / 平台包拆分设计。
3. 复杂度高于收益。

---

## 5. 推荐的目标用户体验

推荐的最终安装路径：

```bash
npm install -g @powerformer/looper
looper daemon install
looper daemon start
```

推荐的最终升级路径：

```bash
looper upgrade
```

说明：

- `looper daemon install`
  - 自动识别当前 macOS 架构
  - 下载对应的 `looperd` binary
  - 放到 `~/.looper/bin/looperd`
- `looper daemon start`
  - 启动已安装的 daemon
- `looper upgrade`
  - 默认检查并升级 CLI 与 daemon 到最新版本
  - CLI 通过 npm 升级
  - daemon 通过 GitHub Releases binary 升级

在 Phase 1 尚未实现自动下载前，也可以先采用过渡方案：

```bash
npm install -g @powerformer/looper
# 用户手动从 GitHub Releases 下载 looperd
looperd
```

---

## 6. 对 `looperd` 使用 Bun `--compile` 的设计

## 6.1 可行性结论

`looperd` 适合使用 Bun single executable：

```bash
bun build --compile src/index.ts --outfile looperd
```

这会把 Bun runtime 一起打进可执行程序，避免目标机器再安装 Bun。

## 6.2 macOS 构建矩阵

需要按 macOS 架构分别构建：

- `darwin-arm64`
- `darwin-x64`

当前一期仅覆盖：

- `darwin-arm64`
- `darwin-x64`

Linux 不进入当前实施范围。

换言之：

> **当前一期只支持 macOS，不支持 Linux。**

## 6.3 不采用“单一 universal binary”假设

本方案明确不依赖“一份 binary 通吃所有平台”的前提。

实际发布模型是：

> **每个受支持的 macOS 架构各有一份 looperd artifact。**

---

## 7. 当前最大的技术约束：SQLite migration 资源

## 7.1 问题

当前 migration 通过运行时文件系统扫描与读取 `.sql` 文件实现。

例如：

- `apps/looperd/src/storage/sqlite/migrate.ts`
- `apps/looperd/src/runtime/index.ts`

这类实现依赖：

- `readdirSync(migrationsDir)`
- `readFileSync(join(migrationsDir, fileName))`
- 相对 `import.meta.url` 推导 migrations 目录

对普通源码 / dist 目录是可行的，但对 `--compile` 场景并不稳妥。

## 7.2 推荐决策

推荐把 migrations 从“运行时目录资源”改为“编译期内嵌资源”。

即，把每个 SQL migration 编译进 TS 模块，例如：

```ts
export const SQLITE_MIGRATIONS = [
  {
    id: "0001_init",
    fileName: "0001_init.sql",
    sql: `...`,
  },
];
```

这里不再保留多种实现分支，直接固定方案：

1. 现有 `.sql` 文件继续保留，作为 migration 的 source of truth
2. 增加一个 build-time/codegen 脚本，从 `.sql` 生成 TS 模块，例如 `migrations.gen.ts`
3. `migrate.ts` 运行时只从生成模块读取 migration 列表与 SQL 内容
4. 生成文件不手写维护

这意味着需要同时替换当前两类运行时文件系统依赖：

1. migration 列表扫描
2. 单个 migration SQL 文件读取

也就是说，不仅要替换：

- `readAvailableMigrations()` 中的目录扫描

还要替换：

- `runPending()` 中基于文件路径的 `readFileSync(...)`

## 7.3 为什么推荐内嵌，而不是继续依赖文件路径

内嵌 migration 的优势：

1. binary 真正自包含。
2. 避免 compiled executable / dist / source 三套路径分支继续膨胀。
3. release 与本地运行路径语义统一。
4. 更容易测试与调试。

结论：

> **把 migration 改为内嵌资源，是支持 compiled daemon 的首要工程步骤。**

---

## 8. 升级流程设计

## 8.1 顶层升级命令

推荐新增顶层命令：

```bash
looper upgrade
```

Phase 1 推荐只保证下面两个入口：

```bash
looper upgrade --check
looper upgrade --daemon
```

完整的：

```bash
looper upgrade
```

保留为后续 phase。

默认语义（最终形态）仍然是：

> **升级整个 Looper 安装，而不是只升级某一个组件。**

即 `looper upgrade` 默认同时处理：

- `looper` CLI
- `looperd` daemon

## 8.2 为什么升级命令应放在顶层

不推荐只提供 `looper daemon upgrade`，因为这只覆盖 daemon，而用户真正想表达的是：

> “把 Looper 升级到最新版本。”

因此升级入口应是顶层：

- `looper upgrade`

而不是只做：

- `looper daemon upgrade`

后者可以保留为内部子能力或兼容别名，但不应作为主 UX。

## 8.3 命令行为

### `--check`

只检查，不执行写操作。

这是最推荐最先落地的版本，因为：

1. 不改动本地安装状态
2. 可以先验证版本发现链路
3. 可以先验证 npm registry / GitHub Releases plumbing

### `--daemon`

只升级 daemon。

这是第二个应优先落地的入口，因为 daemon 的 binary 下载替换是本方案里收益最高的一步。

### 默认行为（后续 phase）

`looper upgrade` 执行顺序建议为：

1. 检查当前 CLI 版本
2. 检查当前 daemon 版本
3. 检查 npm registry 中最新 CLI 版本
4. 检查 GitHub Releases 中最新 daemon 版本
5. 输出将要升级的内容
6. 先升级 daemon
7. 再升级 CLI
8. 输出 restart 指引

推荐输出示意：

```txt
Checking for updates...

  CLI     @powerformer/looper  0.1.0 → 0.2.0
  Daemon  looperd              0.1.0 → 0.2.0

Upgrading daemon... ✓
Upgrading CLI... ✓

Restart the daemon to use the new version:
  looper daemon restart
```

## 8.4 当前版本与最新版本的来源

### CLI 当前版本

建议在 CLI build 时内嵌版本常量，而不是运行时调用 `npm list -g` 推断。

### CLI 最新版本

从 npm registry 获取，例如：

- `npm view @powerformer/looper version`

### daemon 当前版本

优先级建议：

1. daemon 正在运行时：通过 `/api/v1/status` 返回的 `service.version` 获取
2. daemon 未运行但 binary 已安装时：执行本地 `looperd --version`
3. 未安装时：视为 `not installed`

其中 `looperd --version` 必须在 daemon bootstrap 之前短路返回，不能走完整配置加载、路径校验、数据库初始化与服务启动流程。

### daemon build metadata

当前约定：

1. `looperd --version` 只输出语义化版本号，保证脚本调用与 CLI fallback 易于解析
2. `/api/v1/status` 额外暴露 `service.build`，用于返回构建元数据
3. `service.build` 当前至少包含：
   - `versionSource`：当前版本号来自哪个 source of truth（现阶段固定为 `apps/cli/package.json`）
   - `gitCommitSha`：若 release/build 流程注入则返回 commit sha，否则为 `null`
   - `buildTimestamp`：若 release/build 流程注入则返回构建时间，否则为 `null`
4. 这些字段由 `apps/looperd/scripts/generate-artifacts.ts` 在构建时生成到 `apps/looperd/src/generated/version.ts`

### daemon 最新版本

从 GitHub Releases REST API 获取，不依赖 `gh` CLI。

## 8.5 升级动作的职责划分

### CLI 升级（后续 phase）

CLI 仍通过 npm 升级：

```bash
npm install -g @powerformer/looper@latest
```

### daemon 升级

daemon 升级通过 release binary 下载完成：

1. 根据 macOS 架构选择 artifact
2. 下载到临时文件，例如 `~/.looper/bin/looperd.new`
3. 校验 SHA-256 checksum
4. 原子替换为 `~/.looper/bin/looperd`

## 8.6 哪些动作可以自动化，哪些不应该自动化

### 可以自动化

1. 检查最新版本
2. 检测平台和架构
3. 下载 daemon binary
4. 校验 daemon 的 SHA-256 checksum
5. 原子替换 daemon binary
6. 通过 npm 升级 CLI（后续 phase）

### 不应自动化

**daemon restart 不应默认自动执行。**

升级完成后应输出：

```bash
looper daemon restart
```

由用户在合适时机执行。

## 8.7 关键边界情况

### daemon 尚未安装

`looper upgrade` 遇到 daemon 未安装时，不应失败，而应等价于安装最新 daemon。

### daemon 未运行

如果 daemon 未运行，则通过本地安装 binary 的 `--version` 获取当前版本；若本地 binary 也不存在，则视为未安装。

### CLI 正在升级自身

CLI 升级应放在 daemon 升级之后执行。

### 网络或权限失败

升级命令必须是**可重复执行**的：

1. daemon 下载失败不能留下半写入文件
2. npm 全局安装失败要给出清晰错误信息
3. 二次执行应能安全恢复

### major version 升级

若未来采用更严格的兼容策略，可考虑对 major version 升级增加确认或 `--force`。该点不是 Phase 1 必需，但建议预留。

## 8.8 推荐的分阶段实现

### Phase A - 版本基础设施

需要先补齐：

1. CLI 版本常量
2. daemon 版本常量
3. `looper --version`
4. `looperd --version`（bootstrap 前短路）
5. `/api/v1/status` 返回真实 daemon version

### Phase B - `looper upgrade --check`

先实现只读版本比较：

1. 检查当前 CLI / daemon 版本
2. 检查最新 CLI / daemon 版本
3. 输出 diff

### Phase C - `looper upgrade --daemon`

在 release workflow 与 binary 发布稳定后，实现 daemon 自动下载安装与升级。

### Phase D - `looper upgrade`

在 daemon 升级稳定后，再实现默认同时升级 CLI 与 daemon 的完整流程。

### Phase E - 后续增强

后续可选增强：

1. `--pre`
2. 每日一次的升级提示
3. 更细粒度的版本兼容检查
4. 可选的 `daemon restart` 交互确认

---

## 9. CLI 与 daemon 的版本关系

## 9.1 目标原则

CLI 与 daemon 在共享版本号前提下，仍应允许**短暂轻度版本错位**，前提是 HTTP API 兼容。

原因：

1. CLI 通过 npm 更新。
2. daemon 通过 release / 本地安装更新。
3. 两者不必强制完全同版本，但必须有兼容约束。

## 9.2 兼容策略

建议：

1. 所有现有管理接口继续挂在 `/api/v1/*`
2. 不在 minor release 中做破坏性协议变更
3. 如需破坏性变更，显式升级到 `/api/v2/*`

初期不做复杂的 semver 握手协议，只需保证：

- CLI 新版尽量兼容同主版本 daemon
- daemon 在 `status` 返回里可暴露版本信息，供后续提示使用

---

## 10. 分阶段落地方案

## 10.1 Phase 1：回收 npm 包职责

目标：让 npm 包重新只负责 CLI。

动作：

1. `@powerformer/looper` 只发布 `looper` bin。
2. 去掉 npm 包中对 `looperd` 与 migrations 的承载。
3. README 改成：
   - CLI 通过 npm 安装
   - daemon 暂时通过 Releases 或源码方式获取

交付结果：

- 发布模型恢复清晰边界
- 不再把 daemon 的 macOS binary 分发问题塞进 npm 包

## 10.2 Phase 2：为 `looperd` 增加 compile 构建

目标：本地与 CI 可产出 daemon binary。

动作：

1. 在 `apps/looperd/package.json` 增加 `compile` 脚本
2. 增加按 target 的平台构建脚本
3. 先覆盖 macOS，后续补 Linux

交付结果：

- 可以在 CI 中稳定生成 `looperd` binary
- compile 产物至少能够成功执行 `--version`；完整 daemon 运行依赖 Phase 3 完成

补充约定：

> **compile 是 release artifact 能力，不是本地开发主流程依赖。**

也就是说：

1. `bun run dev` / `bun run build` / `bun run test` 不应依赖 `--compile`
2. 本地已知存在 Bun `--compile` 环境问题时，可以在 compile 脚本中 fail-fast，避免静默产出不可用 binary
3. 是否可发布，以 release workflow 在受控 macOS runner 上的 smoke test 为准，而不是要求每台开发机本地 compile 都成功

## 10.3 Phase 3：migration 内嵌化

目标：让 `looperd` compile 产物真正自包含。

动作：

1. 新增 codegen 脚本，由 `.sql` 生成 `migrations.gen.ts` 或等价模块
2. `migrate.ts` 改为从生成模块读取 migration 列表与 SQL 内容
3. 删除对运行时 migration 目录的主路径依赖

交付结果：

- compiled binary 无需额外 `.sql` 文件目录

## 10.4 Phase 4：release workflow

目标：自动发布 daemon binary。

动作：

1. 增加 tag 驱动 release workflow
2. matrix 构建 `looperd`
3. 上传 GitHub Release artifacts
4. 生成 checksum 等下载校验元数据
5. npm publish CLI

交付结果：

- CLI 与 daemon 拥有各自稳定发布通道

## 10.4.1 为什么 release workflow 是关键前置条件

release workflow 不是可有可无的附属项，而是这条分发方案的关键前置条件。

原因：

1. `looperd` 采用 compiled binary 分发
2. `looper daemon install` 需要稳定的 release artifact
3. `looper upgrade --daemon` / `looper upgrade` 需要自动发现并下载最新 binary
4. checksum 校验也依赖 workflow 产出

因此：

> **没有稳定的 GitHub Release workflow，这套 dual-channel 分发方案就无法真正闭环。**

## 10.4.2 workflow 最小职责

release workflow 至少需要承担下面职责：

1. **按 tag 触发**
   - 例如 `v0.2.0`
2. **matrix 构建 `looperd`**
   - 至少支持 `darwin-arm64`
   - 至少支持 `darwin-x64`
3. **上传 release artifacts**
   - 产物名必须稳定、可预测
4. **生成并上传 checksum**
   - 供 `looper upgrade` 校验
5. **发布 npm CLI**
   - 发布 `@powerformer/looper`
6. **产出可编程消费的下载元数据**
   - Phase 1 至少保证 artifact 命名规则固定
   - manifest JSON 可作为后续增强，而不是当前前置条件

## 10.4.3 产物命名约定

建议明确稳定命名规则，例如：

- `looperd-darwin-arm64`
- `looperd-darwin-x64`
- `looperd-darwin-arm64.sha256`
- `looperd-darwin-x64.sha256`

如后续需要带版本号，也应保证规则稳定，例如：

- `looperd-v0.2.0-darwin-arm64`

关键不是具体样式，而是：

> **CLI 必须能在不依赖脆弱字符串猜测的前提下，稳定定位 release asset。**

## 10.4.4 关于 manifest

manifest 不是当前 phase 的必需条件。

Phase 1 可以直接依赖：

1. 稳定的 artifact 命名规则
2. GitHub Releases REST API

当前 release workflow 已显式验证：

1. Release assets 名称固定为 `looperd-darwin-{arm64,x64}` 与对应 `.sha256`
2. Phase 1 不发布 Linux artifacts
3. tag / checksum / `--version` 均在 workflow 内校验

因此在进入 `looper daemon install` / `looper upgrade` 实现前，现有发布元数据已经足以支撑 CLI 定位下载目标，当前不补 manifest。

只有当后续发现单纯依赖 release asset 命名不足以支撑安装/升级逻辑时，再补 manifest。

## 10.4.5 推荐的 workflow 分层

建议拆成两个逻辑阶段：

### 阶段 A - daemon release artifacts

负责：

1. compile `looperd`
2. 上传 binaries
3. 上传 checksums
4. 如有需要，再上传 manifest

### 阶段 B - npm publish

负责：

1. 发布 `@powerformer/looper`
2. 保证 CLI 版本与当前 tag 对齐

这样即使未来需要单独重跑 daemon 构建，也更容易定位问题。

## 10.4.6 workflow 需要验证的内容

release workflow 除了“能跑完”，还应验证：

1. tag 与 npm version 一致
2. compiled daemon 能成功执行 `--version`
3. checksum 与实际 artifact 一致
4. Release 页面上的 artifact 名称符合 CLI 预期
5. CLI 能根据最新 release 正确定位下载目标
6. macOS target 使用 macOS runners，而不是假设从 Linux cross-compile 到 macOS

补充说明：

> **在 Phase 1/2，compiled daemon 的最终可用性判断应以后述 smoke validation 为准。**

因此可以接受：

1. 某些本地开发环境对 `bun --compile` 存在已知问题
2. compile 脚本对这些环境直接报错并提示使用受控 runner 或显式 override

但不能接受：

1. 本地产出一个看似成功、实际执行会挂死的 binary
2. release workflow 未验证 `looperd --version` 就发布 artifact

## 10.5 Phase 5：CLI 安装助手与升级命令

目标：把 dual-channel 安装体验收敛到 CLI 命令里。

建议新增：

- `looper daemon install`
- `looper daemon start`
- `looper daemon restart`
- `looper upgrade`

交付结果：

- 用户仍然只需记住 `looper` 命令
- daemon 二进制下载细节被 CLI 封装
- 升级入口也被统一封装

---

## 11. 需要的代码与仓库改动

## 11.1 `apps/cli`

需要：

1. 保持 npm 发布路径简洁
2. 后续新增 daemon install / start / restart 命令
3. CLI 构建继续保持 Node-compatible
4. 新增顶层 `upgrade` 命令
5. 支持版本检查、平台识别、下载与 npm 升级桥接
6. 明确 daemon binary 查找顺序：`~/.looper/bin/looperd` → `$PATH` → 报错

不建议：

1. 默认把 CLI 改成 compiled binary 主发布物
2. 在 npm 包中继续塞 `looperd` compiled artifact

## 11.2 `apps/looperd`

需要：

1. 增加 `bun build --compile` 构建脚本
2. 增加 per-platform target 构建
3. 将 migration 改为内嵌资源
4. 暴露 daemon version / build metadata
5. 支持 `--version`

## 11.3 `.github/workflows`

需要新增 release workflow，负责：

1. npm publish CLI
2. compile daemon
3. 上传 macOS artifacts
4. 生成 SHA-256 校验信息
5. 保证 `looper upgrade` 所需下载元数据稳定可预测
6. 按 tag 自动触发 release
7. 校验 artifact 命名与 manifest 输出符合约定

补充约束：

1. macOS artifacts 需要在 macOS runners 上构建
2. 不假设通过 Linux runner cross-compile 出可用的 macOS binary
3. 当前 release workflow 不包含 Linux artifacts

---

## 12. README 与文档调整建议

README 的安装部分应改为下面结构：

1. 安装 CLI：

```bash
npm install -g @powerformer/looper
```

2. 安装 daemon：

```bash
looper daemon install
```

3. 启动 daemon：

```bash
looper daemon start
```

Phase 1 如未实现稳定的进程管理模型，则 README 应先使用更保守的手动运行说明，而不是承诺复杂的后台托管行为。

4. 升级：

```bash
looper upgrade
```

5. 验证连接：

```bash
looper status
looper daemon status
```

在 `looper upgrade` 尚未落地前，README 应暂时使用手动升级说明或 `--check` 预览说明。

---

## 13. 风险与注意事项

## 13.1 release 运维复杂度提升

与单纯 npm publish 相比，compiled daemon 会新增：

- macOS 构建矩阵
- release artifact 管理
- checksum / 版本命名

但这部分复杂度是合理且必要的，因为 daemon 本身就不是天然的“纯 JS 可直接分发脚本”。

## 13.1.1 compiled binary 体积

`bun build --compile` 产物会包含 Bun runtime，因此 daemon binary 体积会明显大于普通 JS 文件分发。

当前实测（本仓库 `apps/looperd/scripts/compile.ts` 默认产物）：

- `looperd-darwin-arm64`：约 **58.9 MiB**
- `looperd-darwin-x64`：约 **64.1 MiB**

因此当前 phase 可接受的预期范围可先定为：

- **darwin-arm64 / darwin-x64 单个 artifact 预期约 59–64 MiB**
- **只要单个 release binary 未明显超过 70 MiB，就仍视为可接受**

这会影响：

- GitHub Release 上传/下载时间
- `looper daemon install` 的等待体验
- 本地 `~/.looper/bin/` 占用空间

因此安装/升级命令后续应考虑加入清晰的下载进度或至少输出明确状态。

## 13.2 CLI / daemon 版本错位

如果 release 节奏不同，可能出现：

- 新 CLI 对旧 daemon
- 旧 CLI 对新 daemon

需要通过稳定的 `/api/v1` 与状态接口版本信息来控制风险。

## 13.3 migration 内嵌改造不能半做

如果仍然同时维护：

- source 路径
- dist 路径
- compiled 路径

会继续堆积路径探测逻辑。

因此建议尽快统一到“内嵌 migration”模型。

## 13.4 升级过程中的部分成功状态

`looper upgrade` 可能出现部分成功，例如：

- daemon 已升级，但 CLI npm 升级失败
- CLI 已升级，但 daemon 下载失败

因此升级流程必须具备：

1. 幂等性
2. 临时文件 + 原子替换
3. 清晰的结果汇总输出
4. 可重复执行恢复

## 13.5 macOS Gatekeeper / code signing

从 GitHub Releases 下载的未签名 macOS binary 可能触发 Gatekeeper。

这意味着：

1. Phase 1 可能需要文档中提供手动放行指引
2. 更长期应评估 code signing

否则首次安装体验会受到影响。

## 13.6 `looper daemon start` 的进程管理模型

`looper daemon start` 不是一个纯文案命令，它隐含了进程托管模型选择：

- 前台 exec
- 后台 detached spawn
- PID file
- launchd / systemd

当前 spec 不把完整服务管理器集成纳入 Phase 1。

因此建议：

> **Phase 1 先以手动运行 daemon 或最小化启动方式为主，不承诺完整服务托管。**

---

## 14. 最终结论

本方案的最终结论是：

> **Looper 应采用“双通道分发”：CLI 继续走 npm，daemon 改走 compiled binary。**

并且：

> **不建议把 CLI 默认也切到 single binary；但 daemon 应优先切到 single binary。**

近期最值得做的工程动作顺序是：

1. 让 npm 包重新只负责 CLI
2. 为 `looperd` 建立 compile 构建链路
3. 把 SQLite migrations 改成内嵌资源
4. 建立 release workflow
5. 最后优先在 CLI 中补 `daemon install`、`upgrade --check`、`upgrade --daemon`
6. 等 daemon 分发稳定后，再补完整 `looper upgrade` 与更强的 `daemon start/restart`

这条路径在质量、可维护性、用户体验、发布成本之间最均衡。
