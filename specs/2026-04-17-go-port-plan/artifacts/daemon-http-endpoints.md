# `looperd` daemon HTTP endpoint inventory

Source of truth inspected from:

- `apps/looperd/src/server/index.ts`

## Shared request/response behavior

- All `/api/v1/*` requests are dispatched from `createLooperdApi()` in `apps/looperd/src/server/index.ts:94-210`.
- Machine-verifiable compatibility artifact: `specs/2026-04-17-go-port-plan/artifacts/daemon-http.compat.json`.
- Machine-verifiable JSON request-body fixtures: `specs/2026-04-17-go-port-plan/artifacts/daemon-http.requests.compat.json`.
- Machine-verifiable JSON success-response fixtures: `specs/2026-04-17-go-port-plan/artifacts/daemon-http.responses.compat.json`.
- Every `/api/v1/*` request passes through `authorizeRequest()` before route dispatch (`apps/looperd/src/server/index.ts:101`, `apps/looperd/src/server/index.ts:266-285`).
- Auth behavior:
  - when `config.server.authMode !== "local-token"`, requests are accepted without auth
  - when `config.server.authMode === "local-token"`, requests must send `Authorization: Bearer <token>`
  - if local-token auth is enabled but `config.server.localToken` is missing, the daemon returns `500 AUTH_MISCONFIGURED`
  - if the bearer token is missing or wrong, the daemon returns `401 UNAUTHORIZED`
- Request correlation:
  - `x-request-id` is accepted as an optional inbound header and echoed back in the JSON envelope; otherwise a UUID is generated (`apps/looperd/src/server/index.ts:97-99`, `186-206`).
- Response envelope:
  - success: HTTP `200` with `{ ok: true, data, requestId }` (`apps/looperd/src/server/index.ts:186`)
  - error: route-specific status with `{ ok: false, error: { code, message, details? }, requestId }` (`apps/looperd/src/server/index.ts:198-206`)
- Response content type is always `application/json; charset=utf-8` (`apps/looperd/src/server/index.ts:2103-2109`).

## Endpoint inventory

### `GET /api/v1/healthz`

- Route registration: `apps/looperd/src/server/index.ts:117-120`
- Handler: `buildHealthResponse()` at `apps/looperd/src/server/index.ts:291-299`
- Returns daemon start time and storage healthcheck state.

### `GET /api/v1/status`

- Route registration: `apps/looperd/src/server/index.ts:121-124`
- Handler: `buildStatusResponse()` at `apps/looperd/src/server/index.ts:301-383`
- Returns service, storage, scheduler, loop, safety, notifications, and tool status.

### `GET /api/v1/config`

- Route registration: `apps/looperd/src/server/index.ts:125-128`
- Handler: `buildConfigResponse()` at `apps/looperd/src/server/index.ts:385-404`
- Returns the normalized runtime config snapshot.

### `GET /api/v1/events`

- Route registration: `apps/looperd/src/server/index.ts:129-132`
- Handler: `buildEventsResponse()` at `apps/looperd/src/server/index.ts:407-425`
- Query params:
  - `limit` optional
- Validation/status notes:
  - invalid `limit` returns `400 VALIDATION_FAILED`

### `GET /api/v1/events/:entityType/:entityId`

- Route registration: `apps/looperd/src/server/index.ts:133-136`
- Handler: `buildEntityEventsResponse()` at `apps/looperd/src/server/index.ts:427-453`
- Path parsing notes:
  - both `entityType` and `entityId` are required after `/events/`
  - values are URL-decoded from the path
- Validation/status notes:
  - missing params return `400 VALIDATION_FAILED`

### `GET /api/v1/pull-requests`

- Route registration: `apps/looperd/src/server/index.ts:137-140`
- Handler: `buildPullRequestsResponse()` at `apps/looperd/src/server/index.ts:455-478`
- Returns the stored pull request snapshot list.

