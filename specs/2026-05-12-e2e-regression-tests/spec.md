# Looper 端到端回归测试方案

## 1. 背景

近期多次回归绕过了现有单测和 code review：

- PR #255 引入了真实 `gh` 不支持的 `--json authorAssociation` 字段，导致 `looperd` 无法正常启动，随后由 PR #261 修复。
- PR #194 改变 issue worker 复用语义后，导致 worktree 创建不可靠，agent 可能直接在用户当前仓库 `cwd` 修改代码，污染用户工作区。
- 多个 `resolve-comments` 回归与 stale PR head、no-push rerun、无新 commit 但仍有 unresolved threads 等跨组件状态有关。

这些问题的共同点是：现有测试主要验证包内函数或宽松 mock 行为，缺少覆盖真实二进制、真实 git repo、真实 `gh` 命令契约、daemon 启动与 runner 隔离边界的端到端安全不变量。

本 spec 定义一套小而关键的三层测试体系，用于防止 P0/P1 回归进入 main：

```text
unit test
  ↓
contract / invariant integration test（真实内部流程 + strict fake 外部边界）
  ↓
real sandbox E2E（真实 GitHub sandbox）
```

其中第二层是主力防线：它不是宽松 mock test，而是 **deterministic E2E without real network**。真实 sandbox E2E 只负责验证 Looper 对真实 GitHub 行为的理解没有偏差。

---

## 2. 目标

1. 明确 Looper 的三层测试职责：unit、contract/invariant integration、sandbox E2E。
2. 用少量高价值 integration / E2E 覆盖 Looper 的核心安全不变量。
3. 在 PR CI 中快速发现：daemon 无法启动、真实 `gh` 命令不兼容、agent 写入用户 cwd、worktree 缺失等致命问题。
4. 为 GitHub mutation 和 comment resolution 建立可选的 sandbox E2E，供 main/release/手动触发前运行。
5. 让每个 P0/P1 regression fix 都能留下可复现的失败用例。
6. 避免把 E2E 做成庞大、慢、不稳定的全流程系统测试。

## 3. 非目标

1. 不替代现有单测；单测仍负责细粒度逻辑。
2. 不要求所有 PR 都访问真实 GitHub 并执行写操作。
3. 不要求在第一阶段覆盖所有角色的完整 happy path。
4. 不引入 Docker 作为必需依赖；优先使用 Go test、temp HOME、temp git repo、fake agent、strict fake gh。

---

## 4. 测试分层

Looper 采用三层测试，而不是两层。原因是：

- 只有 unit + real E2E 时，unit 太贴近实现，防不住跨组件回归；real E2E 又太慢、太贵、太不稳定，不能作为 PR 主防线。
- 中间的 contract / invariant integration 层使用真实 Looper 内部流程和严格 fake 外部边界，能高频、确定性地覆盖 #255、#194 和 resolve-comments 类回归。

### 4.0 Unit test：细粒度逻辑防线

职责：验证纯逻辑、小状态机、小 parser、小 helper。

适合覆盖：

- config merge / normalize / validate。
- failure classification / resume policy。
- queue state transition / retry backoff。
- prompt marker parsing。
- `gh` response parsing。
- worktree path helper。

约束：

- 不起 daemon。
- 不跑真实 git repo。
- 不需要 fake `gh` executable。
- 失败应能定位到单个函数或小模块。

### 4.1 Contract / invariant integration：PR 主防线

职责：用真实 Looper 内部流程 + strict fake 外部边界，验证用户可见不变量。这层也可以称为 “deterministic E2E without real network”。

这层不依赖真实 GitHub 写权限，只使用本地资源：

- `TestMain` 构建出的真实 `looper` / `looperd` 二进制，供同包测试复用。
- 临时 `HOME`、临时 `~/.looper`、临时 config、动态端口。
- 临时真实 git repo，以及需要 push/head 变化时使用的 temp bare repo origin。
- fake agent 可执行文件，用于模拟 agent 修改文件、输出 completion marker、返回成功/失败。
- strict fake `gh` 可执行文件，用于验证 GitHub CLI/API/GraphQL 命令契约。

适合覆盖：

