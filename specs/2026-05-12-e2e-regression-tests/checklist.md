# Looper 端到端回归测试 Checklist

## Phase 0 - Scope and conventions

- [ ] 明确采用三层测试：unit → contract/invariant integration → real sandbox E2E
- [ ] 明确不采用两层（unit + real E2E）作为主策略
- [ ] 明确 contract/invariant integration 是 PR 主防线
- [ ] 明确 sandbox E2E 只用于 main/nightly/release/手动触发
- [ ] 明确 deterministic E2E without real network 的边界
- [ ] 创建 `internal/e2e` 测试包
- [ ] 创建 `internal/e2e/harness` helper 包
- [ ] 创建 `internal/e2e/githubcontract` 或等价 contract fixture 包
- [ ] 明确所有 E2E 默认不访问真实网络
- [ ] 明确所有 E2E 必须使用 temp HOME / temp runtime path
- [ ] 明确 fake tools 通过 config 绝对路径注入，PATH 只作为补充
- [ ] 明确 looperd 使用动态端口
- [ ] 明确 daemon readiness 固定为 `GET /api/v1/status`
- [ ] 明确 fake `gh` allowlist 来源于真实 `gh` fixture
- [ ] 明确失败时必须输出 daemon logs、config、fake gh argv、fake agent cwd evidence
- [ ] 为 PR #255 / #261 和 PR #194 建立 regression 注释约定
- [ ] 明确 `TestMain` 构建 `looper` / `looperd` 一次并在 E2E 中复用

## Phase 0.5 - Layer responsibilities

- [ ] 记录 unit test 负责纯逻辑、小状态机、小 parser、小 helper
- [ ] 记录 unit test 不起 daemon、不跑真实 git repo、不依赖 fake gh executable
- [ ] 记录 integration test 使用真实 Looper 内部流程
- [ ] 记录 integration test 使用 strict fake 外部边界
- [ ] 记录 integration test 覆盖 daemon boot、gh contract、worktree isolation、resolve-comments scenario
- [ ] 记录 sandbox E2E 负责验证真实 GitHub 行为、auth/scope、rate limit、review thread mutation
- [ ] 记录 sandbox E2E 不替代 integration test
- [ ] 记录 P0/P1 regression 优先补 integration scenario
- [ ] 记录只有真实 GitHub 行为疑点才升级为 sandbox E2E

## Phase 1 - Contract/invariant integration harness

- [ ] 实现 `internal/e2e/harness/binaries.go`
- [ ] 实现 `internal/e2e/harness/config.go`
- [ ] 实现 `internal/e2e/harness/daemon.go`
- [ ] 实现 `internal/e2e/harness/fake_agent.go`
- [ ] 实现 `internal/e2e/harness/fake_gh.go`
- [ ] 实现 `internal/e2e/harness/git.go`
- [ ] 实现 `internal/e2e/harness/ports.go`
- [ ] 实现 `internal/e2e/harness/assertions.go`
- [ ] 实现 `internal/e2e/harness/temp_home.go`
- [ ] 实现 temp HOME helper
- [ ] 实现 isolated runtime path helper
- [ ] 实现 dynamic port helper
- [ ] 实现 seeded git repo helper
- [ ] 实现 temp bare origin helper
- [ ] 实现 git HEAD/status/index snapshot helper
- [ ] 实现 assert user repo unchanged helper
- [ ] 实现 assert cwd inside worktree helper
- [ ] 实现 assert cwd not repo path helper
- [ ] 实现 fake agent executable helper
- [ ] fake agent 支持 `success-with-diff`
- [ ] fake agent 支持 `success-no-diff`
- [ ] fake agent 支持 `write-file`
- [ ] fake agent 支持 `modify-file`
- [ ] fake agent 支持 `commit`
- [ ] fake agent 支持 `transient-failure`
- [ ] fake agent 支持 `malformed-marker`
- [ ] fake agent 支持 `timeout` / `no-marker`
- [ ] fake agent 读取 `LOOPER_COMPLETION_MARKER` 或当前 executor marker 配置
- [ ] fake agent 输出真实 runner 可解析的 completion JSON
- [ ] fake agent 写入 `cwd-evidence.json`
- [ ] 实现 strict fake gh executable helper
- [ ] fake gh 支持 argv / stdin / cwd / env 记录
- [ ] fake gh 从真实 fixture 加载 `--json` allowlist
- [ ] fake gh 支持接近真实 `gh` 的 unsupported-field 错误输出
- [ ] fake gh 支持跨进程 state 文件
- [ ] fake gh 支持 strict / replay / record 模式
- [ ] 实现 fake osascript helper，避免测试依赖 macOS notification 状态
- [ ] 实现 looperd start/stop helper
- [ ] 实现 `/api/v1/status` readiness wait helper
- [ ] 实现失败时自动 dump daemon logs/config/artifacts helper