### `GET /api/v1/pull-requests/:repo/:prNumber`

- Route registration: `apps/looperd/src/server/index.ts:141-144`
- Handler: `buildPullRequestRouteResponse()` at `apps/looperd/src/server/index.ts:1642-1689`
- Path parsing notes:
  - `repo` is URL-decoded from the path
  - `prNumber` must be a positive integer
- Validation/status notes:
  - missing `repo` or `prNumber` returns `400 VALIDATION_FAILED`
  - invalid `prNumber` returns `400 VALIDATION_FAILED`
  - unknown snapshot returns `404 PR_NOT_FOUND`

### `GET /api/v1/pull-requests/:repo/:prNumber/status`

- Route registration: same prefix handler as above; subresource branch at `apps/looperd/src/server/index.ts:1680-1682`
- Handler: `buildPullRequestStatusResponse()` at `apps/looperd/src/server/index.ts:1691-1714`
- Returns pull request review/check state plus reviewer/fixer loop status.

### `GET /api/v1/loops`

- Route registration: `apps/looperd/src/server/index.ts:145-150`
- Handler: `buildLoopsResponse()` at `apps/looperd/src/server/index.ts:480-484`
- Returns all persisted loop records.

### `POST /api/v1/loops`

- Route registration: `apps/looperd/src/server/index.ts:145-150`
- Handler: `buildLoopsCreateResponse()` at `apps/looperd/src/server/index.ts:1223-1273`
- Body fields consumed:
  - required: `projectId`, `type`, `targetType`
  - optional: `status`, `metadata`, `targetId`, `repo`, `prNumber`, `issueNumber`
- Validation/status notes:
  - missing project returns `404 PROJECT_NOT_FOUND`
  - reviewer/fixer creation without a configured coding agent returns `400 AGENT_NOT_CONFIGURED`
  - conflicting active loop returns `409 LOOP_CONFLICT`
  - malformed body/fields return `400 VALIDATION_FAILED`

### `GET /api/v1/loops/:selector`

- Route registration: prefix handler at `apps/looperd/src/server/index.ts:160-162`
- Handler: `buildLoopRouteResponse()` at `apps/looperd/src/server/index.ts:486-522`
- Selector semantics:
  - accepts either numeric loop sequence or loop id
- Validation/status notes:
  - missing selector returns `400 VALIDATION_FAILED`
  - unknown loop returns `404 LOOP_NOT_FOUND`

### `GET /api/v1/loops/:selector/logs`

- Route registration: subresource branch at `apps/looperd/src/server/index.ts:506-509`
- Handler: `buildLoopLogsResponse()` at `apps/looperd/src/server/index.ts:1419-1458`
- Returns latest run and agent log summary for the loop.

### `POST /api/v1/loops/:selector/start`

- Route registration: subresource branch at `apps/looperd/src/server/index.ts:511-514`
- Handler path: `buildLoopRouteResponse()` → `mutateLoopStatus()` (`apps/looperd/src/server/index.ts:524-556`)
- Validation/status notes:
  - reviewer/fixer start without a configured coding agent returns `400 AGENT_NOT_CONFIGURED`
  - unknown loop returns `404 LOOP_NOT_FOUND`

### `POST /api/v1/loops/:selector/pause`

- Route registration: subresource branch at `apps/looperd/src/server/index.ts:516-519`
- Handler path: `buildLoopRouteResponse()` → `mutateLoopStatus()` (`apps/looperd/src/server/index.ts:524-556`)
- Validation/status notes:
  - unknown loop returns `404 LOOP_NOT_FOUND`

### `POST /api/v1/workers`

- Route registration: `apps/looperd/src/server/index.ts:151-153`
- Handler: `buildWorkersCreateResponse()` at `apps/looperd/src/server/index.ts:615-753`
- Body fields consumed:
  - optional selectors/context: `projectId`, `repo`, `baseBranch`, `title`
  - exactly one work mode: `prompt`/`specPath`, or `prNumber`, or `issueNumber`
