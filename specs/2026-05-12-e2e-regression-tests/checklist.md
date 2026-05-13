# Looper 端到端回归测试 Checklist

## Phase 0 - Scope and conventions

- [x] 明确采用三层测试：unit → contract/invariant integration → real sandbox E2E
- [x] 明确不采用两层（unit + real E2E）作为主策略
- [x] 明确 contract/invariant integration 是 PR 主防线
- [x] 明确 sandbox E2E 只用于 main/nightly/release/手动触发
- [x] 明确 deterministic E2E without real network 的边界
- [x] 创建 `internal/e2e` 测试包
- [x] 创建 `internal/e2e/harness` helper 包
- [x] 创建 `internal/e2e/githubcontract` 或等价 contract fixture 包
- [x] 明确所有 E2E 默认不访问真实网络
- [x] 明确所有 E2E 必须使用 temp HOME / temp runtime path
- [x] 明确 fake tools 通过 config 绝对路径注入，PATH 只作为补充
- [x] 明确 looperd 使用动态端口
- [x] 明确 daemon readiness 固定为 `GET /api/v1/status`
- [x] 明确 fake `gh` allowlist 来源于真实 `gh` fixture
- [x] 明确失败时必须输出 daemon logs、config、fake gh argv、fake agent cwd evidence
- [x] 为 PR #255 / #261 和 PR #194 建立 regression 注释约定
- [x] 明确 `TestMain` 构建 `looper` / `looperd` 一次并在 E2E 中复用

## Phase 0.5 - Layer responsibilities

- [x] 记录 unit test 负责纯逻辑、小状态机、小 parser、小 helper
- [x] 记录 unit test 不起 daemon、不跑真实 git repo、不依赖 fake gh executable
- [x] 记录 integration test 使用真实 Looper 内部流程
- [x] 记录 integration test 使用 strict fake 外部边界
- [x] 记录 integration test 覆盖 daemon boot、gh contract、worktree isolation、resolve-comments scenario
- [x] 记录 sandbox E2E 负责验证真实 GitHub 行为、auth/scope、rate limit、review thread mutation
- [x] 记录 sandbox E2E 不替代 integration test
- [x] 记录 P0/P1 regression 优先补 integration scenario
- [x] 记录只有真实 GitHub 行为疑点才升级为 sandbox E2E

## Phase 1 - Contract/invariant integration harness

- [x] 实现 `internal/e2e/harness/binaries.go`
- [x] 实现 `internal/e2e/harness/config.go`
- [x] 实现 `internal/e2e/harness/daemon.go`
- [x] 实现 `internal/e2e/harness/fake_agent.go`
- [x] 实现 `internal/e2e/harness/fake_gh.go`
- [x] 实现 `internal/e2e/harness/git.go`
- [x] 实现 `internal/e2e/harness/ports.go`
- [x] 实现 `internal/e2e/harness/assertions.go`
- [x] 实现 `internal/e2e/harness/temp_home.go`
- [x] 实现 temp HOME helper
- [x] 实现 isolated runtime path helper
- [x] 实现 dynamic port helper
- [x] 实现 seeded git repo helper
- [x] 实现 temp bare origin helper
- [x] 实现 git HEAD/status/index snapshot helper
- [x] 实现 assert user repo unchanged helper
- [x] 实现 assert cwd inside worktree helper
- [x] 实现 assert cwd not repo path helper
- [x] 实现 fake agent executable helper
- [x] fake agent 支持 `success-with-diff`
- [x] fake agent 支持 `success-no-diff`
- [x] fake agent 支持 `write-file`
- [x] fake agent 支持 `modify-file`
- [x] fake agent 支持 `commit`
- [x] fake agent 支持 `transient-failure`
- [x] fake agent 支持 `malformed-marker`
- [x] fake agent 支持 `timeout` / `no-marker`
- [x] fake agent 读取 `LOOPER_COMPLETION_MARKER` 或当前 executor marker 配置
- [x] fake agent 输出真实 runner 可解析的 completion JSON
- [x] fake agent 写入 `cwd-evidence.json`
- [x] 实现 strict fake gh executable helper
- [x] fake gh 支持 argv / stdin / cwd / env 记录
- [x] fake gh 从真实 fixture 加载 `--json` allowlist
- [x] fake gh 支持接近真实 `gh` 的 unsupported-field 错误输出
- [x] fake gh 支持跨进程 state 文件
- [x] fake gh 支持 strict / replay / record 模式
- [x] 实现 fake osascript helper，避免测试依赖 macOS notification 状态
- [x] 实现 looperd start/stop helper
- [x] 实现 `/api/v1/status` readiness wait helper
- [x] 实现失败时自动 dump daemon logs/config/artifacts helper