## Phase 2 - Daemon boot smoke

- [ ] 添加 `TestSmokeLooperdBootsWithDefaultConfig`
- [ ] 添加 `TestSmokeLooperdBootsWithRolesConfig`
- [ ] 添加 `TestSmokeLooperdBootsWithUnknownConfigFields`
- [ ] 添加 `TestSmokeLooperdBootsWithExplicitToolPaths`
- [ ] 添加 invalid `osascript` path + enabled=true fail-fast 测试
- [ ] 验证 `/api/v1/status` HTTP 200
- [ ] 验证 status response 包含 pid / version / status 类稳定字段
- [ ] 验证 DB path 可写
- [ ] 验证 logs path 可写
- [ ] 验证 backups path 可写
- [ ] 验证 worktree root 可写
- [ ] 验证 missing optional config 不导致启动失败
- [ ] 验证 unsupported required tool path 会产生清晰启动失败
- [ ] 验证 daemon 可被正常停止
- [ ] 验证测试不依赖固定端口
- [ ] 失败时 dump stderr、`~/.looper/logs`、config、fake gh invocation log
- [ ] 将 daemon boot smoke 加入 PR 默认 CI

## Phase 3 - GitHub CLI command contract

- [ ] 建立真实 `gh` fixture 目录：`internal/e2e/githubcontract/testdata/gh-schema/`
- [ ] 增加 fixture 刷新脚本：`scripts/refresh-gh-fixtures.sh`
- [ ] 为 `gh issue list --json` 建立 fixture-driven supported field allowlist
- [ ] 为 `gh pr list --json` 建立 fixture-driven supported field allowlist
- [ ] 为 `gh pr view --json` 建立 fixture-driven supported field allowlist
- [ ] 为 `gh api repos/:owner/:repo/issues/:number` 建立 route contract
- [ ] 为 GraphQL query / mutation 建立 contract
- [ ] 添加 `TestInvariantGatewayUsesSupportedGHJSONFields`
- [ ] 添加 PR #255 regression test，确保 list summary 不请求 `authorAssociation`
- [ ] 添加 PR #261 regression test，确保需要 author association 时走 detail fallback
- [ ] 添加读取字段必须出现在请求字段中的反向契约测试
- [ ] 验证 `owner/repo` repo 形态
- [ ] 验证 `github.com/owner/repo` repo 形态
- [ ] 验证 `ghe.example.com/owner/repo` repo 形态
- [ ] fake gh 对 unsupported `--json` field 必须 fail
- [ ] fake gh 记录 argv + stdin 供失败诊断
- [ ] 增加 opt-in real-gh read-only smoke
- [ ] real-gh smoke 提示 fixture 是否过期
- [ ] 将 `internal/infra/github/**` 变更映射到 gh contract E2E job

## Phase 4 - Worktree isolation invariant