- daemon boot smoke。
- fake `gh` contract。
- worktree isolation。
- fake agent cwd evidence。
- resolve-comments stale head / no-push / no-diff。
- loop reuse / checkpoint restore。
- config/runtime path compatibility。

建议入口：

```sh
go test ./internal/e2e -run 'TestSmoke|TestInvariant' -count=1
```

PR 默认 integration smoke 目标耗时控制在 60 秒左右；更深的 path-filter integration 可以放宽到 1-2 分钟。

### 4.2 PR 条件跑：高风险路径 integration

当 PR 修改以下路径时，额外运行更窄但更深的 E2E：

- `cmd/looper/**`
- `cmd/looperd/**`
- `internal/agent/**`
- `internal/api/**`
- `internal/config/**`
- `internal/daemon/**`
- `internal/fixer/**`
- `internal/infra/github/**`
- `internal/loops/**`
- `internal/reviewer/**`
- `internal/runtime/**`
- `internal/scheduler/**`
- `internal/storage/**`
- `internal/sweeper/**`
- `internal/worker/**`
- `internal/worktree*/**`
- `pkg/**`
- `go.mod`
- `go.sum`

规则：

- 修改 `internal/infra/github/**`：运行 strict gh contract tests。
- 修改 `internal/worker/**`、`internal/fixer/**`、`internal/api/**` 或 worktree 相关路径：运行 worktree isolation invariant。
- 修改 `internal/config/**`、`internal/runtime/**`、`cmd/looperd/**`：运行 daemon boot matrix。
- 修改 `internal/fixer/**` 或 `internal/reviewer/**`：运行 resolve-comments scenario tests。
- `go.mod` / `go.sum` 命中或 path filter 失败时，全量运行本地 E2E。

### 4.3 Real sandbox E2E：真实 GitHub 现实校验

这层使用专门 sandbox 仓库，例如 `nexu-io/looper-sandbox`，允许创建 issue、PR、review comment、resolve thread。

职责：验证 Looper 对真实 GitHub 行为、auth/scope、rate limit、review thread mutation 的理解没有偏差。

不适合：

- 每个 PR 必跑。
- 覆盖大量细分状态机。
- 替代 contract / invariant integration。

建议入口：

```sh
LOOPER_E2E_GITHUB=1 go test ./internal/e2e/github -run TestGitHubSandbox -count=1
```

触发策略：

- main 合并后。
- nightly。
- release cut 前。
- 手动 workflow dispatch。

普通 PR 不应被 sandbox E2E 阻塞。

### 4.4 CI 分层策略

```text
普通 PR:
  unit tests
  + small contract/invariant integration smoke

高风险路径 PR:
  unit tests
  + targeted contract/invariant integration tests

main / nightly / release:
  unit tests
  + contract/invariant integration tests
  + real sandbox E2E
```

---

## 5. Contract / invariant integration 核心不变量

### 5.1 Daemon 必须能启动

测试名称建议：`TestSmokeLooperdBootsWithDefaultAndRoleConfigs`

覆盖：

1. 使用 temp HOME 和动态端口启动真实 `looperd`。
2. 使用默认 config 启动。
3. 使用包含 roles 配置的 config 启动。
4. 使用包含未知旧字段的 config 启动，确认兼容。
5. 使用显式 fake tool paths 启动：`tools.ghPath`、`tools.gitPath`、`tools.osascriptPath`。
6. 固定 readiness 为 `GET /api/v1/status`：HTTP 200，且返回 pid / version / status 类稳定字段。
7. 确认 runtime paths 可写：DB、logs、backups、worktree root。
8. 关闭 daemon，确认进程正常退出。

验收：

- daemon 在 15 秒内 ready。
- stderr/log 中没有 config validation fatal、unsupported gh field、missing runtime path 等致命错误。
- 失败时必须 dump：looperd stderr、`~/.looper/logs`、config、fake gh invocation log。

### 5.2 Agent 永远不能在用户 cwd 修改代码

测试名称建议：`TestInvariantWorkerUsesIsolatedWorktreeAndLeavesUserRepoClean`

覆盖：