## Phase 2 - Daemon boot smoke

- [x] 添加 `TestSmokeLooperdBootsWithDefaultConfig`
- [x] 添加 `TestSmokeLooperdBootsWithRolesConfig`
- [x] 添加 `TestSmokeLooperdBootsWithUnknownConfigFields`
- [x] 添加 `TestSmokeLooperdBootsWithExplicitToolPaths`
- [x] 添加 invalid `osascript` path + enabled=true fail-fast 测试
- [x] 验证 `/api/v1/status` HTTP 200
- [x] 验证 status response 包含 pid / version / status 类稳定字段
- [x] 验证 DB path 可写
- [x] 验证 logs path 可写
- [x] 验证 backups path 可写
- [x] 验证 worktree root 可写
- [x] 验证 missing optional config 不导致启动失败
- [x] 验证 unsupported required tool path 会产生清晰启动失败
- [x] 验证 daemon 可被正常停止
- [x] 验证测试不依赖固定端口
- [x] 失败时 dump stderr、`~/.looper/logs`、config、fake gh invocation log
- [x] 将 daemon boot smoke 加入 PR 默认 CI

## Phase 3 - GitHub CLI command contract

- [x] 建立真实 `gh` fixture 目录：`internal/e2e/githubcontract/testdata/gh-schema/`
- [x] 增加 fixture 刷新脚本：`scripts/refresh-gh-fixtures.sh`
- [x] 为 `gh issue list --json` 建立 fixture-driven supported field allowlist
- [x] 为 `gh pr list --json` 建立 fixture-driven supported field allowlist
- [x] 为 `gh pr view --json` 建立 fixture-driven supported field allowlist
- [x] 为 `gh api repos/:owner/:repo/issues/:number` 建立 route contract
- [x] 为 GraphQL query / mutation 建立 contract
- [x] 添加 `TestInvariantGatewayUsesSupportedGHJSONFields`
- [x] 添加 PR #255 regression test，确保 list summary 不请求 `authorAssociation`
- [x] 添加 PR #261 regression test，确保需要 author association 时走 detail fallback
- [x] 添加读取字段必须出现在请求字段中的反向契约测试
- [x] 验证 `owner/repo` repo 形态
- [x] 验证 `github.com/owner/repo` repo 形态
- [x] 验证 `ghe.example.com/owner/repo` repo 形态
- [x] fake gh 对 unsupported `--json` field 必须 fail
- [x] fake gh 记录 argv + stdin 供失败诊断
- [x] 增加 opt-in real-gh read-only smoke
- [x] real-gh smoke 提示 fixture 是否过期
- [x] 将 `internal/infra/github/**` 变更映射到 gh contract E2E job

## Phase 4 - Worktree isolation invariant

