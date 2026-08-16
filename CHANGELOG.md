# Changelog

Notable changes to worktender. Format follows [Keep a
Changelog](https://keepachangelog.com/en/1.1.0/); versioning is
[semver](https://semver.org/spec/v2.0.0.html), with the caveat that `herdr plugin
install` tracks branch HEAD rather than a tag — the version in
`herdr-plugin.toml` is what the no-Go install path pins its download to.

## [Unreleased]

### Fixed

- **A worktree adopted by a reconcile is now staffed by that same reconcile.**
  (#147) `sync`, `startup` and the `worktree.created` / `worktree.opened` hooks
  each ran plan-then-perform inside `lock.Repeat`, which begins another pass
  only when a different process marked the repository dirty. An ordinary run
  therefore gets one pass, and that pass planned from a snapshot taken before
  it acted: `staff()` reads `state.Workspaces`, which cannot hold a workspace
  the same pass went on to open. Adoption left a bare shell until somebody
  reconciled a second time, and the event fast path — issue to worktree to
  agent — stopped at the worktree.

  The three copies of the loop are now one `reconcileOpen`, which runs a
  staffing pass after the loop whenever `perform` reports it opened something.
  That pass staffs and never adopts: re-planning adoptions would let a worktree
  herdr has not yet reported as open be adopted twice, which is a second
  workspace on one checkout. It sits outside the loop rather than inside
  because a repository being marked dirty on every pass could otherwise starve
  it, which is the same bug arriving intermittently.

## [0.9.0] — 2026-08-03

### Added

- **`ls`, `prune` and `prune-apply` now run with herdr absent.** (#122) Every
  removal guard is already entirely git and gh — uncommitted work, commits base
  does not have, a deleted upstream, a merged pull request — so the verdicts
  never needed herdr. The one fact that did was the enumeration, which came from
  herdr's `worktree.list`; `gitx.Worktrees` now answers it from
  `git worktree list --porcelain` when there is no herdr to ask.

  The workspace, pane, agent and counter columns come back empty, and that is
  the honest answer rather than a degraded one: with no herdr those facts do not
  exist, where a herdr that failed to answer has them and will not say which.
  The distinction is recorded as `State.HerdrAbsent` rather than inferred from
  an empty workspace list, because a repository whose checkouts herdr has simply
  not opened yet is exactly what `adopt` exists for — reading that as "herdr is
  gone" would stop the plugin working on first run.

  Absence is established by **dialling** herdr's socket, not by reading
  `HERDR_SOCKET_PATH`. Those are different facts and the difference is the whole
  safety argument: herdr exports that variable into the commands and panes it
  starts and not into the user's own terminal, so its absence there says nothing
  about whether herdr is running. Read as absence, a terminal beside a live
  herdr reports no workspaces and no agents, the guard sparing a checkout an
  agent is standing in never fires, and `prune-apply` force-removes it.
  The endpoint is `$HERDR_SOCKET_PATH` when herdr named one, and otherwise every
  endpoint a herdr could be on: the default session at
  `$XDG_CONFIG_HOME/herdr/herdr.sock` and one per named session under
  `.../herdr/sessions/`. Absence means nothing answering at any of them —
  enumerating the named ones is the same inference error one level down, since a
  plain shell beside `herdr --session work` would otherwise find nothing at the
  default path and call that proof. A stale `HERDR_SOCKET_PATH` naming a socket
  that is gone falls through to the search rather than ending it, for the same
  reason. Two sessions running and nothing saying which is refused rather than
  guessed: the wrong session lists the wrong workspaces, which is the original
  failure through a different door. A pleasant side effect: run from a plain
  shell beside a running herdr, default or named, `ls` now shows the workspace
  and agent columns populated rather than empty.

  On Windows herdr is a named pipe rather than a socket under a config
  directory, and worktender does not know its name — so absence cannot be
  established and is not assumed. It refuses there unless `HERDR_SOCKET_PATH`
  is set, which herdr sets for the commands it runs, leaving the plugin path
  unaffected. Previously the unix layout was used on every platform, so a path
  that never exists on Windows returned ENOENT and unlocked the degraded path
  unconditionally.

  Exactly one dial outcome counts as proof: no socket at the path. A running
  herdr always has one on disk. Every other failure is fatal for every command
  rather than degraded through, because "cannot tell" resolving to "not there"
  would reopen the same hole one layer down.

  A refused connection is explicitly *not* proof, and that is measured rather
  than assumed: on macOS a live listener whose accept queue is full refuses the
  connection with the same `ECONNREFUSED` a socket with nobody behind it gives,
  so degrading on it would let a busy herdr read as gone. It is also the only
  classification that agrees across platforms — dialling a directory gives
  `ENOTSOCK` on macOS and `ECONNREFUSED` on Linux, so any broader rule unlocks
  the destructive path on one platform and not the other. The cost is that a
  herdr killed without unlinking its socket makes worktender refuse rather than
  degrade; the error names the socket to remove.

  `start`, `dispatch`, `sync` and `gate` exit **2**, the environment class, and
  name what is missing. Not a usage error: the command was spelled correctly and
  the machine could not answer it. `ls --blocked`, `ls --reports` and
  `ls --all-repos` join them: each asks what agents are doing, and an empty
  answer would read as "no agent is blocked" rather than "no way to tell".

  One guard genuinely cannot run: the one sparing a checkout an agent is
  standing in. There is nothing for it to protect — an agent lives in a pane
  inside a workspace, and both are herdr's — and the guards protecting *work*
  are git's and untouched. That holds only because absence is dialled for;
  `README.md` says both parts rather than leaving either implied.

  This is about herdr not *running*. Installed as a plugin herdr is present by
  definition, so a way to install the binary without herdr is what would make
  this reachable by anyone new; that part is not in this change.

### Fixed

- **`on-event` and `startup` no longer accept arguments silently.** (#69) They
  were the last two commands that took whatever they were given and exited `0`.
  Every other command now rejects an unknown flag or a stray argument with a
  usage error; these two dropped their arguments before they were ever read,
  so a scoped invocation was indistinguishable from an unscoped one — on the
  two commands that can start coding agents without being asked.

  Nothing legitimate is affected: herdr invokes both with a fixed array and no
  arguments, the event payload arriving in the environment rather than on the
  command line. The refusal comes *before* the `WORKTENDER_EVENTS` opt-in is
  read, because a malformed invocation is malformed either way and answering
  one with the events-off notice reports the argument as accepted. It reaches
  nothing the opt-in guards: no herdr call, no payload load.

### Added

- **`ls` and `prune` now report a workspace whose checkout has vanished.** (#75)
  herdr can hold a workspace open on a directory git no longer lists — what
  removing a checkout out from under it leaves behind. Nothing visited that
  state: `adopt` and `prune` both walk worktrees, and `staff` walks workspaces
  but only ever reads through to a worktree, so a workspace that outlived its
  checkout was in no pass's iteration. Found live against 0.7.0, where herdr
  held three workspaces pointing into a directory that did not exist, `ls`
  printed one row, and `prune` reported nothing — which is the output a clean
  repository gets, on precisely the question this plugin exists to answer.

  `ls` marks such a row `?` and `prune` reports a `ghost` verdict; `--json`
  carries `"ghost": true` on the entry, with `branch` null because there is no
  checkout left to have one. It is the mirror of #66 — that was a workspace
  vanishing underneath the collector, this is one outliving its checkout.

  **Diagnosis only, deliberately.** Closing a workspace is a different authority
  from removing a worktree and does not inherit the prune guards: there is no
  checkout left to test for uncommitted work, so the strongest guard this plugin
  has is not unsatisfied but unavailable, and a ghost's panes can still hold a
  live agent whose conversation closing the workspace would destroy. Nothing
  here removes or closes anything. If a removal is added later it wants its own
  command rather than a widening of `prune-apply`.

  Staffing also no longer plans an agent for a ghost. The join that hid these
  workspaces did not merely omit them — `staff` read through to a zero worktree,
  so an agentless ghost was planned an agent on an empty branch in a directory
  that is not there.

### Changed

- **`ls` no longer prints the branch name twice.** (#133) `start` names a
  worktree's directory after its branch, so on every worktree this plugin
  created the `branch` and `dir` columns held the same string — and it is the
  widest string in the table. The default six-column listing reached 133
  characters on a realistic fleet, overflowing a 120-column terminal before
  `--pr` or `--reports` were asked for, and roughly 50 of those characters were
  the duplication.

  `dir` now prints `-` when it matches the branch. The column keeps its place on
  the rows where it disagrees, which is the case it exists for: a checkout
  adopted rather than created — `worktree/brave-valley` sitting in
  `brave-valley-66f8` — still prints its directory, and that is the row a reader
  most needs it for. Dropping the column outright would have lost that.

  This is the table only. `--json` carries `dir` populated on every row exactly
  as before, so nothing reading the listing as data has to reassemble the name.
  `README.md` and `docs/json.md` both say what the new `-` means, since the
  directory column is now the one place a dash is not an absence.

## [0.8.1] — 2026-08-02

### Fixed

- **The report column was documented as printing an empty cell, and prints a
  dash.** (#134) `docs/json.md` said of `found` and `error` that "the table has
  room for neither and prints an empty cell for both". `reportCell` returns the
  empty string for both, and `cell` maps that to `-` like every other absent
  fact. Loose rather than wrong anywhere else in the document, but this is the
  page whose *Why the table is not enough* section turns on one `-` meaning four
  different things, so an "empty cell" reads as a fifth. It now says `-`, and
  says that a worktree with no pane to ask prints the same one — three absences
  in that column, on the distinction between a fleet that is quiet and a listing
  that never reached it.

  The `ls --reports` figure lied the same way, in both `README.md` and
  `docs/json.md`: whitespace where the unreported worker's dash goes. Both
  figures are now what the renderer prints, and `site_drift_test.go` renders the
  fleet they depict and compares them cell by cell, so the next hand-drawn
  column that disagrees with the binary fails rather than ships.

## [0.8.0] — 2026-08-02

### Added

- **`--release-agents` on `prune` and `prune-apply`, and a prune guard that
  tells an attached agent from a busy one.** herdr frees an agent when its pane
  goes away and at no other moment — there is no `agent release` — so a worker
  that finished its task still occupies the pane it was started in. The guard
  read that as live work, which made the ordinary end state of a successful
  dispatch a worktree nothing could ever remove. Measured after five pull
  requests merged: five checkouts, five lines reading `agent running`, and the
  only way out was closing each workspace by hand in the herdr UI — the manual
  bookkeeping this plugin exists to end.

  The guard now turns on what the agent is doing. `working` and `blocked` are
  live work, and so is any status this build has no name for, because an
  unreadable guard is an unsatisfied one. `idle` and `done` are an agent sitting
  at a prompt with nothing in hand.

  **Nothing is removed by default that was not removed before.** A finished
  agent is still a keep — but a keep that names what would remove it, rather
  than a sentence about live work that reads as a warning. `--release-agents` is
  where that line now goes: it closes the workspace, which is what lets go of
  the agent, and it is re-read at execution time like every other guard, so an
  agent that picked work up between the plan and the removal is skipped. It is
  not a herdr action and cannot be — an action is a fixed command array with no
  argument surface — so it is always something a person typed. Pass it to both
  halves, or the dry run describes a plan the apply will not carry out.

  `prune --json` gained `releases_agents` on each result, so a coordinator
  tracking its own workers can see which removals ended one.

- **A state counter on every `ls` row, so a stalled worker stops looking like a
  working one.** `agent_status` is a point-in-time enum with no time in it: the
  same `idle` for a worker that finished two seconds ago, one that finished
  forty minutes ago, and one that never received its brief at all — which is not
  hypothetical, it is what three workers reported for the whole of #82. Every
  field on a row was a snapshot and none was a rate, so a watcher built on `ls`
  could only fire on state *changes*, and a stuck worker produces none.

  The obvious fix was a timestamp, and herdr does not have one. Measured against
  a live herdr 0.7.5 / protocol 18 — every field of every pane, workspace and
  agent in the session — not one carries a time. `state_change_seq` is what it
  has instead, and `agent_status_seq` is that value passed through untouched.

  A counter, not a clock, and deliberately not converted into one: the rate
  depends on how busy the session is, so seconds cannot be recovered from a
  single reading. Two readings can, and the caller is the one holding a clock.
  Read down one listing it still answers the one-shot question — the row far
  below its neighbours is the worker herdr last saw do anything. What counts as
  *stalled* stays the caller's call, which is why there is no
  `stalled_for_seconds`.

  Measured semantics, since herdr's schema documents none of them: session-wide,
  monotonic, and stamped on herdr's own notion of a state change rather than on
  the status column — it moves for a `working` → `idle` → `working` that column
  hides, and holds still across a `done` → `idle` relabelling. Null when herdr
  has no agent in the row's pane, never `0`.

  It costs one `agent.list` per invocation, not per row or per repository, and
  it is skipped when no row has a pane. A lookup that fails costs the listing
  the column and nothing else. **No watcher, no poll loop, no resident process**
  — this makes a stall observable, which is the opposite job.

- **`ls --all-repos`, so a listing can answer for more than the repository you
  are standing in.** `ls` took a single root and every call below it was scoped
  to that root, which left someone running agents in six repositories with no
  view of them at all — the case this plugin's own README opens by describing.
  The only cross-repository surface was `doctor`'s repos block, and it prints
  counts. The discovery already existed: `startup` computes exactly the set of
  repositories herdr has worktree workspaces for, and `doctor` was calling it
  and throwing away everything but a number. `--all-repos` lists it in full,
  from anywhere, including outside a repository entirely. One repository failing
  prints its error on its own line and costs the others nothing.

  `--pr` is refused with it rather than paired: the lookup runs one `gh` call
  per branch in series *and* is scoped to a single repository, so across several
  it would be both slow and asking the wrong repository.

- **`ls --blocked`, and `doctor` naming blocked worktrees instead of counting
  them.** herdr's `agent_status` has carried `blocked` all along and nothing
  acted on it: `ls` showed it only for the repository you were in, and
  `doctor` averaged it into `"2 working, 1 blocked"` sorted busiest-first —
  which put the one status that needs a human last on the line, since it is
  nearly always the rarest. It is the status worth surfacing because it is the
  one that stays put: working resolves itself, idle is finished or waiting,
  blocked sits there until somebody looks. `doctor` now sorts it first and names
  which worktree it was, in the table and in `blocked` in the document.

  This is herdr's status for the workspace's agent, not a worker's `worktender
  report --status blocked`, which is this plugin's own envelope and reaches only
  whoever gated on it. They are different signals and nothing folds one into the
  other.

- **`ls --all-repos --json` is grouped by repository**, and `worktrees` is null
  when it is — exactly one of the two fields is ever non-null, so a consumer can
  tell which question was asked from the document alone. Grouped rather than a
  `repository` field per row because a repository that could not be read has no
  rows to hang its name off, and one whose rows were all filtered away still has
  to appear as an empty group: *asked, and none* versus *not asked* is the
  distinction the JSON exists to keep. See [docs/json.md](docs/json.md).

- **`start` prints the commit it forked from, and says when that is stacked
  work.** `worktree: <branch> on <base>` named a ref, and a ref is not a fixed
  point. `--base <ref>` makes it easy to fork from a branch whose pull request is
  still open — a useful thing to do, and the way a second slice proceeds while
  the first is in review — but this repository squash-merges, and a squash merge
  puts one new commit on the trunk and none of the base branch's own. So the
  moment the base lands, the stacked branch is on commits the trunk has never
  contained and its pull request shows the base's entire diff as its own.

  Repairing that is `git rebase --onto origin/main <fork-point>`, which needs the
  commit the branch was forked from — and afterwards that commit survives in the
  branch's reflog and nowhere else, which a worker that force-pushed has probably
  lost. It is now in the scrollback by default, alongside `stacked:` and
  `repair:` lines when the fork is not something the base already has. An
  unresolvable ref prints no fork point at all rather than a guess.

  Stacking is documented rather than discouraged: [dispatch.md](docs/dispatch.md)
  covers it end to end, and both skills carry the short form.

- **`--json` on `ls`, `doctor`, `sync`, `prune` and `prune-apply`.** Every output
  path went through `text/tabwriter` and nothing else, so anything that wanted to
  *consume* worktender — a status line, a TUI, a fleet view, a coordinating agent
  asking about its own workers — had to parse a column layout whose widths are
  computed from the data. The flag replaces the table rather than joining it: one
  shape or the other on stdout, because an action's output is read back out of
  the plugin log and parsed. See [docs/json.md](docs/json.md).

  The document is a projection of the same `[]Row` and `[]Result` the table
  renders, not a second collection path, so the two cannot drift apart and leave
  the table the liar.

  **The shape may move before 1.0**, deliberately: the first consumer should not
  also be a compatibility constraint.

- **`ls --json` tells "this branch has no pull request" from "`gh` could not be
  asked".** The table has one `-` for both, and that is the ambiguity that costs
  the most: an unauthenticated `gh` fails exactly like a branch nobody opened a
  pull request for, the verdict that follows is *keep*, and prune keeps
  everything while every printed reason reads as ordinary. `pr` is now an object
  carrying `state` or `error`, and `null` when nothing was asked at all.

- **`gate --any a,b,c` — one wait over a whole fleet.** It releases on the
  first worker to satisfy the predicate and names which one, so the caller drops
  that target and gates again on the rest.

  Waiting on one worker at a time is what this replaces. `start` returns as soon
  as the brief is typed, so a coordinator picking one worker to block on has no
  basis for the choice: wait on the slow one and the four that finished sit idle
  with their reports unread. Five sequential gates at the 15 minute default is a
  75 minute worst case for work that all landed in the first ten. `blocked` is
  the sharper half — the one status only the coordinator can clear was heard
  only while the coordinator happened to be gated on that worker.

  `--target` is now repeatable and fills the same list, so it is `--any` spelled
  one at a time; flag's own last-one-wins would have dropped a target silently.
  There is no `--all`: it is a loop over `--any` in the caller, which has to be
  able to write that loop anyway.

  The timeout is for the wait rather than for each worker — "how long am I
  prepared to sit here" does not multiply by the number of workers sat on. A
  worker that dies, like one that reports `blocked`, ends the wait with a
  failure **naming it**, because the caller's remedy is to drop that one and
  gate on the rest. Every target is resolved before the wait opens, so one
  mistyped name out of five fails immediately rather than at the deadline, and
  naming one worker twice — an agent name and its own pane id both resolve — is
  refused rather than watched twice.

- **The homepage and both descriptions now lead with what the plugin does, not
  with what annoyed its author.** The hero was *"Eleven checkouts. Nobody
  deletes anything."* — a good line answering the wrong question, and by then
  contradicting the page beneath it, which documents the `start` → `gate` round
  trip.

  Both of its claims had also been overtaken. Worktrees are no longer a
  differentiator: Claude Code's own subagents take `isolation: "worktree"`. And
  cleanup is the end of the funnel — nobody goes looking for a worktree cleaner
  until they are already drowning.

  What survives comparison with an ephemeral subagent is not the worktree. It is
  that a worker is a **session you can attach to, rescue, and leave running**,
  and that it hands back three slots and a pull request number rather than a
  context window full of findings — which is the ceiling on how many subagents
  you can fan out to. The homepage says that now, in a callout that credits
  subagents for the case where they are the better tool.

  The line was not deleted. It opens the pruning section, where it sets up the
  argument it was always making.

  The GitHub About field and `herdr-plugin.toml` said something different again,
  at 133 and 172 characters — and the plugin directory card cuts at about 94, so
  the payoff never rendered and `start` was invisible. All four surfaces —
  card, manifest, README subtitle, site meta — now make one claim, and the
  shortest is 84 characters so nothing truncates.

- **`ls --reports`: what each worker last told its coordinator.** The
  coordinator skill instructs a handoff "before your own context is cleared",
  and there was nowhere structured to write one. The tempting fix was a
  `.coordinator` store, which would have made this plugin stateful — `ls` is a
  projection over git and herdr, not a database.

  It was also unnecessary, because most of what a coordinator would write down
  is **already persisted and simply was not reachable**. `report` attaches the
  envelope to the worker's own pane as herdr metadata and a gate reads it back,
  so every worker's last report was sitting there, readable from nowhere but
  inside a running gate.

  So the fleet is asked rather than remembered. `ls --all-repos --reports
  --json` returns what everything is currently saying; the pull requests are the
  durable record of what landed. A handoff shrinks to the one thing neither can
  supply — why the work was sliced that way and what has already been verified.

  **In-flight state, not a history:** metadata lives on the pane, so a released
  worker's last report goes with it. Coherent rather than broken, and stated in
  the docs so nobody builds a fleet history on it.

  The note stays out of the table. It is 200 characters of untrusted text and a
  column is the wrong frame; the JSON carries it, and `found` is kept distinct
  from `error` because a worker that has not reported and a pane that could not
  be read are different facts.

  Explicitly **not** a channel agents write to and read from each other. That
  reopens the threat model `docs/dispatch.md` closes: a worker relaying a
  stranger's issue text into a peer's context as trusted instruction. This is
  single-reader and derived.

- **The hand-written site pages fail loudly when they lie.** `docs/*.md` is
  rendered by `site/build.py` and cannot drift; `site/pages/*.html` is
  hand-written for the narrative material that has no markdown source, and was
  pinned to nothing. Two things shipped wrong that way — an `ls` figure a
  column-and-a-half out of date, and a `start` command the page selling the
  plugin never mentioned — and nothing failed in between.

  Four guards, each verified against the bug it exists to catch:

  - the `ls` figure's column count, against what `internal/wt` actually renders
  - every subcommand in `commands` mentioned somewhere in the pages or docs,
    exempting the two herdr invokes rather than a person
  - every `x.html#y` from a page, and every `x.md#y` from a doc, resolving to a
    real heading — checked against the markdown source, so no Python is needed
  - every hand-written page present in `build.py`'s nav, since one that is not
    is rendered into nothing

  Not generating the pages. They exist because narrative material has no
  markdown source, and that stays true.

### Changed

- **`--json` on `start`, `dispatch`, `report` and `gate`.** It reached `ls`,
  `doctor`, `sync`, `prune` and `prune-apply` — the *inspection* commands —
  while the four an agent actually orchestrates with had none. The plugin
  described itself as driven by your agent rather than by you, and its
  machine-readable surface stopped exactly where the agent's work started.

  **`gate --json` is the one that was missing something an exit code cannot
  supply.** With `--any` over five workers, *which one released* is the whole
  answer, and no number can carry it. The document names it in `target`, carries
  the envelope in `report`, and lists every worker waited on in `waiting` — each
  with the baseline its channels already held when the gate opened, which is how
  a coordinator that gated too late tells that case from a worker that never
  reported.

  **These four write their document on the failure paths too**, unlike the
  reconcile commands: a `blocked` still has to say whose. Everything they would
  have printed for a human moves to stderr, including `report`'s envelope, which
  is one of the two channels a gate reads and may not simply vanish.

  Each carries `exit_code` beside its content. Redundant with `$?` on purpose —
  a document read off a pipe is routinely separated from its exit status, and
  the two disagreeing is then a visible bug rather than a silent one.

- **Five exit codes instead of two, keyed to what the caller does next.**
  ⚠️ **Breaking:** `1` narrows from *any failure* to *usage error*. Anything
  checking `!= 0` is unaffected; anything checking `== 1` changes meaning.

  There were two codes, so `gate` returning `1` could mean the worker reported
  `blocked` and needs a person, or that herdr's socket had died — and those
  demand opposite responses. Five outcomes shared one code, separable only by
  matching on stderr prose, which is the same trap `gh pr view` set for the
  pull request lookup below.

  The codes name a response rather than a cause, because a coordinating agent
  has four available and needs to pick one: **1** usage, fix the invocation and
  do not retry; **2** environment, the machine rather than the call; **3** needs
  a human, escalate because no retry clears it; **4** no answer arrived, so
  redispatching is reasonable. `gate` is fully classified — `blocked` is **3**,
  a timeout or a dead pane is **4**, an unresolvable target is **1**.

  **`2` is also the catch-all for anything unclassified**, deliberately.
  Defaulting to `1` would tell a coordinator to rewrite a correct invocation;
  defaulting to `3` would wake somebody for a bug. An unrecognised failure
  routed to `2` costs at most a retry.

  Taken now rather than additively: with no known dependents this is the
  cheapest the renumbering will ever be, and leaving `1` as a catch-all would
  have shipped the ambiguity being removed.

- **The pull request lookup asks `gh pr list --head`, not `gh pr view`.** Both
  answer the same question; only one answers it in a way that is not English.
  `gh pr view` exits non-zero for "this branch has no pull request" exactly as it
  does for "I am not logged in", so the two were told apart by matching a
  substring of gh's stderr prose — a sentence in another project, changeable in
  any minor release, with nothing here to notice. `gh pr list` returns an empty
  array and exits 0 for the first and fails only for the second.

  `ls --json`'s three-valued `pr` and `doctor`'s gh check both rest on that
  distinction, and a rewording would have folded every branch into "gh could not
  be asked": an error on every row, and prune keeping everything for a reason
  that reads as ordinary. The error text a consumer sees now says `gh pr list`
  where it said `gh pr view`. Behaviour is otherwise unchanged, with one
  addition — a branch carrying several pull requests reads as open if any of
  them is, because reading the closed one prunes live work.

  The suite still pins the contract against a fake gh, but an empty array and an
  exit status are a contract; the fake is no longer repeating a sentence back to
  itself. A second test asks the gh that is actually installed and skips when
  there is none that can reach GitHub, so a future gh that stops answering this
  way fails on someone's machine instead of going quiet. Measured against gh
  2.96.0.

- **A `wt.Row` now holds an empty string where a fact is absent, not `"-"`.** The
  dash is a rendering choice and lives in the renderer; a struct that stored it
  could only hand the ambiguity on. The table's output is unchanged.

- **The gate's release line now names the worker, on `--target` too.** It was
  `gate: released after 4s` and is now `gate: worker released after 4s`. With
  `--any` the name is the answer — it is what the caller drops before gating on
  the rest — and a line that carried it only sometimes would be worse to script
  against than one that always does. A caller matching the old prefix exactly
  breaks; the report envelope printed under it is unchanged.

### Fixed

- **The counter's guidance invited a stall detector that fires on healthy
  workers.** (#112) `agent_status_seq` stamps a *state change*, so a worker that
  stays in one state does not move it — and a worker thinking hard is exactly
  that. Beside `idle` a frozen counter is finished-or-wedged, which is the
  question #90 added it to answer and it still answers. Beside `working` it says
  nothing at all: a long turn and a wedge are the same frozen number, and three
  documents recommended reading the column without saying so. Measured on a
  worker of this repository's own: 213 state changes behind the fleet, frozen
  there for over half an hour, twelve dollars spent in the window, and it
  renamed its own branch to the fix it had settled on.

  The second option the issue raised — surface something that moves during a
  turn — is closed, and closed by measurement rather than by argument. Sampled
  ninety times across fourteen minutes of continuous work on a live herdr
  0.7.5 / protocol 18, every number herdr has for a pane held at one value:
  `state_change_seq`, the agent's `revision`, the pane's `revision`, the
  `revision` on `pane.read`, and `pane.get`'s `scroll`, which never left `0` in
  either of two panes producing output throughout and is a scrollback position
  rather than an output count. `pane.process_info` carries pids and no CPU time.
  Neither free-form map on an agent record is a counter either: `state_labels`
  was empty on all sixteen agents in the session, and the `tokens` that were set
  are worktender's own report envelope, written when a worker calls `report` and
  therefore at the end of the work rather than during it.

  The only thing that moved was the pane's rendered text, where the agent's own
  footer took 58 distinct values on the way from `$2.15` to `$13.21` — zero
  movements against 57, same worker, same window. That is cumulative spend,
  which herdr offers only as characters an agent chose to draw, so it is
  documented as the second signal rather than scraped into a JSON field.

  No behaviour changed and the field is unaltered. `docs/json.md` gains the
  table of what a frozen counter means beside each status and the composite that
  works — alert only when the counter **and** spend have both held still —
  `README.md` and the coordinator skill carry the caveat where they make the
  recommendation, and the claim is a test rather than a sentence: two `working`
  rows on one counter are asserted to be byte-identical, so the day a listing
  can tell a long turn from a wedge, the documents that say it cannot are what
  fails.

- **A docs link that named a section kept its `.md` on the site.** The site's
  link rewriter matched `href="thing.md"` and nothing after it, so a
  cross-document link carrying a `#fragment` — `docs/reference.md` had one, and
  the counter guidance above adds another — was published pointing at a file
  that does not exist there. The fragment now survives the rewrite.

- **`start` pressed Enter once, into an agent that was not yet reading keys.**
  (#108) The brief arrived whole and sat unsubmitted, and an identical `herdr
  pane send-keys <pane> enter` typed by hand seconds later started the agent —
  the reporter's own finding, and the shape of the answer. herdr's
  `agent.start` returns when it recognises the agent's prompt box — measured at
  3.02-3.05s across three runs, and reliably so — but Claude Code draws that
  box several seconds before it will act on a submit, and every key sent in the
  gap is discarded. Measured against Claude Code 2.1.220 in a scratch
  workspace: one press briefed nothing in five runs out of five, and the
  composer held the brief with no stray line break in it, so the key was
  dropped rather than inserted. Pressing again lands it on the third press,
  4.3-5.7s later, three runs out of three. `start` now offers the submit every
  two seconds until the agent shows a sign of life, which end to end takes it
  from 0/3 briefed to 3/3 in about five seconds.

  **The wait that #105 added has gone, because it was measuring the wrong
  thing and reading it backwards.** It waited for the brief to appear in the
  pane before pressing, on the theory that the Enter was landing inside an
  unfinished paste. Both halves are false: against a settled composer an Enter
  6.7ms behind the text submits, and against an unsettled one no delay helps
  at all. Worse, the read-back was inverted. A composer still starting up
  renders the paste as raw text, so it matched and reported the brief as seen —
  the state in which the submit is about to be lost. Once Claude Code has
  started it collapses the paste to `[Pasted text #1]`, which no snapshot
  source contains, so it matched nothing and reported the brief as missing —
  the state in which the submit works. `start` was paying five seconds for a
  readiness signal it then read the wrong way round, and the failure message it
  produced named the wrong repair.

  A press that arrives after the brief has already gone is harmless — measured
  twice over: Claude Code will not send an empty composer — and the loop stops
  at the first sign of life regardless.

- **The stacking guidance told agents to run the wrong rebase.** (#109) It said
  that while the parent's pull request is still open, "an ordinary `git rebase
  origin/main` is enough — the child's own commits are the only ones it has to
  replay". `git rebase <trunk>` replays every commit the child has that the
  trunk does not, and while the parent is unmerged that is the parent's commits
  too. Measured in a scratch repository, trunk → parent (2 commits) → child
  (1 commit), trunk then moved on: `git rebase main` on the child reported
  `Rebasing (1/3)` and rewrote all three, two of which the child did not write.
  Any conflict in those lands on whoever ran it, in code they never touched.

  The replacement is one shape for both sides of the merge —
  `git rebase --onto <target> <fork-point>`, where the fork point is what holds
  the replay to the child's own commits. After the parent lands the target is
  `origin/main`, which is what the guidance already said and is unchanged.
  Before it lands the target is the parent's branch, after rebasing that branch
  onto the trunk: `Rebasing (1/1)`, and the child ends up on the parent's actual
  tip rather than on copies of it. Bare `git rebase <parent-branch>` is not the
  same instruction — it survives only while git can still match the rewritten
  patches, and stops inside a commit the parent wrote as soon as the parent's
  own rebase resolved a conflict. The fork point also moves: after a restack it
  is the parent's new tip, not the sha `start` printed.

  `start`'s `repair:` line said "rebase before it merges", which is the same
  advice compressed; it now names the target for each side. The claim is a test
  rather than a sentence now (`stacking_test.go`) — it builds that scratch
  repository and counts what git replays, because this shipped past both an
  author and a reviewer who checked it by reading.

- **The brief no longer carries the issue; it says to go and read it.** (#94)
  The body used to be flattened onto one line, capped at 4000 runes and pasted
  between markers, and the result arrived at the worker **twice and cut
  mid-sentence**. Every one of those three mechanisms was a workaround for
  putting untrusted prose where instructions go, and dropping the body retires
  all of them at once: the worker runs `gh issue view` and reads the same text
  as tool output, uncut and unflattened. Nothing an issue author writes reaches
  the brief now — the title survives only as a branch name `reconcile.Slug` has
  already reduced to `[a-z0-9-]`. `start` does not even ask `gh` for the body.

  It also fixes what the length cost. **Measured against protocol 17**, in a
  scratch pane rather than argued: a pane delivers text to its process in reads
  of at most **1022 bytes**, so a 4400-byte brief arrived as five separate
  bursts — and the Enter followed the last of them by **10µs**, in a read of its
  own. That is no separation at all for a TUI batching its input, which is why a
  submit "sent after the text" could still be swallowed as part of the paste.
  The brief is now one burst.

  The hypothesis in the issue — that `pane.send_text` returns before the TTY has
  drained, so the Enter lands mid-stream — **was tested and is false.** A
  4400-byte payload followed immediately by `pane.send_keys` arrived byte-exact
  with the newline strictly last, twice over. The ordering was never the problem;
  the proximity was.

- **`start` reads the brief back out of the pane before pressing Enter.** (#94)
  The same accepted-versus-delivered gap `writeReport` closes on its metadata
  tokens, applied to the payload that matters most. It polls `pane.read` until
  the tail of the brief shows up — comparing only letters and digits, so a
  composer's wrapping and borders do not hide it — and that observed render is
  the separation the Enter needs, rather than a sleep guessing at how much is
  enough.

  It is best effort by design: not seeing the text is not evidence it is absent,
  since a TUI may collapse a paste into a placeholder, so the Enter is pressed
  either way and the existing confirmation stays the judge. What the answer buys
  is the diagnosis. A brief that never appeared and one sitting unsubmitted need
  different advice, and the old message told you to press Enter again in both
  cases — which is what #94 followed into a composer holding a mangled brief.

- **Staffing waits out a pane that is still busy, because herdr does not.** (#95)
  `start` on a fresh worktree failed with `agent_pane_busy` while its shell was
  still in direnv, nix or a login banner, leaving a created worktree with no
  agent and needing a second, different command to finish. `AgentStartTimeout`
  was 60 seconds and said in its own comment that it bounded exactly this wait.
  **It did not.** Measured against protocol 17: `agent.start` on an occupied pane
  answers `agent_pane_busy` in **1.6–3.0ms**, identically with `timeout_ms`
  unset, 1000, 60000 and 120000. The constant described a guarantee that did not
  exist, and the comment now says what was measured.

  So `execute.staff` does the waiting — every staffing path, not just `start`,
  since `sync` staffed the same fresh worktrees. It retries only
  `agent_pane_busy`, the one code a later attempt can change, and re-reads the
  "has somebody else staffed this workspace" guard on every attempt, because a
  minute of waiting is a minute in which they can. A staffing that waited says
  so in its report line, and one that gives up names what it was waiting for.

- **A reused branch is read by its latest pull request, not by whichever one
  `gh` listed first.** `GhPRLookup` preferred an `OPEN` pull request and
  otherwise took `payload[0]` — gh's list order, which is undocumented, not
  guaranteed by the API, and was asserted by nothing. A branch that was merged,
  pushed to again, and whose second pull request was closed unmerged has both a
  `MERGED` and a `CLOSED` one and neither is open. If the merged one arrived
  first, `verdict()` read "PR merged", which is unconditional authority to
  remove a worktree — so a checkout holding the second attempt's commits was
  pruned, under a reason that says the work landed. `git branch -d` will not
  force, so the commits stayed reachable in the repository; the checkout and the
  accurate reason did not.

  The lookup now asks for `createdAt` and takes the newest. An open pull request
  still wins outright. Where the timestamps cannot decide — a tie, or a gh that
  stops sending the field — `CLOSED` beats `MERGED` rather than falling back to
  the array order, because keeping costs disk and the other direction costs
  work. Pinned by tests that assert `CLOSED` + `MERGED` in both orders.

- **`start` submits the brief, and confirms it was taken up.** It typed the
  brief with a trailing newline and reported "briefed". The newline did not
  submit: a brief is kilobytes arriving in one burst, the TUI reads a burst as a
  paste, and a newline inside a paste is a line break. The brief sat in the
  composer, herdr answered ok for having typed it, `start` exited 0, and the
  worker showed up in `ls` as `idle` — which reads as "waiting for work" and
  meant "was never given any". Three for three on real issues.

  Enter is now its own key event through `pane.send_keys`, and `start` then
  waits up to 15s for herdr to report the agent working before it says
  "briefed". Anything but `idle` counts — an agent that came straight back
  asking permission has plainly read its brief. When the wait runs out `start`
  fails and names the pane to press Enter in, because ok from herdr means it
  delivered keystrokes and not that an agent received a prompt. This is the
  read-back `writeReport` already does for a 200-character note; the brief is
  the entire content of the work and had no confirmation at all.

  `PaneSendText`'s doc comment said a newline submits and that text must be one
  line for that reason. It does not, and the reason has been rewritten to what
  was measured. The brief stays one line regardless: whether a newline submits
  depends on how the TUI classified the burst, and untrusted issue text does not
  get to make that choice either way.

- **`start` can be run.** Neither documented path worked. Go's `flag` stops at
  the first non-flag argument, so the order the usage string, the README and the
  worktrees skill all printed — `start 42 --model sonnet` — counted the flags as
  issue numbers and was refused by a message repeating the order that had just
  failed. Flags now parse on either side of the number, so the documented order
  is the one that works.

  And with the arguments in an order that parsed, `start` refused anyway: it
  creates a checkout, so it will not guess a repository, and the context it
  would resolve one from is injected only when herdr invokes a plugin action —
  which `start` cannot be, since an action is a fixed command array and `start`
  is nothing without its issue number. It now takes **`--repo <path>`**, the
  same flag with the same meaning `prune` and `prune-apply` take, and the
  refusal names it.

- **Agent names are now scoped to the repository, so two repositories can be
  staffed at once.** They were derived from a checkout's directory basename
  (`sync`) or an issue branch (`start`), and neither is unique across
  repositories: two repositories with a worktree called `api`, or an issue #12
  each, produced one name.

  Measured against herdr protocol 18, whose schema says nothing about
  uniqueness: **herdr enforces it.** `agent.start` answers `agent_name_taken`
  and names the pane already holding the name. So the second repository got no
  agent at all, under an error pointing at the first repository's pane — and a
  `sync` that staffed nothing looked like a `sync` with nothing to do. (The
  worse possibility, an ambiguous name resolving `gate` onto the wrong
  repository's worker, does not happen: `agent.get` never sees two.)

  A name now carries a six-character digest of the repository root and the whole
  basename: `wt-42-fix-the-thing-016aab`. The digest goes last because a
  truncated head is the second half of the same defect — herdr's 32-character
  limit made two long branches of *one* repository converge — and a
  disambiguator at the front is the first thing the limit cuts. The `worktender-`
  prefix for a name not starting with a letter is now `wt-`, which is where the
  digest's characters were bought back; issue branches always need it.

  **The name a live agent holds does not change under it.** herdr frees a name
  when its pane goes away, so the only names that exist are the ones running
  agents hold, and `sync` re-derives on the next pass either way. Copy the
  `gate` line `start` prints rather than retyping the branch.

- **A pane event ended a gate without the pane being checked.** `pane.exited`,
  `pane.closed` and `pane.agent_status_changed` were trusted to the subscription
  that asked for them, which held while a gate had exactly one pane's
  subscriptions on its stream. Every pane-scoped frame is now matched on its own
  `pane_id`, as `pane.updated` and `workspace.closed` already were.


## [0.7.0] — 2026-08-01

### Added

- **`worktender start <issue>` — an issue number in, an agent working on it
  out.** It reads the issue with `gh`, creates a worktree on
  `<number>-<title-slug>`, starts an agent in the new pane, and types a brief
  covering the whole round: read the issue, explore, change, test, self-review,
  open a pull request, then `report`.

  This is the half that was missing. The documented loop was `herdr worktree
  create`, then `dispatch --pane <pane>`, then `herdr agent prompt`, then
  `gate` — three tools, one of which was not this one, and **a pane id that came
  from nowhere**, because `ls` did not print one. The `<pane>` in the coordinator
  skill was a placeholder with no command behind it.

  Staffing goes through the same `KindStaff` action `sync` and `dispatch` build,
  so the occupied-pane re-check in `execute.staff()` covers this path by
  construction rather than by a rule someone has to remember.

  **The issue body reaches the agent as framed, untrusted data**: announced as
  such before it arrives, delimited, flattened onto one line, and never
  presented as an instruction. Anyone who can file an issue writes it, and
  escaping solves nothing — a perfectly escaped instruction is still an
  instruction where instructions go. Truncation past 4000 runes is *announced*,
  on the same rule the report note follows by refusing to truncate at all.

  It briefs with `pane.send_text` rather than `agent.prompt`, which blocks
  against an agent herdr still holds as `launch_pending` — a state a live agent
  can sit in indefinitely. A newline submits, which is why the brief is one line
  and why flattening is a correctness property rather than tidiness.

  `start` deliberately does **not** wait. Start every slice, then `gate` them one
  at a time; a start that gated would serialise the fleet. It prints the exact
  `gate` line for what it just started, agent name included.

  This is the only thing here that creates a worktree — the reconcile commands
  still adopt what they find. The manifest description and the `worktrees` skill
  both said the plugin does not create worktrees, and now say what it does.

- **`ls` prints the pane, and `ls --pr` the pull request state.** The pane is
  what `dispatch --pane` takes and the listing previously had no way to produce
  it. The pull request column is opt-in because it costs one `gh` invocation per
  branch, in series — and it is *absent* rather than dashed when it was not
  asked for, since a `-` is the same cell a branch with no pull request prints.

- **`doctor` prints the path to the binary.** The documented alternative is a
  `jq` expression over `herdr plugin list --json`, carried in the README, both
  skills and the dispatch page; the process already knows where it lives.

### Fixed

- **`sync` now names the repository it resolved.** `prune` has printed its root
  since the divergence was seen live, because the two halves resolve it
  differently and "nothing to do" is indistinguishable from "nothing to do
  *here*" until the output says where here is. `sync` resolves through the same
  `newSession` call — herdr's invocation context first, the working directory
  only as a fallback — so it could act somewhere other than where the caller
  believed they were standing, with none of the disclosure.

- **`sync` no longer asks `gh` about every worktree and discards the answers.**
  It built the collector with the `gh`-backed pull request lookup and then
  filtered prunes out — and pull request state only ever authorises a prune. So
  every branch cost a network round trip, in series, while the repository lock
  was held, to decide nothing. The event and startup paths already dropped the
  lookup for exactly this reason; `sync` was the third path and missed it.

- **One vanished workspace no longer fails a whole repository** (#66). herdr
  lists workspaces and is then asked for each one's panes, and a workspace closed
  between those two calls answered `workspace_not_found` — which propagated out
  of collection, so the reconcile lost every *other* worktree's verdict too.
  Seen live as `prune` exiting 1 having printed nothing but its `repository:`
  header, on the pass straight after a `sync` that had opened the workspace.

  A workspace that no longer exists is one there is nothing left to decide
  about, so it is skipped and named on stderr. **Only that code is survivable**:
  every other `pane.list` failure still fails the collection, because it still
  means the repository's state is unknown. There is a test for each half — the
  skip, and the refusal to widen it into ignore-on-error.

  `internal/herdrtest` grew a `CodedError` for this. Its fake herdr reported
  every handler failure as `handler_error`, so a test could not previously
  express *which* failure it was — and this fix branches on exactly that.

### Changed

- **Comments cut from 40% of source to 22%**, across every non-test file. The
  reasoning that earns its place is still here — why topology never authorises a
  removal, why the note is unreachable from the gate's predicate, why the lock
  fails open. What went was the litigation: issue numbers a reader cannot look
  up, paragraphs recounting approaches that were tried and removed, and three
  headers of 47, 40 and 35 lines that were doc pages sitting in front of the
  code. Blocks of twelve lines or more went from 22 to 7. No behaviour changed;
  the diff was checked line by line for it.

- **`ls` gained a column**, so anything parsing its output by position will need
  adjusting: the pane now sits between the workspace and the agent status.

## [0.6.0] — 2026-08-01

### Added

- **`worktender update`, and a `version` line in `doctor`** (#60). herdr has no
  `plugin update` — its plugin subcommands are `install`, `uninstall`, `link`,
  `unlink`, `enable`, `disable`, `list`, `config-dir`, `action`, `log` and
  `pane` — so an install pinned a commit and then stayed on it silently. One sat
  on `8ef0de9` across four releases while `doctor`, the `--permission-mode`
  passthrough and the docs split all landed, and nothing in ordinary use said so.

  `doctor` is the half that matters, because an update command nobody knows they
  need is not much use: it names the installed version and commit and reports
  when the origin default branch has moved past them. `update` performs the
  fetch-and-rebuild — an install is a **shallow, detached clone with no local
  branch**, so `git pull` cannot work in one and it fetches one commit deep and
  resets onto `FETCH_HEAD` instead.

  **Getting onto it is a reinstall, not `update`.** Every install that exists
  predates the command, so on 0.5.0 and earlier it answers `unknown command
  "update"` — reading these notes and trying what they announce is exactly how
  you find that out. The first move is `herdr plugin install steig/worktender`,
  which is also the only one that corrects the recorded commit below; the hand
  path is what `update` performs, and it does not. This is once per install
  rather than permanent: anything carrying `update` moves forward with it.

  The rebuild is staged beside the live binary and **renamed into place**, never
  written over it: an update is normally run by the binary it replaces, and herdr
  may be running an action through the same file. `scripts/build.sh` takes
  `WORKTENDER_BUILD_OUT` for that.

  That staging is a request to a script that comes from the checkout being
  fetched, and every earlier release writes `bin/worktender` regardless — so the
  live binary is stamped before the build and compared after. "Nothing was
  staged" and "the binary was replaced in place" are indistinguishable from the
  staged file alone, and reporting the second as the first would assert a state
  nothing had checked.

  Both halves say the one thing neither can fix. **herdr records the installed
  commit at install time and never re-reads the checkout**, so after any in-place
  update `herdr plugin list` — the one command that answers "what am I running" —
  names a commit that is no longer on disk. The manifest version beside it *is*
  re-read, so the two disagree. `update` prints it and `doctor` repeats it.

  It refuses a checkout on a branch (that is `herdr plugin link`, and it is
  yours to move with git) and a checkout with uncommitted changes.

## [0.5.0] — 2026-08-01

### Added

- **`worktender doctor` — one command that says what is wrong** (#56). Three of
  this plugin's documented failures are environmental, silent, and shaped
  exactly like ordinary operation, so each was previously diagnosed by
  remembering it exists.

  It reports herdr's reachability and version, whether `gh` is installed *and*
  authenticated, the events opt-in **as the gate parses it** rather than as it
  is spelled, and every repository herdr has a worktree workspace for with its
  worktree and agent counts.

  `gh` missing or unauthenticated is a `warn`, not a `fail`: a repository that
  does not use pull requests is entitled to no `gh`, but it silently costs prune
  everything a merged pull request would have authorised. A herdr that cannot be
  reached is said once and the command stops, rather than repeated as the cause
  of every line below it.

  Read-only, takes no lock, and works from outside a repository — someone who
  cannot tell what is wrong often cannot tell where they are either, so the
  repository list is herdr's open workspaces rather than the caller's directory.

### Changed

- **The docs now point at the binary instead of `plugin action invoke`.** `ls`,
  `sync`, `prune` and `prune-apply` have always been subcommands — every action
  is defined as `./bin/worktender <id>` — but the skill asserted the opposite
  ("there is no `worktender ls` on your PATH"), so every documented path went
  through `invoke` and then a second call to fish the output out of the plugin
  log.

  That indirection is the single most-documented agent mistake in this
  repository, and it was self-inflicted. Resolve the binary once and the output
  is on stdout with a real exit code. The actions remain, because a keybinding
  or menu has no other way in.

- **`min_herdr_version` is 0.7.5, up from 0.7.0.** Every measured behaviour
  behind `report` and `gate` was tested against 0.7.5 and nothing at runtime
  checks the version, so the floor now matches what was actually verified.
  herdr enforces the field, which turns a silent incompatibility into a refused
  install.

### Removed

- **`WORKTENDER_UNSANDBOXED_OK` is gone, and `--permission-mode` no longer
  refuses anything.** `bypassPermissions` and `acceptEdits` now pass straight
  through to the agent.

  The gate could not distinguish a caller who had built a sandbox from one who
  had read the variable's name, so it never established the thing it asked
  about — while every unattended dispatch stalled on it, which is the exact
  failure `dispatch` exists to prevent. A worker with no human at its pane stops
  at the first prompt and no coordinator can clear it.

  **What worktender cannot do is unchanged, and it still says so.** It cannot
  sandbox the agent it starts: `claude` takes no sandbox flag and this plugin
  does not write your agent's configuration. Using one of those modes now prints
  a warning to stderr naming what was granted and what boundary is missing. The
  boundary is the caller's to provide — a sandbox profile, or a separate uid.
  An allowlist still provably cannot substitute for one.

  Nothing is defaulted: without `--permission-mode`, dispatch changes nothing
  about what an agent may do.

### Fixed

- **The README explains how a report reaches a gate again** (#45). The #28
  restructure dropped the paragraph saying a report travels as pane metadata and
  never replaced it, so the largest section in the document described `report`
  and `gate` without once saying how one reaches the other.

  What replaces it is also more accurate than what was lost: the gate reads
  **both** the metadata and the pane's terminal buffer, every look, and neither
  wins. So a worker that merely prints a well-formed envelope has reported, and
  identity is a per-channel counter rather than content — two identical reports
  are two reports.

### Changed

- **Split the README into `docs/`** (#47). At 542 lines it carried the problem
  statement, quickstart, action surface, the whole dispatch design, exit codes,
  event hooks, the removal model and the supply-chain argument in one scroll —
  reference material a first-time reader does not need and a returning reader
  could not find. README is now 216 lines; the rest moved to `docs/dispatch.md`,
  `docs/pruning.md`, `docs/events.md`, `docs/reference.md` and `docs/trust.md`.

  **The unsandboxed warning stayed in README, above the install decision.**
  Moving it into `docs/trust.md` would have re-created #18 in a new form. So did
  the two quickstart surprises — that `prune-apply` deletes local branches, and
  that `gh` must be authenticated. No prose was lost; this was a re-container,
  not an edit.

### Added

- Exit codes and the errors a user is most likely to meet.
- That `base` is `origin/HEAD` rather than an assumed `main`, that `list` is an
  alias for `ls`, how agent names are derived, that adoption never focuses the
  workspace, and that the repository lock is fail-open and is not a safety
  control.

## [0.4.1] — 2026-07-31

### Fixed

- **`prune` and `prune-apply` now both name the repository they resolved.** (#41)
  They do not resolve it the same way, and must not disagree in silence: listing
  may fall back to the working directory, applying may not — because herdr runs
  plugin commands with cwd set to the plugin root, itself a git repository, so a
  removal that fell back there would point at this plugin's own checkout.

  That asymmetry is deliberate and stays. What it cost was legibility: observed
  live as a dry run listing six worktrees followed by an apply reporting
  "nothing to do" about a different root, with nothing in either output showing
  they had disagreed. Printing the root does not prevent the divergence; it
  makes it impossible to have without seeing it.

### Added

- Documented how to bind a key to an action (#9). herdr has no `plugin_action`
  key type — its key commands are `command`, `pane`, `popup` and `split` — so a
  binding runs `herdr plugin action invoke` like any other command. This plugin
  ships no `[[keys.command]]` entries of its own on purpose.

## [0.4.0] — 2026-07-31

### Added

- **A deleted upstream can now authorise a removal, paired with ancestry.** Prune
  previously required a merged pull request, so it went inert in any repository
  that does not use them — the differentiated half of this plugin doing nothing
  for a whole class of users. (#8)

  A branch whose remote counterpart has been deleted **and** whose commits base
  already has is now pruned. Both halves are required and neither is sufficient: a
  gone upstream alone is abandonment as often as completion, and being an ancestor
  of base alone is the ambiguity that has always resolved to keeping.

  It is admissible because a deleted upstream is a *human action* rather than a
  graph shape, which is precisely the fact topology lacks. A branch forked off
  merged work and never pushed has no upstream to delete; a branch that landed was
  pushed and had its remote ref removed. Squash and rebase workflows remain
  uncovered — they rewrite commits, so the branch is not an ancestor at all — and
  continue to need a pull request.

  Prune reads remote-tracking refs, so `git fetch --prune` must have run for this
  to fire. A stale ref reads as still present and keeps the worktree, which fails
  in the safe direction.

- **`dispatch` — staff one named pane with a model and a permission mode.** (#26)
  `sync` staffs a bare `claude` with no arguments, so there was no way to
  influence how a staffed agent runs. `sync` keeps doing exactly that, on
  purpose: it fires from keybindings and event hooks where no role exists to
  route on, and an unattended reconciler should hold no opinion about cost or
  autonomy. A deliberate dispatch is where those belong.

  Dispatch runs through the same executor `sync` uses, so the pane re-check
  before `agent.start` covers it by construction rather than by a rule someone
  has to remember. #26 asked for that rule to be written down; not having a
  second staffing path is stronger than writing it down.

  **`--permission-mode bypassPermissions` and `acceptEdits` are refused** unless
  `WORKTENDER_UNSANDBOXED_OK` confirms the worker is sandboxed by something
  else. worktender cannot sandbox it — `claude` takes no sandbox flag, and this
  plugin does not write your agent's configuration — so granting autonomy
  without a boundary is exactly the combination #26 warned against, and it is
  stated rather than smoothed over. Nothing is defaulted: without the flag,
  dispatch changes nothing about what an agent may do.

### Changed

- `reconcile.Action` gained `AgentArgs` and is therefore no longer a comparable
  struct. It is always empty on anything the reconciler plans.

## [0.3.0] — 2026-07-31

Renamed. Everything user-facing moved; nothing about how removal decides changed.

### Changed — BREAKING

- **Renamed `muster` to `worktender`.** `muster` collided with an existing herdr
  plugin (`kichel.muster`, published four weeks earlier and listed in the same
  `herdr-plugin` marketplace topic). Plugin ids are namespaced so nothing
  technically clashed, but two plugins called Muster in one index is a
  discoverability problem, and it was cheaper to fix at zero installs than ever
  again.
  - plugin id `steig.muster` → `steig.worktender`
  - binary `bin/muster` → `bin/worktender`
  - actions retitled `Muster: …` → `Worktender: …`
  - module path `github.com/steig/muster` → `github.com/steig/worktender`
- **`MUSTER_EVENTS` → `WORKTENDER_EVENTS`.** The old name enables nothing. Both it
  and `HERDR_WT_EVENTS` before it are detected and refused with a line naming the
  current spelling, so a stale opt-in fails loudly instead of going quietly inert.
  **If you had events enabled, you must re-export under the new name.**
- **Report metadata keys `muster_*` → `worktender_*`.** `report` and `gate` ship
  together, so this only matters if you mix versions: a 0.2.0 worker's report is
  invisible to a 0.3.0 gate, and vice versa.

### Added

- `SECURITY.md` — private reporting route, supported versions, and the trust
  boundary in full, including the accepted limits on what a report authenticates.
- This changelog.

### Fixed

- **README rewritten around what the tool is for.** It opened on mechanics and
  reached install before ever stating the problem, and spent 386 words on
  supply-chain trust before the table of what the tool does. Now: problem
  statement, quickstart, and a worked dispatch example, with the trust and
  rationale material kept in full and moved below them.
- **Documented that `prune-apply` deletes the local branch**, not just the
  checkout. Behaviour is unchanged and was always `git branch -d`, never `-D` —
  it was simply never written down.
- **Documented that `gh` must be authenticated, not merely installed.** An
  unauthenticated `gh` reads as "no pull request", so nothing is ever pruned while
  the printed reasons look entirely ordinary.
- Documented `sync`'s two-pass convergence, transcript resume via `--continue`,
  and that `--until` is repeatable.
- README linked `github.com/herdr/herdr`, which does not exist. Corrected to
  `github.com/herdrdev/herdr`.
- The manifest description advertised worktree *creation*, which this plugin has
  never done and explicitly declines. That string is the marketplace listing copy.
- The README's opening example showed `plugin action invoke` printing a table
  inline. It returns an invocation record; the output is in the plugin log. The
  first code block taught the mistake the shipped skill exists to prevent.
- `skills/worktrees/SKILL.md` still described a `pane.agent_detected` hook, removed
  in #17.

## [0.2.0] — 2026-07-31

### Added

- **`report` and `gate`** — a fixed-slot hand-off pair for agent-to-agent
  dispatch. A worker reports `planned|blocked|done` with an optional PR number and
  a 200-character note; a coordinator blocks on `gate` until the predicate holds.
  Neither is registered as a herdr action, because an action has no argument
  surface and writes its output to a log the blocked caller is not reading.
- **`[[startup]]` one-shot** — a single adopt-and-staff pass per open repository
  after herdr's server is ready, replacing the zsh original's 90-second
  `wt watch` poll loop. Makes no network calls.
- Event hooks for `worktree.created` and `worktree.opened`, off by default.

### Changed — BREAKING

- **Renamed `herdr-wt` to `muster`**, including `HERDR_WT_EVENTS` →
  `MUSTER_EVENTS`. The old variable enables nothing and is refused loudly.

### Fixed

- Events gate armed on values meaning *off*; `MUSTER_EVENTS=off` could start
  agents. Unrecognised values now fail closed and say so. (#14)
- Metadata reports were forgeable while a comment claimed otherwise. The claim was
  wrong, not the code; the limit is now documented rather than overstated. (#16)
- Install path left an unverified binary on disk when the checksum fetch failed,
  at exactly the path the manifest execs. A trap now covers every exit path, and a
  missing or mismatched checksum aborts. (#19)
- Bidi characters in a branch name could spoof the prune confirmation. Every
  rendered cell is escaped to `\u{XXXX}` rather than stripped — a stripped name
  renders as a legitimate one. (#22)
- `ls` swallowed a `WorkspaceList` failure and reported it as an empty session,
  which is indistinguishable from a real one. (#21)
- A repolock could be stolen through a zero-byte lock file. (#20)
- `prune` could force-remove a checkout hosting a live agent when the workspace id
  was empty, silently skipping the agent guard. (#13)
- `gate` identified a report by its content, so two identical reports read as one
  and a coordinator dispatching the same slice twice was heard once. Now counted
  per channel. (#15)
- A tool-call envelope never reached the pane, so `report` and `gate` could not
  meet. (#12)

## [0.1.0] — 2026-07-31

First tagged release, so the no-Go install path had something to download. (#2)

Worktree lifecycle reconcile and execute, the four herdr actions
(`ls`/`sync`/`prune`/`prune-apply`), the merged-PR removal rule, and the
`skills/worktrees` agent skill.

[0.7.0]: https://github.com/steig/worktender/releases/tag/v0.7.0
[0.6.0]: https://github.com/steig/worktender/releases/tag/v0.6.0
[0.5.0]: https://github.com/steig/worktender/releases/tag/v0.5.0
[0.4.1]: https://github.com/steig/worktender/releases/tag/v0.4.1
[0.4.0]: https://github.com/steig/worktender/releases/tag/v0.4.0
[0.3.0]: https://github.com/steig/worktender/releases/tag/v0.3.0
[0.2.0]: https://github.com/steig/worktender/releases/tag/v0.2.0
[0.1.0]: https://github.com/steig/worktender/releases/tag/v0.1.0