1. 创建 temp git repo，提交初始文件，并记录 `HEAD`、`git status --porcelain`、index 状态。
2. 在用户 repo 中放置 dirty sentinel 文件或未提交修改。
3. 配置 Looper project 指向该 repo。
4. 触发 worker/fixer 执行，fake agent 尝试写文件、修改文件、提交。
5. fake agent 写入 `cwd-evidence.json`，记录实际 `cwd`、argv、关键 env。
6. 断言 fake agent cwd 位于 Looper worktree 下，且不等于用户 repo、不等于启动 `looperd` 的 cwd。
7. 断言用户 repo 的 `HEAD`、`git status --porcelain`、dirty sentinel、index 状态未被 Looper 改动。
8. 断言 Looper worktree 路径存在且包含 agent 产生的改动。
9. 断言 run metadata / logs 记录的 working directory 是 worktree，而不是用户 repo root。

PR #194 回归必须覆盖四类路径：

- fresh schedule：无 checkpoint 时必须创建 worktree。
- reuse / active loop：已有 loop 或 worker 复用时仍必须使用 worktree。
- restore：worktree 被外部删除后必须重新创建或安全失败，不能 fallback 到用户 repo。
- bad checkpoint：checkpoint 中 worktree path 等于 repo path 时必须 reject / recover，不能继续执行 agent。

验收：

- 用户 repo 不能出现 agent 写入的文件。
- 用户 repo 原有 dirty state 不能被清理、覆盖、提交。
- worktree path 必须非空、位于 Looper runtime worktree root 下。
- agent commit/push 如发生，必须发生在隔离分支/worktree，不得写入用户当前分支。

### 5.3 GitHub CLI 命令契约必须来自真实 `gh`

测试名称建议：`TestInvariantGatewayUsesSupportedGHJSONFields`

PR #255 的根因是 mock 允许了真实 `gh` 不支持的字段，因此 fake `gh` 的 allowlist 不能手写维护。

覆盖：

1. 建立 `internal/e2e/githubcontract/` 或 `internal/e2e/testdata/gh-schema/` fixture。
2. fixture 由真实 `gh` 命令生成并 commit，提供刷新脚本，例如 `scripts/refresh-gh-fixtures.sh`。
3. fake `gh` 从 fixture 加载支持字段，而不是硬编码。
4. 对 `gh issue list --json`、`gh pr list --json`、`gh pr view --json`、`gh api repos/:owner/:repo/issues/:number`、GraphQL query/mutation 做契约校验。
5. 如果请求真实 `gh` 不支持的字段，fake `gh` 直接非零退出，并输出接近真实 `gh` 的错误。
6. 对 summary list 与 detail view 分别建 contract，避免把 detail-only 字段误用于 list。
7. 对 host-qualified repo 验证：`owner/repo`、`github.com/owner/repo`、`ghe.example.com/owner/repo`。
8. 增加反向契约：代码读取的字段必须出现在实际 `--json` 请求字段里，避免“请求字段删了但解析逻辑静默读空”。

可选 real-gh smoke：

```sh
LOOPER_E2E_REAL_GH=1 go test ./internal/e2e/githubcontract -count=1
```

该测试只读真实 `nexu-io/looper`，验证关键 `gh ... --json ...` / `gh api ...` 命令可以执行，并提示 fixture 是否过期。

### 5.4 Resolve-comments 必须覆盖状态迁移

测试名称建议：`TestScenarioResolveCommentsStaleHeadAndNoDiffPaths`

第一阶段不要用纯 fake GitHub 状态替代 git 行为。推荐模型：

```text
user repo clone/worktree
        |
        v
temp bare repo as origin
        |
        v
fake gh pr view 从 bare repo rev-parse branch 得到 head SHA
```

真实 `git` 负责 commit、push、branch head 变化；fake `gh` 负责 PR metadata、review thread 状态、GraphQL resolve/unresolve mutation、issue/PR closed 状态。

覆盖近期回归：

1. push 后 PR head SHA 变化，fixer 必须刷新 head，不能使用 stale checkpoint head。
2. no-push rerun 后 checkpoint head stale，仍能正确识别 unresolved threads。
3. 无新 commit 但仍有 unresolved threads 时，不应错误 pause。
4. no-diff branch 不应创建空 PR。
5. issue/PR 已关闭后，resumed worker/fixer 必须停止。
6. GraphQL resolve mutation 被调用后，fake gh 跨进程 state 文件中的 thread 状态必须变为 resolved。

