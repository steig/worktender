// Command worktender drives git worktrees as herdr workspaces.
//
// herdr runs it as a plugin: each subcommand below is registered as an action
// in herdr-plugin.toml and invoked by herdr, which supplies HERDR_SOCKET_PATH
// and the launch context in the environment.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/steig/worktender/internal/execute"
	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/jsonout"
	"github.com/steig/worktender/internal/reconcile"
	"github.com/steig/worktender/internal/repolock"
	"github.com/steig/worktender/internal/wt"
)

// commands is every subcommand run dispatches, and the source usage is built
// from. One list rather than two, so usage cannot drift from what exists.
var commands = []string{"ls", "doctor", "update", "start", "sync", "dispatch", "prune", "prune-apply", "report", "gate", "on-event", "startup"}

var usage = "usage: worktender <" + strings.Join(commands, "|") + ">"

// releaseLock releases and says so when it fails.
//
// A failed release leaves the lock file on disk, so every other reconcile of
// this repository coalesces into a pass that is not running until
// repolock.MaxHold expires. It reports rather than returns because every caller
// is a defer whose return value is already spoken for.
func releaseLock(lock *repolock.Lock, out io.Writer) {
	if err := lock.Release(); err != nil {
		fmt.Fprintf(out, "warning: %v; this repository stays locked for up to %s\n", err, repolock.MaxHold)
	}
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "worktender:", err)
		os.Exit(exitCode(err))
	}
}

// run dispatches a subcommand. Every failure returns an error so it reaches the
// process exit code: herdr records a plugin action that exits 0 as "succeeded".
func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		return usagef("%s", usage)
	}

	switch args[0] {
	case "ls", "list":
		return lsCommand(args[1:], out)
	case "doctor":
		// Read-only, takes no lock, works from outside a repository.
		return doctorCommand(args[1:], out)
	case "update":
		// Moves this plugin's own install forward; touches no repository of the
		// user's.
		return updateCommand(args[1:], out)
	case "start":
		// Issue number in, agent working on it out. Creates a worktree, so it
		// must be told which repository.
		return startCommand(args[1:], out)
	case "sync":
		// Adopt and staff. Finished worktrees are only listed.
		return syncCommand(args[1:], out)
	case "dispatch":
		// Staffs one named pane; changes no worktree, so it takes no lock.
		return dispatchCommand(args[1:], out)
	case "prune":
		// Lists finished worktrees and removes nothing.
		return pruneCommand(args[1:], out, false)
	case "prune-apply":
		// The explicit opt-in to actually removing them.
		return pruneCommand(args[1:], out, true)
	case "report":
		// A worker filling slots for its coordinator; touches neither herdr nor
		// the repository.
		return reportCommand(args[1:], out)
	case "gate":
		// A coordinator waiting on a worker; reads herdr, not the repository.
		return gateCommand(args[1:], out)
	case "on-event":
		// Invoked by herdr, never by hand. Off unless opted in.
		return onEventCommand(args[1:], out)
	case "startup":
		// Invoked once by herdr after the server is ready. Off unless opted in.
		return startupCommand(args[1:], out)
	default:
		return usagef("unknown command %q; %s", args[0], usage)
	}
}

// stateDir is where herdr lets this plugin keep state between invocations.
// Empty when we are not running under herdr, which repolock treats as "no lock
// available" and proceeds through.
func stateDir() string { return os.Getenv("HERDR_PLUGIN_STATE_DIR") }

// commandLockWait is how long a human-invoked command waits for a reconcile
// already in progress. An event hook coalesces instead, but a person asked for
// this one, so it queues.
const commandLockWait = 30 * time.Second

// reconcilePasses bounds the coalescing loop: one pass, plus one for whatever
// arrived while it ran, plus slack.
const reconcilePasses = 3

// session is what every command needs: a herdr connection, the repository being
// worked on, and the directory the user invoked from.
type session struct {
	client *herdrapi.Client
	root   string
	dir    string
}

