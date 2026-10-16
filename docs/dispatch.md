# Dispatching a worker, and waiting for it

This is what the plugin is for once more than one agent is involved: a
coordinating agent hands a slice of work to another and needs to know when it is
done — without reading everything that agent did to get there.

```sh
# Resolve the binary once. A standalone install is on PATH; a plugin install
# is not, because herdr owns it. `worktender doctor` prints the answer either
# way, so you need the jq only once.
worktender=$(command -v worktender) || worktender=$(herdr plugin list --json \
  | jq -r '.result.plugins[] | select(.plugin_id == "steig.worktender") | .plugin_root')/bin/worktender

# COORDINATOR — when the slice is a GitHub issue, one command does all of it.
# --repo because start creates a checkout and is not a herdr action, so nothing
# tells it which repository unless you do. Flags go on either side of the number.
"$worktender" start 42 --repo . --model sonnet
# The agent name is not the branch: it carries a digest of the repository,
# because herdr's agent namespace is global. `start` prints this exact line.
"$worktender" gate --target wt-42-fix-the-thing-016aab --until done --require-pr --timeout 20m

# ...and with several running, wait on all of them at once. The first to report
# releases, and the gate says which one it was.
"$worktender" gate --any 42-fix-the-thing,43-other,44-third --until done --timeout 20m

# ...and when it is not, the pieces are still separate. Dispatch, then wait.
"$worktender" dispatch --pane w22:p1 --name reconcile-split --model sonnet
"$worktender" gate --target reconcile-split --until done --require-pr --timeout 20m

# WORKER — from inside its own pane:
"$worktender" report --status done --pr 42 --note "landed the reconcile split"
```

`start` is `worktree.create` + `dispatch` + a briefing **it confirms**, in that
order, against one issue. The briefing is typed and then submitted as a separate
Enter key event, offered again every couple of seconds for as long as the agent
stays idle; `start` waits for herdr to report the agent working before it says
"briefed". herdr answering ok means it delivered keystrokes, not that an agent
received a prompt — and an agent herdr calls started is not yet an agent reading
its keys, so the first Enter is routinely dropped. A brief left sitting in a
composer is a worker with nothing to do that a listing reports as `idle`.

**`start` never types on a trust screen.** A new worktree is a folder Claude Code
has not seen, so it may open on its folder-trust dialog, a hooks-review screen,
or the dialog asking you to approve managed settings, instead of its composer —
and there the Enter meant for the brief would accept the dialog on your behalf.
Before typing the brief, and again before every Enter, `start` reads the visible
pane; if it shows any of these it sends nothing, exits **3** naming the screen,
and leaves the worktree and the agent for you. Answer the dialog in the pane,
then brief the agent yourself. The match is on the dialog's text, taken from
Claude Code 2.1.300's source for the trust and managed-settings dialogs and from
#196 for the hooks screen, so a Claude Code release that rewords any of them can
slip past it.

The brief names the issue rather than carrying it — the worker runs `gh
issue view` itself, which keeps it short enough to arrive in one piece and keeps
untrusted prose out of the prompt entirely.
It exists because the pane id the middle step needs came from
nowhere: `ls` did not print one, so the documented loop had a `<pane>` in it and
no command that produced it. `ls` prints panes now, and `start` does not need
you to look.

**`start` does not wait.** Starting five issues and then waiting on all five is
the point; a start that gated would serialise the fleet.

The gate prints the report and exits 0 when the predicate holds. Its failures
carry different codes, because they need different answers:

| Code | When | What you do |
| --- | --- | --- |
| **3** | the worker reported `blocked` | Escalate. It will not reach `done` on its own, and retrying blocks again. |
| **4** | it timed out, or the worker's pane died before reporting | No answer arrived. Redispatching is reasonable. |
| **1** | a target could not be resolved | Drop it from the list. herdr answers the same for a mistyped name and for an agent that has since exited. |
| **2** | herdr's socket was unreachable | The machine, not the wait. |

