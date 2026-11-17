# Comment quality

Every comment MUST include: (1) a location via inline anchor or exact file/section/symbol reference, (2) the concrete problem, (3) why it matters, (4) evidence from the changed lines or spec section, and (5) a specific suggested change.

Every finding MUST include: (1) disposition/severity/scopeBasis/scopeEvidence, (2) an exact file/section/symbol reference, (3) the concrete problem, (4) why it matters, (5) evidence from the changed lines or spec section, and (6) a specific suggested change.

Use a short priority-prefixed title and one concise paragraph. Keep the title imperative and at most 80 characters including the prefix, for example `[P2] Preserve coverage for clean reviews`. In structured findings, put this in `title` and only the explanatory paragraph in `body`. Native inline comments have no title field: start their `body` with `**[P2] Preserve coverage for clean reviews**`, then a blank line and the paragraph.

Priority describes urgency, separately from the existing disposition and merge severity:

- **P0 — critical:** immediate action; a release-blocking or widespread failure, severe data loss, or security exposure that does not depend on speculative assumptions or unusual inputs.
- **P1 — high:** fix in the next cycle; a serious regression or broken core workflow under realistic, identified conditions. State those conditions instead of implying universal impact.
- **P2 — normal:** a concrete defect or requirement gap with limited impact that should be fixed through normal work.
- **P3 — low:** a minor improvement, wording/style issue, or nit. Do not inflate these to P2 just to fit a shorter scale.

Choose priority from demonstrated impact and urgency, not from the amount of explanation or effort to fix. P2 can still have `severity=blocking` when it is an in-scope correctness defect; priority is not a replacement for `blocking|non_blocking|nit` or `must_fix|follow_up|needs_human`. Keep all supported in-scope findings regardless of priority.

Aim for 2–4 sentences, usually 50–100 words or equivalent in the review language: concrete trigger/condition → faulty behavior and consequence → specific fix direction. Include only the evidence needed to establish that chain. Add detail only when necessary to make the issue actionable; never remove the trigger or impact just to meet a length target. Keep investigation notes, rejected hypotheses, long code excerpts, and repeated metadata out of the published prose. The requirements above are content requirements, not separate Problem/Impact/Evidence/Fix sections. Keep disposition and scope fields in structured output; cite a repository rule in prose only when it explains the finding. The inline anchor supplies the location unless the relevant symbol or actual fault lies elsewhere.

Do not repeat the overall body/summary as a comment; comments must add distinct actionable feedback.

A comment is invalid if it only names a category (for example, 'gaps around X', 'issues with Y', or 'concerns about Z'), says only 'add tests' without naming the behavior and where the test belongs, lacks a concrete location or section reference, asks a question without proposing a resolution path, or compresses multiple unrelated concerns into one vague summary.

Bad comment example: 'Spec review found actionable gaps around role-specific trigger schema, auto-discovery gating boundaries, and exact env/config-source behavior.' This is bad because it has no file, line, section, concrete missing requirement, evidence, or suggested wording.

Good implementation comment example:

**[P2] Preserve coverage for clean reviews**

When a native review reports coverage with no findings, this branch skips serializing the completion because it checks only the findings count. The saved checkpoint therefore loses any reported coverage limitations for clean reviews. Persist the completion when coverage is present, even if the findings list is empty.

Good spec/docs comment example:

**[P2] Define the role trigger schema**

The Role triggers section introduces role-specific triggers but omits their fields, defaults, and validation rules. Implementers cannot determine how an omitted condition or invalid trigger should behave, so implementations can disagree while appearing to satisfy the spec. Add a schema table and one valid and one invalid example to this section.

For complex linting/parsing logic, prefer recommending fixture-matrix tests over isolated one-regression tests.
