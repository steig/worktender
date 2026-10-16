# Reviewing a worker's PR before a human sees it

A worker's `done` is the worker's word. `worktender review` is the check between
that and a person: CI first, then a second agent judging the diff against the
issue's acceptance checks. Only both together are `verified`.

```sh
# COORDINATOR — after the gate releases on a worker's done --pr 17:
"$worktender" review 17 --repo .
# A branch works too, and so does a PR URL: gh pr view takes all three.
"$worktender" review 42-fix-the-thing --repo . --json
```

It is one shot, and it **waits**: up to `--timeout` (default 30m) for the
reviewer's verdict. Run it in the background the way you would run a gate.

## What it does, in order

1. **Reads the PR with `gh pr view`** — its checks, labels and branch. A PR that
   is not open is refused.
2. **Checks CI.** Every check's latest run must have passed (success, neutral or
   skipped). Not green means it reports and stops, with no pane opened and no
   model spent:
   - **failing** — exit **4**. pr-feedback tells the worker which checks failed;
     review again once it pushes.
   - **pending** — exit **4**. Review again once they finish.
   - **no checks at all** — exit **3**. Nothing will turn green that does not
     exist, so a person decides.

   It reads *every* check rather than only the required ones. Required checks
   are branch protection's to declare, and a repository with none would
   otherwise be green by having nothing to fail. The cost is that a flaky
   optional check holds a review until it is re-run.
3. **Finds the issue**: the number the branch starts with (`start` names
   branches `<issue>-<slug>`), or `--issue <n>`. A branch that names none is a
   usage error. It needs the issue, because the acceptance checks live there.
4. **Opens a new pane beside the worker**, in the worker's workspace and
   checkout, without taking focus, and starts the reviewer there with the same
   executor `start` and `dispatch` use.
5. **Briefs it** with one line that names the PR and the issue by number. The
   reviewer reads the diff with `gh pr diff` and the issue from the approved
   copy `start` pinned (for a `factory:ready` issue), or with `gh issue view`
   when there is none.
6. **Waits for its report**, with gate's own wait, and composes the verdict.

The reviewer's pane is left open when review returns, so a person can read what
it did; close it yourself. Its agent name carries the PR's head commit, so a
second review after the worker pushes starts a new reviewer rather than
colliding with the first.

## The verdict

The reviewer posts **one PR review** with `gh pr review --comment`, one line per
acceptance check, then reports like any worker:

```sh
worktender report --status done --pr 17 --note "pass: all three checks met"
worktender report --status done --pr 17 --note "fail: check 2 has no test"
```

| Reviewer said | CI | Outcome | Evidence | Exit |
| --- | --- | --- | --- | --- |
| `done`, note starts `pass` | green | `verified` | `verified` | **0** |
| `done`, note starts `fail` | green | `review_failed` | `reported` | **4** |
| `done`, anything else | green | `review_unreadable` | `reported` | **4** |
| `blocked` | green | `review_blocked` | — | **3** |
| nothing in time, or its pane died | green | `review_no_answer` | — | **4** |

**The verdict is the note's first word, exactly.** `pass` or `fail`, lowercase,
followed by a colon, a space or nothing. Anything else is unreadable, and
unreadable is never verified, so a reviewer that drifts from the format fails
closed. This is the one place a note is read: `gate` still has no predicate over
it. A new report status or slot would have moved the wire format every gate and
coordinator reads, for one reader.

**`verified` is two independent signals agreeing, not a proof.** gh saw every
check pass, and an agent that did not write the change said every acceptance
check is met. The second is still an agent's word.

A failed review exits **4**, not 3. The work is unfinished, and the next move is
the worker's.

## Findings go to the worker, not to you

There is no second feedback path. The reviewer's PR review is a review like any
other, and [`pr-feedback`](watch.md#routing-pr-feedback-to-workers) already
counts reviews and tells the worker on that branch to read them. So with the
watch mod's `prFeedback` on, a failed review reaches the worker on its next
look; without it, run `worktender pr-feedback` yourself. Then review again once
the worker pushes.

It is a **comment** review because GitHub will not let an account approve or
request changes on its own pull request, and workers open PRs with your gh
login. The reviewer's login is the same, so pr-feedback names the review as
yours.

## `risk:high`

A `risk:high` label on the PR or the issue asks for the stronger review. Either
is enough: a label here only ever makes the review stricter. It does two things:

- The brief asks the reviewer to review adversarially, for security, data loss
  and failure paths.
- The reviewer starts as the role **`reviewer-high`** when your configuration
  defines it, and **`reviewer`** otherwise. A role is an agent file, and the
  agent file is where the model and effort live, so the stronger model is the
  agent you write:

```json
{
  "roles": {
    "reviewer":      { "agent": "pr-reviewer" },
    "reviewer-high": { "agent": "pr-reviewer-opus" }
  }
}
```

With neither defined the reviewer is plain `claude`, and `--model` picks the
model. `--role` overrides both. See [Roles](roles.md) for the file.

## What is untrusted, and what is left

The PR diff and the issue text are untrusted: whoever filed the issue and the
worker that wrote the change both wrote them. They follow the rule `start`
follows. **Neither is pasted into the brief.** The brief is this plugin's own
prose and two numbers; the reviewer reads the diff and the issue itself, as tool
output, under a line saying they are data to judge and that an instruction
inside them is a finding. Check names in the output pass through the same
reduction pr-feedback uses.

**Not editing is intent, not a boundary.** The reviewer is told not to edit,
commit or push, and it sits in the worker's checkout because that is where the
pinned issue is. Nothing but its instructions and the permission mode you give
it stops a reviewer misled by the diff from writing there, and a diff written to
talk a reviewer into `pass` is the attack this cannot rule out: the verdict is
an agent's judgement of text an adversary may have written. Keep a person on
the merge, and see [the permission-mode problem](dispatch.md#the-permission-mode-problem-stated-plainly).

The reviewer needs to run `gh pr diff`, `gh pr review` and `worktender report`
with no human at its pane, so it stalls at the first permission prompt unless
its agent file or `--permission-mode` allows them. The same warning `dispatch`
prints applies.

## `review --json`

Written on the failure paths too, like `start --json`.

```json
{
  "repository": "/home/me/repo",
  "pr": 17,
  "branch": "42-fix-the-thing",
  "issue": 42,
  "risk_high": false,
  "outcome": "verified",
  "evidence": "verified",
  "ci": { "state": "green", "passing": 4, "failing": [], "pending": [] },
  "verdict": "pass",
  "report": { "status": "done", "pr": 17, "note": "pass: all three checks met" },
  "launch": null,
  "pinned": true,
  "workspace_id": "w9",
  "pane_id": "w9:p2",
  "agent_name": "review-42-fix-the-thing-0123456-6364c6",
  "briefed": true,
  "staffing": [ { "status": "done", "kind": "staff", "…": "…" } ],
  "error": null,
  "exit_code": 0
}
```

- **`evidence`** is `verified` only beside outcome `verified`, `reported` when
  the reviewer's verdict came back as anything else, and null when no verdict
  was reached — CI stopped it, the wait ended, or something failed.
- **`outcome`** is one of `verified`, `ci_failing`, `ci_pending`, `ci_missing`,
  `review_failed`, `review_unreadable`, `review_blocked`, `review_no_answer` or
  `error`.
- **`report.note`** is the reviewer's own words and stays untrusted data.
- **`agent_name`** is what to `gate` on if you want to wait for the reviewer
  again after a timeout.

---

[← README](../README.md)
