# Watching the fleet from Claude Code

`worktender-watch` is a Claude Code mod that tells the session you coordinate
from when a worker needs you, instead of that session sitting inside `gate`.

```
/plugin install worktender-watch --marketplace steig/worktender
```

Answer `y` to add the marketplace, then pick a scope. It is active at once.

## What it shows

Every 30 seconds it runs `worktender ls --all-repos --reports --json` — about
20 ms — and from that draws three things:

- **A status line entry**, `wt: 2 working · 1 blocked · 1 done`. Nothing when no
  agents are running or herdr cannot be reached.
- **A band above the prompt** when something needs a look: a worker that
  reported `done` (with its pull request) or `blocked`, or an agent herdr reports
  stopped at a prompt nobody is there to answer. **Ask Claude to check** submits
  a prompt asking for verification; **Dismiss** clears it for good.
- **A toast** the first time each of those appears. Whatever had already been
  reported when the session started is not announced.

`/wt` looks immediately and lists the same. `/wt undismiss` brings dismissed
items back.

The session's own pane is not a worker. It is left out of the counts and of
"waiting at a prompt", since it waits at one every time you are asked to
approve something.

## Settings

In `/config`, under `worktender-watch`:

| Setting | Default | |
| --- | --- | --- |
| `wake` | `toast` | `turn` also starts a turn asking Claude to verify what was reported. That spends tokens without you asking, so it is opt-in. |
| `intervalSeconds` | `30` | How often to look. |
| `worktender` | `worktender` | The binary, if it is not on `PATH`. |

## What it never does

**A report's note is not read.** The note is the worker's own text, and its task
usually came from an issue anyone could write. The band, the toast and every
prompt the mod submits are built from status, pull request number, repository
and branch alone.

It starts no worker, merges nothing and closes nothing. The only thing it can do
on its own is queue a prompt, and only with `wake` set to `turn`.

## The mod or `gate`

They answer the same question for different callers:

- **An interactive session coordinating workers:** the mod. Start the workers,
  carry on or stop, and act on what it raises.
- **Anything that cannot be woken** — a script, `claude -p`, CI: `gate`, which
  blocks until a report arrives and exits with a code that says what happened.

Two sessions on one machine each running the mod will each announce the same
report.

## Developing it

The mod is in [`mods/worktender-watch`](../mods/worktender-watch).

```sh
claude --plugin-dir mods/worktender-watch      # run it from the checkout
claude plugin validate mods/worktender-watch
claude plugin test mods/worktender-watch
```

CI runs both checks on a pinned Claude Code version; the mod API is early
access, so moving that version is a deliberate change.

---

[← README](../README.md)