// herdrNeed says whether a command can do its job with herdr absent. A named
// type rather than a second bool: `newSession(true, false)` at a call site says
// nothing about which switch is which, and these two are easy to swap.
type herdrNeed bool

const (
	// herdrRequired is for the commands whose whole job is herdr — opening
	// workspaces, starting agents, reading a pane.
	herdrRequired herdrNeed = true
	// herdrOptional is for the commands whose guards are entirely git and gh.
	// They run with herdr absent, reporting empty workspace, pane and agent
	// columns, because those facts do not exist rather than could not be read.
	herdrOptional herdrNeed = false
)

// dialHerdrIfPresent returns a live client, or a nil one when herdr is not
// running and the caller said it could manage without.
//
// A nil client is the signal the layers below read: reconcile.Collector sources
// the checkouts from git instead, and wt.Ls leaves the herdr columns empty. It
// is deliberately nil rather than a stub that errors, so a caller that forgets
// to handle it panics in a test rather than degrading in front of a user.
//
// It is herdrapi.Probe behind this and not herdrapi.New, and the difference is
// the whole safety argument. New reads HERDR_SOCKET_PATH, which answers whether
// herdr *started us* — false in the user's own terminal, where herdr may be
// running behind it with agents in panes. Degrading on that reading disarms the
// guard sparing a checkout an agent is standing in, and `prune-apply` then
// force-removes it. Probe dials, so absence means absence.
//
// Exit 2 restates the class an untagged error would already land in — see
// exitCode — and is written out because this site knows which class it is
// rather than inheriting the catch-all. Not a usage error: the command was
// spelled correctly and the machine could not answer it.
func dialHerdrIfPresent(need herdrNeed) (*herdrapi.Client, error) {
	client, err := herdrapi.Probe()
	switch {
	case err == nil:
		return client, nil

	// A dial that did not finish establishes nothing, and only proof of absence
	// licenses degrading — see herdrapi.ErrHerdrUnknown. Fatal even for the
	// commands that can manage without herdr, because "cannot tell" resolving
	// to "not there" is how a checkout an agent is standing in gets removed.
	case errors.Is(err, herdrapi.ErrHerdrUnknown):
		return nil, withCode(exitEnvironment, fmt.Errorf(
			"%w; refusing to assume it is not, because that would disarm the guard "+
				"protecting a checkout an agent is working in", err))

	case need == herdrRequired:
		return nil, withCode(exitEnvironment, fmt.Errorf(
			"%w; this command needs herdr running. `ls`, `prune` and `prune-apply` do not", err))
	}
	return nil, nil
}

// newSession resolves which repository to work on.
//
// allowFallback decides what happens when herdr supplied no invocation context.
// Read-only commands may fall back to the process working directory; commands
// that change things must not, because herdr runs plugin commands with cwd set
// to the plugin root — itself a git repository.
//
// A malformed context is fatal either way: it is a bug to surface, not a state
// to default around.
//
// With herdr absent there is never a context, so the fallback is the only path
// that resolves anything at all — which is why the two axes are separate: a
// command can require a named repository and still not require herdr. A
// herdr-free `prune-apply` therefore falls back even though it changes things:
// the hazard the refusal exists for is herdr's, and with no herdr there is no
// herdr to invoke us from the plugin root.
func newSession(allowFallback bool, need herdrNeed) (*session, error) {
	client, err := dialHerdrIfPresent(need)
	if err != nil {
		return nil, err
	}
	// The refusal below guards against herdr running the command with cwd set
	// to the plugin root, which is itself a git repository — so a destructive
	// command falling back to cwd would target this plugin's own checkout. That
	// is a hazard herdr creates by invoking us, and a probe that found no herdr
	// has ruled it out: the working directory is the user's shell's.
	if client == nil {
		allowFallback = true
	}

	ctx, err := herdrapi.LoadContext()
	if err != nil {
		if !errors.Is(err, herdrapi.ErrNoContext) {
			return nil, err
		}
		if !allowFallback {
			return nil, withCode(exitUsage, fmt.Errorf("%w; refusing to guess which repository to change", err))
		}
	}

	dir := ctx.LaunchDir()
	if dir == "" {
		if !allowFallback {
			return nil, usagef("herdr supplied no launch directory; refusing to guess which repository to change")
		}
		if dir, err = os.Getwd(); err != nil {
			return nil, err
		}
	}

	// herdr hands us the repository root when it already knows the workspace
	// is a worktree; otherwise ask git.
	root := ctx.RepoRoot()
	if root == "" {
		if root, err = gitx.RepoRoot(dir); err != nil {
			return nil, err
		}
	}
	return &session{client: client, root: gitx.Resolve(root), dir: gitx.Resolve(dir)}, nil
}

