# Mixed-schema precedence rules

Date: 2026-05-13

Accepted mixed-schema patterns for the global config surface:

- legacy top-level `reviewer.*` alongside canonical `roles.reviewer.behavior.*`; canonical values win for overlapping effective fields
- legacy reviewer discovery paths under `roles.reviewer.{autoDiscovery,triggers,specReview}` alongside canonical `roles.reviewer.discovery.*`; canonical values win for overlapping effective fields
- legacy alias fields `defaults.allowAutoApprove` and `defaults.fixAllPullRequests` alongside canonical reviewer/fixer targets; canonical values win for overlapping effective fields
- environment variables and CLI flags still override every file-backed value, whether the file uses legacy, canonical, or mixed inputs

Rejected mixed-schema patterns:

- any mixed input that gives the same canonical target an incompatible structural shape, such as a scalar where the canonical subtree expects an object
- example: combining legacy `reviewer.*` with `roles.reviewer.behavior` set to a scalar instead of an object must fail during config decoding instead of guessing how to merge

Implementation note:

- deprecation guidance is emitted once per accepted legacy logical path so mixed inputs stay deterministic without silently hiding the canonical replacement path
