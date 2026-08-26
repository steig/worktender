# worktender

**A fleet of coding agents, one git worktree each — started on a GitHub issue,
watched while they run, and cleaned up once the work has landed.**

A [herdr](https://github.com/herdrdev/herdr) plugin. herdr is the terminal
multiplexer coding agents run in; this plugin keeps its workspaces and your git
worktrees pointing at the same reality.

Documentation is rendered at **<https://steig.github.io/worktender/>** — the
same words as the markdown here, plus an overview and *Patterns*, which exist
nowhere else.

## The problem

Running several coding agents at once means running several worktrees. Making
the directory is the easy half, and every tool does it — what nothing does is
the round trip: an issue, a checkout named for it, an agent briefed on it, and a
way to know when it is finished. `start` and `gate` are that half.

The other end is where it goes wrong. A few weeks in there are eleven checkouts
on disk, some have a herdr workspace and some do not, some still have an agent
sitting in them and some are ghosts, and the one you are least certain about is
the one you least want to delete. So nobody deletes anything — and the honest
reason is that no cleanup script has ever been trustworthy enough to run without
reading its output line by line first.

worktender covers both ends. It starts an agent on an issue in a worktree of its
own, and it reconciles `git worktree list` against herdr's workspaces and agents
— adopting what herdr does not know about, staffing empty workspaces, and
removing finished checkouts **on the rule that ambiguity always keeps the
worktree.**

```sh
$ worktender ls
* main                      w21  w21:p1  idle     1057  worktender
  feat/1-reconcile-execute  w22  w22:p1  working  1055  1-reconcile-execute
  fix/257-erasure-comments  w1K  w1K:p1  idle     812   257-erasure-comments
  worktree/brave-valley     -    -       -        -     brave-valley-66f8
```

Columns are branch, herdr workspace, pane, agent status, state counter, and
directory. `*` marks the repository's main checkout; `-` means herdr has nothing
for that worktree — the last row is a checkout with no workspace and no agent,
which is exactly what `sync` picks up.

The directory column is the exception to that reading: it prints `-` when the
directory is named after the branch, which is every worktree `start` made. It
has something to say on the rows above, where it disagrees — a checkout adopted
rather than created sits somewhere with no relation to its branch. `--json`
carries both fields populated either way.

`?` marks a **ghost**: a workspace herdr is still holding open on a checkout git
no longer has, which is what removing a directory out from under it leaves
behind. It has no branch, because there is no checkout left to have one, and
`prune` names it too. Naming is all either does. Closing a workspace is a
different authority from removing a worktree: there is no checkout left to test
for uncommitted work, so the strongest guard here is not unsatisfied but
unavailable, and its panes may still hold a live agent whose conversation
closing it would destroy. Close it in herdr once you are sure.

The pane is the one `dispatch --pane` takes.

The counter is herdr's own, and it is what `idle` cannot tell you: `idle` is the
same cell for a worker that finished two seconds ago and one that never received
its brief at all. Read it *down* the column rather than across a row — the third
worktree above is 240-odd state changes behind the rest, which beside `idle` is
the shape of a worker that stopped. It is a counter and not a clock because
herdr exposes no timestamp on an agent at all; two readings and your own clock
are what turn it into a duration, and what counts as *stalled* stays yours to
decide.

**Read the status beside it, always.** It counts state *changes*, so a worker
that stays in one state does not move it — and a worker thinking hard is exactly
that. Frozen beside `idle` is finished-or-wedged, which is the question this
column answers; frozen beside `working` is a long turn or a wedge and the column
cannot say which. See
[Machine-readable output](docs/json.md#where-it-goes-blind-a-frozen-counter-on-a-working-row).

Add `--pr` for a pull request column, which is off by default because it costs
one `gh` call per branch:

```sh
$ worktender ls --pr
* main                      w21  w21:p1  idle     1057  -       worktender
  feat/1-reconcile-execute  w22  w22:p1  working  1055  OPEN    1-reconcile-execute
  fix/257-erasure-comments  w1K  w1K:p1  idle     812   MERGED  257-erasure-comments
```

`--reports` adds a column carrying what the worker in each pane last told its
coordinator, read back off the pane's own metadata — the same place `report`
attached it and a gate reads it:

```sh
$ worktender ls --reports
* main                      w21  w21:p1  idle     1057  -        worktender
  feat/1-reconcile-execute  w22  w22:p1  working  1055  planned  1-reconcile-execute
  fix/257-erasure-comments  w1K  w1K:p1  idle     812   done #4  257-erasure-comments
```

This is what a coordinator asks after its context is cleared, instead of having
written the fleet down. **It is in-flight state and not a history** — metadata
lives on the pane, so a released worker's last report is gone with it. The
durable record of finished work is the pull request it named, which is `--pr`.
The 200-character note is not in the table; it is untrusted text and it is in
the JSON, where a consumer can decide what to do with it.

Agents in six repositories are six invocations and six directories to remember
to visit, so `--all-repos` lists every repository herdr has a worktree
workspace for — from anywhere, including outside a repository entirely. One
that cannot be read says so on its own line and costs the others nothing:

```sh
$ worktender ls --all-repos
/Users/you/code/worktender
  *  main                   w21  w21:p1  idle     1057  worktender
     77-cross-repo          w30  w30:p1  blocked  812   77-cross-repo
/Users/you/code/lighthouse
  *  main  w4  w4:p1  working  1061  lighthouse
```

`--pr` is deliberately not available across repositories: the lookup runs in
series and is scoped to one repository, so it would be both slow and asking the
wrong repository.

`--blocked` keeps only the worktrees herdr reports a blocked agent in — the one
status where the session has stopped and nobody but you can restart it. Working
resolves itself and idle is finished or waiting; blocked sits there until
somebody looks, which is why it is worth a question of its own:

```sh
$ worktender ls --all-repos --blocked
/Users/you/code/worktender
     77-cross-repo  w30  w30:p1  blocked  812  77-cross-repo
```

Repositories with nothing blocked are left out rather than drawn as empty
headings, and nothing blocked anywhere says so instead of printing nothing.
This is herdr's own agent status, not a worker's `report --status blocked`,
which is worktender's own envelope and reaches only whoever gated on it.
`doctor` names blocked worktrees too, rather than folding them into a count.

**Every command takes `--json`** if you are building on this rather than
reading it. `start`, `dispatch`, `report` and `gate` — the four an agent
orchestrates with — write their document on the failure paths too, and carry the
exit code in it, because with `--any` a number cannot say *which* of five
workers the gate was about. See [Machine-readable output](docs/json.md).

Everything is a subcommand of one binary, which herdr installs rather than
putting on `PATH`. Resolve it once:

```sh
worktender=$(herdr plugin list --json \
  | jq -r '.result.plugins[] | select(.plugin_id == "steig.worktender") | .plugin_root')/bin/worktender
```

The same four reconcile commands are *also* registered as herdr actions, so a
keybinding or menu can reach them — but that path is worse to script against,
and this is what newcomers trip on first: **`invoke` returns an invocation
record, not the action's output.** What the action printed is in the plugin log.
Call the binary and the output is just on stdout.

## Starting work on an issue

```sh
$ worktender start 42 --repo .
repository: /Users/you/code/thing
worktree: 42-fix-the-thing on origin/main (workspace w9, pane w9:p1)
fork point: origin/main is 31db5d1c9b7e4a02f6c1d8e5a3b90f2c4d6e8a10
done  staff  42-fix-the-thing  started claude as wt-42-fix-the-thing-016aab in w9:p1

briefed wt-42-fix-the-thing-016aab on #42; wait for it with:
  worktender gate --target wt-42-fix-the-thing-016aab --until done --require-pr
```

The agent name is not the branch name: herdr's agent namespace spans every
repository at once, so the name carries a digest of the repository. Copy the
line `start` prints rather than retyping it.

With several running, wait on all of them at once — the first to report releases
the gate and it says which one:

```sh
$ worktender gate --any wt-42-fix-the-thing-016aab,wt-43-other-9c21f4 --until done
gate: waiting on wt-42-fix-the-thing-016aab (pane w9:p1), wt-43-other-9c21f4 (pane w10:p1) for status done, up to 15m
gate: wt-43-other-9c21f4 released after 4m12s
```

One command from an issue number to an agent working on it: it reads the issue
title with `gh`, creates a worktree named for it, starts an agent in the new
pane, and types a brief covering the whole round — read the issue, explore,
change, test, self-review, open a PR, then `report`.

`--repo` because `start` creates a checkout, so it refuses to guess which
repository — and unlike the reconcile commands it has no herdr action to be
invoked through, because an action carries no arguments and `start` is nothing
without its issue number. Flags may be written on either side of the number.

**The brief is confirmed, not claimed.** It is typed, then submitted with a
separate Enter key event, and `start` waits for herdr to report the agent
working. herdr answering ok means it delivered keystrokes, not that an agent
received a prompt — and one Enter is not enough on its own, because herdr
reports an agent started as soon as it recognises its prompt box, which a TUI
draws seconds before it will act on a submit. Keys sent in that gap are
discarded, so `start` offers the submit again every couple of seconds for as
long as the agent stays `idle`. An agent still `idle` when that wait runs out
fails the command.

Start several, then wait on the lot of them. `start` deliberately does not wait;
`gate` is the other half.

**`--base <ref>` forks from something other than the trunk**, which is how a
second slice starts while the first is still in review. The fork point is
printed because a ref name is not a fixed point: this repository squash-merges,
and a squash merge puts one new commit on the trunk and none of the base
branch's own — so a stacked branch outlives its base only if someone kept the
commit it was forked from. `start` prints it, and says so when the fork is not
something the trunk already has. See
[Stacking on a branch that is still in review](docs/dispatch.md#stacking-on-a-branch-that-is-still-in-review).

**The brief does not carry the issue.** It names it and tells the worker to run
`gh issue view`, which reads the same text as tool output rather than as prose
pasted into a prompt. Nothing an issue author writes reaches the brief at all —
the title only survives as a branch name, already reduced to `[a-z0-9-]`. Nothing
about the agent's autonomy is defaulted either: without `--permission-mode`,
`start` changes nothing about what it may do.

**A worktree seconds old is not ready for an agent.** Its shell is still in
direnv, nix or a login banner, and herdr refuses to start an agent against it —
immediately, whatever `timeout_ms` the request carried. Staffing waits the pane
out itself, for up to a minute, re-checking each time that nobody else has
claimed the workspace meanwhile.

## Quickstart

```sh
# 1. install — read Trust below first; this runs unsandboxed
herdr plugin install steig/worktender

# 2. resolve the binary; herdr owns the install, so it is not on PATH
worktender=$(herdr plugin list --json \
  | jq -r '.result.plugins[] | select(.plugin_id == "steig.worktender") | .plugin_root')/bin/worktender

# 3. see where you stand — and, if anything looks wrong, why.
#    doctor also prints the line above, so you only need the jq once.
"$worktender" ls
"$worktender" doctor

# 4. adopt every orphan checkout, staff every idle workspace
"$worktender" sync

# 5. ask what looks finished — a DRY RUN, it removes nothing
"$worktender" prune                     # or: prune --repo /path/to/repo

# 6. only once you have read step 5's reasons
"$worktender" prune-apply

# 7. when doctor's version line says origin has moved past you
"$worktender" update
```

Each of those four is also a herdr action — `Worktender: list worktrees` and
friends — for reaching them from a keybinding or the plugin menu.

A few things worth knowing before step 5 surprises you:

- **`prune-apply` deletes the local branch too**, not just the checkout. It uses
  `git branch -d` and never `-D`, so a branch git considers unmerged survives and
  the output says how to force it. When `origin/<branch>` still exists that is
  reported rather than quietly left behind.
- **`gh` must be authenticated, not merely installed.** A merged pull request is
  the strongest authority this plugin accepts for "finished", and an
  unauthenticated `gh` is indistinguishable from "this branch has no PR" — so
  almost nothing is pruned, and the reasons look entirely ordinary while that
  happens. If prune keeps everything on a repository where you expect otherwise,
  check `gh auth status` first.
- **The repository comes from herdr, not from where you are standing.** Run as an
  action it resolves herdr's current workspace, which on a machine with several
  repositories open is routinely not the one you meant — a dry run inside a repository
  with four staffed worktrees, planning against a different project. Both halves print
  the root they resolved, so read the `repository:` line before acting on a plan. Pass
  `--repo <path>` to settle it: a path anywhere inside a repository resolves to its
  root, and a path that is not one is an error rather than a fallback.
- **A worker that finished still holds its pane**, because herdr frees an agent
  only when the pane goes away. Its worktree is kept, and the line says so and
  says what would remove it: `prune-apply --release-agents` closes the workspace
  and takes the agent with it. Pass the flag to `prune` as well, or the dry run
  describes a plan the apply will not carry out. An agent that is *working* is
  never released, flag or no flag.
- **Prune reads remote-tracking refs, so run `git fetch --prune` first** if you
  want a deleted upstream to count. A stale tracking ref reads as still present,
  which keeps the worktree — being out of date fails in the safe direction.
- **`sync` converges over two passes, not one.** A checkout adopted this pass has
  no workspace yet, so it cannot be staffed until the next. Running `sync` twice
  against a brand-new orphan is expected, not a bug.
- **An install stays where it was installed.** herdr has no `plugin update`, so
  nothing moves it forward on its own — one install sat four releases behind
  without a word. `doctor`'s `version` line says when origin has moved past you
  and `update` fetches and rebuilds; the one thing neither can fix is that
  `herdr plugin list` keeps reporting the commit it recorded at install time.
  **Step 7 cannot be how you first reach step 7**, either — an install older than
  0.6.0 has no `update` to run, so `herdr plugin install steig/worktender` is the
  way onto it, and the only way to correct that recorded commit.
  See [Staying current](docs/reference.md#staying-current).

## Requirements

- **herdr 0.7.0+** — this is a plugin; it talks to herdr over its local socket.
  Every measured behaviour behind `report` and `gate` was tested against **0.7.5**
  and nothing checks the running version, so prefer 0.7.5+ if you intend to use
  the hand-off pair. **`ls`, `prune` and `prune-apply` do not need it** — see
  [Without herdr](#without-herdr).
- **git**
- **jq** — for reading action output out of the plugin log, as above.
- **gh**, *authenticated* *(optional)* — only used to read pull request state.
  Without it, the only removals left are the ones a deleted upstream authorises
  (see [How removal is decided](docs/pruning.md)), and a
  repository that uses pull requests will prune almost nothing.

## Without herdr

The removal rules are entirely git and gh — uncommitted work, commits base does
not have, a deleted upstream, a merged pull request. None of them is a herdr
question. So the commands built on them run with herdr absent:

```sh
$ worktender ls
*  main                    -  -  -  -  worktender
   120-json-stops-here     -  -  -  -  -
   worktree/brave-valley   -  -  -  -  brave-valley-66f8
```

The workspace, pane, agent and counter columns are empty because **those facts
do not exist**, not because they could not be read — with no herdr there are no
workspaces and no agents. `prune` and `prune-apply` reach exactly the verdicts
they would otherwise, and `prune-apply` removes what it says it will.

The commands whose whole job is herdr — `start`, `dispatch`, `sync`, `gate` —
exit **2**, the environment class, and say what is missing. Not a usage error:
the command was spelled correctly and the machine could not answer it. So do
`ls --blocked` and `ls --reports`: both ask what agents are doing, and an empty
answer would read as *no agent is blocked* rather than as *no way to tell*.
`ls --all-repos` is the same — its scope is herdr's open workspaces.

### How absence is established

By **dialling herdr's socket**, not by looking at `HERDR_SOCKET_PATH`.

The distinction is the one this whole section rests on. herdr exports that
variable into the plugin commands and panes it starts, and **not** into your own
terminal — so its absence is the normal state of a shell whether or not herdr is
running behind it. Treating that as "no herdr" in your terminal would report a
repository as having no workspaces and no agents while herdr held four of them,
and `prune-apply` would then delete a checkout an agent was working in.

So worktender connects to `$HERDR_SOCKET_PATH` when herdr named one, and
otherwise to every endpoint a herdr on this machine could be listening on — the
default session at `$XDG_CONFIG_HOME/herdr/herdr.sock` (`~/.config/herdr/`), and
one per named session at `.../herdr/sessions/<name>/herdr.sock`. This costs one
connect per invocation — microseconds against a live socket, an immediate "no
such file" against none — and it buys the guarantee that degrading is never a
guess.

Enumerating the named sessions matters for the same reason dialling does. A
plain shell beside `herdr --session work`, with no default session running,
would otherwise find nothing at the default path and call that proof — and
`prune-apply` would delete a checkout that session's agent was working in. The
mirror reaches it too: a stale `HERDR_SOCKET_PATH` from a session that has
exited, while another herdr runs. A name that resolves to nothing is evidence
about the name, so worktender falls through it and keeps looking.

**Exactly one outcome counts as proof that herdr is gone: there is no socket at
any of them.** A running herdr always has its socket on disk, and none anywhere
is the ordinary state of a machine that does not run herdr — the case this
exists for.

If **two** sessions are running and nothing says which, worktender stops and
asks you to set `HERDR_SOCKET_PATH`. Guessing is not the smaller error: the
wrong session lists the wrong workspaces, so a checkout with a live agent in it
reads as held by nobody — the same failure through a different door.

Every other failure is "cannot tell", and worktender stops with exit 2 rather
than assume, because "cannot tell" resolving to "not there" is how the guard
gets disarmed.

That includes a **refused connection**, which is the tempting one: a socket file
with nobody accepting reads as a herdr that died. It is not proof, because it is
not only produced by a dead herdr. Dialling, measured on both platforms:

| what is at the path | macOS | Linux |
| --- | --- | --- |
| nothing | `ENOENT` | `ENOENT` |
| a directory or an ordinary file | `ENOTSOCK` | `ECONNREFUSED` |
| a socket, no listener | `ECONNREFUSED` | `ECONNREFUSED` |
| **a live listener whose accept queue is full** | **`ECONNREFUSED`** | timeout |

The last row settles it. On macOS a herdr that is running, listening and merely
backed up refuses the connection, and nothing cheap tells that apart from a
socket with nobody behind it — so treating `ECONNREFUSED` as proof would let a
busy herdr read as gone, which is the whole failure this section exists to
prevent. Retrying does not help: a herdr under sustained load stays refused.
`ENOENT` is also the only rule that gives the same verdict on both platforms for
every row, so the destructive path cannot unlock on one and not the other.

The cost is the third row: a herdr killed without cleaning up leaves its socket
behind, and worktender then refuses rather than degrading. That is the right way
round — refusing prints an error you can act on, degrading wrongly deletes a
checkout — and the error names the stale socket to remove.

Two consequences worth knowing:

- Run worktender from your terminal while herdr is up — default session or
  named — and you get the **full** listing, workspace and agent columns
  included, without exporting anything.
- On **Windows** herdr is addressed by a named pipe rather than a socket under a
  config directory, and worktender does not know how that pipe is named. So it
  cannot establish absence there and refuses instead of degrading: `ls`, `prune`
  and `prune-apply` need `HERDR_SOCKET_PATH`, which herdr sets for the commands
  it runs. Run as a herdr plugin, Windows is unaffected. Making the degraded
  path work from a bare Windows shell needs pipe discovery and is not done here.

### The one guard that cannot run

With herdr running, `prune` refuses a worktree whose pane hosts a working agent.
With herdr genuinely down that guard cannot run — and there is then nothing for
it to protect, because an agent lives in a pane inside a workspace, and both are
herdr's. That argument is only sound because absence is established by a dial:
it is reasoning about a herdr that is *not there*, and it would be worthless as
reasoning about a herdr that merely went unnamed. The guards that matter to
*your* work — uncommitted changes, unmerged commits — are git's, and they are
untouched either way.

One more thing that is not a guard but is worth stating: run from a plain shell,
`prune-apply` holds no repository lock, because the lock lives in the plugin
state directory herdr provides. Two prune-applies at once, or one racing a
herdr-driven reconcile, are not serialised against each other. Every guard is
re-checked at the moment of removal regardless, so the cost is repeated work
rather than lost work.

Note this is about herdr not *running*. Installed as a herdr plugin, herdr is
present by definition; what this covers is the binary invoked from a plain
shell.

## Actions

| Action | What it does |
| --- | --- |
| `Worktender: list worktrees` | Every worktree in the current repository, with its herdr workspace and agent status. |
| `Worktender: sync worktrees` | Opens a workspace for any worktree that lacks one, and starts an agent in any workspace sitting idle as a bare shell. Never removes anything. |
| `Worktender: prune (list)` | Reports which worktrees look finished and which were spared, and why. Changes nothing. |
| `Worktender: prune (apply)` | Actually removes them, and their local branches. |

Output lands in `herdr plugin log list --plugin steig.worktender`.

`prune` and `prune-apply` are two actions rather than one with a confirmation,
because a plugin action has no prompt surface — there is nowhere to ask "are you
sure?". Splitting them is the confirmation. It also means no stray keybinding can
reach a removal.

Staffing starts `claude`, and **resumes rather than restarts**: a checkout that
already has a Claude Code transcript in `~/.claude/projects` is picked up with
`--continue`, so re-staffing does not throw away the conversation.

## Trust

**A herdr plugin is not sandboxed.** This one runs as you, with your files, your
shell and your credentials, and what it does with them is start coding agents and
delete git worktrees and branches. That is what it is *for* rather than a side
effect, but installing it is a decision to let code from someone else's
repository do those things on your machine, and it is worth making on purpose.

The two capabilities most worth knowing before you install:

- **Removal needs either a merged pull request, or a deleted upstream over
  commits base already has.** Anything ambiguous is kept, and the reason is
  printed.
- **The hooks that would start agents without being asked are off** until you
  turn them on.

With a Go toolchain the binary is compiled from the source that was just cloned,
so what you can read is what you run. Without Go, a prebuilt release binary is
downloaded, pinned to the manifest version and checksummed — which proves the
download arrived intact and **nothing about who published it**.

The full argument, including what the checksum does not establish, is in
[docs/trust.md](docs/trust.md). How to report something privately is in
[SECURITY.md](SECURITY.md).

For local development, from a checkout:

```sh
herdr plugin link .
```

`link` points herdr at the working tree, so manifest edits take effect
immediately.

## Documentation

The markdown below stays canonical, so an agent that clones this repository
reads the same words the [site](https://steig.github.io/worktender/) renders.
What the site adds is *Patterns* — delegating to agents without losing the
thread, with five worked examples — which has no markdown source here.

| | |
| --- | --- |
| [Dispatching a worker](docs/dispatch.md) | `dispatch`, `report` and `gate` — handing a slice to another agent and waiting for it, and why the report has fixed slots. |
| [How removal is decided](docs/pruning.md) | What authorises a removal, why git topology never does it alone, and the guards. |
| [Events and startup](docs/events.md) | The hooks that adopt and staff automatically, and the one-shot pass that covers what they cannot. |
| [Reference](docs/reference.md) | Exit codes, the errors you are likely to meet, keybindings, and the smaller behaviours. |
| [Trust](docs/trust.md) | What running unsandboxed means here, and what the install path does and does not prove. |

## For coding agents

An agent driving this plugin gets a few things wrong without being told: that
`plugin action invoke` returns an invocation record rather than the action's
output, that `prune` and `prune-apply` are different in kind, and that enabling
events is not its call to make. A skill covering that ships in this repository:

```sh
npx skills add steig/worktender --skill worktrees -g
```

A second skill covers the other end — an agent that *dispatches* worktender's
agents rather than being one:

```sh
npx skills add steig/worktender --skill coordinator -g
```

It encodes what a session running as a router needs and keeps getting wrong:
never read a worker's diff, verify with targeted commands instead of relaying
claims, ask whether something was run or merely reasoned, pass a brief inline so
no worker stalls on a file-read prompt, and keep anything touching live or shared
state out of a dispatch entirely.

Or vendor `skills/worktrees/SKILL.md` and `skills/coordinator/SKILL.md` into your
own agent configuration, which pins them rather than tracking this repository.

Nothing here writes to your agent's configuration during install, and nothing
should. Running unsandboxed as it does, a plugin that quietly edits how your
coding agent behaves is the same kind of surprise as one that starts spawning
agents on install. The same rule covers autonomy: worktender does not set
permission modes or sandbox profiles on your behalf.

## Development

```sh
go test ./...
```

Tests run **real git in a temp directory** against a fake herdr speaking the same
protocol, with a fake `gh`. Nothing touches a live session and nothing reaches the
network.

Types in `internal/herdrapi/types_gen.go` are generated from herdr's own API
schema, so a field herdr renames becomes a compile error rather than a
silently-nil lookup:

```sh
herdr api schema --json > internal/herdrapi/schema.json
go generate ./...
```

The docs site builds from `docs/*.md` plus the hand-written pages in
`site/pages/`, into `_site/`:

```sh
python3 -m venv .venv && .venv/bin/pip install -r site/requirements.txt
.venv/bin/python site/build.py && python3 -m http.server -d _site 8765
```

## License

MIT — see [LICENSE](LICENSE).
