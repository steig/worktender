# Events

herdr can invoke this plugin when worktrees appear, so a new checkout is adopted
and staffed the moment it exists instead of the next time you remember to run
`sync`.

**Events are off by default. They do nothing until you opt in:**

```sh
export WORKTENDER_EVENTS=1
```

That is deliberate. These hooks start coding agents, and a plugin that begins
spawning agents the moment it is installed has handed you an autonomous trigger
you never asked for. Opting in is one exported variable; opting out after a
surprise is not.

Turning it off works the way you would expect: `0`, `false`, `no`, `off` and
`disabled` all disable events, in any capitalisation. So does unsetting it.
Anything the variable does not recognise leaves events **off** and says so on the
next hook — a value nobody wrote a rule for is not a request to start agents, and
a typo in an opt-in is cheaper to notice than a typo in an opt-out is to survive.

The variable is read from herdr's own environment, so export it before starting
herdr (or in your shell profile) rather than in a single pane.

If you opted in under an older name — `MUSTER_EVENTS`, or `HERDR_WT_EVENTS`
before that — **it enables nothing**, and the next hook says so rather than
failing quietly.

When enabled, the event path **adopts and staffs only — it never prunes.** Removal
stays something you ask for by name.

## Startup

Events cover the session. They cannot cover the time herdr was not running — a
worktree you added from a plain shell, a workspace restored without the agent that
used to live in it. That gap opens exactly once, so it is closed exactly once:
herdr runs a single adopt-and-staff pass per open repository after the server is
ready, and the command exits.

There is no watcher and no poll loop. The zsh original had one — `wt watch`,
waking every 90 seconds to ask the GitHub API about every worktree — and this is
what replaced it. The startup pass makes no network calls at all: pull request
state only ever authorises a removal, and startup never removes anything.

It shares the `WORKTENDER_EVENTS` opt-in above, and is off without it. Same
reason, more so: it starts agents across every open repository at once, on every
launch.

## The fleet controller

A second startup one-shot, `worktender fleet staff`, can staff the machine's
fleet controller — the always-on session that loads the fleet-coordinator
skill and dispatches workers across every repository. **It is off by default
and armed only by its own variable, `FLEET_CONTROLLER`,** which reads exactly
the values `WORKTENDER_EVENTS` reads and fails closed the same way: falsey
spellings and unset are off, and a value the gate does not recognise leaves
the controller down and says which value it refused.

The gate is separate from `WORKTENDER_EVENTS` because the blast radii are
different: events start workers inside worktrees you already made, while this
starts the session that goes on to create work across the whole machine. You
can opt in to either without getting the other. Like the events gate it is
read from herdr's own environment — set by you, before herdr starts, as a
deliberate choice. **No agent worktender starts is ever handed this variable,
and nothing an agent can run arms it.**

When armed, the one-shot leases on **agent presence, never pane existence**:
while a live agent named `fleet` exists anywhere in the session, it staffs
nothing. herdr restores panes across a restart but not the processes that
were in them, so "the fleet pane is open" is true precisely when the
controller is gone — which is why the pane is never the lease. The controller
lives in a dedicated workspace labelled `fleet`, in the fleet state directory
(`~/.local/state/fleet`, beside its ledger), and a re-staff after a restart
resumes the controller's own conversation with `--continue` rather than
starting its memory over.

---

[← README](../README.md)
