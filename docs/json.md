# Machine-readable output

Every command takes `--json`. It replaces the text rather than joining it:
**one shape or the other on stdout, never both.** An action's output is read
back out of the plugin log and parsed, so a document with a stray line above it
is a document nobody can parse.

The commands split into two groups, and they behave differently in one way worth
knowing up front:

- **`ls`, `doctor`, `sync`, `prune`, `prune-apply`** — inspection and
  reconciliation. Documented below.
- **`start`, `dispatch`, `report`, `gate`** — the four an agent orchestrates
  with. **These write their document on the failure paths too**, because the
  exit code alone cannot say *which* of five workers a `--any` gate was about,
  and that is the whole answer. Everything they would have printed for a human
  goes to stderr instead.

Each of the four also carries **`exit_code`**: the code the process is about to
leave with. It is redundant with `$?` and deliberately so — a document read off
a pipe is routinely separated from its exit status, and the two disagreeing is
then a visible bug rather than a silent one.

> **The shape may move before 1.0.** It is a supported way to consume this
> plugin, not a stability promise. Pin the version you built against.

## Why the table is not enough

The alignment is computed from the data, so a column moves the moment a branch
name gets longer. And the table has one word for four different absences:

```sh
$ worktender ls --pr
* main                      w21  w21:p1  idle     1057  -       worktender
  worktree/brave-valley     -    -       -        -     -       brave-valley-66f8
```

Every `-` there means something different — no workspace, no pane, no agent, no
counter, no pull request — and the last one means *two* things the table cannot
separate:
this branch has no pull request, and `gh` could not be asked. That second
reading is the expensive one. An unauthenticated `gh` fails exactly like a
branch nobody opened a pull request for, and the verdict that follows is *keep*,
so prune keeps everything while every reason it prints reads as ordinary.
`doctor` exists to explain that in prose. The JSON says it in a field.

The directory column spends the same `-` on something that is not an absence at
all: it prints one when the directory is named after the branch, which is every
worktree `start` made. The table would otherwise put the widest name in it
twice. Nothing is lost — `dir` is populated in the JSON on every row, whether or
not the table drew it.

## `ls --json`

```sh
$ worktender ls --pr --json
{
  "worktrees": [
    {
      "main": true,
      "branch": "main",
      "workspace_id": "w21",
      "pane_id": "w21:p1",
      "agent_status": "idle",
      "agent_status_seq": 1057,
      "pr": null,
      "report": null,
      "dir": "worktender",
      "ghost": false
    },
    {
      "main": false,
      "branch": "worktree/brave-valley",
      "workspace_id": null,
      "pane_id": null,
      "agent_status": null,
      "agent_status_seq": null,
      "pr": { "state": null, "error": "gh pr list worktree/brave-valley: gh: To get started with GitHub CLI, please run: gh auth login" },
      "report": null,
      "dir": "brave-valley-66f8",
      "ghost": false
    }
  ],
  "repositories": null
}
```

**Exactly one of `worktrees` and `repositories` is non-null, and both keys are
always present.** That is the contract: `worktrees` answers for one repository
and `repositories` is the `--all-repos` grouping, so a consumer reads which
question was asked off the document rather than off the flags it thinks it
passed. Neither key is ever omitted — an absent key would be indistinguishable
from a worktender too old to have it, which is the one thing this pair exists to
tell apart.

Absence is `null`, always. `pr` has three states and they are the reason this
object exists rather than a string:

| `pr` | Means |
| --- | --- |
| `null` | Nobody asked. `--pr` was not passed, or this is the main checkout, which is never asked about — trunk has no pull request and every lookup is a network round trip. |
| `{"state": null, "error": null}` | Asked, and this branch has no pull request. |
| `{"state": "OPEN", "error": null}` | Asked and answered. Also `MERGED` and `CLOSED`. |
| `{"state": null, "error": "…"}` | **`gh` could not be asked.** Not the same as no pull request, and the one the table cannot show you. |

`pane_id` is the pane `dispatch --pane` takes, which is what makes this listing
something to act on rather than only display.

### `ghost`

`ghost` is `true` on an entry that is a **herdr workspace rather than a
worktree**: herdr is still holding it open on a checkout git's worktree list
does not have, which is what removing a directory out from under it leaves
behind. `branch` is `null` on such an entry, because there is no checkout left
to have one, and `dir` is the basename of the path herdr still believes in. The
table marks the same row `?`.

