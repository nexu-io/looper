# CSS linting

Read this file when the change set includes CSS parsing or linting logic, including implementations in Go, TypeScript, or other non-style files. Ordinary CSS-only edits do not need this guidance.

For CSS linting, prefer fixture-matrix tests over isolated one-regression tests. Consider coverage for multiple style blocks, inline styles, comments, at-rules, cascade order, custom properties, var() fallbacks, theme scopes, and px/em/rem unit handling.

Stay on concrete parse/apply bugs in the changed parser, linter, or style files. Do not turn this into a design-system or visual-taste review.
