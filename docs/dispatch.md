# Dispatching a worker, and waiting for it

This is what the plugin is for once more than one agent is involved: a
coordinating agent hands a slice of work to another and needs to know when it is
done — without reading everything that agent did to get there.

```sh
# Resolve the binary once. A standalone install is on PATH; a plugin install
# is not, because herdr owns it. `worktender doctor` prints the answer either
# way, so you need the jq only once.
worktender=$(command -v worktender) || worktender=$(herdr plugin list --json \
  | jq -r '.result.plugins[] | select(.plugin_id == "steig.worktender") | .plugin_root')/bin/worktender

# COORDINATOR — when the slice is a GitHub issue, one command does all of it.
# --repo because start creates a checkout and is not a herdr action, so nothing
# tells it which repository unless you do. Flags go on either side of the number.
"$worktender" start 42 --repo . --model sonnet
# `start` prints this exact line. It names the pane, not the agent: herdr can
# stop resolving an agent name while it runs, and the pane lasts as long as it.
"$worktender" gate --target w9:p1 --until done --require-pr --timeout 20m

# ...and with several running, wait on all of them at once. The first to report
# releases, and the gate says which one it was.
"$worktender" gate --any w9:p1,w10:p1,w11:p1 --until done --timeout 20m

# ...and when it is not, the pieces are still separate. Dispatch, then wait.
"$worktender" dispatch --pane w22:p1 --name reconcile-split --model sonnet
"$worktender" gate --target w22:p1 --until done --require-pr --timeout 20m

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
composer is a worker with nothing to do that a listing reports as `idle`.

`worktender start <issue> --brief <text|->` types that text as the first prompt
instead of the generic one (`-` reads stdin and waits for it to close). It
replaces the generic brief whole: the worker is not told to treat the issue as
untrusted data, nor pointed at the pinned issue, the lessons or `report`, so the
text has to say what it needs. A brief starting with `!` or `/` is refused, and
newlines are folded to spaces.

**`start` never types on a trust screen.** A new worktree is a folder Claude Code
has not seen, so it may open on its folder-trust dialog, a hooks-review screen,
or the dialog asking you to approve managed settings, instead of its composer —
and there the Enter meant for the brief would accept the dialog on your behalf.
Before typing the brief, and again before every Enter, `start` reads the visible
pane; if it shows any of these it sends nothing, exits **3** naming the screen,
and leaves the worktree and the agent for you. Answer the dialog in the pane,
then brief the agent yourself. The match is on the dialog's text, taken from
Claude Code 2.1.300's source for the trust and managed-settings dialogs and from
#196 for the hooks screen, so a Claude Code release that rewords any of them can
slip past it.

The brief names the issue rather than carrying it — the worker runs `gh
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
worktender start <issue> [--role <role>] [--model <model>] [--permission-mode <mode>] [--base <ref>] [--branch-prefix <type>] [--repo <path>] [--focus]
worktender dispatch --pane <id> --name <agent> [--role <role>] [--model <model>] [--permission-mode <mode>] [--resume]
worktender report --status planned|blocked|done [--pr N] --note <text>
worktender gate --target <agent|pane> | --any <a,b,c> [--until done] [--require-pr] [--repo <path>] [--timeout 15m] [--dagr]
```

## Branch names

`start` names the branch `<issue>-<title-slug>`. When the repository has a hook
that requires a `type/` prefix, `start` finds it and adds one (`feat/` when the
hook allows it, otherwise the first type listed). It looks in `.husky/` and the
git hooks directory, only at lines that mention a branch beside a
`(feat|fix|...)/` alternation. That is best-effort: a hook written another way
is not seen, and the branch comes out unprefixed. `--branch-prefix fix` (or
`fix/`) sets the prefix yourself and skips detection; `--branch-prefix ""` forces
none.

herdr names the checkout directory after the branch with each `/` as `-`, so
`feat/42-x` lives in `feat-42-x`.

## Starting a worker as a role

`--role` starts the worker as a role from your own configuration — a Claude Code
agent, a brief style, and directories it may read — instead of a bare `claude`
with the engineering brief. A label on the issue can pick it too:

```sh
$ worktender start 42 --repo .
role: ux (label role:ux) · agent: ux-designer (user) · brief: design · dirs: /home/you/kb/design-system
```

The configuration, how a role is chosen, the brief styles and what a label can
and cannot reach are in [Roles](roles.md).

## Acceptance checks

A gate can only check what an issue states mechanically. An issue whose "done"
is prose gets verified against whoever is reading it. So an issue **cleared for
an agent** — one carrying the `factory:ready` label — states its done as checks
a machine can run, in a section of its own:

```markdown
## Acceptance

