# Architecture decision records

This directory holds the design decisions that shape Behemoth. Each decision lives in its own numbered file. The goal is simple: when you (or a new contributor) ask "why is it built this way?", the answer is written down, including the options that were rejected and the conditions under which the decision should change.

These instructions apply to every developer and contributor.

## Style

Behemoth records decisions as **Architecture Decision Records (ADRs)** in the Nygard/MADR style. The content follows the [Google developer documentation style guide](https://developers.google.com/style) (active voice, present tense, one idea per sentence, sentence-case headings). See [`docs_style_guide.md`](../../docs_style_guide.md) for the Behemoth-specific rules.

Behemoth adds three sections to the standard format: **Decision drivers**, **Invariants**, and **Revisit when**. See [Why these additions](#why-these-additions).

## When to write an ADR

Write an ADR when a decision meets any of these conditions:

- It is hard or expensive to reverse (for example, a file format, a public API shape, or a database schema).
- It affects more than one package or plugin.
- You considered more than one reasonable option.
- A future reader is likely to ask "why didn't we just do X?"
- It establishes a rule that other code depends on (an invariant).

Don't write an ADR for routine implementation choices, bug fixes, or decisions that are obvious from the code.

## Conventions

- **One decision per file.** Keep each file in this directory, named `NNNN-short-title.md` (for example, `0007-ledger-snapshot-atomicity.md`). Numbers are sequential and never reused.
- **Immutable once accepted.** Never rewrite the substance of an accepted decision. Add a new record that supersedes it, and update the old record's status to `Superseded by NNNN`. You may fix typos and broken links in an accepted record.
- **Status values:** `Proposed`, `Accepted`, `Rejected`, `Deprecated`, `Superseded by NNNN`.
- **Write for a reader with no context.** That reader might be you in a year or a new contributor. Don't write "as discussed." Spell out the reasoning.
- **Name the cost of the chosen option.** A record that lists only benefits isn't trustworthy.
- **Record rejected options too.** Rejected ideas are the part most easily forgotten, and the part that stops someone from proposing them again.
- **Link, don't copy.** Link to code, issues, and other ADRs instead of pasting their contents.
- **Submit it with the change.** Include the ADR in the same pull request as the code that implements the decision, or in an earlier pull request with status `Proposed`.

## Template

Copy this into a new file named `NNNN-short-title.md`.

```markdown
# NNNN. Decision title (a short noun phrase or verb phrase)

- **Status:** Proposed | Accepted | Superseded by NNNN
- **Date:** YYYY-MM-DD
- **Deciders:** names or roles
- **Affects:** packages or components, for example `core/migration`

## Context and problem

What situation forces a decision now? State the problem, not the solution.
Include the constraints and the current behavior. Link to the relevant code,
issues, or earlier decisions.

## Decision drivers

The criteria that matter, in priority order. For example:
- Never leave schema, ledger, and snapshot out of sync.
- Don't touch a developer's database unless they opt in.
- Keep the plugin API small.

## Options considered

### Option A: name
Short description.
- **Pros:** ...
- **Cons:** ...
- **Fails when:** the concrete scenario in which this option breaks.

### Option B: name
...

## Decision

We chose Option X.   (Use "we" here; ADRs are the one place it reads naturally.)

## Rationale

Why X beats the others against the decision drivers. Address each rejected
option by name and say why it lost.

## Consequences

- **Gains:** ...
- **Costs and risks we accepted:** ...
- **Follow-up work:** issue links.

## Invariants (optional)

Rules that must stay true because of this decision, and what breaks if they don't.

## Revisit when

Concrete, observable triggers, not "someday." For example:
- A second database driver cannot provide full atomicity.
- Migration files regularly exceed N operations.
- Users ask for a one-step generate-and-apply flow.

## References

Links to code, issues, prior ADRs, external articles.
```

## Why these additions

Behemoth's template extends the standard Nygard/MADR sections with the following.

- **"Decision drivers"** makes the criteria explicit and ranked. When two options both look reasonable, the ranking explains why one won, and lets a later reader check whether the priorities have changed.
- **"Fails when" on each option** forces a concrete reason an option was rejected. This is usually the detail you forget first, and the one that stops the same option from being re-proposed.
- **"Invariants"** suits Behemoth, where many decisions are rules that code elsewhere depends on (for example, "foreign keys are never inline in a create-table operation"). Recording what breaks if the rule is violated protects the rule from well-meaning refactors.
- **"Revisit when"** records the assumptions the decision depends on. Later, you can check whether those assumptions still hold, instead of guessing whether the decision is stale.

## Reference ADRs from code

Link code to its decision with a comment in this exact form:

```go
// See ADR-0007.
```

Add the reference where the decision shows up in the code, especially where the code looks surprising, restrictive, or inefficient. For example:

```go
// PathGenerateOnly is the default: Behemoth doesn't touch a developer's
// database unless they explicitly choose PathManaged. See ADR-0003.
cfg.Path = PathGenerateOnly
```

```go
// Foreign keys are always separate operations, never inline in
// CreateTable, to avoid circular table references. See ADR-0005.
bare.ForeignKeys = nil
```

Follow these rules:

- **Use the format `ADR-NNNN`** with four digits, so you can find every reference with a text search (`grep -r "ADR-0007" .`).
- **Reference several records** by listing them: `See ADR-0003, ADR-0005.`
- **Keep the comment short.** The comment states the constraint or the effect in one sentence, and the ADR holds the reasoning. Don't paste rationale into the comment.
- **Put ADR references in comments on unexported code and in internal docs.** Don't put them in doc comments on exported identifiers. API and website docs must stand alone, so state the observable behavior there and leave out the reference.
- **Update references when a decision is superseded.** When you supersede an ADR, search for its number and update the code comments that point to it, or add a pointer to the new record.

ADR references replace vague comments such as "per the earlier discussion" or "as decided previously," which a reader can't follow. A reference gives a stable, searchable link to the full reasoning.

## Index

Keep this list current, newest last.

| ADR | Title | Status |
|---|---|---|
| [0001](0001-reporting-a-hook-refusal.md) | Report a hook chain's error through one helper, with one code | Accepted |
| [0002](0002-deriving-external-jwt-keys.md) | Derive the signing keys of external JWTs from the master secret | Accepted |
| [0003](0003-remote-jwt-verification-and-outbound-calls.md) | Verify remote JWTs in `crypto` over a key source, and call other services through one client | Proposed |
| [0004](0004-carrying-the-caller-in-the-context.md) | Carry the caller of an operation in the context | Proposed |
| [0005](0005-treating-a-missing-caller-as-anonymous.md) | Treat a call without a caller as anonymous | Proposed |
| [0006](0006-requiring-declared-permissions.md) | Require a declared permission in every self-service flow and admin operation | Proposed |
| [0007](0007-layering-the-packages.md) | Lay out the packages in layers, with the plugin contracts in `core` | Proposed |
| [0008](0008-renewing-the-session-token-on-promotion.md) | Renew the session token when a pending session becomes active | Proposed |
| [0009](0009-recording-the-factors-of-a-session.md) | Record on each session the factors that authenticated it | Accepted |
| [0010](0010-bounding-concurrent-password-hashes.md) | Bound the password hashes that run at once in the hasher | Proposed |
