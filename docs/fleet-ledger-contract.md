# The fleet ledger contract, v1

The fleet ledger is the append-only record a fleet controller keeps of what it
asked for and what came back: dispatches, acks, reports, verifications,
deadlines, escalations, and the human decisions routed through the loop. It is
an oversight record first and replay state second — the reason it has a richer
vocabulary than a machine would need.

Two sides code against this file. The fleet-coordinator skill *writes* the
ledger; `worktender fleet board` *reads* it and renders it merged with live
worktree, agent and report state. Neither side owns the format alone, which is
why it is pinned here rather than in either implementation.

This contract was decided in
[worktender#154](https://github.com/steig/worktender/issues/154).

## File

One append-only JSONL file at a fixed path:

```
$XDG_STATE_HOME/fleet/ledger.jsonl        # ~/.local/state/fleet/ledger.jsonl by default
```

Machine-local state, in no repository, never synced. Rotation is deferred; the
design is rotation-friendly — rename the file and start a fresh one — and
nothing in this contract assumes the file reaches back to the beginning of
time.

## Envelope

Every line is one JSON object carrying five envelope fields:

| field  | type   | meaning                                              |
| ------ | ------ | ---------------------------------------------------- |
| `v`    | int    | format version; `1` for everything this file defines |
| `ts`   | string | ISO 8601 timestamp of the append                     |
| `task` | string | controller-minted task id                            |
| `type` | string | entry type, one of the nine below                    |
| `by`   | string | controller session name that wrote the entry         |

A line missing any envelope field is malformed. Readers count it and move on
rather than aborting: one bad line must not cost the board the fleet.

## Entry types

The type-specific fields ride beside the envelope in the same flat object.
Every one of them is optional at the JSON level — absence is "not said" — but
an entry that omits the fields its own type is for is a writer bug.

- **`dispatch`** — work handed to an executor. `executor` (`"worker"` |
  `"peer"`), `target` (the agent or pane it went to), `repo` (the repository
  root path, absolute — the board matches worktrees on it), `issue` (int),
  `deadline` (ISO 8601, when one was set).
- **`ack`** — a peer accepting or declining. `result` (`"accepted"` |
  `"declined"`), optional `note`.
- **`report`** — what the executor said when it finished or gave up. `status`
  (`"done"` | `"blocked"`), `pr` (int, omitted or 0 when none), optional
  `note`.
- **`verify`** — the controller checking the work rather than relaying the
  claim. `result` (`"pass"` | `"fail"`), `evidence` (one line).
- **`escalate`** — something a person must see. `severity` (`"safety"` |
  `"ordinary"`), `reason`. Only the safety tier pushes notifications; ordinary
  escalations surface on the board and in digests.
- **`steer`** — a message sent to a peer about work it owns. `note`.
- **`nudge`** — a contract restatement to an executor. `note`.
- **`deadline`** — a deadline's lifecycle. `result` (`"set"` | `"expired"` |
  `"met"`), `deadline` (ISO 8601, on `set`).
- **`human`** — Tom's decisions routed through the loop. `action`
  (`"merge-executed"`, `"escalation-acknowledged"`, `"permission-granted"`,
  `"permission-denied"`, `"instruction"`), optional `note`. The controller
  ledgers what it sees; a GitHub action taken silently surfaces later through
  `verify`, by design.

## Concurrency

Two MUSTs, and no locking:

- The controller is the **sole writer** — that is the agent-presence lease,
  not a file lock — and MUST emit each entry as a single `O_APPEND` write of
  at most 4KB ending in `\n`.
- Readers MUST discard a final line that lacks its newline: that is a write in
  flight, not corruption.

There is no per-line fsync. A crash may lose the tail, and replay tolerates
it, because the world — pull requests, panes, agents — is the fallback truth.

## Replay

An entry is *terminal* for its task when it is a `verify`, an `escalate`
resolved by a `human` entry (`escalation-acknowledged`), or an `ack declined`
followed by a reassignment `dispatch`. Replay scans back from the tail
collecting tasks with no terminal entry — bounded at 7 days or the last
`human` round-closed marker, whichever comes first. Open loops beyond the
horizon surface once as a stale-loops digest rather than silently re-arming.

The division of authority: the ledger says what *should* be in flight;
`ls --all-repos --reports` and the live agent list say what *is*.

## Versioning

- **Additive changes do not bump `v`.** New optional fields and new entry
  types arrive under `v: 1`; boards render unknown types raw and ignore
  unknown fields.
- **Breaking changes bump `v`.** The board supports the current version and
  the previous one; an entry with a `v` it does not know renders raw, with a
  one-time warning.

---

[← README](../README.md)