- [ ] `go test ./...` exits 0
- [ ] `test -f docs/acceptance.md` exits 0
- [ ] `worktender doctor` exits 0
- [ ] `go test -count=1 -run TestX -v . | grep -q -- '--- PASS: TestX'` exits 0
```

The grammar is strict:

- The heading is exactly `## Acceptance`, at the start of a line and outside a
  code fence. The section runs to the next heading.
- Each non-blank line in it is `- [ ] ` or `- [x] `, then the **command** in
  single backticks, then one space, then the **outcome** it should give: an
  exit code, output, a file present.
- The command and the outcome are printable ASCII (0x20 to 0x7E), and neither
  contains a backtick.
- Any other line refuses the whole issue, by line number. A skipped check is a
  check nobody runs. A second `## Acceptance` heading is refused too.

The grammar makes "the commands" well defined, so the approver, `start`, and
later a gate all read the same list. It is not what makes running them safe.
Approval is.

The [`factory-ready` issue template](https://github.com/steig/worktender/blob/main/.github/ISSUE_TEMPLATE/factory-ready.md)
lays this out.

### Drafting checks

Most issues are written as prose. `worktender draft` proposes an Acceptance
section for one, for a person to read and approve:

```sh
worktender draft 42 --repo .            # print a proposed section
worktender draft 42 --repo . --write    # print it, ask, then put it on the issue
```

The drafter is `claude -p`, headless, launched with
`--restricted --strict-mcp-config --tools Read,Grep,Glob` and
`--permission-mode dontAsk`: it can read files and nothing else. No shell,
no editing, no network, no MCP. It gets the issue framed exactly as `approve`
frames it, every line prefixed `| ` and said to be untrusted, and the
repository's test, build and lint entry points.

**It does not run in your checkout.** It runs in a throwaway export of the
files `HEAD` tracks (`git archive HEAD`, unpacked into a private temporary
directory and removed once the drafter returns), so an untracked `.env`, an
ignored key or build output is not there to read. Directories and regular
files only: a tracked symlink is left out, so it cannot point outside. The
runners below are detected from that export, so uncommitted changes to a
Makefile or `package.json` do not count.

**Its answer is untrusted too.** Every line it proposes is parsed with the
grammar above, and a line that does not parse is dropped. So is any command
outside the allow-list. The list is of exact argument shapes, not command
names, because a runner's own flags can run anything (`go test -exec`,
`make SHELL=`, `npm exec`, `grep -f /etc/...`):

- the repository's own runners, detected from its files. `go test`, `go vet`
  and `go build` (from `go.mod`) take only `./` package patterns and the
  flags `-run <name pattern>`, `-count=N`, `-race`, `-v` and `-short`. The
  others take no argument beyond their name: `npm test` and
  `npm run <script>` for each script in `package.json` (`pnpm` or `yarn` when
  their lockfile is there), `make <target>` for each Makefile target,
  `just <recipe>` for each justfile recipe, and `cargo test`, `cargo build`
  and `cargo clippy`;
- `test -f|-d|-e <path>` and `grep [-q|-qE|-qF] <pattern> <path>`, where the
  path is relative to the repository root, has no `..`, and is at most 100 of
  `A-Za-z0-9_./-`, and the pattern is at most 60 of `A-Za-z0-9 _.:/-`. A
  `grep` of a source file (`.go`, `.ts`, `.js`, `.py`, `.rs` and the like) is
  dropped: a string in code pins one implementation and proves nothing the
  tests do not, so the drafter is told to prefer a narrowed test run
  (`go test -run TestName ./...`) and keep `grep` for docs;
- `worktender doctor` and `worktender ls`, with no argument.

The expected outcome is one of `exits 0`, `exits 1`, `exits non-zero`,
`prints nothing` or `prints <word>`, the word at most 40 of `A-Za-z0-9._-`.
These limits are there because the section is published: a pattern, a path,
a `-run` value (at most 60 characters) or an outcome is where an injected
issue could have the drafter write something out, so none of them can hold
much. They narrow the channel, they do not close it: what keeps secrets out is
that the drafter never sees them, and that you read the section before it is
written.

A command is one simple command. Any shell character, wherever it is (a pipe,
`;`, `&&`, `$(`, a backtick, a redirect, a glob, `~`), drops it, so nothing
can chain from an allowed command to another. Every drop is printed with its
reason, the drafter's line quoted beside it and never run. At most six checks
are kept. If none survive, `draft` exits **3**.