It is diagnosis and never a removal, in the JSON as in the table. Closing a
workspace is a different authority from removing a worktree, and this plugin
does not take it: there is no checkout left to test for uncommitted work, so the
strongest guard here is not unsatisfied but *unavailable*, and the workspace's
panes may still hold a live agent whose conversation closing it would destroy.
`prune` reports the same entry and removes nothing.

Before this existed the state was in no listing at all. `Reconcile` walks
worktrees to adopt and to prune, and walks workspaces only to read through to a
worktree, so a workspace that outlived its checkout was visited by nothing —
and `prune` printing `nothing to do` is the same output a clean repository
gets.

### `agent_status_seq`, and why it is not a time

`agent_status` is a point-in-time enum with no time in it. It is the same
`idle` for a worker that finished two seconds ago, one that finished forty
minutes ago, and one that never received its brief at all. `agent_status_seq`
is what separates them.

It is herdr's `state_change_seq` for the agent in `pane_id`, passed through
untouched: **a counter, not a clock.** That is not a design preference. Measured
against a live herdr 0.7.5 / protocol 18, no field on a pane, a workspace or an
agent carries a timestamp of any kind, so there is no last-activity time to
surface and nothing here invents one.

What the counter does, measured over a live session rather than read off herdr's
schema, which documents none of it:

- **Session-wide and monotonic.** Nineteen agents held nineteen distinct values
  from one range, so two rows are comparable: the lower one is the one herdr
  stopped seeing first.
- **Stamped when herdr sees the agent's state change** — which is not the same
  set of moments as `agent_status` changing, in either direction. It moved for
  an agent whose status read the same in two consecutive samples, so it catches
  a `working` → `idle` → `working` that the status column hides entirely. And it
  stayed put across a `done` → `idle` transition, which is a worker that has
  done nothing being relabelled.
- **Null** when herdr has no agent in that pane. Never `0` for a live agent —
  a zero would read as one that has never done anything.

#### Where it goes blind: a frozen counter on a `working` row

It counts state *changes*, so an agent that stays in one state does not move it
— and **an agent thinking hard is exactly that.** A long turn is not an unusual
case for a worker doing careful empirical work; it is the normal one, and it is
when a coordinator most wants to know the worker is fine.

So what a frozen counter means depends entirely on the status beside it:

| Counter, between two of your reads | `agent_status` | What it says |
| --- | --- | --- |
| Moved | any | **Alive**, and herdr saw it move. |
| Frozen | `idle`, `done` | Finished, or wedged. This is the pair #90 exists to separate, and it does. |
| Frozen | `working` | **Nothing.** Deep in one turn, or completely stopped — the counter cannot tell you which. |
| Frozen | `blocked` | Waiting for a person. The status already said so; the counter adds nothing. |

The `working` row is the one that bites, because it is the row an obvious stall
detector fires on. Measured on a worker of this repository's own: 213 state
changes behind the fleet and frozen there for over half an hour, which by the
counter alone was the most obviously stalled agent in the session. Over the same
window it spent twelve dollars, filled twelve percent more context, and renamed
its own branch to the fix it had settled on.

**Nothing else in this document separates the two, and nothing else herdr has
does either.** Measured against a live herdr 0.7.5 / protocol 18, sampling one
continuously working agent ninety times across fourteen minutes: its
`state_change_seq` held at one value throughout, and so did every other number
herdr exposes
for that pane — the agent's `revision`, the pane's `revision`, the `revision` on
`pane.read`, or `pane.get`'s `scroll`, which never left `0` in either of two
panes producing output throughout and is a scrollback position rather than an
output count. `pane.process_info` carries pids and no CPU time.

