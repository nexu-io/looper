# CSS linting

Read this file only when CSS or style files are in the changed set.

For CSS linting, prefer fixture-matrix tests over isolated one-regression tests. Consider coverage for multiple style blocks, inline styles, comments, at-rules, cascade order, custom properties, var() fallbacks, theme scopes, and px/em/rem unit handling.

Stay on concrete parse/apply bugs in the changed style files. Do not turn this into a design-system or visual-taste review.