Branch on the code, not on the message. The full table is in
[Reference](reference.md#exit-codes-and-errors).

```sh
worktender start <issue> [--role <role>] [--model <model>] [--permission-mode <mode>] [--base <ref>] [--repo <path>] [--focus]
worktender dispatch --pane <id> --name <agent> [--role <role>] [--model <model>] [--permission-mode <mode>] [--resume]
worktender report --status planned|blocked|done [--pr N] --note <text>
worktender gate --target <agent|pane> | --any <a,b,c> [--until done] [--require-pr] [--timeout 15m] [--dagr]
```

## Starting a worker as a role

`--role` starts the worker as a role from your own configuration — a Claude Code
agent, a brief style, and directories it may read — instead of a bare `claude`
with the engineering brief. A label on the issue can pick it too:

```sh
$ worktender start 42 --repo .
role: ux (label role:ux) · agent: ux-designer (user) · brief: design · dirs: /home/you/kb/design-system
```

The configuration, how a role is chosen, the brief styles and what a label can
and cannot reach are in [Roles](roles.md).

## Acceptance checks

A gate can only check what an issue states mechanically. An issue whose "done"
is prose gets verified against whoever is reading it. So an issue **cleared for
an agent** — one carrying the `factory:ready` label — states its done as checks
a machine can run, in a section of its own:

```markdown
## Acceptance

- [ ] `go test ./...` exits 0
- [ ] `test -f docs/acceptance.md` exits 0
- [ ] `worktender version` prints 0.14.0
```

The grammar is strict:

- The heading is exactly `## Acceptance`, at the start of a line and outside a
  code fence. The section runs to the next heading.
- Each non-blank line in it is `- [ ] ` or `- [x] `, then the **command** in
  single backticks, then one space, then the **outcome** it should give: an
  exit code, output, a file present.
- The command and the outcome are printable ASCII (0x20 to 0x7E), and neither
  contains a backtick.
- Any other line refuses the whole issue, by line number. A skipped check is a
  check nobody runs. A second `## Acceptance` heading is refused too.

The grammar makes "the commands" well defined, so the approver, `start`, and
later a gate all read the same list. It is not what makes running them safe.
Approval is.

The [`factory-ready` issue template](https://github.com/steig/worktender/blob/main/.github/ISSUE_TEMPLATE/factory-ready.md)
lays this out.

### Approving the raw bytes, not the page

An agent runs these commands, so a person with write access has to approve them
first. They approve what the agent will receive, not what GitHub draws:

```sh
worktender approve 42 --repo .
```

`approve` fetches the issue once and prints the **canonical payload**: the
title, the parsed acceptance commands, and the body. All of it is raw and none
of it is rendered. Every body line is prefixed `| `, and every invisible or
control character (zero-width, bidi, carriage return, tab) is printed as a
visible `\u{...}` by `safetext`. Then it prints the payload's sha256 and asks
for `yes`, unless you pass `--yes`. On yes, it posts the comment
`worktender-approved: sha256=<hex>` and applies `factory:ready`, both with your
own `gh` auth.

**Read the output.** The approver approves whatever they were shown. That is
the residual risk, and nothing downstream can catch what they missed.

Approval is over the raw bytes because approving the rendered page kept
failing. Two earlier drafts tried to make the raw markdown match what GitHub
draws: one stripped comments and refused tags, the other used an ASCII-only
grammar. Each one lost to a renderer behaviour it had not modelled. Link text
hides its URL, emphasis eats characters, and HTML folds content away. A hash
over the raw payload has no renderer in it. Any edit, visible on the page or
not, changes the hash.

**`start` checks the approval.** For a `factory:ready` issue, `start` reads the
title, body and last 100 comments in one GraphQL response. It rebuilds the
same payload and proceeds only when a comment from an account with write or
admin access carries exactly that hash. Access is checked with
`gh api repos/{owner}/{repo}/collaborators/{login}/permission`. Any of these is
refused with **3**, before any worktree exists:

- the issue was edited after approval;
- nobody approved it;
- the only matching comment is from an account without write access, or a
  deleted account;
- the Acceptance section does not parse.

```
worktender: issue #42 changed since approval, or was not approved: no comment from an account with write access carries sha256 …; run `worktender approve 42`
```

The code is 3 rather than 1 because no change to the invocation fixes it. A
person has to read the payload and approve it, which no retry and no
coordinator can do for them. If `gh` cannot answer at all, the exit is **2**,
the same as for an unreadable issue.

**The worker reads the approved payload, not the live issue.** From that same
single fetch, `start` writes the payload byte for byte to
`$(git rev-parse --git-path worktender/issue.md)` in the new worktree. That is
inside the worktree's git dir, so it is never committed and goes away when the
worktree does. Every line of issue text in it starts with `| `, so nothing an
issue says can pass for one of the file's own lines, including `end of body`.
The file says that later edits to the live issue are unapproved. It also tells
the worker not to run a check that reaches beyond checking the work (network,
credentials, anything outside the repository) and to report it as blocked
instead. That sentence is a second line of defence. The approval is the
control.

**The brief counts the checks. It does not carry them.** A ready issue's brief
points at the pinned file instead of `gh issue view`, and says how many checks
`start` parsed. The worker runs them and says in its `done` note which pass,
for example `acceptance 3/4 pass, #2 fails`. No text from the issue author goes
into the prompt itself.

An issue **without** the label is read exactly as before. `start` asks `gh` for
the same fields, fetches no snapshot, writes no file, and types the same brief.

The worker's report is a claim, not proof. Re-running the checks at the gate,
so that it does not take the report's word, is a separate step.

## A durable, global inbox for agent-to-agent messages

`report` and `gate` are a live handshake between one worker and the coordinator
that dispatched it: the moment either side exits, the channel is gone. `inbox`
is the other thing — a message that has to survive that, and be findable by
whoever asks later, not just by whoever it was sent to.

```sh
# Any agent, in any worktree, on any repository — the inbox is fleet-wide, not
# scoped to one checkout.
"$worktender" inbox post --thread 169 --from wt-169-inbox-016aab \
  --note "storage lands under the plugin state dir, one NDJSON file per thread"

# Read a thread in order.
"$worktender" inbox read --thread 169

# Or find it without knowing which thread it landed in.
"$worktender" inbox search "NDJSON file per thread"
```

```sh
worktender inbox post --thread <id> --from <name> --note <text> [--json]
worktender inbox read --thread <id> [--json]
worktender inbox search [--json] <query>
```

**It is not wired to `SendMessage` or any other live channel, on purpose.**
worktender has no hook into that transport, and auto-logging every message
would be a scope decision this plugin should not make silently. An agent that
wants something durable calls both: its live channel to deliver it now, `inbox
post` to make it findable later.

**Pull only.** `read` and `search` are the whole interface — there is no
watch or subscribe here, because that is what the live channel is already for.
A coordinator or a freshly spun-up worker with no memory of this conversation
runs `inbox search` when it is explicitly told to look for something; nothing
here pushes a message at an agent that did not ask.

**Thread ids are freeform**, same posture `report` already takes toward its
note: an issue number or a task slug is the convention, but nothing here
enforces or interprets one. They are restricted to letters, digits, `-`, `_`
and `.` — enough for either convention, and never a path separator, so a
caller-supplied id can never resolve outside the inbox directory.

**No retention.** Append-only, unbounded, for now — a deliberate gap rather
than an oversight. Better to ship without a pruning story and add one once
real growth is observed than to guess a TTL today.

## What a merged pull request cost

`worktender stats` answers whether the factory is helping: per dispatch, how
long it took from `start` to its last `done`, how many tries it needed, and how
much coordination it took against how much it changed.

```sh
$ "$worktender" stats
ISSUE  BRANCH          ROLE  PR  MERGED  WALL     ATTEMPTS  INBOX  GATES  DIFF  CEREMONY/100L
#42    42-fix-thing    eng   7   yes     1h0m0s   2         3      2      200   2.50
#43    43-other-thing  -     8   yes     15m0s    1         0      1      10    10.00

2 dispatches, 2 with a pull request, 2 merged
per merged PR: median wall clock 37m30s, 1.50 attempts, rework rate 50%, 3.00 ceremony
```

It reads a local ledger, `<plugin state dir>/ledger.jsonl`, that three commands
append one line to as they go:

- **`start`** records the dispatch once its agent has the brief — issue, branch,
  role, agent, agent kind and agent name. Only dispatches made by `start` are
  counted: a worker staffed by `sync` or `dispatch` has no issue to count against.
- **`report`** records each status a worker claims, with its time and pull
  request, keyed by the repository and branch it was made from.
- **`gate`** records each wait a coordinator opens, against every worker it covers.

The inbox is read where it already is: messages on the thread named for the
issue number, posted after the dispatch started.

What the columns mean:

- **`ATTEMPTS`** is the number of `done` reports. A second one means the first
  did not stick — review sent it back, a check failed, it was re-briefed.
  **Rework rate** is the share of merged pull requests that needed more than one.
- **`WALL`** is `start` to the *last* `done`, because a dispatch that went round
  again was not finished the first time it said so.
- **`CEREMONY/100L`** is inbox messages plus gate runs, per hundred lines the
  pull request changed (additions plus deletions, from gh). Ceremony rising while
  the diff stays flat is the sign of agents proving to each other that earlier
  checks ran instead of moving the work.
- **`MERGED`** and **`DIFF`** come from `gh pr view`, the one thing worktender
  cannot observe itself. `?` is gh failing to answer; `--json` says why.

Recording is best effort: a ledger that cannot be written warns on stderr and
the command carries on, because a report refused over a metrics file loses the
report to save a statistic about it. Like the inbox, the ledger is append-only
and unbounded for now. It records no tokens or cost — that would mean reading
session-log content, a decision of its own.

## Stacking on a branch that is still in review

`start --base <ref>` forks the new worktree from any ref, not just the trunk. So
a second slice can proceed on top of the first while the first is still in
review, which is a genuinely useful thing to do and nothing here refuses it.
**One thing about it is worth knowing first.**

A squash merge does not put the base branch's commits on the trunk; it puts one
new commit there. So the moment the base lands, the branch stacked on it is
sitting on commits that exist nowhere in the trunk's history — and its own pull
request shows **the base's entire diff as its own**.

`start` prints the commit it forked from, and when that commit is not one the
base branch already has, it says so:

```
worktree: 42-fix-the-thing on feat/76-machine-readable-output (workspace w22, pane w22:p1)
fork point: feat/76-machine-readable-output is 31db5d1c9b7e4a02f6c1d8e5a3b90f2c4d6e8a10
  stacked: feat/76-machine-readable-output holds commits origin/main does not. A squash merge lands
           none of them there, and this branch's PR would then show its diff too.
  repair:  git rebase --onto origin/main 31db5d1c9b7e4a02f6c1d8e5a3b90f2c4d6e8a10
           once it merges. Before that, --onto feat/76-machine-readable-output, having rebased that first.
```

In order of preference:

- **Do not stack unless the base is about to land.** A slice that can wait for
  its parent to merge has none of this.
- **Restack the child while the base's pull request is still open.** Two
  commands, in this order: rebase the base branch onto the trunk, then on the
  child `git rebase --onto <base-branch> <fork-point>`. The fork point is where
  the child's own commits start, and naming it is what holds the replay to
  those — the child ends up on the base's actual tip with its own commit on top.

  **A plain `git rebase origin/main` on the child is not the lighter version of
  that.** It replays every commit the child has and the trunk does not, and
  while the base is unmerged that is the base's commits as well as the child's.
  They come back rewritten under the child's name, and a conflict in one of them
  lands on whoever ran the rebase, in code they did not write.
- **`--onto` afterwards.** `git rebase --onto origin/main <fork-point>` replays
  only the commits after the fork point, which is what makes the base's diff
  stop being the child's.

Both repairs are the same command with a different target. The fork point is
not fixed, though: after a restack it is the base branch's new tip, not the sha
`start` printed. Give `--onto` the old one and it replays the base's commits all
over again, which is the thing being avoided.

**The fork point is the part that goes missing**, and it is why `start` prints
it. `--onto` needs the commit the branch was forked from — not the ref name,
which by then has moved or been deleted. Afterwards the branch's own reflog is
the only place that commit survives, and a worker that force-pushed has probably
lost it. At fork time it costs a line; later it can cost the branch.

The line prints on any fork the base does not already have, because how a pull
request will be merged is not knowable at fork time. Under a merge-commit
workflow it costs you nothing: the base's own commits reach the trunk, so the
child's diff stays the child's. This repository squash-merges.

## Waiting on a fleet

`--any` takes several workers and releases on the **first** of them to satisfy
the predicate, naming it. Drop that one and gate again on the rest:

```sh
"$worktender" gate --any 12-thing,13-other,14-third --until done --timeout 20m
# gate: waiting on 12-thing (pane w1:p1), 13-other (pane w2:p1), 14-third (pane w3:p1) for status done, up to 20m
# gate: 13-other released after 4m12s (evidence: reported)
"$worktender" gate --any 12-thing,14-third --until done --timeout 20m
```

Waiting on one worker at a time is what this replaces, and the reason is that
nothing tells them apart in advance: `start` returns as soon as the brief is
typed, so a coordinator picking one to block on has no basis for the choice.
Pick the slow one and the workers that finished sit idle with their reports
unread, and five sequential 15 minute gates are a 75 minute worst case for work
that all landed in the first ten.

`blocked` is the sharper half. It is the one status only the coordinator can
clear, and it used to be heard only while the coordinator happened to be gated
on *that* worker.

Three things follow from `--any` and are worth knowing before you build a loop
on it:

- **The timeout is for the wait, not for each worker.** It is how long you are
  prepared to sit there, and that does not multiply by the number of workers you
  are sitting there for.
- **A worker that dies ends the wait, and the failure names it.** So does a
  `blocked`. Both are yours to act on, and the name is what you drop before
  gating on the rest — waiting on in silence would leave a death unmentioned
  until the deadline.
- **There is no `--all`.** It is a loop over `--any` in the caller, dropping each
  worker as it releases, and the caller has to be able to write that loop
  anyway: `--all` would still have to say what it did when one of the fleet
  reported `blocked` halfway through.

Every target is resolved before the wait opens, so one mistyped name out of five
fails immediately rather than at the deadline. Naming one worker twice — its
agent name and its pane id are both accepted, and both resolve — is refused
rather than watched twice.

## Watching a dispatch in herdr-dagr

`--dagr` keeps a [herdr-dagr](https://github.com/aemrebarut/herdr-dagr) run file
current while the gate waits, so the dagr pane (`open-dagr`) draws the fleet
live. It writes `.dagr/run.json` in the main checkout — the path dagr reads by
default — replacing it in one rename on every report, death and timeout the
gate sees. Without the flag nothing is written.

```sh
"$worktender" gate --any 12-thing,13-other,14-third --until done --timeout 20m --dagr
```

Each worker is one task, and only what the gate observed is drawn:

| The gate saw | Task | Attempt | Evidence |
| --- | --- | --- | --- |
| no report yet, or `planned` | `working` | `working`, the note as progress | — |
| `blocked` | `blocked`, unblocked by the coordinator | `working` | — |
| `done` | `done` | `done`, the pr as receipt | ◇ reported |
| the pane or workspace ended | `failed` | `lost` | ◆ verified |
| herdr's agent status went `unknown` | `blocked` | `lost` | ≈ heuristic |
| the timeout | unchanged, with a note | unchanged | — |

A `done` is never drawn ◆ verified: it is the worker's word, as it is in the
gate's own output. A timeout changes no worker, because the gate running out
of time says nothing about whether they are still working.

Rounds of `--any` share the file. A worker the current gate is not watching
keeps whatever the last gate to watch it drew, and a worker gated again after it
settled opens a second attempt rather than repainting the first. Delete the file
to start a fresh view.

The `.dagr` directory is created with a `.gitignore` of `*`, so the run file
never shows up as untracked; a `.dagr` you already had is left as it was. A
`.dagr/run.json` that worktender did not write — another producer's run — is
refused before the wait opens (exit 1) rather than overwritten. A write that
fails mid-wait is a warning; it never changes the gate's outcome or exit code.

## Why dispatch is separate from `sync`

`sync` staffs a bare agent with no arguments, and that is deliberate. It runs
from a keybinding and from event hooks, where nothing knows what the work is —
so there is no role to route on, and giving an unattended reconciler an opinion
about which model to spend or how much autonomy to grant is how a hook that
fires on every new worktree quietly starts doing both. **`sync` stays dumb;
dispatch routes.**

Dispatch goes through the same executor `sync` does, which is what guarantees it
re-checks the pane before starting. `agent.start` against a pane that already
hosts an agent does not bounce off — it lands on a live conversation and
destroys context that exists nowhere else.

## The permission-mode problem, stated plainly

A dispatched worker has no human at its pane, so it stalls on the first
permission prompt and stays stalled — and a coordinating agent structurally
cannot clear it. That stall is the failure this command exists to prevent, so
`--permission-mode` passes through to the agent, including the modes that stop
it asking at all.

It comes with a real caveat rather than a reassurance:

**worktender cannot sandbox the agent it starts.** `claude` takes no sandbox
flag; sandboxing lives in settings.json, and this plugin does not write your
agent's configuration. So `bypassPermissions` and `acceptEdits` grant autonomy
*without* the boundary that should accompany it, and worktender **says so on
stderr** every time one is used rather than refusing it.

Give the worker a boundary that does not depend on command spelling — a sandbox
profile, or a separate uid. An allowlist does not substitute. A guard on
spelling only holds where the action has exactly one spelling: `$(...)` and
`find -exec` are never auto-allowed by a prefix rule because they can run
anything, and during this plugin's own development a worker denied
`Bash(herdr agent start:*)` reached a live agent anyway by calling herdr's
socket from Go, logging zero denials. Blocking the CLI blocked the convenient
path, not the capability.

An earlier version **refused** these modes unless `WORKTENDER_UNSANDBOXED_OK=1`
was exported. That is gone. It could not tell a caller who had built a sandbox
from one who had read the variable's name, so the confirmation proved nothing
while every unattended dispatch stalled on it. The warning survives; the gate
does not.

Nothing is defaulted. Without `--permission-mode`, dispatch changes nothing about
what an agent may do.

Details that bite:

- **Dispatch, then gate.** The gate ignores whatever the pane already held when it
  opened — that was a previous task's answer, and releasing on it is the stale
  hand-off the gate exists to prevent.
- **The worker reads `HERDR_PANE_ID` from its own environment and never asks.**
  Running `report` from another pane's shell files the report against *that* pane.
- **Outside herdr, `report` prints the envelope, warns on stderr, and exits 0.** A
  caller checking only the exit code sees success where nothing was delivered.
- **`--until` is repeatable** — pass it more than once to release on any of
  several statuses. So is `--target`, which is `--any` spelled one at a time.
- **`--timeout` defaults to 15 minutes, and there is no wait-forever option.** A
  gate that cannot expire wedges a coordinator with no diagnosis, which is worse
  than no gate.
- A `--pr` that is not a positive integer is **fatal, not dropped**. A note over
  200 characters is **refused, not truncated** — shorten it and report again.

Report `blocked` when you are actually blocked: that releases the coordinator's
gate with a failure instead of making it wait out its clock, and the only party
who can unblock you is the one sitting in that wait.

## Why the report has no free text

**A report is three fixed slots**: a status from a closed set, an optional pull
request number, and a note capped at 200 characters. The shape is the feature,
not a limitation of it.

A worker's task usually arrived as a GitHub issue, whose body is written by anyone
who can file one, so a worker may be relaying a stranger's words — and a report is
therefore not a message the worker composes but slots the *coordinator* renders
into its own prompt. The note reaches the coordinator quoted and announced as
untrusted data, and the cap bounds how much of it can ever reach the context most
worth protecting.

The same reasoning bounds the gate. `--until` matches the status, `--require-pr`
matches the presence of the pull request number, and there is no way to write a
predicate over the note. A `--note-contains` would hand whoever wrote that issue
the decision of when the coordinator's next agent starts.

**What a gate does not establish is authorship.** It proves a well-formed report
appeared in the worker's pane after the gate started, and nothing about who
composed it. Any process already holding the herdr socket could write those slots
onto another pane. That is a limit to state rather than a hole to engineer around:
it needs code already running as you, so it crosses no privilege boundary — and a
shared secret would not close it either, because the dispatch prompt sits in the
worker's context beside the untrusted text, so anything that can talk the worker
into faking a report can read the secret out of the same context and include it.

Treat a `done` as a claim. Check the pull request it names.

The gate says as much on every outcome, as **evidence**: the release line and
every failure message end with it, and `--json` carries it as a field.

| Evidence | Outcome | What it rests on |
| --- | --- | --- |
| `reported` | `released`, `blocked` | The worker's report. The gate checked its shape and that it is new, not that it is true. |
| `verified` | `timeout`, and `worker_gone` when the pane exited or closed or its workspace closed | The gate's own clock, or herdr announcing the pane gone. Nothing the worker said. |
| `inferred` | `worker_gone` when herdr's agent status went `unknown` | What an agent that has gone away looks like, which is not herdr saying it went. |

## How a report actually reaches a gate

**Two channels, read on every look, and neither wins.**

`report` attaches the envelope to the worker's own pane as herdr metadata, and
also prints it. The gate reads the metadata *and* the pane's terminal buffer
each time it wakes.

Metadata exists because the pane alone never worked for the agent kind this is
used with: Claude Code collapses a finished tool call to `Ran 1 shell command`,
so a worker that *runs* `report` leaves the envelope in its transcript and
nothing on screen.

But metadata does not supersede the terminal, and that asymmetry was tried and
removed. A reader that stopped at metadata the moment it held anything left a
worker that reported `planned` over the tool call and finished by echoing `done`
permanently unreleased.

Three consequences worth knowing before you build on this:

- **A worker that merely prints a well-formed envelope has reported.** It need
  never run the command. The parser tolerates Claude Code's `⏺ ` decoration and
  arbitrary indentation, because one demanding the bare header at column zero
  read nothing at all from the pane of the very agent this exists to gate.
- **Identity is a per-channel counter, never content.** Two byte-identical
  reports are two reports, so a coordinator dispatching the same slice twice is
  heard twice — which content comparison would have made inaudible. The
  terminal's counter legitimately goes *down* as the buffer scrolls, and the
  gate follows it down on purpose.
- **Neither channel authenticates authorship**, only shape and position. See
  above, and [SECURITY.md](../SECURITY.md).

---

[← README](../README.md)