- Validation/status notes:
  - missing coding-agent config returns `400 AGENT_NOT_CONFIGURED`
  - invalid/missing work mode returns `400 VALIDATION_FAILED`
  - missing repo/base branch after resolution returns `400 VALIDATION_FAILED`
  - referenced project or PR lookup failures return route-specific `404` errors

### `POST /api/v1/planners`

- Route registration: `apps/looperd/src/server/index.ts:154-156`
- Handler: `buildPlannersCreateResponse()` at `apps/looperd/src/server/index.ts:755-830`
- Body fields consumed:
  - required: `projectId`
  - required effective value: `issueNumber` positive integer
- Validation/status notes:
  - missing coding-agent config returns `400 AGENT_NOT_CONFIGURED`
  - missing project returns `404 PROJECT_NOT_FOUND`
  - invalid issue number or missing project repo returns `400 VALIDATION_FAILED`

### `GET /api/v1/projects`

- Route registration: `apps/looperd/src/server/index.ts:157-159`
- Handler: `buildProjectsRouteResponse()` at `apps/looperd/src/server/index.ts:1174-1221`
- Returns serialized project records.

### `POST /api/v1/projects`

- Route registration: `apps/looperd/src/server/index.ts:157-159`
- Handler: `buildProjectsRouteResponse()` at `apps/looperd/src/server/index.ts:1174-1221`
- Body fields consumed:
  - required: `repoPath`
  - optional: `id`, `name`, `baseBranch`, `worktreeRoot`, `repo`
- Validation/status notes:
  - project management unavailable returns `500 PROJECTS_UNAVAILABLE`
  - invalid ids/body values return `400 VALIDATION_FAILED`
  - id collision returns `409 PROJECT_ID_CONFLICT`

### `GET /api/v1/runs`

- Route registration: `apps/looperd/src/server/index.ts:163-166`
- Handler: `buildRunsResponse()` at `apps/looperd/src/server/index.ts:832-843`
- Query params:
  - `loopId` optional

### `GET /api/v1/runs/active`

- Route registration: `apps/looperd/src/server/index.ts:167-170`
- Handler: `buildActiveRunsResponse()` at `apps/looperd/src/server/index.ts:899-909`
- Query params:
  - optional: `type`, `projectId`, `repo`, `prNumber`
- Validation/status notes:
  - `repo` and `prNumber` must be provided together or the route returns `400 VALIDATION_FAILED`
  - `prNumber` must be a positive integer when present

### `GET /api/v1/runs/active/:selector`

- Route registration: prefix handler at `apps/looperd/src/server/index.ts:171-176`
- Handler: `buildActiveRunRouteResponse()` at `apps/looperd/src/server/index.ts:558-595`
- Selector semantics:
  - accepts the same loop selector rules as `/api/v1/loops/:selector`
- Validation/status notes:
  - missing selector returns `400 VALIDATION_FAILED`
  - unknown loop returns `404 LOOP_NOT_FOUND`
  - missing active run for the selected loop returns `404 ACTIVE_RUN_NOT_FOUND`

### `POST /api/v1/runs/active/:selector/stop`

- Route registration: subresource branch at `apps/looperd/src/server/index.ts:578-591`
- Handler: `buildActiveRunRouteResponse()` at `apps/looperd/src/server/index.ts:558-595`
- Validation/status notes:
  - missing selector returns `400 VALIDATION_FAILED`
  - unknown loop returns `404 LOOP_NOT_FOUND`
  - runtime control unavailable in this process returns `501 RUNTIME_CONTROL_UNAVAILABLE`

## Unsupported paths and methods

- Any path not matched by the route switch returns `404 ROUTE_NOT_FOUND`.
- Methods outside the per-route allowlist return `405 METHOD_NOT_ALLOWED` via `assertMethod()` (`apps/looperd/src/server/index.ts:212-227`).