第二阶段在 sandbox 中做真实 comment/thread mutation。

---

## 6. 测试架构

### 6.1 包结构

建议新增：

```text
internal/e2e/
  harness/
    assertions.go      # repo/worktree/cwd/log assertions
    binaries.go        # TestMain build looper + looperd once
    config.go          # isolated config writer with explicit tool paths
    daemon.go          # start/stop/readiness/log dump helpers
    fake_agent.go      # agent stub executable + cwd evidence
    fake_gh.go         # strict gh CLI/API/GraphQL stub
    git.go             # create seeded repos, bare origins, status snapshots
    ports.go           # dynamic port allocation
    temp_home.go       # isolated HOME and runtime paths
  smoke_daemon_test.go
  invariant_worktree_test.go
  github_contract_test.go
  resolve_comments_scenarios_test.go
internal/e2e/githubcontract/
  contract_test.go
  testdata/gh-schema/
internal/e2e/github/
  sandbox_test.go      # opt-in real GitHub mutation tests
scripts/refresh-gh-fixtures.sh
```

### 6.2 Harness 约束

1. 每个测试必须使用独立 temp HOME，不得读写真实 `~/.looper`。
2. fake tools 必须优先通过 config 的绝对路径注入：`tools.ghPath`、`tools.gitPath`、`tools.osascriptPath`、agent command path。PATH 只能作为补充，不能作为主机制。
3. looperd 必须使用动态端口，禁止写死端口。
4. readiness 固定为 `GET /api/v1/status`。
5. 测试结束必须清理 daemon 进程；失败时打印 daemon logs。
6. 所有外部等待必须有短超时和清晰错误信息。
7. 默认不访问网络；访问真实 GitHub 必须由 env gate 显式开启。

### 6.3 Fake agent 契约

fake agent 必须对齐真实 executor，而不只是打印 stdout：

- 读取 `LOOPER_COMPLETION_MARKER` 或当前 executor 使用的 completion marker 配置。
- 输出真实 runner 可解析的 completion JSON，包含对应路径需要的 status/result、changed files、commits、PR lifecycle 信息。
- 写入 `cwd-evidence.json`，记录 `cwd`、argv、关键 env、时间戳。
- 支持模式：
  - `success-with-diff`
  - `success-no-diff`
  - `write-file`
  - `modify-file`
  - `commit`
  - `transient-failure`
  - `malformed-marker`
  - `timeout` / `no-marker`

关键断言是 fake agent 看到的真实 cwd：测试必须从 `cwd-evidence.json` 反向取证，而不是只相信 Looper metadata。

### 6.4 Fake gh 契约

fake gh 不是宽松 mock，而是命令契约验证器：

- 从真实 `gh` fixture 加载 supported `--json` fields。
- 对 argv 做精确匹配或 schema 校验。
- 记录 argv、stdin、cwd、env 到 invocation log。
- 对 unsupported field 直接退出非零，并输出接近真实 `gh` 的错误。
- 支持 `gh issue list`、`gh pr list`、`gh pr view`、`gh api repos/:owner/:repo/issues/:number`、GraphQL query/mutation。
- 支持 host-qualified repo matrix。
- 支持跨进程 state 文件，用于 review thread、PR head、closed state 等场景推进。
- 支持 strict/replay/record 三种模式：CI 默认 strict，本地可 replay fixture，维护者可 record 刷新 fixture。

---

## 7. CI 集成

### 7.1 PR 默认 CI

在现有 CI 之后增加：

```sh
go test ./internal/e2e -run 'TestSmoke|TestInvariant' -count=1
```

要求：

- 默认不需要 GitHub token。
- job 设置明确超时。
- 失败时上传 logs artifact。
- 这是 contract / invariant integration smoke，不是 real sandbox E2E。

### 7.2 Path filter CI

新增 workflow 或 job，根据 changed files 决定运行：

