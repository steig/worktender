# Dispatching a worker, and waiting for it

This is what the plugin is for once more than one agent is involved: a
coordinating agent hands a slice of work to another and needs to know when it is
done — without reading everything that agent did to get there.

```sh
# Resolve the binary once. It is not on PATH; herdr owns the install.
# `worktender doctor` prints this line, so you need the jq only once.
worktender=$(herdr plugin list --json \
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
composer is a worker with nothing to do that a listing reports as `idle`. The brief names the issue rather than carrying it — the worker runs `gh
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
worktender start <issue> [--model <model>] [--permission-mode <mode>] [--base <ref>] [--repo <path>] [--focus]
worktender dispatch --pane <id> --name <agent> [--model <model>] [--permission-mode <mode>] [--resume]
worktender report --status planned|blocked|done [--pr N] --note <text>
worktender gate --target <agent|pane> | --any <a,b,c> [--until done] [--require-pr] [--timeout 15m]
```

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
# gate: 13-other released after 4m12s
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