// newSessionIn resolves against a repository the caller named, skipping herdr's
// context entirely.
//
// The context is the right source when herdr is the one invoking — an action
// carries no arguments, so there is nothing else to go on. It is the wrong
// source when a person is: the context names herdr's current workspace, which
// on a machine with several repositories open is routinely not the one they are
// standing in or thinking about. Observed live: a dry run inside a repository
// with four staffed worktrees planned against a different project's checkout.
//
// `dir` is set to the resolved root rather than to what was passed, so a `--repo
// .` from a subdirectory behaves the same as naming the root. Nothing here falls
// back: a path that is not a repository is an error, because the whole point of
// naming one is to stop the resolution from wandering.
func newSessionIn(repo string, need herdrNeed) (*session, error) {
	client, err := dialHerdrIfPresent(need)
	if err != nil {
		return nil, err
	}

	root, err := gitx.RepoRoot(repo)
	if err != nil {
		return nil, usagef("--repo: %w", err)
	}

	resolved := gitx.Resolve(root)
	return &session{client: client, root: resolved, dir: resolved}, nil
}

// plan collects the current state and decides what the repository needs.
func (s *session) plan(releaseAgents bool) ([]reconcile.Action, error) {
	return s.planWith(reconcile.NewCollector(s.client, s.root), releaseAgents)
}

// planWith is plan against a collector the caller has adjusted, which is how
// the event path drops the PR lookup.
//
// releaseAgents is policy rather than a fact, which is why it is set here and
// not on the collector: the collector reads the world, and whether a finished
// agent may have its pane taken away is something the caller asked for.
func (s *session) planWith(collector *reconcile.Collector, releaseAgents bool) ([]reconcile.Action, error) {
	state, err := collector.Collect()
	if err != nil {
		return nil, err
	}
	state.ReleaseAgents = releaseAgents
	return reconcile.Reconcile(state), nil
}

// output is where a command's answer goes, and in which of the two shapes.
//
// The JSON is a projection of exactly the results the table renders, written
// once at the end rather than per pass: a consumer parses a single document
// from stdout, and a reconcile runs its body up to reconcilePasses times.
type output struct {
	w    io.Writer
	json bool
	// releaseAgents records that the plan being printed was made with
	// --release-agents, because the line telling the reader how to apply it
	// then has to name a command rather than a herdr action.
	releaseAgents bool
	// held is the JSON mode's accumulator across those passes. Text mode holds
	// nothing, because each pass has already printed.
	held []execute.Result
}

func newOutput(w io.Writer, asJSON bool) *output { return &output{w: w, json: asJSON} }

// notes is where a human aside goes — a lock that would not release, the hint
// pointing at prune-apply. Never stdout in JSON mode: an aside printed beside
// the document is exactly what breaks the consumer reading it.
func (o *output) notes() io.Writer {
	if o.json {
		return os.Stderr
	}
	return o.w
}

// record takes one pass's results.
func (o *output) record(results []execute.Result) {
	if o.json {
		o.held = append(o.held, results...)
		return
	}

	fmt.Fprint(o.w, execute.Render(results))
	if execute.Counts(results)[execute.StatusPlanned] > 0 {
		fmt.Fprintln(o.w, "\n"+o.applyHint())
	}
}