- [ ] 添加 `TestInvariantWorkerUsesIsolatedWorktreeAndLeavesUserRepoClean`
- [ ] 创建真实 temp user repo 并提交初始文件
- [ ] snapshot 用户 repo HEAD
- [ ] snapshot 用户 repo `git status --porcelain`
- [ ] snapshot 用户 repo index 状态
- [ ] 在 user repo 中放置 dirty sentinel
- [ ] 触发 worker 执行 fake agent `write-file`
- [ ] 断言 fake agent 生成 `cwd-evidence.json`
- [ ] 断言 fake agent cwd 位于 Looper worktree
- [ ] 断言 fake agent cwd 不等于 user repo path
- [ ] 断言 fake agent cwd 不等于 looperd 启动 cwd
- [ ] 断言用户 repo 未出现 fake agent 写入文件
- [ ] 断言用户 repo dirty sentinel 未被清理、覆盖、提交
- [ ] 断言用户 repo HEAD/status/index 不变
- [ ] 断言 worktree 中存在 fake agent 写入文件
- [ ] 断言 run metadata 记录 worktree path
- [ ] 添加 PR #194 fresh schedule regression test
- [ ] 添加 PR #194 reused loop / active worker regression test
- [ ] 添加 worktree 被外部删除后的 restore/recreate test
- [ ] 添加 checkpoint worktree path == repo path 必须 reject/recover test
- [ ] 添加 agent commit/push 到隔离分支而非用户当前分支的断言
- [ ] 添加 fixer worktree isolation 等价测试
- [ ] 将 `internal/worker/**`、`internal/fixer/**`、`internal/api/**`、worktree 相关路径映射到 worktree E2E job

## Phase 5 - Resolve-comments scenario tests

- [ ] 建立 temp bare repo as origin helper
- [ ] 建立 fake GitHub cross-process state file helper
- [ ] fake gh 从 bare repo 派生 PR head SHA
- [ ] fake gh 支持 unresolved review threads 列表
- [ ] fake gh 支持 GraphQL resolve/unresolve mutation
- [ ] fake gh 支持 thread resolved/unresolved 状态变化
- [ ] fake gh 支持 no-push rerun checkpoint state
- [ ] fake gh 支持 closed issue/PR state
- [ ] 添加 stale checkpoint head after successful push regression test
- [ ] 添加 no-push rerun stale checkpoint head regression test
- [ ] 添加 no-new-commit but unresolved threads remain regression test
- [ ] 添加 no-diff branch before PR creation regression test
- [ ] 添加 target already closed stops resumed worker/fixer regression test
- [ ] 验证 GraphQL resolve mutation 被调用且 state file 状态正确
- [ ] 验证失败路径不会错误进入永久 paused
- [ ] 将 `internal/fixer/**`、`internal/reviewer/**` 变更映射到 resolve-comments scenario E2E job

## Phase 6 - CI integration strategy

- [ ] 在 PR CI 中加入本地 E2E smoke job
- [ ] 将该 job 命名/描述为 contract/invariant integration smoke
- [ ] PR 默认 integration smoke 目标耗时约 60s
- [ ] 所有 E2E 使用 `-count=1`
- [ ] 设置 E2E job 超时
- [ ] E2E job 失败时上传 logs artifact
- [ ] artifact 包含 temp HOME
- [ ] artifact 包含 config
- [ ] artifact 包含 sqlite DB
- [ ] artifact 包含 looperd logs
- [ ] artifact 包含 fake gh invocation log
- [ ] artifact 包含 fake agent cwd evidence
- [ ] artifact 包含 bare origin refs
- [ ] artifact 包含 worktree list
- [ ] 添加 changed-files path filter
- [ ] path filter 出错时全跑
- [ ] `go.mod` / `go.sum` 命中时全跑
- [ ] 为 daemon/config/runtime/cmd 变更运行 daemon boot matrix
- [ ] 为 github gateway 变更运行 gh contract tests
- [ ] 为 worker/fixer/reviewer/API/worktree 变更运行 worktree/resolve scenario tests
- [ ] 确保 `go test ./...` 仍保留为基础检查
- [ ] 确保默认 PR E2E 不依赖真实 GitHub token
- [ ] 确保 sandbox 不进入普通 PR 必跑链路
- [ ] 普通 PR 跑 unit tests + small integration smoke
- [ ] 高风险路径 PR 跑 unit tests + targeted integration tests
- [ ] main/nightly/release 跑 unit tests + integration tests + sandbox E2E

