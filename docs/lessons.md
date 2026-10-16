# Lessons

When a worker's first attempt at a pull request does not stick and the second
one does, the fix is usually something the next worker in that role should
know before it starts. Lessons carry that forward, with the brakes on: a wrong
lesson spreads exactly as easily as a right one, so nothing here learns by
itself.

```sh
$ worktender lessons propose --write      # find failed-then-fixed dispatches, draft them
$ $EDITOR ~/.config/worktender/lessons/engineer.proposed.md   # you write the lesson
$ worktender lessons approve worktender-227                    # you approve it
$ worktender start 42 --repo .                                 # the next engineer reads it
```

## The files

Lessons live beside the [role configuration](roles.md), one pair of files per
role, in `~/.config/worktender/lessons/` (or under `$XDG_CONFIG_HOME`):

| File | Who writes it | Who reads it |
| --- | --- | --- |
| `<role>.proposed.md` | `lessons propose --write`, then you | you, and `lessons approve`. **Never a worker.** |
| `<role>.md` | `lessons approve`, or you by hand | `start`, for every worker in that role |

Both are yours. No repository can ship one, for the reason there is no
per-repository role configuration: a file a checkout could supply would be a
repository writing into its own workers' prompts.

Each lesson is a block:

```markdown
## worktender-227
provenance: https://github.com/steig/worktender/pull/227
motivated-by: ledger: #204 on 204-stats-escapes started 2026-10-01T10:00:00Z; 2 done reports, 1 pr-feedback round
added: 2026-10-16
expires: 2026-12-15
approved-by: steig

Run `go vet ./...` as well as the tests before reporting done; CI runs both.
```

| Key | Meaning |
| --- | --- |
| `## <id>` | Lowercase letters, digits and `-`. `propose` names it `<repository>-<pr>`. |
| `provenance` | The pull request the lesson came from. Required, and an `https://` URL. |
| `motivated-by` | The ledger cycle that made it a candidate. |
| `added`, `expires` | Dates. A lesson is last read the day before `expires`. |
| `approved-by` | Who approved it. |

Unknown keys are refused, naming the file and line: a misspelt `expire:` that
silently did nothing would be a lesson that never lapses. Lines beginning `| `
are evidence from GitHub, never part of the lesson.

## Proposing

`worktender lessons propose` reads the [dispatch ledger](dispatch.md#what-a-merged-pull-request-cost) and finds the
dispatches that failed and were then fixed: ones that reported `done` more than
once, or reported `done` after a `pr-feedback` round. For each, it asks
`gh pr view` for the pull request and its commits, and prints a draft with the
commits added after the first attempt ended.

```sh
worktender lessons propose [--role <role>] [--since 720h] [--write]
```

The draft's text is a `TODO`. **Only a person can say what a cycle taught** —
the commits say what changed, not why. `--write` appends the drafts to each
role's `.proposed.md`, skipping any id already proposed or live; without it,
nothing is written. A dispatch started with no role has no role's lessons to
join, and is counted rather than drafted.

The evidence lines are commit headlines, written by whoever pushed: untrusted,
escaped, and quoted with `| `. They stay in the proposals file, which no worker
ever reads, and `approve` drops them.

## Approving

```sh
worktender lessons approve <id> [--role <role>] [--days 60] [--yes]
```

Prints the lesson exactly as it will be read, says which role's workers will
read it and until when, and asks you to type `yes` — the same act as
[`worktender approve`](dispatch.md#approving-the-raw-bytes-not-the-page) for an issue. It then moves the lesson into
`<role>.md`, stamped with today's date, an expiry `--days` from now (60 by
default), and `approved-by`: your `gh` login, or `git config user.name` when gh
cannot say.

It refuses a lesson still carrying the `TODO`, one without a provenance URL,
and one whose id is already live.

Editing `<role>.md` by hand is fine: it is your file, and you are the approval.
That is also how a lesson is renewed — move its `expires` forward.

## What a worker reads

`start`, briefing a worker in a role, reads that role's `<role>.md` and takes
the lessons that are complete, unexpired, and among the first five in the file.
Each is cut to one line of at most 400 characters. They are written to
`worktender/lessons.md` in the worktree's git directory — never committed, gone
with the worktree — framed as guidance a person approved, each with its pull
request, approver and expiry.

**The lessons are not typed into the brief.** The brief has to arrive in a
single pane read, and five lessons would be several times that. So the brief
gains one sentence pointing at the file. The longest briefs — an analysis role
with directories, a six-digit issue and a long branch — have no room even for
that; `start` then says so and leaves the file pinned for you to point the
worker at. A `factory:ready` issue's worker is pointed at the lessons from the
foot of its approved copy instead, below the hashed payload.

`start` prints what it skipped and why:

```
lessons: skipped 1 expired, 2 past the cap of 5 for role engineer; `worktender lessons list` says which
lessons: 5 lessons for role engineer pinned to /repo/.git/worktrees/42-fix/worktender/lessons.md
```

A role with no lessons file starts exactly as it did before lessons existed.

## Listing, removing, expiring

```sh
worktender lessons list [--role <role>]
worktender lessons remove <id> [--role <role>]
```

`list` shows every lesson, live or proposed, with its state: `live`,
`expired`, `invalid` (and why), past the cap of five, or `proposed`.

`remove` deletes the lesson from the live file — from every role's that holds
it, unless `--role` narrows it. **Removal is traceable by construction.** A
lesson has one home, and `start` reads it at brief time with nothing cached
anywhere, so a removed lesson reaches no worker started afterwards. A worker
already running keeps the copy it was briefed with.

`worktender doctor` warns about an expired or invalid lesson, by role and id.
`start` already skips it; the warning is the prompt to renew or remove it,
rather than letting dead rules accumulate into ceremony.

---

[← README](../README.md)