// applyHint is how to carry out the plan just printed. A herdr action is a
// fixed command array with no argument surface, so a plan made with a flag
// cannot be applied by one and must not point at it.
func (o *output) applyHint() string {
	if o.releaseAgents {
		return "run `worktender prune-apply --release-agents` to remove the worktrees listed above"
	}
	return "run the `Worktender: prune (apply)` action to remove the worktrees listed above"
}

// reconcileJSON is what `sync`, `prune` and `prune-apply` write for a machine.
//
// The repository is in the document because text mode prints it as a line above
// the table — a line that must not appear on a stdout being parsed, and a fact
// no consumer should have to re-derive to learn what was acted on.
//
// The shape may move before 1.0.
type reconcileJSON struct {
	Repository string               `json:"repository"`
	Results    []execute.ResultJSON `json:"results"`
}

// flush writes the JSON document, and does nothing in text mode.
func (o *output) flush(repository string) error {
	if !o.json {
		return nil
	}
	return jsonout.Write(o.w, reconcileJSON{
		Repository: repository,
		Results:    execute.JSON(o.held),
	})
}

// perform executes actions, records the report, and fails when any action did.
// It reports whether a workspace was opened, which is what tells the caller
// there is now something to staff — see reconcileOpen.
func (s *session) perform(o *output, actions []reconcile.Action, applyPrune bool) (adopted bool, err error) {
	executor := &execute.Executor{
		Client:     s.client,
		Root:       s.root,
		CallerDir:  s.dir,
		ApplyPrune: applyPrune,
	}
	results := executor.Run(actions)

	o.record(results)

	for _, r := range results {
		if r.Action.Kind == reconcile.KindAdopt && r.Status == execute.StatusDone {
			adopted = true
			break
		}
	}

	if failed := execute.Counts(results)[execute.StatusFailed]; failed > 0 {
		return adopted, fmt.Errorf("%d of %d action(s) failed", failed, len(results))
	}
	return adopted, nil
}

// reconcileOpen runs the adopt-and-staff reconcile that sync, the worktree
// event hooks and startup all want, and then staffs what it opened.
//
// The trailing pass is the point. A pass plans from a snapshot taken before it
// acts, so a workspace it adopts cannot appear in its own plan — and a
// workspace with no agent is exactly what staffing exists to fix. Without this,
// adoption leaves a bare shell until someone reconciles a second time.
//
// It performs staffing only, never adoption. Re-planning both would let a
// worktree herdr has not yet reported as open be adopted twice, which is a
// second workspace on the same checkout.
//
// newOut is called per pass because the event and startup paths write a fresh
// report each time while sync accumulates one document across passes.
func (s *session) reconcileOpen(lock *repolock.Lock, collector *reconcile.Collector, newOut func() *output) error {
	adopted := false
	err := lock.Repeat(reconcilePasses, func() error {
		actions, err := s.planWith(collector, false)
		if err != nil {
			return err
		}
		opened, err := s.perform(newOut(), reconcile.Only(actions, reconcile.KindAdopt, reconcile.KindStaff), false)
		adopted = adopted || opened
		return err
	})
	if err != nil || !adopted {
		return err
	}

	actions, err := s.planWith(collector, false)
	if err != nil {
		return err
	}
	_, err = s.perform(newOut(), reconcile.Only(actions, reconcile.KindStaff), false)
	return err
}

// jsonFlag registers --json on a flag set. One description, so the commands
// cannot advertise the switch differently.
func jsonFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("json", false, "write a machine-readable document instead of the table")
}

const lsUsage = "usage: worktender ls [--all-repos] [--blocked] [--pr] [--reports] [--json]"

func lsCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	withPR := fs.Bool("pr", false, "ask gh for each branch's pull request state")
	allRepos := fs.Bool("all-repos", false, "list every repository herdr has a worktree workspace for, not only this one")
	blocked := fs.Bool("blocked", false, "keep only the worktrees herdr reports a blocked agent in")
	reports := fs.Bool("reports", false, "ask each staffed pane what its worker last reported")
	asJSON := jsonFlag(fs)

	if err := fs.Parse(args); err != nil {
		return usagef("%v; %s", err, lsUsage)
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q; %s", fs.Arg(0), lsUsage)
	}
	opts := wt.Options{Blocked: *blocked, JSON: *asJSON}
	if *reports {
		// Unlike --pr this works across repositories: the lookup is one herdr
		// call on a pane herdr already told us about, so there is no wrong
		// repository to ask and nothing is scoped to one.
		opts.LookupReport = paneReport
	}

	if *allRepos {
		if *withPR {
			return usagef("--pr cannot be combined with --all-repos: the lookup is one `gh` call per branch in series, and it is scoped to one repository, so across several it would be slow and asking the wrong repository; run `ls --pr` in the one you care about")
		}
		// No session: the scope is herdr's open worktree workspaces, so this
		// answers from outside a repository the same way `doctor` does — and
		// for the same reason it is the one `ls` that herdr's absence leaves
		// nothing to answer. An empty listing would be a lie about the fleet.
		client, err := dialHerdrIfPresent(herdrRequired)
		if err != nil {
			return err
		}
		roots, err := openRepositories(client)
		if err != nil {
			return fmt.Errorf("list workspaces: %w", err)
		}
		return wt.LsAll(client, roots, opts, out)
	}

	// Read-only: usable from a plain shell.
	s, err := newSession(true, herdrOptional)
	if err != nil {
		return err
	}

	// Both of these are questions about agents, and with no herdr there is
	// nothing that could answer them. Refused rather than answered emptily:
	// `--blocked` would print "no blocked agents", and `--reports` a column of
	// dashes that reads as "nobody has reported" — each a claim about a fleet
	// this process cannot see. It is the degradation listRows refuses for a
	// workspace list that failed, arriving through the flags instead.
	if s.client == nil {
		switch {
		case *blocked:
			return withCode(exitEnvironment, fmt.Errorf(
				"%w; --blocked filters on herdr's agent status, and an empty listing would read as "+
					"no blocked agents rather than as no way to tell", herdrapi.ErrNoHerdr))
		case *reports:
			return withCode(exitEnvironment, fmt.Errorf(
				"%w; --reports reads each worker's last report off its pane, and herdr owns the panes",
				herdrapi.ErrNoHerdr))
		}
	}

	// Opt-in, because it is one `gh` invocation per branch and they run in
	// series. A listing people run to see where they stand has to stay fast.
	var lookupPR func(string) (string, error)
	if *withPR {
		lookupPR = func(branch string) (string, error) {
			state, err := reconcile.GhPRLookup(s.root, branch)
			return string(state), err
		}
	}
	return wt.Ls(s.client, s.root, s.dir, lookupPR, opts, out)
}

const syncUsage = "usage: worktender sync [--json]"

// syncCommand adopts unopened worktrees and staffs agentless workspaces. It
// never prunes: removals are the prune commands' job.
func syncCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := jsonFlag(fs)

	if err := fs.Parse(args); err != nil {
		return usagef("%v; %s", err, syncUsage)
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q; %s", fs.Arg(0), syncUsage)
	}

	// Opens workspaces and starts agents, so it must be told where.
	s, err := newSession(false, herdrRequired)
	if err != nil {
		return err
	}
	o := newOutput(out, *asJSON)

	// Named for the reason `prune` names it: sync resolves the repository from
	// herdr's invocation context first, so it can act somewhere other than where
	// the caller believes they are standing. The JSON carries it as a field.
	if !o.json {
		fmt.Fprintf(o.w, "repository: %s\n", s.root)
	}

	// Serialise against an event hook reconciling the same repository. The
	// executor re-checks its guards regardless, so this is about not doing the
	// work twice, not about safety.
	lock, err := repolock.AcquireWithin(stateDir(), s.root, commandLockWait)
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("another worktender reconcile has held %s for more than %s; try again", s.root, commandLockWait)
	}
	defer releaseLock(lock, o.notes())

	collector := reconcile.NewCollector(s.client, s.root)
	// No gh: PR state only ever authorises a prune, and prunes are filtered out
	// below. Every lookup is a network round trip per worktree, deciding nothing.
	collector.LookupPR = nil

	err = s.reconcileOpen(lock, collector, func() *output { return o })
	// Written even when a pass failed: text mode has already printed what the
	// earlier passes did, and the document must say the same.
	return firstError(err, o.flush(s.root))
}