- [x] 添加 `TestInvariantWorkerUsesIsolatedWorktreeAndLeavesUserRepoClean`
- [x] 创建真实 temp user repo 并提交初始文件
- [x] snapshot 用户 repo HEAD
- [x] snapshot 用户 repo `git status --porcelain`
- [x] snapshot 用户 repo index 状态
- [x] 在 user repo 中放置 dirty sentinel
- [x] 触发 worker 执行 fake agent `write-file`
- [x] 断言 fake agent 生成 `cwd-evidence.json`
- [x] 断言 fake agent cwd 位于 Looper worktree
- [x] 断言 fake agent cwd 不等于 user repo path
- [x] 断言 fake agent cwd 不等于 looperd 启动 cwd
- [x] 断言用户 repo 未出现 fake agent 写入文件
- [x] 断言用户 repo dirty sentinel 未被清理、覆盖、提交
- [x] 断言用户 repo HEAD/status/index 不变
- [x] 断言 worktree 中存在 fake agent 写入文件
- [x] 断言 run metadata 记录 worktree path
- [x] 添加 PR #194 fresh schedule regression test
- [x] 添加 PR #194 reused loop / active worker regression test
- [ ] 添加 worktree 被外部删除后的 restore/recreate test
- [x] 添加 checkpoint worktree path == repo path 必须 reject/recover test
- [x] 添加 agent commit/push 到隔离分支而非用户当前分支的断言
- [x] 添加 fixer worktree isolation 等价测试
- [x] 将 `internal/worker/**`、`internal/fixer/**`、`internal/api/**`、worktree 相关路径映射到 worktree E2E job

## Phase 5 - Resolve-comments scenario tests

- [x] 建立 temp bare repo as origin helper
- [x] 建立 fake GitHub cross-process state file helper
- [x] fake gh 从 bare repo 派生 PR head SHA
- [x] fake gh 支持 unresolved review threads 列表
- [x] fake gh 支持 GraphQL resolve/unresolve mutation
- [x] fake gh 支持 thread resolved/unresolved 状态变化
- [x] fake gh 支持 no-push rerun checkpoint state
- [x] fake gh 支持 closed issue/PR state
- [x] 添加 stale checkpoint head after successful push regression test
- [x] 添加 no-push rerun stale checkpoint head regression test
- [x] 添加 no-new-commit but unresolved threads remain regression test
- [x] 添加 no-diff branch before PR creation regression test
- [x] 添加 target already closed stops resumed worker/fixer regression test
- [x] 验证 GraphQL resolve mutation 被调用且 state file 状态正确
- [x] 验证失败路径不会错误进入永久 paused
- [x] 将 `internal/fixer/**`、`internal/reviewer/**` 变更映射到 resolve-comments scenario E2E job

## Phase 6 - CI integration strategy

- [x] 在 PR CI 中加入本地 E2E smoke job
- [x] 将该 job 命名/描述为 contract/invariant integration smoke
- [x] PR 默认 integration smoke 目标耗时约 60s
- [x] 所有 E2E 使用 `-count=1`
- [x] 设置 E2E job 超时
- [x] E2E job 失败时上传 logs artifact
- [x] artifact 包含 temp HOME
- [x] artifact 包含 config
- [x] artifact 包含 sqlite DB
- [x] artifact 包含 looperd logs
- [x] artifact 包含 fake gh invocation log
- [x] artifact 包含 fake agent cwd evidence
- [x] artifact 包含 bare origin refs
- [x] artifact 包含 worktree list
- [x] 添加 changed-files path filter
- [x] path filter 出错时全跑
- [x] `go.mod` / `go.sum` 命中时全跑
- [x] 为 daemon/config/runtime/cmd 变更运行 daemon boot matrix
- [x] 为 github gateway 变更运行 gh contract tests
- [x] 为 worker/fixer/reviewer/API/worktree 变更运行 worktree/resolve scenario tests
- [x] 确保 `go test ./...` 仍保留为基础检查
- [x] 确保默认 PR E2E 不依赖真实 GitHub token
- [x] 确保 sandbox 不进入普通 PR 必跑链路
- [x] 普通 PR 跑 unit tests + small integration smoke
- [x] 高风险路径 PR 跑 unit tests + targeted integration tests
- [x] main/release 跑 unit tests + integration tests + sandbox E2E

## Phase 7 - GitHub sandbox E2E

