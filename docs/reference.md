# Reference

Exit codes, errors, keybindings, and the behaviours that do not belong anywhere else.

## Finding out what is wrong

```sh
$ worktender doctor
version  0.5.0 @f074c65  warn  origin/main is at @a1b2c3d; run `worktender update`
herdr    0.7.5          ok
gh       authenticated  ok
events   unset          off

repos
  worktender    3 worktrees  1 working
  house         7 worktrees  1 idle, 1 working
```

Four of this plugin's failures are environmental, silent, and shaped exactly
like ordinary operation, so `doctor` exists to name them without being asked the
right question first:

- **`gh` missing or unauthenticated** collapses to "this branch has no pull
  request", so prune keeps almost everything while every reason it prints reads
  as reasonable. Reported as `warn` rather than `fail` — a repository that does
  not use pull requests is entitled to no `gh` at all.
- **An unrecognised `WORKTENDER_EVENTS`** leaves events off, and the notice
  saying so is printed by a hook that will not fire. `doctor` reports the value
  as the gate *parses* it, not as it is spelled.
- **A herdr that cannot be reached**, which makes every other answer here
  meaningless — so it is said once and the command stops.
- **An install left behind**, which is the one that hides longest. herdr has no
  `plugin update`, so an install pins a commit and stays on it — one sat on
  `8ef0de9` across four releases. The `version` line names what is installed and
  says when origin has moved past it. It is the one check that asks the network —
  `git ls-remote`, bounded at ten seconds, writing nothing into the checkout —
  and an origin that cannot be reached leaves drift reported as unknown rather
  than as fine.

It is read-only, takes no lock, and works from outside a repository: someone who
cannot tell what is wrong often cannot tell where they are either. The
repository list is herdr's open worktree workspaces rather than wherever the
caller is standing.

`doctor` is a command rather than an action, and deliberately: an action's
output lands in the plugin log, which is exactly the indirection a diagnostic
should not have.

## Staying current

```sh
$ worktender update
install: /Users/you/.config/herdr/plugins/github/steig.worktender-3ebd1704d63b
origin/main is at @a1b2c3d; fetching
0.5.0 @f074c65 -> 0.6.0 @a1b2c3d

herdr still records @f074c65 for this plugin, so `herdr plugin list` will keep
naming a commit that is no longer installed.
nothing here can correct that record; a reinstall re-clones and re-records it:
  herdr plugin install steig/worktender
```

herdr has no `plugin update` — its plugin subcommands are `install`,
`uninstall`, `link`, `unlink`, `enable`, `disable`, `list`, `config-dir`,
`action`, `log` and `pane` — so this command exists because nothing else can
move an install forward.

Three things worth knowing:

- **An install is a shallow, detached clone with no local branch**, so `git pull`
  cannot work in one. `update` fetches the origin default branch one commit deep
  and resets onto `FETCH_HEAD`, which is the same shape herdr's installer left.
- **The rebuild never writes over the live binary.** An update is normally run
  *by* the binary it replaces, and herdr may be running an action through the
  same file, so the build is staged beside it and renamed into place. Anything
  already running keeps the old image until it exits.

  The staging is a *request*, though: `update` runs the build script from the
  checkout it just fetched, and every release before this one writes
  `bin/worktender` regardless. That cannot be prevented from here, so the live
  binary is compared before and after and the failure says which happened —
  replaced in place, or never built at all. The two look identical from the
  staged file alone, and calling the first the second would assert a state
  nobody checked.
- **`herdr plugin list` will report the pre-update commit afterwards.** herdr
  records the commit at install time and never re-reads the checkout — the
  manifest *version* it re-reads, so the two disagree. Nothing in this plugin can
  correct that record; `update` says so, and `doctor` repeats it on every run.

It refuses two checkouts: one on a **branch** — that is what `herdr plugin link`
leaves, and it is yours to move with git — and one with **uncommitted changes**,
which a hard reset would destroy.