// pruneName and pruneUsage keep the two halves' errors saying which half they
// came from. `prune` and `prune-apply` are one function and separate commands,
// and an error naming the wrong one sends you to the wrong place.
func pruneName(apply bool) string {
	if apply {
		return "prune-apply"
	}
	return "prune"
}

func pruneUsage(apply bool) string {
	return "usage: worktender " + pruneName(apply) + " [--repo <path>] [--release-agents] [--json]"
}

// pruneCommand reports finished worktrees, and removes them only when apply is
// set. It deliberately excludes adoptions and staffing: asking to prune must
// not open workspaces or start agents as a side effect.
func pruneCommand(args []string, out io.Writer, apply bool) error {
	fs := flag.NewFlagSet(pruneName(apply), flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "repository to act on, instead of the one herdr is currently in")
	releaseAgents := fs.Bool("release-agents", false,
		"also remove finished worktrees whose agent has stopped, closing its workspace and freeing the agent")
	asJSON := jsonFlag(fs)

	if err := fs.Parse(args); err != nil {
		return usagef("%v; %s", err, pruneUsage(apply))
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q; %s", fs.Arg(0), pruneUsage(apply))
	}

	// Named repository wins outright. Listing is otherwise read-only and may
	// fall back to the working directory; applying removes worktrees and may not.
	var s *session
	var err error
	if *repo != "" {
		s, err = newSessionIn(*repo, herdrOptional)
	} else {
		s, err = newSession(!apply, herdrOptional)
	}
	if err != nil {
		return err
	}
	o := newOutput(out, *asJSON)
	o.releaseAgents = *releaseAgents

	// Both halves name the repository they resolved, because they do not resolve
	// it the same way — listing may fall back to the working directory, applying
	// may not — and must not disagree in silence. Splitting prune from
	// prune-apply is the confirmation step, and that only holds if the second
	// acts on what the first described. The JSON carries it as a field.
	if !o.json {
		fmt.Fprintf(o.w, "repository: %s\n", s.root)
	}

	// Listing changes nothing, so it needs no claim on the repository; only the
	// half that removes worktrees serialises against a concurrent reconcile.
	if !apply {
		actions, err := s.plan(*releaseAgents)
		if err != nil {
			return err
		}
		_, err = s.perform(o, reconcile.Only(actions, reconcile.KindPrune, reconcile.KindKeep, reconcile.KindGhost), false)
		return firstError(err, o.flush(s.root))
	}

	lock, err := repolock.AcquireWithin(stateDir(), s.root, commandLockWait)
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("another worktender reconcile has held %s for more than %s; try again", s.root, commandLockWait)
	}
	defer releaseLock(lock, o.notes())

	// A single pass, not Repeat: re-running a removal because more work was
	// marked would act on a trigger someone else observed. The mark is left for
	// the next reconcile.
	actions, err := s.plan(*releaseAgents)
	if err != nil {
		return err
	}
	_, err = s.perform(o, reconcile.Only(actions, reconcile.KindPrune, reconcile.KindKeep, reconcile.KindGhost), true)
	return firstError(err, o.flush(s.root))
}

// firstError prefers the failure the command is about over the one writing it
// out. A document that could not be written is worth reporting, but not instead
// of the actions that failed.
func firstError(err, flushErr error) error {
	if err != nil {
		return err
	}
	return flushErr
}