**`draft` never labels and never approves.** Without `--write` it only prints.
`--write` prints the exact section, then asks you to type `yes`, as `approve`
does; any other answer, or no terminal to answer from, writes nothing and
exits **1**. Only then does it edit the issue body with your own `gh` auth,
putting the section at the top. **`--yes` skips that question, and with it the
human check before a model's output is published to the issue**; use it only
where you would have typed yes without reading. It refuses an issue that already has an Acceptance section unless you
pass `--replace`, and refuses one edited while the drafter ran. The next step
is still `worktender approve`. The approval covers the drafted checks byte for
byte, like any others, and it is still the control: the allow-list keeps the
obvious out of the proposal, and the approver reads what is left.

Each draft is recorded in the [ledger](#what-a-merged-pull-request-cost) as a
`draft` event, with how many checks were kept and dropped and whether they
were written.

### Approving the raw bytes, not the page

An agent runs these commands, so a person with write access has to approve them
first. They approve what the agent will receive, not what GitHub draws:

```sh
worktender approve 42 --repo .
```

`approve` fetches the issue once and prints the **canonical payload**: the
title, the parsed acceptance commands, and the body. All of it is raw and none
of it is rendered. Every body line is prefixed `| `, and every invisible or
control character (zero-width, bidi, carriage return, tab) is printed as a
visible `\u{...}` by `safetext`. Then it prints the payload's sha256 and asks
for `yes`, unless you pass `--yes`. On yes, it posts the comment
`worktender-approved: sha256=<hex>` and applies `factory:ready`, both with your
own `gh` auth.

To approve exactly what a person was shown, pass the hash they saw:
`worktender approve 42 --sha256 <hex>`. It fetches and prints as above, and if
the payload hashes to `<hex>` it approves without asking. If the issue changed
since, it approves nothing and exits 3. `--sha256` takes 64 lowercase hex
characters and cannot be combined with `--yes`.

From a tab with no TTY, such as a coordinator's, `worktender approve 42 --pane`
opens a pane in the issue's herdr workspace and runs the plain
`worktender approve 42` there, so you type `yes` at the prompt. It cannot be
combined with `--yes` or `--sha256`, and the command it types never carries
them. This is a convenience, not an access control: anything that can call
herdr can send keys to that pane, or run `approve --yes` itself. To keep a
coordinator from approving, give it a `gh` token that cannot comment on issues
or apply labels.

**Read the output.** The approver approves whatever they were shown. That is
the residual risk, and nothing downstream can catch what they missed.

Approval is over the raw bytes because approving the rendered page kept
failing. Two earlier drafts tried to make the raw markdown match what GitHub
draws: one stripped comments and refused tags, the other used an ASCII-only
grammar. Each one lost to a renderer behaviour it had not modelled. Link text
hides its URL, emphasis eats characters, and HTML folds content away. A hash
over the raw payload has no renderer in it. Any edit, visible on the page or
not, changes the hash.

**`start` checks the approval.** For a `factory:ready` issue, `start` reads the
title, body and last 100 comments in one GraphQL response. It rebuilds the
same payload and proceeds only when a comment from an account with write or
admin access carries exactly that hash. Access is checked with
`gh api repos/{owner}/{repo}/collaborators/{login}/permission`. Any of these is
refused with **3**, before any worktree exists:

- the issue was edited after approval;
- nobody approved it;
- the only matching comment is from an account without write access, or a
  deleted account;
- `WORKTENDER_APPROVERS` is set and no matching comment is from a login on it;
- the Acceptance section does not parse.

```
worktender: issue #42 changed since approval, or was not approved: no comment from an account with write access carries sha256 …; run `worktender approve 42`
```

**Write access does not tell a person from a worker's account**, which needs
write access to push. Name the people whose approvals count:

```sh
export WORKTENDER_APPROVERS=steig,alice
```

It is a comma-separated list of GitHub logins, matched without regard to case.
When it is set, an approval counts only if its author has write access *and*
is on the list. An approval from anyone else is refused, and the refusal names
the approver and the variable:

```
worktender: issue #42 was not approved by anyone in WORKTENDER_APPROVERS with write access: of the approvals of sha256 …, @worker-bot is not listed; have someone listed run `worktender approve 42`
```

It does nothing against a worker on your own token, which approves as you; that
is what [`WORKTENDER_WORKER_GH_TOKEN`](reference.md#giving-workers-their-own-github-token)
is for, and the two together are the point. Unset, any write-access account
counts, as before, and `doctor` warns. Set but blank, or nothing but commas,
nobody counts. The event hooks read it from herdr's own environment, like
`WORKTENDER_EVENTS`, so export it before starting herdr.

The code is 3 rather than 1 because no change to the invocation fixes it. A
person has to read the payload and approve it, which no retry and no
coordinator can do for them. If `gh` cannot answer at all, the exit is **2**,
the same as for an unreadable issue.

### Verifying that the checks prove the change

A check that already passes before the work proves nothing about it. `go test
-run TestX` exits 0 with "no tests to run" until the worker writes `TestX`.
`worktender verify` runs the approved checks on both sides of a change. The
form that guards against it is the last line of the example above, which exits 0
only when `TestX` ran and passed. Write it by hand: a check runs under `sh -c`,
so the pipe works there, but `draft` drops any shell character and will not
propose it.

```sh
worktender verify 42 --repo .            # this checkout's HEAD against its merge-base
worktender verify 42 --repo . --pr 17    # the PR's head against its merge-base
```

It reads the checks with the same parse and approval check `start` uses, so an
issue edited since approval is refused with **3**. Each check runs under
`sh -c` in an export of the merge-base's tracked files, then in an export of
the head's, the same `git archive` export `draft` uses, never in a checkout.
Every check gets a fresh export, so a file one check writes is not there for
the next. Each run has `--timeout` (default ten minutes), which kills the
check's whole process group. With `--pr`, the PR has to be the issue's: on a
branch named for it (`42-…`) or closing it, or `verify` exits **3**. Its head
has to be in the local repository already; `verify` says how to fetch it.

**Dependencies are installed once per commit, without scripts.** When the
commit tracks a `pnpm-lock.yaml` or `package-lock.json`, `verify` runs
`pnpm install --frozen-lockfile --ignore-scripts` or `npm ci --ignore-scripts`
in an export and links the result into each check's export. Because
`--ignore-scripts` is on, a check that needs a `postinstall` or `prepare` step
(esbuild's binary, sharp, `prisma generate`) fails on base and head alike, and
for that reason. A failed install fails every check on that commit, and the
notes carry the tail of its output. A `yarn.lock` or `bun.lockb` with no
supported lockfile beside it is not installed from; the notes say so.

**The export is not a sandbox.** It keeps your checkout's untracked files out of
the way, nothing more. A check like `go test` runs the head's code as you, with
your environment, your `$HOME` and your network, so `--pr` on a pull request
whose code you have not read runs that code with your credentials. Verify heads
you would run the tests of anyway.

The outcome is judged as written: `exits 0`, `exits 1`, `exits non-zero`,
`prints nothing` (no output on stdout or stderr), or `prints <word>`, the word
being one `draft` could propose (at most 40 of `A-Za-z0-9._-`). `prints <word>`
is a case-sensitive substring match on stdout and stderr together, and ignores
the exit code. Any other outcome cannot be judged. Each check is then one of:

- **discriminating**: fails on base, passes on head;
- **guard**: passes on both;
- **failing**: fails on head;
- **unjudged**: an outcome `verify` cannot read, or a run that timed out or was
  killed by a signal on either side.

One line per check, with both commits. It exits **0** only when no check fails
on head, none is unjudged, and at least one discriminates. Otherwise it exits
**3**: a change whose checks all passed before it started is a person's call.

The worker's own tests get the same question. Once the checks pass, `verify`
takes the head's export, puts every changed `.go` file not named `*_test.go`,
`go.mod` and `go.sum` back as they were on the merge-base, keeps the changed
test files, and runs `go test -count=1 -v` on their `Test` functions there.
Only a test that ran and failed shows the tests need the change. Otherwise it
exits **3**:

- every test passed: `hollow: tests pass with the change reverted`;
- the tests do not build without the change, which says a test calls
  something new, not that it checks it;
- no test ran to a pass or a failure (all skipped, or excluded by build tags);
- the run timed out;
- a changed test file holds no `Test` function `verify` can find.

It skips this, saying so, when the change touches no Go source, `go.mod` or
`go.sum`, or no `*_test.go` file.

**The worker reads the approved payload, not the live issue.** From that same
single fetch, `start` writes the payload byte for byte to
`$(git rev-parse --git-path worktender/issue.md)` in the new worktree. That is
inside the worktree's git dir, so it is never committed and goes away when the
worktree does. Every line of issue text in it starts with `| `, so nothing an
issue says can pass for one of the file's own lines, including `end of body`.
The file says that later edits to the live issue are unapproved. It also tells
the worker not to run a check that reaches beyond checking the work (network,
credentials, anything outside the repository) and to report it as blocked
instead. That sentence is a second line of defence. The approval is the
control.

**The brief counts the checks. It does not carry them.** A ready issue's brief
points at the pinned file instead of `gh issue view`, and says how many checks
`start` parsed. The worker runs them and says in its `done` note which pass,
for example `acceptance 3/4 pass, #2 fails`. No text from the issue author goes
into the prompt itself.

An issue **without** the label is read exactly as before. `start` asks `gh` for
the same fields, fetches no snapshot, writes no file, and types the same brief.

The worker's report is a claim, not proof. Re-running the checks at the gate,
so that it does not take the report's word, is a separate step.

## Pulling issues from a queue

`worktender factory` takes the human out of picking each issue. One pass looks
at the repository's open issues labelled **`factory:ready`**, and while fewer
than `--max` workers (default 2) hold a slot, starts the oldest with `start` and
moves its label to **`factory:in-progress`**. Then it exits. It is not a daemon;
run it from a loop or from cron.

Keep `--max` at 3 or below until review's catch rate has been measured: see
[Canaries](canaries.md) for the procedure and the gate.

```sh
# See what a pass would do, without starting anything.
worktender factory --repo ~/code/thing --dry-run

# A loop. One loop cannot overlap with itself, which is the simplest way to
# never run two passes at once.
while true; do worktender factory --repo ~/code/thing --max 2; sleep 300; done

# Or cron, with flock(1) so a slow pass is skipped rather than doubled.
*/5 * * * * flock -n /tmp/worktender-factory.lock /path/to/worktender factory --repo /home/you/code/thing
```

**An issue gets onto the queue through `worktender approve <issue>`**, not by
hand-labelling it. `approve` shows the payload, posts its hash comment and
applies `factory:ready` (see [Acceptance checks](#acceptance-checks)). A label
applied any other way puts an issue on the queue that `start` will refuse.
Both labels must exist in the repository before the first pass: run `gh label
create factory:ready` and `gh label create factory:in-progress`.

- **Every live worker holds a slot until it reports done.** `idle` counts: a
  worker between turns is still mid-task, waiting on its checks, a review or a
  `pr-feedback` notice, and it will spend again when it resumes. Without that,
  every pass would start more workers while the earlier ones waited. A slot is
  freed when the worker's last `report` says `done` (the same report
  `ls --reports` shows), or when its agent is gone: the pane closed or the
  worktree pruned. A report that cannot be read leaves the slot held. herdr has
  to be running, even for `--dry-run`, because the count comes from herdr.
- **Nothing starts while the default branch is red.** Each pass asks gh for the
  latest completed `ci` workflow run on the branch workers fork from
  (`origin/HEAD`), looking past cancelled, skipped and neutral runs to the last
  one that judged the code. If it failed, the pass starts nothing, names the
  run's URL and exits **3**; `--dry-run` does the same. If gh cannot answer,
  the pass warns and goes on.
- **Oldest first,** by issue number.
- **Each worker's model follows its slice.** `--model` is passed to every
  start in the pass; without it, an issue's `model:<name>` label, then
  `default_model`, picks it, as on `start`. See
  [choosing a model](roles.md#choosing-a-model).
- **Never twice.** An issue is skipped when any local or remote branch, or any
  checkout herdr knows, is named for its number the way `start` names branches
  (`42` or `42-…`), or when it already carries `factory:in-progress`. Matching
  on the number rather than the whole name means an edited title still matches.
- **Claimed on origin before it starts.** Before each start the pass pushes
  `refs/worktender/claims/<issue>` to origin, create-only. If the ref is
  already there, another coordinator holds the issue: the pass skips it
  (`claimed-elsewhere` in the notes and in `--json`) without spending a slot.
  Claims live under `refs/worktender/`, so they never show up as branches. A
  start that fails before it made a branch or checkout deletes its claim
  again; one that fails later keeps it, since a worker may be live there, and
  says so. A pass that cannot push the claim at all (no origin, no access, a
  ruleset or hook refusing it) stops there and exits **2**.
- **Claim, start, then relabel.** If the relabel fails after a successful
  start, the issue stays labelled `factory:ready`, the pass exits **2**, and the
  next pass skips it because its branch and claim exist. The other order would
  leave an issue labelled in progress with nothing working on it whenever a
  start failed.
- **Approval is `start`'s to check**, for a manual start and a queued one alike;
  the factory does not restate it. When `start` refuses an issue (exit 3:
  edited since it was approved, or never approved by an account with write
  access), the pass skips it, prints start's own message (which names the
  `worktender approve <issue>` to run), leaves its label alone, does not count
  it against the cap, and starts the next one. It still exits 3, so the refusal
  reaches whoever reads the log.
- **A failed start** for a reason of the issue's own — a role nobody defined, an
  agent that never took its brief — is reported and the pass moves on to the
  next issue; the pass still exits non-zero. A failure of the machine (exit 2:
  herdr or gh unreachable) stops the pass.
- **Closed issues lose `factory:in-progress`.** A worker's merged pull request
  closes its issue but leaves the label on it. Every pass, at the cap or not,
  removes `factory:in-progress` from each closed issue still carrying it,
  deletes that issue's claim ref, and says which (`cleared` in `--json`);
  `--dry-run` only lists them. Open issues and other labels are never touched.
  A failed removal is a warning: the pass still works the queue, then exits
  **2**.
- **A stale claim is cleared by hand.** Claims do not expire. One left behind —
  a pass killed mid-start, a start that failed after making its checkout, or a
  closed issue that never got its in-progress label — keeps that issue off
  every queue until you delete it:
  `git push origin :refs/worktender/claims/<issue>`. List them with
  `git ls-remote origin 'refs/worktender/claims/*'`.

```sh
worktender factory [--max <n>] [--model <model>] [--repo <path>] [--dry-run] [--json]
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

## What a merged pull request cost

`worktender stats` answers whether the factory is helping: per dispatch, how
long it took from `start` to its last `done`, how many tries it needed, and how
much coordination it took against how much it changed.

```sh
$ "$worktender" stats
ISSUE  BRANCH          ROLE  PR  MERGED  WALL     ATTEMPTS  INBOX  GATES  DIFF  CEREMONY/100L  ESCAPED
#42    42-fix-thing    eng   7   yes     1h0m0s   2         3      2      200   2.50           no
#43    43-other-thing  -     8   yes     15m0s    1         0      1      10    10.00          revert #12

2 dispatches, 2 with a pull request, 2 merged
per merged PR: median wall clock 37m30s, 1.50 attempts, rework rate 50%, 3.00 ceremony
tokens per merged PR, median over the 2 of 2 with a transcript: 395 in, 41800 out, 3120000 cache read, 190500 cache write
  claude-sonnet-5-5: 790 in, 83600 out, 6240000 cache read, 381000 cache write
escape rate 1/2 (9–91%) of merged PRs reverted or blamed for a regression within 14d of merging; under 5, not enough data to judge yet
```

`--since 168h` keeps only dispatches started in the last week; the default is
all of history.

It reads a local ledger, `<plugin state dir>/ledger.jsonl`, that five commands
append one line to as they go:

- **`start`** records the dispatch once its agent has the brief — issue, branch,
  role, agent, agent kind and agent name. Only dispatches made by `start` are
  counted: a worker staffed by `sync` or `dispatch` has no issue to count against.
- **`report`** records each status a worker claims, with its time, pull
  request and the agent session herdr names for its pane, keyed by the
  repository and branch it was made from.
- **`gate`** records each wait a coordinator opens, against every worker it covers.
- **`pr-feedback`** records each notice it types at a worker, by repository,
  branch and pull request.
- **`draft`** records each section its drafter proposed for an issue: how many
  checks it kept and dropped, and whether `--write` put them on the issue.
  `stats` does not read it yet.

The inbox is read where it already is: messages on the thread named for the
issue number, posted after the dispatch started.

What the columns mean:

- **`ATTEMPTS`** is the number of `done` reports. A second one means the first
  did not stick — review sent it back, a check failed, it was re-briefed.
  A `pr-feedback` round is a retry too, so a dispatch with N rounds shows at
  least N+1, however many times it reported done.
  **Rework rate** is the share of merged pull requests that needed more than one.
- **`WALL`** is `start` to the *last* `done`, because a dispatch that went round
  again was not finished the first time it said so.
- **`CEREMONY/100L`** is inbox messages plus gate runs, per hundred lines the
  pull request changed (additions plus deletions, from gh). Ceremony rising while
  the diff stays flat is the sign of agents proving to each other that earlier
  checks ran instead of moving the work.
- **`MERGED`** and **`DIFF`** come from `gh pr view`, the one thing worktender
  cannot observe itself. `?` is gh failing to answer; `--json` says why.
- **`ESCAPED`** is whether a merged pull request turned out not to be finished,
  and what showed it; `-` when it has not merged or gh could not be searched.

### Tokens

What a dispatch spent is read from its worker's own Claude Code transcripts,
`~/.claude/projects/*/<session>.jsonl` and the subagent transcripts beside it.
The session ids come from the ledger: `report` asks herdr for its pane's
`agent_session` as it records each report, because by the time a pull request
merges the pane is gone. A worker restarted mid-dispatch has two sessions, and
both count.

Each API response is counted once, by its message id — Claude writes one line
per content block, each repeating the usage — and the counts are kept per
model: input, output, cache-read and cache-write tokens. They are tokens, not
dollars; a price table in the binary would be wrong the first time a price
changed. A transcript that is gone is skipped without a word, and its
dispatch's tokens are unknown rather than zero, so the summary's totals and
medians are over the merged pull requests whose transcripts were found.

### Escapes

A merged pull request has **escaped** when, within the escape window after it
merged (`--escape-window`, 14 days by default), either:

- **a later merged pull request reverts it.** Any one of: `This reverts commit
  <sha>` in the body, where `<sha>` (7–40 hex digits) is a prefix of its merge
  commit (what `git revert` writes); a title of exactly `Revert "<its title>"`
  (what GitHub's revert button and `git revert` both write); or a body line
  starting `Reverts #<n>` or `Reverts <owner>/<repo>#<n>` naming this
  repository (GitHub's revert button). It counts when the revert merged.
- **an issue blames it.** The issue was opened after the merge, was closed as
  *completed* by a pull request other than this one, and its title or body
  contains, case-insensitively, `regression` or `regressed`, optionally
  `introduced` or `caused`, then `from`, `by`, `in` or `since`, then `#<n>` —
  or `caused by #<n>` / `introduced by #<n>`. So "Fixes regression from #12",
  "Regressed in #12" and "caused by #12" count; "Fixes #12" and "regression
  test for #12" do not. It counts when the issue was opened.

The issue rule is narrow on purpose: an escape rate that alarms on diligence
gets ignored, and one that misses an oddly phrased issue still trends.

gh is asked **twice per repository per run**, however many pull requests there
are — one search for merged pull requests mentioning a revert, one for closed
issues using the regression words — bounded below by the earliest merge being
judged. The search is only a prefilter; the rules above decide. A search that
fails leaves that repository's escapes unknown (`-`, null in JSON) rather than
zero, and the rate is over the pull requests that could be checked. A pull
request merged less than a window ago may still escape, so a recent rate reads
low.

The rate is printed with its 95% Wilson interval, `0/13 (0–23%)`, because a
handful of merges does not pin a rate down: no escapes in 13 is consistent with
nearly one in four. Under 5 checked merges the line says there is not enough
data to judge yet.

### Alerts: `stats --check`

```sh
$ "$worktender" stats --check
alert: ceremony #70 70-tiny-fix: 60.00 inbox messages and gate runs per 100 changed lines (6 over 10 lines), over 20
alert: proof loop #60 60-thing: 4 gate runs and reports since it said done, with no new commit on the branch, over 3
alert: escape rate 3/9 (12–65%) of merged PRs reverted or blamed for a regression within 14d: the 95% interval reaches past 5%
$ echo $?
3
```

One line per alert on stdout and exit 3 (needs a human) when any threshold is
crossed; no output and exit 0 when none is, so a cron job that mails its output
mails only when something is wrong. `--json` carries the same alerts in
`alerts`. It looks back 30 days unless `--since` says otherwise, so a long-past
dispatch does not raise the same alert every day forever.

| alert | fires when | flag (default) |
|---|---|---|
| `ceremony` | a dispatch's inbox messages + gate runs per 100 changed lines exceeds the threshold, and it had at least 5 of them | `--max-ceremony-ratio` (20) |
| `proof_loop` | a dispatch that reported `done` has had more gate runs and reports than the threshold since its last progress — the first `done`, or a later commit on its branch — with nothing new committed | `--max-proof-loop` (3) |
| `escape_rate` | the upper bound of the escape rate's 95% interval is over this fraction, with at least 5 merged pull requests that could be checked | `--max-escape-rate` (0.05) |

Every default but the escape rate's is set to stay quiet on a healthy fleet
and fire on one that plainly is not, because an alert that fires on ordinary
work is switched off within a week and then catches nothing. The escape rate is
a gate instead, and says so when it fires on a fleet that is clean so far:

- **Ceremony 20 per 100 lines** is one coordination event for every five lines
  changed. An ordinary dispatch — a gate or two, a few inbox messages — over a
  change of real size reads well under 5. The **floor of 5 events** keeps a
  one-line fix gated once (100 per 100 lines, by arithmetic alone) quiet.
- **Proof loop 3** leaves room for a coordinator gating once more for review,
  the worker re-reporting, and a second look. A fourth round with nothing new
  committed is the recursive proof loop: the gate already passed and everyone
  is re-proving it. "New commits" are read from the local branch with `git
  log`, not from gh — an unpushed commit is still the worker moving — and a
  branch that can no longer be read (deleted after merge) is never alerted on,
  because nothing can say it did not move.
- **Escape rate 0.05** is the exception: a gate, not an alarm. It fires until
  the interval's upper bound is under one in twenty, so a fleet has to show it
  is safe rather than merely look it — with no escapes at all, that takes
  73 checked merges, and until then the alert ends "no escapes seen, but not
  yet enough merges to show the rate is under 5%". Escapes are found by revert
  and blame phrasing, so the true rate is no lower than the measured one. Under the **floor of 5** it
  stays quiet, and plain `stats` says there is not enough data yet.

The watch mod does not run `--check`; wire it into cron, or a mod, yourself.

Recording is best effort: a ledger that cannot be written warns on stderr and
the command carries on, because a report refused over a metrics file loses the
report to save a statistic about it. Like the inbox, the ledger is append-only
and unbounded for now. It records no tokens or cost — that would mean reading
session-log content, a decision of its own.

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
# gate: 13-other released after 4m12s (evidence: reported)
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

## Watching a dispatch in herdr-dagr

`--dagr` keeps a [herdr-dagr](https://github.com/aemrebarut/herdr-dagr) run file
current while the gate waits, so the dagr pane (`open-dagr`) draws the fleet
live. It writes `.dagr/run.json` in the main checkout — the path dagr reads by
default — replacing it in one rename on every report, death and timeout the
gate sees. Without the flag nothing is written.

```sh
"$worktender" gate --any 12-thing,13-other,14-third --until done --timeout 20m --dagr
```

Each worker is one task, and only what the gate observed is drawn:

| The gate saw | Task | Attempt | Evidence |
| --- | --- | --- | --- |
| no report yet, or `planned` | `working` | `working`, the note as progress | — |
| `blocked` | `blocked`, unblocked by the coordinator | `working` | — |
| `done` | `done` | `done`, the pr as receipt | ◇ reported |
| the pane or workspace ended | `failed` | `lost` | ◆ verified |
| herdr's agent status went `unknown` | `blocked` | `lost` | ≈ heuristic |
| the timeout | unchanged, with a note | unchanged | — |

A `done` is never drawn ◆ verified: it is the worker's word, as it is in the
gate's own output. A timeout changes no worker, because the gate running out
of time says nothing about whether they are still working.

Rounds of `--any` share the file. A worker the current gate is not watching
keeps whatever the last gate to watch it drew, and a worker gated again after it
settled opens a second attempt rather than repainting the first. Delete the file
to start a fresh view.

The `.dagr` directory is created with a `.gitignore` of `*`, so the run file
never shows up as untracked; a `.dagr` you already had is left as it was. A
`.dagr/run.json` that worktender did not write — another producer's run — is
refused before the wait opens (exit 1) rather than overwritten. A write that
fails mid-wait is a warning; it never changes the gate's outcome or exit code.

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

**A `done` is bound to a commit.** `report` fills a `sha:` slot from `git
rev-parse HEAD` in the worker's checkout, not from a flag. Under `--require-pr`
the gate asks gh for the PR's `headRefOid` after it releases, in the repository
`--repo` names (default: the one the gate runs in), and a done whose sha is not
that head — the worker pushed after reporting — or that names no commit is
refused with exit 3, outcome `head_moved`, naming both short SHAs. Merge with
`gh pr merge <N> --match-head-commit <sha>`, so a push after the gate is refused
by GitHub too.

This catches an accidental or late push, not a hostile worker: like every other
slot, the sha is only as honest as whatever is in the pane, which can print a
well-formed envelope or set the pane's tokens itself.

The gate says as much on every outcome, as **evidence**: the release line and
every failure message end with it, and `--json` carries it as a field.

| Evidence | Outcome | What it rests on |
| --- | --- | --- |
| `reported` | `released`, `blocked` | The worker's report. The gate checked its shape and that it is new, not that it is true. |
| `verified` | `head_moved`, `timeout`, and `worker_gone` when the pane exited or closed or its workspace closed | The gate's own clock, or herdr announcing the pane gone. Nothing the worker said. |
| `inferred` | `worker_gone` when herdr's agent status went `unknown` | What an agent that has gone away looks like, which is not herdr saying it went. |

## Being woken by a report

`start` records the pane it was run from as the worker's coordinator, in the
worktree's git dir. When the worker later runs `report`, one line is typed into
that pane and submitted:

```
worktender: #316 done pr 7 sha 50b3374
```

- **Only closed slots.** The issue number, status, pr and the first seven
  characters of the sha. The note is untrusted text and is never typed.
- **Only into a free coordinator.** Its agent must be idle (or done) with an
  empty input box. A report waits up to 30 seconds for that; past it the line
  stays queued, and the next report types everything queued as one line.
- **Only the coordinator `start` saw.** `start` records the pane's terminal and
  agent as well; the wake is typed only if the live pane is still that terminal
  running `claude`, and only for the issue the branch was started for. Queued
  lines older than five minutes are dropped. Enter is pressed only when the
  input box holds exactly the wake line.
- **A hint, not an authenticated message.** Any process running as the same
  user can type into any pane through herdr, so the coordinator must confirm
  with `worktender ls` or `gate` before acting on a wake. The identity check
  protects against a plain shell, another agent, or a reused pane id receiving
  the line. It does not protect against a same-user worker forging a line for a
  Claude pane.
- **Best effort.** A worktree `start` recorded no pane for wakes nobody, and a
  wake that fails warns on stderr without failing the report.

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