Like `doctor`, it is a command rather than an action. An action's output lands in
the plugin log, and herdr running it as an action would be herdr executing the
binary the rebuild replaces.

### Getting onto it the first time

`update` arrived in 0.6.0, so no install that predates it can reach it by
running it — which is every install that existed when it shipped:

```sh
$ worktender update
worktender: unknown command "update"; usage: worktender <ls|doctor|sync|dispatch|prune|prune-apply|report|gate|on-event|startup>
```

Reinstall instead:

```sh
herdr plugin install steig/worktender
```

That is the one to reach for first, and not only because the alternative is
longer: it re-clones *and* re-records, so it is also the only thing that corrects
the commit herdr holds. The hand path — fetch one commit deep, reset onto
`FETCH_HEAD`, rebuild — is exactly what `update` performs, and it leaves `herdr
plugin list` naming the commit the install started on; one checkout still read
`@8ef0de9` after two updates by hand, which `doctor` now reports as a warn.

This is once per install rather than a standing tax. Any install carrying
`update` moves forward with `update`.

## Exit codes and errors

Every failure prints `worktender: <error>` on stderr and exits non-zero. The
code says **what to do next**, not what went wrong — a coordinating agent has
four responses available, and one code per response is what lets it pick without
matching on the message.

| Code | Meaning | What the caller does |
| --- | --- | --- |
| **0** | It happened. | Proceed. |
| **1** | Usage: an unknown flag, a missing argument, a repository you declined to name, a target that cannot report. | Fix the invocation. Retrying unchanged fails identically. |
| **2** | Environment: herdr's socket unreachable, git failing, `gh` unauthenticated. **Also every unclassified failure.** | Fix the machine, then retry. |
| **3** | Needs a human: a worker reported `blocked`, or `start` briefed an agent that never took it up. | Escalate. No retry clears it. |
| **4** | No answer arrived: a gate timed out, a pane died before reporting. | The work may simply be unfinished; redispatching is reasonable. |

**2 is the catch-all on purpose.** The alternatives are worse: defaulting to
**1** tells a coordinator to rewrite a correct invocation, and defaulting to
**3** wakes somebody for a bug. An unrecognised failure routed to **2** costs at
most a retry.

The distinction **3** and **4** draw is the one this replaced two codes for. A
gate returning **1** used to mean the worker is blocked and needs a person *or*
that herdr's socket had died, and those demand opposite actions.

Everything fails loudly on purpose. herdr records a plugin action that exits 0
as "succeeded", so a command that reports a problem and exits 0 is a silent
failure — which is why `sync` and `prune` exit non-zero with `%d of %d
action(s) failed` rather than printing a warning and returning success.

Errors you are most likely to meet:

| Message | Means |
| --- | --- |
| `refusing to guess which repository to change` | You ran a changing command outside herdr. `ls` and `prune` allow it; `sync`, `prune-apply` and the event paths do not. |
| `--repo: not inside a git repository: <path>` | `prune`/`prune-apply` were given a path that is not one. Never a fallback — naming a repository exists to stop the resolution wandering. |
| `another worktender reconcile has held X for more than 30s` | A concurrent pass. Retry. |
| `WORKTENDER_EVENTS="ture" is not a value this gate recognises` | Events stay **off**. Fix the value yourself; nothing here rewrites it. |
| `MUSTER_EVENTS is set, but it was renamed` | A superseded opt-in enabling nothing. So is `HERDR_WT_EVENTS`. |
| `--note is N characters; the limit is 200` | Refused, never truncated. Shorten and report again. |
| `this checkout is on branch X rather than the detached HEAD herdr installs` | `update` was pointed at a linked development checkout. Move that one with git. |
| `no herdr-plugin.toml beside X; update only works from an installed plugin binary` | The binary is not sitting in an install's `bin/`. |
| `the worker reported blocked after Ns` | The gate failed fast rather than waiting out its clock. With `--any` it names which worker, and the others are left running. |
| `no new report reached status done within Ns` | Timed out. The message accounts for every worker waited on, quoting what its pane already held when the gate opened, which it ignored as a previous task's answer. |
| `b is the same worker (pane w2:p1); name it once` | A gate was given an agent's name and its pane id, which herdr resolves to one worker. Watching it twice would let one report look like two. |

