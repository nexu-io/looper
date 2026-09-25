# Comment quality

Every comment MUST include: (1) a location via inline anchor or exact file/section/symbol reference, (2) the concrete problem, (3) why it matters, (4) evidence from the changed lines or spec section, and (5) a specific suggested change.

Every finding MUST include: (1) disposition/severity/scopeBasis/scopeEvidence, (2) an exact file/section/symbol reference, (3) the concrete problem, (4) why it matters, (5) evidence from the changed lines or spec section, and (6) a specific suggested change.

Write substantially more detail than a brief summary; every comment should explain the problem, why it matters, and the concrete change to make.

Do not repeat the overall body/summary as a comment; comments must add distinct actionable feedback.

A comment is invalid if it only names a category (for example, 'gaps around X', 'issues with Y', or 'concerns about Z'), says only 'add tests' without naming the behavior and where the test belongs, lacks a concrete location or section reference, asks a question without proposing a resolution path, or compresses multiple unrelated concerns into one vague summary.

Bad comment example: 'Spec review found actionable gaps around role-specific trigger schema, auto-discovery gating boundaries, and exact env/config-source behavior.' This is bad because it has no file, line, section, concrete missing requirement, evidence, or suggested wording.

Good spec/docs comment example: {"severity":"major","category":"spec","body":"Define the role trigger schema before implementation starts","problem":"The spec introduces role-specific triggers but does not define the schema fields or validation rules.","why":"Implementers cannot know which fields are required, how defaults behave, or how invalid trigger definitions should fail.","evidence":"The Role triggers section describes behavior but does not list fields, defaults, or invalid examples.","suggestedChange":"Add a schema table defining role, event, enabled, conditions, defaults, and validation errors, plus one valid and one invalid example.","path":"docs/reviewer.md","line":42,"side":"RIGHT"}

For complex linting/parsing logic, prefer recommending fixture-matrix tests over isolated one-regression tests.
