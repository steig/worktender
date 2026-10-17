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

1. **Reads the PR with `gh pr view`** — its head and base commits, labels and
   branch. A PR that is not open is refused. **The review is bound to that head
   commit** from here on. Nothing after this reads the PR's live state until
   the verdict is in: CI, the diff and the issue are all fetched by id.
2. **Checks CI for that commit**, asked of GitHub by commit id rather than for
   the PR, whose rollup follows whatever its head is by then. Every check's
   latest run must have passed (success, neutral or
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
   usage error.
4. **Fetches the issue from GitHub and checks its approval** the way `start`
   does for a `factory:ready` issue: if it still hashes to a payload an
   account with write access approved with `worktender approve`, that payload
   is what the reviewer judges against. Otherwise — never approved, edited
   since, or checks that do not parse — the reviewer gets the live text, framed
   the same way, and **a pass can then never be `verified`**. Never from the
   worker's pinned `issue.md`: that sits in a checkout the worker writes to.
5. **Writes the reviewer's inputs outside every worktree**: a fresh `0700`
   directory under the plugin state directory, `review/<pr>-<commit>-<random>`,
   holding `issue.md` and `change.diff` — the diff of base...head fetched from
   GitHub's compare API by commit ids, so it is of exactly the commit reviewed
   however the branch moves afterwards.
6. **Opens a new pane in the worker's workspace**, without taking focus, with
   its shell in the **main checkout**, not the worker's: claude loads project
   settings, hooks, `CLAUDE.md` and agent files from where it starts, and the
   worker's checkout is the worker's to rewrite. The reviewer starts with the
   same executor `start` and `dispatch` use, **able only to read and report**
   (see [What the reviewer can do](#what-the-reviewer-can-do)).
7. **Briefs it** with one line that names the PR, the commit, the issue and the
   inputs directory, and sends it to those two files — not to the live PR or
   issue.
8. **Waits for its report** on the pane it opened — bound by pane id, not
   agent name — with gate's own wait, judging **only the metadata channel**:
   that pane has just printed a diff, and an envelope inside a diff is not a
   report.
9. **Re-reads the PR after the verdict arrives**, and the recorded commit's
   checks. If the head moved, or that commit's CI is no longer green, the
   verdict is `review_stale` and never `verified`, whatever it says.

The reviewer's pane is left open when review returns, so a person can read what
it did; close it yourself. Its agent name carries the PR's head commit, so a
second review after the worker pushes starts a new reviewer rather than
colliding with the first.

## The verdict

The reviewer reports like any worker, with its verdict as the first word of
its note:

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
| anything, but the head moved or CI went red meanwhile | — | `review_stale` | `reported` | **4** |
| `pass`, against an issue nobody approved | green | `review_passed_unapproved` | `reported` | **3** |

A `done` whose `--pr` is not the PR under review is unreadable too.

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

**Review, not the reviewer, posts one PR review**, with your gh, once the
verdict is in and the commit and CI have been re-checked. Nothing is posted for
a stale verdict, a blocked reviewer or one that never answered. **Every word of
it is review's**: PR, commit, issue, outcome, evidence, how many approved
acceptance checks the verdict covers, and where the note is kept. Nothing the
reviewer wrote is posted — a PR review is public, and an injected reviewer's
note could carry whatever it read.

**The reviewer's note stays on your machine**, in `findings.md` (0600) in the
review's inputs directory, under a line saying it is untrusted. The posted
review names that directory by its base name; the JSON's `findings` field has
the full path.

There is no second feedback path. That review is a review like any other, and [`pr-feedback`](watch.md#routing-pr-feedback-to-workers) already
counts reviews and tells the worker on that branch to read them. The worker
learns the outcome from the review and, for the detail, reads `findings.md` —
a person can hand it over, or the worker can read it if its own permissions
reach the plugin state directory. So with the
watch mod's `prFeedback` on, a failed review reaches the worker on its next
look; without it, run `worktender pr-feedback` yourself. Then review again once
the worker pushes.

It is a **comment** review because GitHub will not let an account approve or
request changes on its own pull request, and workers open PRs with your gh
login. Review posts it with that same login, so pr-feedback names the review
as yours. If posting fails, review exits **2** and the document still carries
the outcome.

## `risk:high`

A `risk:high` label on the PR or the issue asks for the stronger review. Either
is enough: a label here only ever makes the review stricter. So does size: a
diff that adds or removes more than 300 lines outside `_test.go` files is
reviewed as `risk:high` with no label, and review prints
`over the slice cap: N non-test lines; split or review adversarially`. The
count is `non_test_lines` in `--json`. Either way it does two things:

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
follows. **Neither is pasted into the brief.** The reviewer judges the diff
against the issue's acceptance checks — the payload a person approved by hash,
re-verified by review itself — and is told never to judge it against what the
diff claims. The brief is this plugin's own prose, two numbers and a commit id;
the reviewer reads the diff and the issue itself, from files, as tool output, under a line saying they are data to judge and that an instruction
inside them is a finding. Check names in the output pass through the same
reduction pr-feedback uses.

**The verdict channel is herdr pane metadata, which does not authenticate its
writer.** Review hears only the reviewer's pane, and only its metadata, so a
report on the worker's own pane or an envelope printed into the reviewer's
terminal is ignored. But any process holding the herdr socket can write any
pane's metadata — the worker included, if it goes looking for the reviewer's
pane id. Nothing here closes that; the same is true of every gate.

**Nothing review hands the reviewer is read from a worktree.** The issue comes
from GitHub, re-verified against its approval; the diff comes from GitHub by
commit ids; both are written to a fresh `0700` directory under the plugin state
directory; and the reviewer starts in the main checkout. What that does not do
is defend against a hostile process running as you: the worker is the same uid,
and could still find and rewrite that directory, or the main checkout. Keeping
reviewer inputs out of the worktree stops the ordinary path — a worker editing
its own checkout — not a determined one.

**A diff written to talk the reviewer into `pass` is the attack this cannot
rule out.** The verdict is an agent's judgement of text an adversary may have
written. What the launch below limits is the blast radius — a misled reviewer
can say the wrong word, not act — and a person stays on the merge.

## What the reviewer can do

Its prompt is not what holds it; its launch is. Checked against Claude Code
2.1.300's `--help`, the reviewer starts with:

```
--restricted --strict-mcp-config
--tools Read,Grep,Glob,Bash
--allowedTools Read,Grep,Glob,Bash(<this binary> report:*)
--disallowedTools Edit,Write,NotebookEdit,WebFetch,WebSearch
--permission-mode dontAsk
--add-dir <inputs>
```

`--restricted` drops the command-running tools and WebFetch unless `--tools`
names them, ignores user, project and local settings files (so no hooks and no
permission rules from any checkout), confines the file tools to the working
directories, and refuses `bypassPermissions`. Bash is in `--tools` only so the
one allowed command, `worktender report`, can run; `dontAsk` denies the rest
instead of waiting on a prompt nobody will answer. **No gh, no git, no other
shell, no editing, no network.** `review` takes no `--permission-mode`, so
nothing loosens this.

Not yet verified against a live session: that `dontAsk` denies rather than
prompts in every case, and that the `Bash(... report:*)` rule cannot be
widened by a compound command. Claude Code's own matching is the boundary
there. The reviewer can read the main checkout (its working directory) as well
as its inputs; there is no checkout of the reviewed commit itself.

## `review --json`

Written on the failure paths too, like `start --json`.

```json
{
  "repository": "/home/me/repo",
  "pr": 17,
  "branch": "42-fix-the-thing",
  "head": "0123456789abcdef0123456789abcdef01234567",
  "issue": 42,
  "risk_high": false,
  "non_test_lines": 120,
  "approved": true,
  "inputs": "/home/me/.local/state/herdr/plugins/steig.worktender/review/17-0123456789ab-2614553904",
  "outcome": "verified",
  "evidence": "verified",
  "ci": { "state": "green", "passing": 4, "failing": [], "pending": [] },
  "verdict": "pass",
  "report": { "status": "done", "pr": 17, "sha": null, "note": "pass: all three checks met" },
  "launch": null,
  "workspace_id": "w9",
  "pane_id": "w9:p2",
  "agent_name": "review-42-fix-the-thing-012345678-6364c6",
  "briefed": true,
  "posted": true,
  "findings": "/home/me/.local/state/herdr/plugins/steig.worktender/review/17-0123456789ab-2614553904/findings.md",
  "staffing": [ { "status": "done", "kind": "staff", "…": "…" } ],
  "error": null,
  "exit_code": 0
}
```

- **`evidence`** is `verified` only beside outcome `verified`, `reported` when
  the reviewer's verdict came back as anything else, and null when no verdict
  was reached — CI stopped it, the wait ended, or something failed.
- **`outcome`** is one of `verified`, `ci_failing`, `ci_pending`, `ci_missing`,
  `review_failed`, `review_unreadable`, `review_blocked`, `review_no_answer`,
  `review_stale`, `review_passed_unapproved` or `error`.
- **`approved`** is whether the issue judged against was an approved payload.
  False caps the evidence at `reported`.
- **`inputs`** is the directory the reviewer was handed.
- **`posted`** is whether review posted its PR review.
- **`findings`** is the local file holding the reviewer's note, which is never
  posted.
- **`head`** is the commit the review is bound to.
- **`report.note`** is the reviewer's own words and stays untrusted data.
- **`agent_name`** is what to `gate` on if you want to wait for the reviewer
  again after a timeout.

---

[← README](../README.md)