Nor is either of the two free-form maps on an agent record, which are the only
place something could publish a counter of its own. `state_labels` was empty on all
sixteen agents in the session, and the `tokens` that were set are [worktender's
own report envelope](dispatch.md#how-a-report-actually-reaches-a-gate) —
`worktender_status`, `worktender_seq` and the rest — written when a worker calls
`report`, which is to say at the end of the work rather than during it.

The only thing that moved was the pane's rendered text, in which the agent's own
footer took 58 distinct values on the way from `$2.15` to `$13.21`. **Zero
movements against 57, over the same worker, in the same window.**

That is the signal that separates thinking from wedged — **cumulative spend** —
and it is deliberately not in this document. herdr offers it only as characters
drawn in a terminal, by an agent that chose that footer and can restyle it
tomorrow; putting a scraped dollar figure in a JSON field would give it a
schema it has not earned. It is also the worse signal for the case the counter
handles well: a finished worker's spend is frozen too, so `done` and dead look
alike on it.

**The two are complementary and neither is sufficient.** A watcher that wants
both reads the counter here and the footer from `pane.read` itself, and alerts
only when *both* have been frozen across the interval it timed:

```sh
# the second signal for a `working` row, out of the pane this listing named
herdr pane read w22:p1 --source visible | grep -o '\$[0-9]*\.[0-9]*' | tail -1
```

That is a scrape and it should be treated as one — the figure belongs to the
agent, not to herdr, and an agent that restyles its footer breaks it silently.
A watcher reading nothing there has learned nothing, which is not the same as
having read a frozen number.

What it is not is elapsed time, and nothing here converts it into any: the rate
depends on how busy the rest of the session is, so seconds cannot be recovered
from one reading. Two readings can, because **the caller has a clock and this
plugin does not run.** A counter that has not moved between two of your own
polls is a worker that did not change state in that interval — an interval you
timed. Carry the status through the comparison, because it is what turns that
into a verdict:

```sh
# workers that did not change state since the last poll, whatever "since" was
"$worktender" ls --all-repos --json \
  | jq -c '[.repositories[] | .root as $r | .worktrees[]?
            | select(.agent_status_seq)
            | {r:$r, b:.branch, st:.agent_status, s:.agent_status_seq}]' > now.json
diff <(jq -c '.[]' was.json) <(jq -c '.[]' now.json)
```

A row that is unchanged and `idle` is finished or wedged. A row that is
unchanged and `working` is the blind spot above, and needs the second signal
before anybody is paged about it.

How long a run of no movement has to be before it counts as stalled is a policy
call, and it stays with you. A `stalled_for_seconds` field would have made it
this plugin's, on a duration it cannot measure.

Read down a single listing the counter also answers the one-shot question: the
row furthest below its neighbours is the worker herdr last saw *change*. Which
is why the status is printed beside it — on a `working` row that is a question
and not an answer.

```sh
# the three herdr saw change least recently, with the status that qualifies it
"$worktender" ls --all-repos --json \
  | jq -r '[.repositories[] | .root as $r | .worktrees[]?
            | select(.agent_status_seq)] | sort_by(.agent_status_seq)
           | .[:3][] | "\(.agent_status_seq)\t\(.agent_status)\t\(.branch)"'
```

**There is still no watcher.** This makes a stall observable; it does not
observe it. The plugin has no resident process — see
[Events](events.md) — and this field adds none.

### `report`, and what it can and cannot tell you

`--reports` asks each staffed pane what the worker in it last told its
coordinator, read back off the pane's own herdr metadata — the same place
`report` attached it and a gate reads it.

```sh
$ worktender ls --reports
* main                      w21  w21:p1  idle     1057  -        worktender
  feat/1-reconcile-execute  w22  w22:p1  working  1055  planned  1-reconcile-execute
  fix/257-erasure-comments  w1K  w1K:p1  idle     812   done #4  257-erasure-comments
```

```json
"report": { "found": true, "status": "done", "pr": 4, "note": "landed", "error": null }
```

- **`found`** is false when the pane carried no report. An ordinary answer — a
  worker that has not reported yet — and a different fact from **`error`**,
  which is the pane not being readable at all. The table has room for neither
  and prints `-` for both — the same `-` a worktree with no pane to ask prints,
  which makes it three absences in one cell on the column where the difference
  is between a fleet that is quiet and a listing that never reached it.
- **`note`** is the worker's own 200 characters and is **untrusted text**: the
  task usually arrived as a GitHub issue whose body anyone could have written.
  It is in the document because a human reading one wants it. Branch on
  `status`, never on this. The note is deliberately not in the table.
- **`report`** is null, rather than an object, when no lookup ran — `--reports`
  was not passed, or the worktree has no pane for a report to be attached to.

**This is in-flight state, not a history.** Metadata lives on the pane, so a
released worker's last report is gone with it. That is coherent rather than
broken: the durable record of finished work is the pull request the report
names, and `--pr` is the column for that. What this recovers is what the fleet
is *currently* saying — which is the half a coordinator loses when its context
is cleared, and the half it should not have been writing down.

Unlike `--pr` this works with `--all-repos`: the lookup is one herdr call
against a pane herdr already named, so there is no wrong repository to ask.

## `ls --all-repos --json`

Across repositories the answer is **grouped**, and `worktrees` is null:

```sh
$ worktender ls --all-repos --blocked --json
{
  "worktrees": null,
  "repositories": [
    {
      "root": "/Users/you/code/worktender",
      "name": "worktender",
      "error": null,
      "worktrees": [
        {
          "main": false,
          "branch": "77-cross-repo",
          "workspace_id": "w30",
          "pane_id": "w30:p1",
          "agent_status": "blocked",
          "agent_status_seq": 812,
          "pr": null,
          "report": null,
          "dir": "77-cross-repo",
          "ghost": false
        }
      ]
    },
    { "root": "/Users/you/code/lighthouse", "name": "lighthouse", "error": null, "worktrees": [] },
    { "root": "/Users/you/code/gone", "name": "gone", "error": "worktree.list: not a git repository", "worktrees": null }
  ]
}
```

This is the other side of the invariant above: `worktrees` is `null` here, and
still present.

Grouped rather than a `repository` field on every row, which was the other
candidate and the cheaper one. Three things decided it:

- **A per-repository failure needs somewhere to live.** `--all-repos` keeps
  going when one repository cannot be read, and a flat array of worktrees has
  nowhere to record that except by inventing a row that is not a worktree.
- **A repository whose rows were all filtered out has to survive as an empty
  group.** *Asked, and none* versus *not asked* is the distinction this whole
  format exists to keep, and a flat array erases it by simply having fewer rows.
  That is `lighthouse` above: read, and nothing blocked.
- **`doctor --json` already reports per repository**, so the two
  cross-repository views nest the same way rather than each inventing one.

Within a group, `worktrees` is `null` when the repository could not be read and
`[]` when it was read and nothing matched. `--pr` is refused with `--all-repos`:
the lookup runs one `gh` call per branch in series and is scoped to a single
repository, so across several it would be slow and asking the wrong repository.

## `sync --json`, `prune --json`, `prune-apply --json`

```sh
$ worktender prune --repo . --json
{
  "repository": "/Users/you/code/thing",
  "results": [
    {
      "status": "planned",
      "kind": "prune",
      "target": "done",
      "detail": "would remove: merged into main",
      "branch": "done",
      "path": "/Users/you/code/thing/.claude/worktrees/done",
      "workspace_id": "w4",
      "pane_id": null,
      "agent_name": null,
      "reason": "merged into main",
      "releases_agents": false
    }
  ]
}
```

- **`repository`** is the `repository:` line the table prints above itself. It is
  a field rather than a line because those commands resolve the repository from
  herdr's context, which on a machine with several open is routinely not the one
  you meant — it is the fact worth reading, so a consumer must not have to
  re-derive it.
- **`status`** is `done`, `planned`, `skipped` or `failed`. `planned` is a dry
  run: `prune` never removes anything, and `prune-apply` is the deliberate
  second step.
- **`reason`** is why the reconciler planned the action; **`detail`** is what
  became of it. The table has room for one of them.
- **`releases_agents`** marks a prune that also takes a finished agent's pane
  away, which only `--release-agents` produces. A coordinator tracking its own
  workers wants to know which removals ended one.
- **`results`** is one document for the whole command, even though `sync`
  reconciles in up to three passes. The exit code is unchanged — a failed action
  still exits non-zero, and the report is written either way, because a consumer
  that learns only the code learns nothing about *which* action failed.

## `doctor --json`

```sh
$ worktender doctor --json
{
  "checks": [
    { "name": "version", "value": "0.8.0 @f074c65", "state": "warn", "note": "origin/main is at @a1b2c3d; run `worktender update`" },
    { "name": "herdr",   "value": "0.7.5",          "state": "ok",   "note": null },
    { "name": "gh",      "value": "not authenticated", "state": "warn", "note": "reads as \"no pull request\", so prune will keep almost everything" },
    { "name": "events",  "value": "unset",          "state": "off",  "note": null }
  ],
  "binary": "/Users/you/.config/herdr/plugins/github/steig.worktender-3ebd/bin/worktender",
  "repositories": [
    {
      "root": "/Users/you/code/thing",
      "name": "thing",
      "error": null,
      "worktrees": 3,
      "agents": { "working": 1, "blocked": 1, "idle": 1 },
      "blocked": [
        { "main": false, "branch": "79-stuck", "workspace_id": "w7", "pane_id": null, "agent_status": "blocked", "agent_status_seq": null, "pr": null, "report": null, "dir": "79-stuck", "ghost": false }
      ]
    }
  ],
  "error": null
}
```

- **`state`** is `ok`, `off`, `warn` or `fail`. `off` is the documented default
  for events and is not a problem; `warn` is a capability lost without being
  told.
- **`repositories`** is `null`, not `[]`, when herdr could not be reached — an
  empty list is the answer for a herdr with nothing open, which is a different
  fact. The reason is in **`error`**, and the command still exits 2 — herdr being
  unreachable is the environment, not the call.
- A repository that could not be read carries its own `error` and null counts,
  and costs the others nothing.
- **`blocked`** names the worktrees rather than leaving them as one number among
  `agents`. It is the one status where the session has stopped and only a person
  can restart it, so which worktree it was is the entire question — a count
  answers nobody. Empty when none are, null when the repository could not be
  read. `pane_id` is null throughout, and `agent_status_seq` with it: `doctor`
  does not ask for panes, and the counter belongs to the agent in one. `ls
  --all-repos --blocked --json` is the view that carries both — the pane a
  dispatch needs, and the counter that says whether it is worth dispatching to.
- The scope is herdr's open worktree workspaces rather than wherever you are
  standing, so `doctor` answers from outside any repository at all — the same
  scope, and now the same read, that `ls --all-repos` lists in full.

## `gate --json`

The one where the exit code is not enough. With `--any` a coordinator needs to
know *which* worker released, and that is not a thing a number can say.

```sh
$ worktender gate --any wt-42-a,wt-43-b --until done --json
{
  "outcome": "released",
  "exit_code": 0,
  "target": {
    "name": "wt-43-b",
    "pane_id": "w10:p1",
    "workspace_id": "w10",
    "baseline": null
  },
  "report": { "status": "done", "pr": 4, "note": "green" },
  "waited_seconds": 252,
  "timeout_seconds": 900,
  "until": ["done"],
  "require_pr": false,
  "waiting": [
    { "name": "wt-42-a", "pane_id": "w9:p1", "workspace_id": "w9", "baseline": null },
    { "name": "wt-43-b", "pane_id": "w10:p1", "workspace_id": "w10", "baseline": null }
  ],
  "error": null
}
```

- **`outcome`** is `released`, `blocked`, `timeout`, `worker_gone` or `error`.
  Finer than the exit code on purpose: `timeout` and `worker_gone` are both
  exit 4, and a coordinator deciding whether to redispatch may reasonably want
  to know a pane *died* rather than that nobody answered in time.
- **`target`** is the worker the outcome is about — which one released, which
  reported blocked, which one's pane died. **Null on a timeout**, the one
  outcome belonging to no single worker.
- **`waiting`** is every worker covered, in the order named. Each carries the
  **`baseline`** its channels already held when the gate opened, which the gate
  ignored as a previous task's answer. That field is how a coordinator that
  gated too late tells the case apart from a worker that never reported.
- **`until`** and **`require_pr`** echo the request, because a document filed
  away is unreadable without knowing what was asked of it.
- **`note`** is carried for a human to read. It is still untrusted data, and
  still not a thing the gate can be asked to match on — see
  [Why the report has no free text](dispatch.md#why-the-report-has-no-free-text).

## `start --json`

```sh
$ worktender start 42 --repo . --json
{
  "repository": "/Users/you/code/thing",
  "issue": 42,
  "branch": "42-fix-the-thing",
  "workspace_id": "w9",
  "pane_id": "w9:p1",
  "agent_name": "wt-42-fix-the-thing-016aab",
  "base": "origin/main",
  "fork_point": "31db5d1c9b7e4a02f6c1d8e5a3b90f2c4d6e8a10",
  "stacked": false,
  "briefed": true,
  "staffing": [ { "status": "done", "kind": "staff", "…": "…" } ],
  "gate_command": "…/worktender gate --target wt-42-fix-the-thing-016aab --until done --require-pr",
  "error": null,
  "exit_code": 0
}
```

- **`agent_name`** is not the branch. herdr's agent namespace spans every
  repository at once, so the name carries a digest of the repository — and it is
  what `gate` takes.
- **`gate_command`** is that wait, assembled. Carried because the digest is not
  something anyone should be retyping.
- **`stacked`** is true when the fork point is not the base's own tip, so this
  branch sits on commits the base does not have — the case worth noticing before
  the base is squash-merged, which would land none of them. Same test the
  printed `stacked:` warning makes.
- **`briefed`** is a separate fact from an agent having started. herdr answering
  ok means keystrokes were delivered, not that anything received them.
- **Null fields are progress.** `branch` is null until the issue has been read,
  `pane_id` until the worktree exists, `agent_name` until an agent started. On a
  failure they say how far it got — and a `branch` and `workspace_id` with a null
  `agent_name` is a checkout the caller now owns and must decide about.

## `dispatch --json`

```sh
$ worktender dispatch --pane w9:p1 --name worker-one --json
{
  "pane_id": "w9:p1",
  "workspace_id": "w9",
  "agent_name": "worker-one",
  "staffing": [ { "status": "done", "kind": "staff", "…": "…" } ],
  "gate_command": "…/worktender gate --target worker-one --until done",
  "error": null,
  "exit_code": 0
}
```

**`gate_command` is null when the dispatch failed.** Naming a wait for an agent
that never started would be an instruction to wait for nothing.

## `report --json`

The envelope **as accepted**, which is not always the envelope as intended.

```sh
$ worktender report --status done --pr 4 --note green --json
{
  "status": "done",
  "pr": 4,
  "note": "green",
  "note_length": 5,
  "note_limit": 200,
  "pane_id": "w9:p1",
  "delivered": true,
  "error": null,
  "exit_code": 0
}
```

- **`delivered`** is whether the metadata channel took it. **False with no
  error** means the envelope printed and nothing else, which is what `report`
  outside herdr looks like — a gate can still read it off the terminal, but only
  while the line is in the buffer. That is not a failed report and it exits 0.
- **`pane_id`** is null in the same case.
- **`note_length`** against **`note_limit`** because the note is *refused*
  rather than truncated when it is too long, and a worker building one
  programmatically should not have to guess how this command counts runes.
- The envelope still goes to a terminal in JSON mode, on **stderr**. It is one of
  the two channels a gate reads, and a worker whose stdout its own tooling
  captured would otherwise have reported to nobody.

## Call the binary, not the action

A herdr action is a fixed command array with no argument surface, so
`Worktender: list worktrees` cannot be asked for `--json` — and its output lands
in the plugin log rather than on your stdout anyway. Resolve the binary once and
run it directly:

```sh
worktender=$(herdr plugin list --json \
  | jq -r '.result.plugins[] | select(.plugin_id == "steig.worktender") | .plugin_root')/bin/worktender

"$worktender" ls --pr --json | jq '.worktrees[] | select(.agent_status == "working")'

# every blocked agent anywhere, as repository + pane, which is what restarts it
"$worktender" ls --all-repos --blocked --json \
  | jq -r '.repositories[] | .root as $r | .worktrees[]? | "\($r)\t\(.branch)\t\(.pane_id)"'
```

## Two things it deliberately does not do

**It does not escape.** The table replaces a bidi override in a branch name with
`\u{202E}`, because git accepts one in a ref and a terminal will draw
`evil‮hctap` as `evilpatch` — and that table is what a human reads before
approving a removal. The JSON carries the raw name, because it is data: an
escaped branch name cannot be handed back to git, and a consumer that draws it
has to do its own escaping at the point it draws, exactly as `Render` does.

**It is not a second collection path.** The document is a projection of the same
`[]Row` and `[]Result` the table renders, built after the same lookups. Two
paths would drift, and then the table would be the liar.

---

[← README](../README.md)