- daemon/config matrix：`TestSmokeLooperdBoots*`
- worktree invariant：`TestInvariantWorkerUsesIsolatedWorktree*`
- gh contract：`TestInvariantGatewayUsesSupportedGHJSONFields*`
- resolve-comments scenarios：`TestScenarioResolveComments*`

规则：

- path filter 出错时全跑。
- `go.mod` / `go.sum` 命中时全跑。
- 所有 E2E 使用 `-count=1`。
- artifacts 至少包含：temp HOME、config、sqlite DB、looperd logs、fake gh invocation log、fake agent cwd evidence、bare origin refs、worktree list。

### 7.3 Nightly / release CI

新增 opt-in workflow：

```sh
LOOPER_E2E_GITHUB=1 go test ./internal/e2e/github -count=1
```

sandbox 要求：

- 使用专用 sandbox repo，例如 `LOOPER_E2E_SANDBOX_REPO=nexu-io/looper-sandbox`。
- 使用专用 secret，例如 `LOOPER_E2E_GITHUB_TOKEN`，不能使用 maintainer 个人 token。
- token 最小权限：metadata read、issues read/write、pull requests read/write、contents read/write，仅限 sandbox repo。
- 测试资源使用唯一 run label / 标题 / branch 前缀，例如 `looper-e2e:<run-id>`。
- scheduled cleanup 清理超过 24h 的 `looper-e2e:*` issue / PR / branch。
- sandbox 失败时输出所有资源 URL。

---

## 8. Regression policy

1. 每个 P0/P1 bug fix 必须包含一个能在修复前失败的 regression test。
2. 如果 bug 属于跨组件生命周期、worktree、GitHub command、daemon boot、resolve-comments，则优先补 contract / invariant integration scenario，而不是只补包内单测。
3. 只有真实 GitHub 行为、auth/scope、review thread mutation、rate limit 相关的疑点，才升级为 sandbox E2E。
4. Code review 必须确认：测试是否验证了用户可见不变量，而不是只验证实现细节。
5. 对真实事故建立固定 issue/PR 映射注释，例如：
   - `// Regression for PR #255 / fix PR #261`
   - `// Regression for PR #194`

---

## 9. Rollout plan

当前实现状态（2026-05-12）：

- Phase 1 已落地：`internal/e2e/harness`、fake agent、strict fake gh、temp HOME/runtime、daemon helpers 均已就位。
- Phase 2 已落地并通过：daemon boot smoke 已覆盖 default config、roles config、unknown top-level fields、explicit tool paths、missing optional sections、invalid `osascript` fail-fast，并已接入 PR 默认 CI。
- Phase 3 已落地并通过：fixture-driven gh schema（含 `issue list` / `pr list` / `pr view` allowlist）、unsupported `--json` failure、`gh api`/GraphQL contract、repo form coverage、反向字段契约、PR #261 detail fallback、opt-in real-gh smoke 已实现；并已将 `internal/infra/github/**` 映射到 gh contract E2E job。
- Phase 4 已部分落地并通过：已覆盖 fresh schedule worktree isolation、cwd evidence、user repo unchanged、isolated commit、worker reuse / active loop、bad checkpoint safe reject、fixer isolation 等价路径；仅剩 worktree restore 路径待补充或澄清产品语义。
- Phase 5 已落地：fake-gh 已支持基于 bare origin 的 PR head、thread state 持久化、resolve/unresolve mutation、closed/open target state、no-push rerun 所需状态种子；并已覆盖 stale-head-after-push、no-push-rerun-stale-head、no-new-commit-unresolved、closed-PR skip、resumed-closed-target、worker no-diff/no-PR 场景。
- Phase 6 已落地：PR 默认 `Contract/invariant integration smoke` 已接入；高风险路径使用 centralized changed-files path filter 路由到 daemon boot / gh contract / resolve-comments / worktree E2E；`go.mod` / `go.sum` 与 path-filter 失败会 fallback 全跑；E2E job 已统一 `-count=1`、超时与失败 artifact 上传（含 temp HOME、config、sqlite DB、looperd logs、fake gh invocation log、fake agent cwd evidence、bare origin refs、worktree list）；main/release 现已衔接 sandbox workflow。
- Phase 7 已落地：已创建 `nexu-io/looper-sandbox`；新增 `LOOPER_E2E_GITHUB=1` / `LOOPER_E2E_SANDBOX_REPO` / `LOOPER_E2E_GITHUB_TOKEN` 运行约定，并在 CI 侧改用 GitHub App（`LOOPER_E2E_GITHUB_APP_ID` repo var + `LOOPER_E2E_GITHUB_APP_PRIVATE_KEY` secret）通过 `actions/create-github-app-token@v3` 按需铸造 repo-scoped 短期 token；新增真实 GitHub sandbox E2E（issue→worker→PR、PR review thread→fixer resolve、worker no-diff、fixer no-new-commit）；新增 `sandbox-e2e.yml`（main + manual）、release preflight 与 24h cleanup workflow；测试命令新增 rate-limit/retry backoff；本地与 CI 均采用 non-interactive git/auth 路径，真实 sandbox suite 已通过。
- Phase 8 已落地：PR template、code review checklist、review blocker policy、历史回归映射与 regression coverage tracking 均已补齐。
- Phase 10 已落地：`go test ./internal/e2e -count=1`、`go test ./internal/e2e/githubcontract -count=1`、`go test ./...`、`go vet ./...`、`go build ./...` 已验证通过；daemon boot smoke、worktree bad-checkpoint regression、unsupported-field gh contract、agent malformed/no-marker parsing、CI path-filter fallback 均已手动验证。

