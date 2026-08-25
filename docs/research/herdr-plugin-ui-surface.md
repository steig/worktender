# Research: herdr plugin UI surface — keybindings, panes, focus, notifications

Wayfinder ticket: [worktender#155](https://github.com/steig/worktender/issues/155)
(map: [worktender#153](https://github.com/steig/worktender/issues/153)).

Question: what does herdr's plugin API actually offer a "fleet cockpit" TUI —
a pane running something like `worktender fleet board --watch`, jump-to-worker
navigation, and attention signals?

There is no `docs/research/` convention in this repo yet; this file starts one.

## Sources and environment

All findings are against primary sources on this machine, probed 2026-08-25:

- **herdr 0.8.0** (`herdr --version`; binary at
  `/nix/store/30gsl1768b3j9wi8j8m8rcr5qi7chwhg-herdr-0.8.0/bin/herdr`):
  CLI `--help` output, `herdr --default-config`, `herdr --skill`, the bundled
  socket API schema (`herdr api schema --output …`, protocol 20, schema v1),
  `~/.config/herdr/session.json`, and `strings` over the binary for validation
  messages the help text does not show.
- **worktender 0.9.1 as the installed working example**: plugin root
  `/nix/store/nkcm0g93rlj7zdsskgkw24h9i8whh9r7-worktender-0.9.1` (from
  `herdr plugin list --json`), its `herdr-plugin.toml`, and `bin/worktender`
  (single 3.9M binary — the whole `bin/` layout).
- **This repo's docs and source**: `docs/reference.md`, `docs/events.md`,
  `startup.go`, `internal/reconcile/reconcile.go`.

herdr has no docs command or doc files on this machine (`herdr docs` /
`herdr guide` are unknown commands); herdr.dev was not consulted. Anything the
above could not settle is marked **UNVERIFIED**. No panes were opened, no
events armed, no herdr state modified.

Caveat on versions: the manifest-facing findings are herdr **0.8.0** behavior.
`~/.config/herdr/release-notes.json` already describes 0.8.2, so the CLI may
have moved; nothing below is verified past 0.8.0.

## 1. Keybinding → new pane running a TUI, focused

**A plugin cannot declare a keybinding.** The manifest surface, per the bundled
schema's `InstalledPluginInfo` (fields: `actions`, `build`, `events`,
`link_handlers`, `panes`, `startup` + metadata), has no keybindings field.
`docs/reference.md` ("Binding a key to an action") states it flatly: "herdr
has no `plugin_action` keybinding type" — keybindings live in the **user's**
`config.toml`, and worktender deliberately ships none.

**But the two halves compose:**

**(a) A plugin declares a pane entrypoint.** The manifest supports pane
entries — schema def `PluginManifestPane`: required `id`, `title`, `command`;
optional `description`, `platforms`, `placement`
(`overlay` | `popup` | `split` | `tab` | `zoomed`, default `overlay`), and
`width`/`height` ("only supported when placement is popup" — binary validation
string). The TOML section name is `[[panes]]` (inferred from the
`InstalledPluginInfo.panes` field name; UNVERIFIED as a spelled-out example —
worktender's own manifest declares no panes, and no manifest doc exists on
this machine). Sketch:

```toml
[[panes]]
id = "board"
title = "Worktender: fleet board"
platforms = ["linux", "macos"]
placement = "tab"
command = ["./bin/worktender", "fleet", "board", "--watch"]
```

**(b) Opening + focusing it is one CLI call** (or one socket request,
`plugin.pane.open`):

```sh
herdr plugin pane open --plugin steig.worktender --entrypoint board --focus
```

`herdr plugin pane open --help` shows `--focus`/`--no-focus`, `--placement`,
`--workspace`, `--target-pane`, `--direction right|down`, `--cwd`, `--env
KEY=VALUE`. The API (`PluginPaneOpenParams`) defaults `focus` to **false**.
Placement targeting rules, from binary validation strings: "overlay and popup
plugin panes target the active pane"; "tab plugin panes support workspace_id
but not target_pane_id or direction"; "split and zoomed plugin panes target an
existing pane"; "popup panes can only open from the normal workspace view";
and there is a single popup slot ("popup already open"). Note the 0.8.0 CLI
help does not list `popup` as a `--placement` value or expose
`--width`/`--height`, though the API schema and binary have them — CLI/API
drift, UNVERIFIED whether the CLI accepts them anyway.

**(c) A user keybinding runs that CLI.** `herdr --default-config` documents
`[[keys.command]]` custom commands (`type = "shell"` detached, `"pane"`
temporary pane, `"popup"` modal popup) — exactly the pattern
`docs/reference.md` prescribes for binding plugin actions:

```toml
[[keys.command]]
key = "prefix+alt+f"
type = "shell"
command = "herdr plugin pane open --plugin steig.worktender --entrypoint board --focus"
```

(`docs/reference.md` names the key-command types as "command, pane, popup and
split", the 0.8.0 default config as shell/pane/popup — the exact set is
per-version; UNVERIFIED which is current.) A `type = "pane"`/"popup" binding
could also just run the TUI directly, at the cost of a fresh instance per
press and teardown on exit.

The binary also carries a `keybinding` invocation-source string next to
`link_click` for the `PluginInvocationContext.invocation_source` field, which
*suggests* some native keybinding→plugin path exists or is coming, but no
config surface for it is documented in 0.8.0 — UNVERIFIED.

**Env a plugin-pane process receives** (binary strings): `HERDR_SOCKET_PATH`,
`HERDR_PLUGIN_ENTRYPOINT_ID`, `HERDR_PLUGIN_CONTEXT_JSON` (workspace/tab/
focused-pane/worktree context — schema def `PluginInvocationContext`), plus
the standard `HERDR_WORKSPACE_ID`/`HERDR_TAB_ID`/`HERDR_PANE_ID` any managed
pane gets (`herdr --skill`), plus whatever `--env` passed.

## 2. Programmatic focus of an arbitrary pane by id

**The socket API has it; the CLI mostly does.** The bundled schema lists a
`pane.focus` request whose params are `PaneTarget` — a bare required
`pane_id`. So at the API level, focusing an arbitrary pane by id exists
(distinct from `pane.focus_direction`).

CLI surface, from `--help` output:

- `herdr pane focus` is **directional only** (`--direction left|right|up|down`
  required, from `--pane <ID>`/`--current`) — it maps to
  `pane.focus_direction`, not `pane.focus`.
- `herdr agent focus <target>` takes an agent name **or the pane id hosting
  it** (`herdr --skill`: "Agent commands accept either a unique live agent
  name or the pane ID currently hosting that agent"). For jump-to-worker —
  where every target is a staffed agent pane — this is the whole feature.
- `herdr workspace focus <workspace_id>` and `herdr tab focus <tab_id>` focus
  containers by id.
- `herdr plugin pane focus <PANE_ID>` focuses **plugin-owned** panes
  ("plugin pane not found" error string implies it rejects others —
  UNVERIFIED live).

So a cockpit TUI reading `herdr agent list` can jump to any worker with
`herdr agent focus <name|pane-id>`, and to arbitrary non-agent panes only via
the raw `pane.focus` socket request (no `herdr api call` generic invoker
exists in 0.8.0; `herdr api` is `snapshot`/`schema` only). UNVERIFIED: no CLI
path to `pane.focus` for a non-agent, non-plugin pane by id.

Bonus semantics (`herdr --skill`): focusing marks `done` state as seen (it
decays to `idle`), so jump-to-worker doubles as attention-queue clearing; CLI
reads do not mark seen.

## 3. Notification / highlight primitives

Several, all callable from any pane or plugin process via the CLI:

- **Toasts**: `herdr notification show <TITLE> [--body TEXT] [--position
  top-left|top-right|bottom-left|bottom-right] [--sound none|done|request]`
  (CLI help; API `notification.show`). Delivery is user-configured under
  `[ui.toast]` (`off` | `herdr` in-app | `terminal` OSC | `system` OS
  service) — the default-config comment shows `# delivery = "off"`, so
  whether toasts render out of the box is user-dependent (UNVERIFIED default).
- **Sounds**: `[ui.sound]` plays on agent state changes in background
  workspaces (`enabled = true` default per config comments); the
  `notification show --sound` values reuse the done/request sounds.
- **Pane title**: `herdr pane rename <PANE_ID> [LABEL]... [--clear]`.
- **Rich sidebar metadata** — the strongest cockpit primitive:
  `herdr pane report-metadata --source <ID> <PANE_ID>` sets display-only
  `--title`, `--display-agent`, `--state-label STATUS=TEXT`, and arbitrary
  `--token NAME=VALUE` with `--ttl-ms`; `herdr workspace report-metadata` does
  tokens per workspace. Custom tokens render in configurable sidebar rows via
  `$name` (`[ui.sidebar.agents]` / `[ui.sidebar.spaces]` in default config).
- **Agent state itself**: `herdr pane report-agent --source <ID> --agent
  <LABEL> --state idle|working|blocked|unknown [--message TEXT] <PANE_ID>` —
  drives the sidebar status dots/symbols, the attention queue
  (`ui.agent_panel_sort = "priority"`), sounds, and toasts. This is the
  mechanism worktender workers use (`report.go`).
- **Window title**: API `client.window_title.set`/`.clear`; 0.8.2 release
  notes describe `ui.window_title` syncing the outer terminal title.
- **Bell**: no herdr bell primitive, but BEL from pane programs passes through
  to the outer terminal (0.8.x release-notes fix in
  `~/.config/herdr/release-notes.json`).
- No badge/highlight-a-pane-border primitive was found beyond the above —
  UNVERIFIED that none exists, but nothing in CLI help, config, or the API
  schema names one.

## 4. Startup one-shots, restarts, and "pane exists" as a lease

- **`[[startup]]` runs once per server start, after the server is ready.**
  Source: worktender's manifest comments and `startup.go` ("herdr's
  `[[startup]]` entry runs once, after the server is ready"), plus
  `docs/events.md`. Whether it re-runs on `herdr update --handoff` live
  handoff or `server reload-config`: UNVERIFIED.
- **What herdr persists across restarts is layout + cwd + agent session refs,
  not commands.** `~/.config/herdr/session.json` stores workspaces, tabs,
  pane records of the shape `{ "cwd": …, "agent_session": {source, agent,
  kind, value} }` — no command line, no process info. So panes come back, but
  an arbitrary TUI running in one does not; only recognized agents are resumed
  natively, gated by `[session] resume_agents_on_restore = true` (default per
  default-config comments). Restart behavior was not exercised live —
  inference from the persistence format, marked accordingly.
- **Therefore "pane exists" is NOT a reliable single-instance lease.** After a
  restart the pane exists while the cockpit process in it is gone — a
  pane-existence lease wedges permanently in exactly the state that matters.
  The working example agrees: worktender's staffing lease is **agent
  presence**, not pane existence (`internal/reconcile/reconcile.go`: staff
  targets workspaces where `len(ws.PaneIDs) > 0 && !hasAgent(…)` — a pane
  containing no agent is precisely what gets restaffed). Whether **plugin**
  panes (which carry an entrypoint id, per `PluginPaneInfo`) are restored at
  all across restarts, and whether their command is re-run: UNVERIFIED —
  nothing in session.json's current contents shows one, and no doc says.
- **Auto-restaffing**: herdr itself re-staffs only native agent sessions (the
  `resume_agents_on_restore` path). worktender's `[[startup]]` one-shot
  restaffs agent-less worktree workspaces — but only when
  `WORKTENDER_EVENTS=1` in herdr's own environment (`docs/events.md`,
  manifest comments). Nothing auto-restarts a non-agent TUI pane. A cockpit
  wanting to survive restarts needs its own `[[startup]]` entry (or the
  keybinding), and should treat "my entrypoint's plugin pane is open AND its
  process answers" as the lease, not pane existence.
- Dedicated workspaces: `workspace create` exists (CLI + API), and worktender
  demonstrates workspace-per-worktree adoption, but no herdr primitive marks a
  workspace as owned by a plugin; a cockpit workspace is an ordinary workspace
  the user can close (`confirm_close = true` default). UNVERIFIED: any
  plugin-workspace ownership concept.

## 5. Constraints on long-running TUIs in plugin panes

Verified constraints, with sources:

- **Actions are the wrong tool**: a plugin action is a fixed argv with no
  argument surface, its output lands in `herdr plugin log list` after exit,
  and herdr records exit-0 as "succeeded" (`docs/reference.md`; manifest
  comments: a registered gate "would block … and then deliver its answer
  somewhere its caller is not looking"). Long-running interactive things
  belong in `[[panes]]` entrypoints, which get a real terminal.
- **Placement limits**: one popup at a time ("popup already open"), popups
  only from the normal workspace view, width/height only for popups; overlay/
  popup attach to the active pane, split/zoomed need a target pane, tab panes
  take a workspace (binary validation strings, §1).
- **Plugin update**: herdr has no `plugin update`; an install is a pinned
  shallow detached clone, and reinstalling re-clones (`docs/reference.md`).
  When worktender's own `update` swaps `bin/worktender`, "anything already
  running keeps the old image until it exits" (staged rename, same doc) — on
  Unix a running TUI survives its binary being replaced and keeps running the
  old code until restarted. Nothing observed suggests herdr kills plugin
  panes on reinstall — UNVERIFIED.
- **Disable/uninstall/unlink**: `herdr plugin disable|uninstall|unlink` take
  only a plugin id (CLI help); what happens to that plugin's open panes
  (killed? orphaned?) is UNVERIFIED — no doc, no help text, no unambiguous
  binary string. "plugin pane disappeared" / "plugin popup disappeared"
  runtime errors show herdr tolerates a pane dying under it.
- **Server lifecycle owns the process**: pane processes are children of the
  herdr server; `server stop` stops "the server and its pane processes"
  (`herdr --skill` safety rules), and restart does not resurrect non-agent
  processes (§4).
- **`plugin_command_limit_reached`** exists as an error (binary strings) — a
  cap on concurrent plugin-spawned commands; whether it counts panes as well
  as actions/hooks, and its value: UNVERIFIED.
- A plugin pane's process gets the socket (`HERDR_SOCKET_PATH`) and context
  env (§1), so a cockpit TUI can drive everything in §2/§3 from inside its
  own pane, including using its injected `HERDR_PANE_ID` to report metadata
  about itself.

## Bottom line for a fleet cockpit

Declare the board as a `[[panes]]` entrypoint (placement `tab` for a
dedicated surface); tell users to bind a `[[keys.command]]` shell command to
`herdr plugin pane open … --focus` (plugins cannot ship keybindings); do
jump-to-worker with `herdr agent focus <name|pane-id>`; signal attention via
`pane report-metadata` tokens + `report-agent` states first and
`notification show` second (toast delivery is user-configured); add a
`[[startup]]` entry if the board should reappear after restarts, and lease on
"process alive", never "pane exists" — herdr restores panes but not the
processes in them.