## Phase 7 - GitHub sandbox E2E

- [ ] 创建或指定 sandbox repo，例如 `nexu-io/looper-sandbox`
- [ ] 配置 `LOOPER_E2E_GITHUB=1` env gate
- [ ] 配置 `LOOPER_E2E_SANDBOX_REPO` secret/env
- [ ] 配置 `LOOPER_E2E_GITHUB_TOKEN` secret
- [ ] token 使用 GitHub App 或 fine-grained PAT，不使用 maintainer 个人 token
- [ ] token 限制到 sandbox repo
- [ ] token 最小权限包含 metadata read
- [ ] token 最小权限包含 issues read/write
- [ ] token 最小权限包含 pull requests read/write
- [ ] token 最小权限包含 contents read/write
- [ ] 定义 sandbox 测试标题/label/branch 前缀：`looper-e2e:<run-id>`
- [ ] 实现测试资源清理逻辑
- [ ] 实现超过 24h 资源 cleanup scheduled workflow
- [ ] 添加 issue 创建与 worker trigger sandbox test
- [ ] 添加 PR review comment 创建与 fixer resolve sandbox test
- [ ] 添加 no-diff / no-new-commit sandbox test
- [ ] 添加 auth/scope 缺失时的清晰 skip/fail 规则
- [ ] 添加 rate limit / retry 策略
- [ ] sandbox 失败时输出 issue/PR/branch URL
- [ ] 接入 nightly workflow
- [ ] 接入 release preflight workflow

## Phase 8 - Regression policy enforcement

- [ ] 更新 PR template，要求说明是否触发 E2E/invariant 风险
- [ ] 更新 code review checklist，包含 worktree、daemon boot、gh contract、resolve-comments 风险项
- [ ] 规定 P0/P1 bug fix 必须包含 regression test
- [ ] 规定跨组件生命周期、worktree、GitHub command、daemon boot、resolve-comments 回归优先补 integration scenario
- [ ] 规定真实 GitHub 行为/auth/scope/thread mutation/rate-limit 回归补 sandbox E2E
- [ ] 为没有 regression test 的 P0/P1 fix 建立 review blocker
- [ ] 为历史 P0/P1 issues 建立 regression coverage tracking
- [ ] 记录每个 regression test 对应的 PR/issue 编号

## Phase 9 - One-week minimum rollout

- [ ] 完成 E2E harness skeleton
- [ ] 完成 daemon boot smoke：default config
- [ ] 完成 daemon boot smoke：roles config
- [ ] 完成 daemon boot smoke：explicit fake tools config
- [ ] 完成 daemon boot smoke：invalid osascript fail-fast
- [ ] 完成 gh contract：fixture-driven allowlist
- [ ] 完成 gh contract：unsupported `--json` fail
- [ ] 完成 gh contract：`gh api` route
- [ ] 完成 gh contract：反向字段契约
- [ ] 完成 worktree invariant：fresh schedule
- [ ] 完成 worktree invariant：worker reuse
- [ ] 完成 worktree invariant：fake agent cwd evidence
- [ ] 完成 worktree invariant：用户 repo HEAD/status 不变
- [ ] 完成 worktree invariant：bad checkpoint reject
- [ ] Stretch：完成 resolve-comments stale-head-after-push 场景

## Phase 10 - Verification

- [ ] 运行 `go test ./internal/e2e -count=1`
- [ ] 运行 `go test ./internal/e2e/githubcontract -count=1`
- [ ] 运行 `go test ./...`
- [ ] 运行 `go vet ./...`
- [ ] 运行 `go build ./...`
- [ ] 手动验证 daemon boot smoke 能在本机稳定通过
- [ ] 手动验证 worktree invariant 能在模拟回归时失败
- [ ] 手动验证 gh contract test 能在请求 unsupported field 时失败
- [ ] 手动验证 fake agent malformed/no-marker 场景不会让测试误通过
- [ ] 手动验证 CI path filter 只触发相关 E2E job
- [ ] 手动验证 path filter 失败时 fallback 全跑
