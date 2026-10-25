# Answering migration questions

This document plans how a developer answers the questions migration planning raises (a narrowing alter, a rename, a dropped column) without editing `draft.json` by hand, and how a run with nobody to ask behaves. It lists the front-ends that were weighed, the order in which they are built, and what the first step changes.

**Status:** steps 1 and 2 built, as of 2026-10-25. Step 3 is to decide; steps 4 to 6 are deferred. [Implementation steps](#implementation-steps) lists the work.

The documents it builds on are:

- [`../migrations/migration_design_decision.md`](../migrations/migration_design_decision.md), *Resolution stage*: the draft file is the default medium, a terminal prompt is allowed as a front-end that writes every answer through to it, and an unattended run with an open question fails closed.
- [`../migrations/cli.md`](../migrations/cli.md): the command line, and the parts of it deferred so far ("Asking the questions").
- `docs/ongoing.md`, M20: a new question is answered with its default in the run that first raises it.

## The problem

Some differences between the declared schema and the database have more than one answer. Planning turns each into a question (`PlanIssue`) and the resolution stage collects the answers. Today the only medium is the draft file:

- The developer opens `migrations/draft.json`, finds the entry, and sets `chosen` to an index counted from 0. A stale answer also needs its `stale` flag deleted.
- A new question is written with its default already chosen, and read back in the same run (M20). Every default is "Leave as-is", so the change is left out without anyone deciding it, and the run can report `No schema changes detected.` while a column still differs.
- The command can't show the questions: the draft's entry type is unexported. It prints how many answers it read and where the file is.

The people who answer are in different places:

| Who | Where | Can be asked |
| --- | --- | --- |
| A developer running `behemoth generate` | a terminal, possibly over SSH or in `docker compose run` | yes |
| A script, CI, the `docker compose up` job | no terminal | no |
| A team reviewing a pull request | the diff of `draft.json` and the migration | not by the command |
| The standalone service (planned) | a configuration file and a network | no |

## Goals

- A developer at a terminal is asked each open question, with what it would change and which answer is the default, and the answer is saved as it is given.
- A run with nobody to ask never answers a question by itself. It stops and lists the open questions, and says how to answer them.
- The draft stays the one record. Every front-end writes through to it, and a run without a front-end reads it.
- Step 1 adds no network listener and no dependency.

## Non-goals

- Changing which questions planning raises, or their options.
- The richer answers the design document defers: pairing the columns of an ambiguous rename, a value for existing rows when a column becomes `NOT NULL`, a type override. They need a draft format that holds more than an index (step 5).
- A shared registry of answers, or a bot that asks on a pull request.

## The front-ends

| Front-end | Feels like | Good for | Cost |
| --- | --- | --- | --- |
| Terminal prompt | `git add -p` | the developer running `generate` | small, standard library only |
| Answers as flags | `-answer 'alter_column_users_plan=Apply alter'` | scripts, CI, the compose job | small |
| `$EDITOR` round trip | `git rebase -i` | several questions at once, with explanations as comments | small to medium |
| Local web page | `behemoth review` opens a page on localhost | the SQL of each option, side-by-side definitions, structured answers | medium to large |
| Answers as data | a key in a configuration file | the standalone service | small, with the service |

All of them sit on the same interface, `ResolutionPresenter`: `Present` shows the questions and records answers, `Collect` reads them back. Each one wraps `FilePresenter`, so the draft file stays the record.

The web page waits for the richer answers of step 5, where a form does what a prompt can't. Until then it costs more than it gives:

- an HTTP server and HTML and JavaScript assets inside a Go library
- a server on localhost that writes files can be reached by any page open in the developer's browser, through a forged request or DNS rebinding. It needs to bind 127.0.0.1, check a random token in the URL, and check the `Host` header.
- opening a browser fails on a headless machine or over SSH, and in Docker it needs a published port

## Step 1: the prompt, and questions that stay open

### Questions stay open until answered (M20)

`FilePresenter.Present` writes a new question with `"chosen": null` and the planner's suggestion in a field of its own, `"default"`. `Collect` skips an open question, so `ResolveIssues` fails closed with `unresolved_issues`, as the design document asks of an unattended run. An answered question keeps its answer, and a stale one still waits for review, as before.

```json
{
  "id": "alter_column_users_plan",
  "table": "users",
  "description": "Column \"plan\"'s definition narrows (may reject or truncate existing data): length 32 -> 16.",
  "options": ["Apply alter", "Leave as-is"],
  "default": 1,
  "chosen": null
}
```

### The prompt

`migration.PromptPresenter` embeds a `FilePresenter` and takes an input and an output. `Present` lets the file presenter update the draft first, then asks each open question, a new one or a stale one, and saves each answer to the draft before it asks the next. `Collect` and `Consume` are the file presenter's.

```
1 question needs an answer. Each answer is saved to migrations/draft.json as you give it.

Question 1 of 1 (alter_column_users_plan)
users: Column "plan"'s definition narrows (may reject or truncate existing data): length 32 -> 16.

  1) Apply alter
  2) Leave as-is (default)
Answer [2]:
```

- The options are numbered from 1, and Enter takes the default.
- For a narrowing, the description says how the definition changes: the type, the length, nullability, the default, auto-increment. The planner writes it there (`columnChange`), so the draft file and the list of a stopped run show it too.
- The draft is written without HTML escapes, so the file reads `length 32 -> 16` and not `-\u003e`.
- A stale question also shows the answer given before, and that the options have changed since.
- An answer that is not a number in range is asked again. When the input ends, it stops asking, and the questions left stay open.
- An interrupt ends the command while it waits for an answer. The answers already given are in the draft.
- It lives in `migration` and reads only the reader it is given. Whether a terminal is there is for the caller to decide.

### Plugging a presenter into Generate

`RunOptions.Presenter`: nil keeps the `FilePresenter` at `DraftPath(cfg)`. The later front-ends plug in the same way.

### The command line

- `cli.Run` takes an input stream, before the two output streams. `cli.Main` passes `os.Stdin`. The command is marked Experimental, so the signature can change.
- `generate` asks only when its input is a terminal: a character device that is not `os.DevNull`. A pipe, a file, and the `/dev/null` a container gets without a TTY are not. See *Detect a terminal with the standard library*.
- `-no-input` never asks, even in a terminal, for a script run from one.
- Prompts go to stderr, so `behemoth generate > plan.txt` still shows them.
- When a run stops on open questions, the command lists them with the `"chosen"` value of each option and the path of the draft, and says how to answer.
- After a run, the note about the draft lists the answers it read by question, not a count. A "Leave as-is" answer keeps a difference out of the migration, so the developer sees which ones did.
- For both, `migration.Question` is an exported, read-only view of a draft entry, from `FilePresenter.Questions`.

### The Docker example

The `docker compose up` job has no terminal. An open question stops it, and the server does not start. The developer answers with the prompt, `docker compose run --rm --entrypoint behemoth migrate generate`, which has a terminal, and runs `docker compose up` again.

## Step 2: answers as flags

`generate -answer 'ID=LABEL'`, repeatable, answers a question without a terminal: in a script, in CI, or in the compose job.

```bash
behemoth generate -confirm -answer 'alter_column_users_plan=Apply alter'
```

- **A label, not an index.** The flag names the option by its label, compared without regard to case. An index would mean another option once the options change; a label that no longer exists is refused instead.
- **Written to the draft.** `FilePresenter.Given` holds the answers, by question ID and label. `Present` writes each one as it writes the question, like a prompt's answer, so a later run without the flag reads it. A given answer replaces an answer the draft held and clears a stale flag: it is an explicit decision, made against the current options.
- **Refused when it answers nothing.** An answer for a question the run does not raise, or with a label that is not one of the question's options, is an error with a code of its own, `invalid_answer`. Every refused answer is reported at once, with the questions the run raises or the options of the question. The draft is still written with the run's questions. A mistyped answer is not dropped: dropped, it would leave its question open under a name the developer did not mean.
- **Before the prompt.** Given answers are applied first. In a terminal, the prompt asks only the questions still open. Without one, a question still open stops the run as in step 1.
- **Usage errors.** A value without `=`, with an empty ID or label, or an ID given twice, is a usage error (exit code 2), reported before the database is opened.
- **The Docker example.** `migrate.sh` passes its arguments to `generate`, so `docker compose run --rm migrate -answer '...'` answers, writes, and applies in one job.
- **`[Limit]`** On the managed path, a run that finds a written, unapplied migration plans nothing and reads no answer, so its flags have no effect. Its message already says to run `migrate` first.

## Later steps

- **Step 3, an editor round trip.** A text file with one line per question and its explanation in comments, opened in `$EDITOR` and parsed back into the draft. To decide after step 2.
- **Step 4, a local web page.** With the security requirements above. After step 5.
- **Step 5, richer answers.** A draft entry that holds a value as well as an index, for the options the design document defers.
- **Step 6, answers as data.** For the standalone service: a mounted draft, or answers in its configuration file. With the service.

## Design decisions

### Stop an unattended run on an open question

**Context:** M20. A new question is answered with its default in the same run, so nobody decides it, and the run does not say that a change was left out.

**Options considered:**

- *Keep the default, and report the questions answered by default.* No run stops. The caller has to refuse to write until the draft has been reviewed, which needs a "reviewed" mark in the draft and a rule for when it is set.
- *Leave a new question open, and fail closed.* The design document's rule for an unattended run. The prompt asks the developer, so only a run with nobody to ask stops.

**Decision:** leave it open and fail closed.

**Cost:** CI and the `docker compose up` job stop until someone answers. A scripted run needs a committed draft, or the flags of step 2.

**Revisit if:** unattended runs need to settle a known kind of question on their own. Step 2's flags, or answers as data, would do it without a default.

### A prompt before a web page

**Context:** the draft file is hard to answer by hand, and the developer is already in a terminal when a question comes up.

**Decision:** the prompt first. It works over SSH and in `docker compose run`, needs no listener, and is the front-end the design document names. The web page follows the richer answers, where a form is worth its cost.

**Revisit if:** the questions grow beyond picking an option before step 5 is planned.

### Detect a terminal with the standard library

**Context:** the prompt must not ask when nobody can answer. A container started without a TTY has `/dev/null` as its input, which is a character device. Checked on 2026-10-25 with Docker 29: the input of such a container is the container's own `/dev/null`, with the same device and inode, and with `-t` it is a pseudo-terminal.

**Options considered:**

- *`os.ModeCharDevice` on `os.Stdin` alone.* `/dev/null` passes it, so the compose job would prompt into nothing.
- *`term.IsTerminal` from `golang.org/x/term`.* Exact on every platform. But the launcher builds its `main` inside the application's module with `go build`, and an application's `go.sum` holds checksums only for what its own packages import. A module that only `cli` imports has none there, and the build fails with "missing go.sum entry". Checked on 2026-10-25 with a module that imports one package of a library and not the one that imports `x/term`. Asking applications to run `go get` first, or building with `-mod=mod`, which writes `go.sum`, breaks the launcher's promise to change nothing in the source tree.
- *A system call per platform, behind build tags.* Exact. `unsafe`, an `ioctl` per Unix family, and code for Windows to keep.
- *A character device that is not `os.DevNull` (`os.SameFile`).* Standard library, a few lines.

**Decision:** a character device that is not `os.DevNull`.

**Cost:** another character device given as input, such as a serial line or `/dev/zero`, is taken for a terminal. On Windows, an input redirected from `NUL` may be too; the prompt then reads the end of the input at once, and the run stops as it does without a terminal.

**Revisit if:** `cli` gets a dependency outside the root module's import graph for another reason, which would face the same `go.sum` problem.

### Number options from 1 in the prompt

**Context:** `chosen` in the draft is an index, counted from 0.

**Decision:** the prompt counts from 1, as people do. The list printed when a run stops shows each option with its `"chosen"` value, `"chosen": 0`, so the two numbers are never shown without their meaning.

## Implementation steps

| Step | Content | Status |
| --- | --- | --- |
| 1 | M20: open questions and fail closed. `PromptPresenter`, `RunOptions.Presenter`, `Question` and `FilePresenter.Questions`. `cli.Run` input stream, terminal check, `-no-input`, the list of open questions, the answers note. The change a narrowing makes in its description, and a draft written without HTML escapes. Tests, `docs/api/cli.md`, the internal docs, and the example's README. | Done |
| 2 | `-answer`: `FilePresenter.Given`, `invalid_answer`, the flag and its usage errors, `migrate.sh` passing its arguments, an `-answer` example in the list of a stopped run. Tests, `docs/api/cli.md`, the internal docs, and the example's README. | Done |
| 3 | `$EDITOR` round trip | To decide after step 2 |
| 4 | Local web page | Deferred, after step 5 |
| 5 | Richer answers | Deferred |
| 6 | Answers as data | With the standalone service |

## Where the code lives

| Piece | Code |
| --- | --- |
| the draft, `Question`, the open questions, the given answers | `migration/resolver.go` (`FilePresenter`, `Given`) |
| the change a narrowing makes, in its description | `migration/planner.go` (`columnChange`) |
| the prompt | `migration/prompt.go` (`PromptPresenter`) |
| `RunOptions.Presenter` | `migration/backend.go` |
| the terminal check, `-no-input`, `-answer`, the lists | `cli/cli.go` |
| the tests | `migration/prompt_test.go`, `migration/given_test.go`, `migration/resolver_test.go`, `cli/cli_test.go` |

`examples/init/migrate.go` builds its own `FilePresenter` and keeps answering by file. An open question stops its `migrate` command with the same error.

## Related entries

- **M20, "A new issue is answered with its default in the run that first raises it".** Step 1 resolved it, and the entry is removed.
