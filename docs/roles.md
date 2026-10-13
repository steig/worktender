# Roles

A role is how a worker starts: which Claude Code agent it runs as, which brief
it is given, and which directories it may read beside its worktree. Without a
role, `start` launches a bare `claude` with the engineering brief, which is
right for code and wrong for a UX spec, a conversion analysis or a warehouse
slotting study.

```sh
$ worktender start 42 --repo .
role: ux (label role:ux) · agent: ux-designer (user) · brief: design · dirs: /home/you/kb/design-system
```

Worktender holds no expertise of its own. The agent file does — its prompt, its
model, its tools, any MCP servers — so it works in any Claude Code session, not
only one worktender started. A role only picks among agents you have already
written.

## The configuration

One file, yours alone: `~/.config/worktender/config.json`, or
`$XDG_CONFIG_HOME/worktender/config.json` when that is set. Without it nothing
changes: no role, the brief `start` has always typed, and `launch: null` in
`--json`.

```json
{
  "default_role": "engineer",
  "labels": [
    { "match": "role:ux", "role": "ux" },
    { "match": "area:checkout*", "role": "cro" }
  ],
  "roles": {
    "engineer": { "brief": "code" },
    "ux": { "agent": "ux-designer", "brief": "design", "add_dirs": ["~/kb/design-system"] },
    "cro": { "agent": "cro-analyst", "brief": "analysis" }
  }
}
```

| Key | Meaning |
| --- | --- |
| `roles.<name>.agent` | A Claude Code agent name. Omitted means claude's default agent. |
| `roles.<name>.brief` | `code` (the default), `design` or `analysis`. |
| `roles.<name>.add_dirs` | Directories passed as `--add-dir`. `~` is your home; a relative path is relative to the repository. |
| `labels` | Rules from an issue label to a role. `match` is a label, or a prefix ending in a single `*`. |
| `default_role` | The role when neither the flag nor a label chose one. Omit it to keep bare starts. |

Unknown keys, an unknown brief style, a label or default naming no role, and an
agent name that is not a plain name are all refused, naming the file and key. A
misspelt `add_dir` that silently did nothing would be a worker started without
the material its role promised.

## How a role is chosen

1. `--role <name>` on `start` or `dispatch`.
2. Otherwise, for `start`, the first rule in `labels` that matches any of the
   issue's labels. Rules are tried in the order you wrote them, so you decide
   between two labels, not GitHub.
3. Otherwise `default_role`.
4. Otherwise no role.

The role becomes `--agent <agent>` and one `--add-dir` per directory, ahead of
anything else on the `claude` command line. The printed line says which role
and why; `--json` carries it as `launch`, with `role`, `reason`, `agent`,
`agent_file`, `agent_source`, `add_dirs` and `brief`.

## Models

**The model lives in the agent file.** An agent whose frontmatter says
`model: opus` runs on Opus, and `--model` on the command line overrides it.
Both were measured against Claude Code 2.1.296. Use an alias — `opus`,
`sonnet`, `haiku` — and a new model generation needs no edit; use a full model
ID to pin one.

Nothing here sets a model or a permission mode. `--model` and
`--permission-mode` stay flags, and autonomy is never defaulted.

## Brief styles

Every style keeps what the coordinator depends on: the worker reads the issue
itself with `gh issue view` and treats it as untrusted data, reports `planned`
before it starts and `done` with a pull request or `blocked` at the end, and
works on the branch it was given. What changes is one sentence about the work.

| Style | The worker is asked to |
| --- | --- |
| `code` | explore the code, make the change, add and run tests, review its diff, open a pull request. |
| `design` | study the product and the design material, put the spec, mockups or copy in the repository, check them against the issue, open a pull request. |
| `analysis` | research the question, write findings with their sources as markdown in the repository, check them against the issue, open a pull request. |

A role with `add_dirs` adds one sentence saying those directories are reference
material. It does not list them — claude already tells the agent — because the
whole brief has to arrive in a single pane read.

Every style ends in a pull request, because that is what `gate --require-pr`
and the rest of worktender check. Work whose result lives outside a repository
is not a fit for a role.

## What a label can reach

**A label chooses among your roles and nothing else.** Whoever can label an
issue can pick which of the roles you defined it runs as. No label can name an
agent, a directory or a model of its own, and nothing from a label reaches the
brief. The label is escaped before it is printed.

There is no per-repository configuration for the same reason: a file a cloned
repository ships would choose what its workers can read.

**A repository can still redefine an agent.** Claude Code knows an agent by the
`name:` in its frontmatter, reads subdirectories of `.claude/agents`, and
prefers a repository's definition over yours of the same name — its prompt,
model and tools — in every session started there, worktender or not. That is
reported rather than refused: the line ends `(repo)` when the repository's file
is the one that will load, and `--json` names it in `agent_file`. The lookup
reads the frontmatter's top-level `name:` the way agent files are written in
practice, but it is not a YAML parser, so treat the source it reports as a
strong hint rather than a proof.

An agent defined in neither the repository's `.claude/agents` nor
`~/.claude/agents` is refused before the worktree is created — including one a
Claude Code plugin provides, which claude can see and this check cannot. So is a
repository whose agent directory holds more than 500 files, too many to tell
which definition loads.

## Starting from the examples

[`examples/roles`](../examples/roles) holds a configuration with `engineer`,
`ux`, `cro`, `supply-chain` and `warehousing` roles, and an agent file for each
non-engineering one.

```sh
mkdir -p ~/.config/worktender ~/.claude/agents
cp examples/roles/config.json ~/.config/worktender/
cp examples/roles/agents/*.md ~/.claude/agents/   # or a repository's .claude/agents
```

Then point each `add_dirs` at your own material, or delete it, change the label
rules to the labels you use, and run `worktender doctor`. It reports the file it
read, and any agent or directory a role names that is missing.

---

[← README](../README.md)