### Phase 1：Integration harness skeleton

建立 `internal/e2e/harness`：`TestMain` 构建二进制、temp HOME、dynamic config/port、daemon readiness、fake gh argv log、fake agent cwd evidence。

### Phase 2：Daemon boot smoke

落地默认 config、roles config、explicit fake tools config、invalid osascript fail-fast。

### Phase 3：GitHub CLI contract

落地 fixture-driven fake gh allowlist、unsupported field failure、`gh api` route contract、读取字段必须被请求的反向契约。

### Phase 4：Worktree 安全不变量

落地 fresh schedule、worker reuse、worktree restore、bad checkpoint 四类路径，确保用户 repo 不被污染；当前仅剩 worktree restore runtime 路径待补。

### Phase 5：Resolve-comments scenario

用 temp bare origin + fake gh state 固化 stale head、no-push、no-diff、closed target 等回归路径。

### Phase 6：CI path filter

把高风险路径变更映射到对应 E2E job，并配置 artifact 上传与 fallback 全跑；当前已落地 PR smoke、centralized path filter、`go.mod`/`go.sum` 全跑、失败 artifacts，以及 main/release + sandbox 编排。

### Phase 7：GitHub sandbox E2E

建立真实 GitHub mutation 的 main/release 测试与 cleanup workflow；当前已接入 main/manual sandbox workflow、release preflight、24h cleanup workflow，并补上真实 GitHub worker/fixer/no-diff 场景、GitHub App token、non-interactive git/auth 配置与 rate-limit/retry backoff，真实 sandbox suite 已验证通过。

---

## 10. 一周内最小首批范围

首批不追求覆盖全部场景，优先最大防护价值：

1. `internal/e2e/harness` skeleton，用于 contract / invariant integration。
2. daemon boot smoke：default config、roles config、explicit fake tools、invalid osascript fail-fast。
3. GitHub CLI contract：fixture-driven allowlist、unsupported `--json` fail、`gh api` route、反向字段契约。
4. Worktree isolation invariant：fresh schedule、worker reuse、fake agent cwd evidence、用户 repo HEAD/status 不变、bad checkpoint reject。

Stretch：增加一条 resolve-comments stale-head-after-push，使用 temp bare origin + fake gh 派生 PR head。

---

## 11. 成功标准

1. PR #255 类型的 unsupported `gh --json` 字段无法通过 CI。
2. PR #194 类型的 cwd 写入/缺失 worktree 无法通过 CI。
3. `looperd` 启动失败无法通过 PR smoke。
4. 最近 resolve-comments P0/P1 回归都有固定 scenario test。
5. 默认 PR integration smoke 耗时稳定在约 60 秒，path-filter integration 稳定在 1-2 分钟内。
6. Main/release sandbox E2E 失败时能直接定位到 GitHub mutation、thread resolution 或 auth/scope 问题。