## Smaller things worth knowing

- **`--json` replaces the table on `ls`, `doctor`, `sync`, `prune` and
  `prune-apply`**, and never joins it — see
  [Machine-readable output](json.md). Human asides that would otherwise sit
  beside the table, like a lock that would not release, go to stderr instead.
- **`--release-agents` on `prune` and `prune-apply`** removes a finished
  worktree whose agent has stopped, closing the workspace — which is the only
  way herdr lets go of an agent. Pass it to *both* halves or the dry run
  describes a plan the apply will not carry out. It never reaches an agent that
  is working, and there is no action for it: see
  [How it decides what to remove](pruning.md).
- **`base` is `origin/HEAD`, not `main`.** It falls back to `main` only when
  origin cannot be asked, so a repository defaulting to `master` or `develop` is
  handled without configuration.
- **`start` prints the commit it forked from, not just the ref.** A ref moves; a
  squash merge of the base leaves none of its commits on the trunk, and the
  fork point is what `git rebase --onto` needs to replay a stacked branch. An
  unresolvable ref prints no fork point rather than a guess — the worktree
  create reports that failure better than this line could. See
  [Stacking on a branch that is still in review](dispatch.md#stacking-on-a-branch-that-is-still-in-review).
- **`list` is an alias for `ls`.**
- **Agent names** come from the checkout's directory basename, lowercased to
  `[a-z0-9-]`, prefixed `wt-` if the result does not start with a letter, and
  suffixed with a six-character digest of the repository root and the full
  basename — `42-fix-the-thing` in `~/code/thing` becomes
  `wt-42-fix-the-thing-016aab`. The whole thing is 32 characters or fewer,
  because that is what herdr accepts.

  **The digest is not decoration.** herdr's agent namespace is global and it
  refuses a duplicate outright (`agent_name_taken`), so two repositories with a
  worktree called `api` — or an issue #12 each — would ask for one name and
  whichever was staffed second would get no agent. It also separates two long
  branches the 32-character limit would otherwise truncate onto each other. So
  **do not retype the name from the branch**: `start` prints the exact `gate`
  line for what it started, and `sync` prints the name it staffed under.
- **Adoption does not focus the workspace**, so adopting a batch does not drag
  you through every one of them.
- **The repository lock is fail-open.** It lives under
  `HERDR_PLUGIN_STATE_DIR`, and if that is absent or unwritable it degrades to a
  lock that excludes nothing, silently. Any lock held longer than five minutes is
  taken. It stops two reconciles duplicating work; **it is not a safety
  control** — the guards that are re-checked immediately before removal are.

## Binding a key to an action

herdr has no `plugin_action` keybinding type — its key commands are `command`,
`pane`, `popup` and `split`. So a binding runs the invoke CLI like any other
command, in your own `config.toml`:

```toml
[[keys.command]]
key = "prefix+alt+s"
type = "popup"
command = "herdr plugin action invoke sync --plugin steig.worktender"
width = "70%"
height = "50%"
```

**Bind `sync` and `prune`, never `prune-apply`.** A key that can reach a removal
is exactly what splitting those two actions was meant to prevent.

This plugin deliberately ships **no** `[[keys.command]]` entries of its own. An
install that silently claims `prefix+alt+s` is the same class of surprise as one
that edits your agent's configuration — and you may already have that key.

That is the whole of the herdr *action* surface, and it is a subset of the
binary's: every action runs `./bin/worktender <id>`, so `ls`, `sync`, `prune`
and `prune-apply` are subcommands you can call directly — and should, since the
action path returns an invocation record rather than the output. `report` and
`gate` have no action equivalent at all: they take arguments and `gate` blocks,
neither of which an action can do.


---

[← README](../README.md)