- [x] 创建或指定 sandbox repo，例如 `nexu-io/looper-sandbox`
- [x] 配置 `LOOPER_E2E_GITHUB=1` env gate
- [x] 配置 `LOOPER_E2E_SANDBOX_REPO` secret/env
- [x] 配置 `LOOPER_E2E_GITHUB_APP_PRIVATE_KEY` secret
- [x] 配置 `LOOPER_E2E_GITHUB_APP_ID` repo var
- [x] token 使用 GitHub App 或 fine-grained PAT，不使用 maintainer 个人 token
- [x] token 限制到 sandbox repo
- [x] token 最小权限包含 metadata read
- [x] token 最小权限包含 issues read/write
- [x] token 最小权限包含 pull requests read/write
- [x] token 最小权限包含 contents read/write
- [x] 定义 sandbox 测试标题/label/branch 前缀：`looper-e2e:<run-id>`
- [x] 实现测试资源清理逻辑
- [x] 实现超过 24h 资源 cleanup scheduled workflow
- [x] 添加 issue 创建与 worker trigger sandbox test
- [x] 添加 PR review comment 创建与 fixer resolve sandbox test
- [x] 添加 no-diff / no-new-commit sandbox test
- [x] 添加 auth/scope 缺失时的清晰 skip/fail 规则
- [x] 添加 rate limit / retry 策略
- [x] sandbox 失败时输出 issue/PR/branch URL
- [x] 接入 main workflow
- [x] 接入 release preflight workflow

## Phase 8 - Regression policy enforcement

- [x] 更新 PR template，要求说明是否触发 E2E/invariant 风险
- [x] 更新 code review checklist，包含 worktree、daemon boot、gh contract、resolve-comments 风险项
- [x] 规定 P0/P1 bug fix 必须包含 regression test
- [x] 规定跨组件生命周期、worktree、GitHub command、daemon boot、resolve-comments 回归优先补 integration scenario
- [x] 规定真实 GitHub 行为/auth/scope/thread mutation/rate-limit 回归补 sandbox E2E
- [x] 为没有 regression test 的 P0/P1 fix 建立 review blocker
- [x] 为历史 P0/P1 issues 建立 regression coverage tracking
- [x] 记录每个 regression test 对应的 PR/issue 编号

## Phase 9 - One-week minimum rollout

- [x] 完成 E2E harness skeleton
- [x] 完成 daemon boot smoke：default config
- [x] 完成 daemon boot smoke：roles config
- [x] 完成 daemon boot smoke：explicit fake tools config
- [x] 完成 daemon boot smoke：invalid osascript fail-fast
- [x] 完成 gh contract：fixture-driven allowlist
- [x] 完成 gh contract：unsupported `--json` fail
- [x] 完成 gh contract：`gh api` route
- [x] 完成 gh contract：反向字段契约
- [x] 完成 worktree invariant：fresh schedule
- [x] 完成 worktree invariant：worker reuse
- [x] 完成 worktree invariant：fake agent cwd evidence
- [x] 完成 worktree invariant：用户 repo HEAD/status 不变
- [x] 完成 worktree invariant：bad checkpoint reject
- [x] Stretch：完成 resolve-comments stale-head-after-push 场景

## Phase 10 - Verification

- [x] 运行 `go test ./internal/e2e -count=1`
- [x] 运行 `go test ./internal/e2e/githubcontract -count=1`
- [x] 运行 `go test ./...`
- [x] 运行 `go vet ./...`
- [x] 运行 `go build ./...`
- [x] 手动验证 daemon boot smoke 能在本机稳定通过
- [x] 手动验证 worktree invariant 能在模拟回归时失败
- [x] 手动验证 gh contract test 能在请求 unsupported field 时失败
- [x] 手动验证 fake agent malformed/no-marker 场景不会让测试误通过
- [x] 手动验证 CI path filter 只触发相关 E2E job
- [x] 手动验证 path filter 失败时 fallback 全跑
