# ADR-0012: SQLite project records are runtime Authority

## Context

Looper currently represents Projects twice: normalized `[[projects]]` entries and SQLite `projects` rows. Runtime modules resolve Provider, repository, project policy, scheduling eligibility, and webhook behavior from `config.Projects`, while CLI/API additions and archival mutate SQLite. A Project can therefore be active in one representation and absent or differently bound in the other.

The concrete failures are incorrect Provider routing, stale Projects continuing to schedule after archival, and API-added Projects being treated as GitHub because their binding is absent from the startup configuration.

## Decision

SQLite project records are the runtime Authority for Project existence and repository/Provider binding.

`[[projects]]` remains a compatibility input. At startup Looper transactionally imports it into SQLite, then materializes a Project Catalog snapshot from active SQLite records. Scheduler, Webhook, and Roles receive the materialized view through the existing normalized project configuration interface. They do not consume the original project list after import.

Every explicit Project mutation validates the complete candidate Catalog before committing SQLite. After commit it publishes that prevalidated snapshot with one atomic replacement. A scheduler pass, claimed run, or accepted webhook delivery captures one snapshot and keeps it for the operation; later operations observe the new snapshot.

Project-level policy from a `[[projects]]` entry is applied only to the matching `source=config` record during materialization. API-owned records use stored fields and global defaults.

## Trade-off

This prevents configuration, storage, Scheduler, and Webhook from disagreeing about which Projects exist or which Provider/repository they use.

The cost is startup materialization, atomic snapshot publication, explicit handling for legacy rows with incomplete metadata, and persisted project-level policy that was previously present only in configuration. Import or validation failures can now block a mutation/startup, already-started work may finish on the prior snapshot, and tests must assert the Project Catalog rather than raw configuration input.

A simpler live mirror from SQLite back into `config.Projects` is insufficient: PR #528 demonstrated that mutation requires synchronization across Scheduler work, webhook forwarding, removal, rebinding, and shutdown. Keeping configuration as the sole Authority is also insufficient because CLI/API-managed Projects are durable product behavior.

## Authority

The Authority for a runtime Project is the active SQLite project record committed by configuration import or an explicit CLI/API project mutation; remote detection is only input to that commit and is never Authority by itself.

## Consequences

- Runtime Project consumers use one atomically published view and capture a stable snapshot per operation.
- Configured Projects continue to work without an immediate configuration-format break.
- Removing a `source=config` Project from `[[projects]]` archives it during the next startup import; `source=api` rows are never removed by config import.
- A config Project ID collision with an API-owned row fails import instead of changing ownership.
- This authority-bearing change requires `@oracle` review before merge.
