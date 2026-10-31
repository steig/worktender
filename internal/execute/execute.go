// Package execute carries out the actions the reconciler decided on.
//
// reconcile.Reconcile is a pure function over a snapshot, and a snapshot goes
// stale, so every destructive step here re-reads its guards immediately before
// acting. Staffing counts as destructive: starting an agent on a pane that
// already has one lands on a live conversation.
//
// Pruning is a dry run unless ApplyPrune is set. A plugin action has no prompt
// surface, so applying is a separate, explicit action instead.
package execute

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/jsonout"
	"github.com/steig/worktender/internal/reconcile"
	"github.com/steig/worktender/internal/safetext"
)

// Status is what became of one action.
type Status string

const (
	// StatusDone means the action was carried out.
	StatusDone Status = "done"
	// StatusPlanned means a destructive action was listed but not performed,
	// because pruning is a dry run by default.
	StatusPlanned Status = "planned"
	// StatusSkipped means a guard stopped the action, or it was explanatory.
	StatusSkipped Status = "skipped"
	// StatusFailed means the action was attempted and did not succeed.
	StatusFailed Status = "failed"
)

// Result records the fate of one action. Detail is always populated: a silent
// no-op is the failure mode this package exists to avoid.
type Result struct {
	Action reconcile.Action
	Status Status
	Detail string
}

// AgentStartTimeout bounds the wait for a pane to become usable — a fresh
// worktree is often still running direnv, nix or a shell banner, and an agent
// cannot start against a busy prompt. It bounds one Run, not one action, so a
// pass over several worktrees cannot multiply it.
//
// THIS PACKAGE DOES THE WAITING. It used to be handed to herdr as timeout_ms in
// the belief that herdr would retry against a busy pane, and herdr does not:
// measured against protocol 17, agent.start on an occupied pane answers
// agent_pane_busy in 1.6-3.0ms with timeout_ms unset, 1000, 60000 and 120000
// alike. The value is passed on anyway — it is still the documented bound for
// herdr's own launch, which nothing here can observe — but the retry that makes
// a fresh worktree staffable is the loop in staff().
//
// It is exported because it is also the longest a reconcile can legitimately
// hold the repository lock. repolock.MaxHold is sized from it, and a test
// asserts the two cannot drift apart.
const AgentStartTimeout = 60 * time.Second

// agentStartTimeoutMS is the same bound in the units the wire protocol wants.
const agentStartTimeoutMS = int(AgentStartTimeout / time.Millisecond)

// agentBusyPoll is how long to leave a busy pane alone between attempts. A
// shell finishing direnv is not watched into readiness, so this only decides how
// promptly the settled pane is noticed.
const agentBusyPoll = 500 * time.Millisecond

// agentPaneBusy is herdr's code for "that pane is not an available shell". It is
// the only failure worth retrying: every other one describes something a second
// attempt cannot change.
const agentPaneBusy = "agent_pane_busy"

// AgentKind is the herdr agent to start when staffing a worktree. Exported
// because the dispatch ledger records it, and a second spelling of "claude"
// there would drift from this one the day it changes.
const AgentKind = "claude"

// Executor performs actions against a live herdr and a real repository.
type Executor struct {
	Client *herdrapi.Client
	// Root is the repository's main checkout.
	Root string
	// CallerDir is the directory the user invoked from, used to refuse
	// removing the ground they are standing on. Empty disables that check.
	CallerDir string
	// ApplyPrune turns prunes from a dry run into actual removals.
	ApplyPrune bool
	// LookupReport re-reads a pane's last report before a ghost is closed.
	// Nil leaves every ghost open, because the close rests on that report and
	// an unverifiable guard is not a satisfied one.
	LookupReport func(paneID string) (reconcile.PaneReport, error)
	// BusyRetryFor bounds how long staffing keeps re-offering a pane herdr
	// reports busy. Zero means AgentStartTimeout; tests set it short.
	BusyRetryFor time.Duration
	// StateDir holds the settings file that hands WorkerTokenEnv to an agent.
	// Empty, with the token set, fails every staffing, because the alternative
	// is an agent on your token.
	StateDir string
}

// WorkerTokenEnv names a GitHub token for agents in place of yours. Every
// agent this package starts gets it as GH_TOKEN, so `sync`, the event hooks and
// `review` give it as surely as `start` and `dispatch` do: a worker restarted
// by a reconcile after herdr restarts is still a worker.
//
// It narrows what an agent's gh does by default; it isolates nothing. The
// agent runs as you, so your stored gh login is one `env -u GH_TOKEN` away.
const WorkerTokenEnv = "WORKTENDER_WORKER_GH_TOKEN"

// WorkerSettingsFile is the settings file in StateDir for the WorkerTokenEnv
// fallback. Exported for doctor, which names one left behind after the
// variable is unset.
const WorkerSettingsFile = "worker-settings.json"

// OwnerTokenEnv names the token for repositories under one GitHub owner. A
// fine-grained token covers one owner, so a dispatcher working in two needs
// two. Set, it wins over WorkerTokenEnv for that owner.
func OwnerTokenEnv(owner string) string {
	return WorkerTokenEnv + "_" + strings.ToUpper(strings.ReplaceAll(owner, "-", "_"))
}

// OwnerSettingsFile is the settings file in StateDir for one owner's token.
// One file per owner, so agents for different owners starting together never
// read each other's token.
func OwnerSettingsFile(owner string) string {
	return "worker-settings-" + strings.ToLower(owner) + ".json"
}

// OwnerTokensSet reports whether any per-owner token variable exists, empty or
// not. From then on a checkout that picks no token is an error and not an
// agent on a looser one.
func OwnerTokensSet() bool {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, WorkerTokenEnv+"_") {
			return true
		}
	}
	return false
}

// ownerPattern is what a GitHub owner looks like. It is also what keeps a
// hostile remote URL from choosing the settings file's name.
var ownerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)

// ReadTokenPrefix starts the variables that give worktender's own gh reads a
// token per owner. It does not start with WorkerTokenEnv+"_", so
// OwnerTokensSet never sees them.
const ReadTokenPrefix = "WORKTENDER_READ_GH_TOKEN_"

// ReadTokenEnv names the read token for repositories under one GitHub owner,
// spelled the way OwnerTokenEnv spells its owner.
func ReadTokenEnv(owner string) string {
	return ReadTokenPrefix + strings.ToUpper(strings.ReplaceAll(owner, "-", "_"))
}

// ReadTokensSet reports whether any read token variable exists, empty or not.
func ReadTokensSet() bool {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, ReadTokenPrefix) {
			return true
		}
	}
	return false
}

// remoteOwner is the GitHub owner in the origin remote of the checkout at
// path. Empty when it cannot be read.
func remoteOwner(path string) string {
	return URLOwner(gitx.RemoteURL(path))
}

// RemoteOwner is remoteOwner for callers outside this package.
func RemoteOwner(path string) string { return remoteOwner(path) }

// URLOwner is the GitHub owner in a remote URL, from either an https or an ssh
// URL. Empty when it cannot be read.
func URLOwner(url string) string {
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "ssh://git@github.com/", "git@github.com:"} {
		if rest, ok := strings.CutPrefix(url, prefix); ok {
			owner, _, _ := strings.Cut(rest, "/")
			if ownerPattern.MatchString(owner) {
				return owner
			}
		}
	}
	return ""
}

// noMCPEnv keeps claude.ai connectors out of a worker. agent.start takes no
// environment, so it travels in the same --settings the token does.
const noMCPEnv = "ENABLE_CLAUDEAI_MCP_SERVERS"

// workerSettings is the settings JSON for a worker: connectors off, and GH_TOKEN
// when token is not empty.
func workerSettings(token string) []byte {
	env := map[string]string{noMCPEnv: "false"}
	if token != "" {
		env["GH_TOKEN"] = token
	}
	raw, _ := json.Marshal(map[string]any{"env": env})
	return raw
}

// workerTokenArgs are the agent arguments that give the agent its worker token
// as GH_TOKEN, or none when there is none for the checkout at path: the owner's
// variable if set, else WorkerTokenEnv.
//
// The token goes through a settings file because there is nowhere else it is
// not shown: agent.start takes no environment, an argument is visible to `ps`
// and in the pane the agent is started in, and typing an export would put it
// on screen.
//
// Unset does not remove the file. A hook running without the variable would
// delete it from under a launch that was handed it a moment earlier, and that
// agent would start on your login; doctor names a file left behind instead.
func workerTokenArgs(dir, path string) ([]string, error) {
	envName, file := WorkerTokenEnv, WorkerSettingsFile
	if OwnerTokensSet() {
		if path == "" {
			// Git would read whichever directory the caller stands in.
			return nil, errors.New("per-owner worker tokens are set but the agent has no checkout to pick one by")
		}
		owner := remoteOwner(path)
		if owner == "" {
			return nil, fmt.Errorf("per-owner worker tokens are set but %s has no readable github.com origin to pick one by", path)
		}
		name := OwnerTokenEnv(owner)
		if value, set := os.LookupEnv(name); set {
			if value == "" {
				return nil, fmt.Errorf("%s is set but empty; unset it to use %s", name, WorkerTokenEnv)
			}
			envName, file = name, OwnerSettingsFile(owner)
		} else if os.Getenv(WorkerTokenEnv) == "" {
			return nil, fmt.Errorf("per-owner worker tokens are set but neither %s nor %s is", name, WorkerTokenEnv)
		}
	}
	token := os.Getenv(envName)
	if token == "" {
		return []string{"--settings", string(workerSettings(""))}, nil
	}
	if dir == "" {
		return nil, fmt.Errorf("%s is set but there is no plugin state directory to keep it in", envName)
	}
	settings := filepath.Join(dir, file)
	raw := workerSettings(token)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("store the worker token: %w", err)
	}
	// Through a rename, so an agent starting beside this one never reads half
	// a file. CreateTemp makes it 0600. The name stays clear of the
	// worker-settings-<owner>.json shape doctor reads as an owner's file.
	tmp, err := os.CreateTemp(dir, ".worker-settings-*.tmp")
	if err != nil {
		return nil, fmt.Errorf("store the worker token: %w", err)
	}
	_, err = tmp.Write(raw)
	if cErr := tmp.Close(); err == nil {
		err = cErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), settings)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return nil, fmt.Errorf("store the worker token: %w", err)
	}
	return []string{"--settings", settings}, nil
}

// Run performs every action in order and returns one Result each.
//
// The wait for busy panes is budgeted across the whole run, not per action.
// Worktrees warm up in wall-clock time and not in turn — a shell that has been
// running direnv while the previous worktree was staffed is that much further
// along — and a per-action budget would let a pass over five of them hold the
// repository lock for five times AgentStartTimeout, which is repolock.MaxHold
// exactly. So the run gets one deadline and they share it.
func (e *Executor) Run(actions []reconcile.Action) []Result {
	busyUntil := time.Now().Add(e.busyRetryFor())

	results := make([]Result, 0, len(actions))
	for _, action := range actions {
		switch action.Kind {
		case reconcile.KindKeep, reconcile.KindGhost:
			// Explanatory only: they let the report say why a worktree was
			// spared, and name a workspace whose checkout is gone, instead of
			// silently omitting either.
			results = append(results, Result{action, StatusSkipped, action.Reason})
		case reconcile.KindAdopt:
			results = append(results, e.adopt(action))
		case reconcile.KindStaff:
			results = append(results, e.staff(action, busyUntil))
		case reconcile.KindPrune:
			results = append(results, e.prune(action))
		case reconcile.KindCloseGhost:
			results = append(results, e.closeGhost(action))
		default:
			results = append(results, Result{action, StatusFailed, "unknown action kind"})
		}
	}
	return results
}

// adopt opens a workspace for a worktree. Non-destructive, so it acts directly.
func (e *Executor) adopt(action reconcile.Action) Result {
	label := action.Branch
	if label == "" {
		label = filepath.Base(action.Path)
	}

	// Never focus: adopting a batch must not drag the user through every
	// workspace it opens.
	if err := e.Client.WorktreeOpen(e.Root, action.Path, label, false); err != nil {
		return Result{action, StatusFailed, fmt.Sprintf("open workspace: %v", err)}
	}
	return Result{action, StatusDone, "opened workspace"}
}

// staff starts an agent in a workspace that has none, re-checking first and
// waiting out a pane that is still busy.
//
// The reconciler decided this workspace was empty from a snapshot that has
// since aged, and an agent appearing in the gap is routine — an event hook and
// a human `sync` can plan from the same state moments apart. agent.start
// against an occupied pane lands on a live conversation.
//
// The repository lock makes the collision rare; this re-check is what makes it
// safe. The lock may fail to exclude, so it cannot be the thing standing here.
//
// A pane that is merely busy is a different fact from an occupied one, and the
// only one worth waiting on: a worktree seconds old is still running direnv,
// nix or a login banner, and herdr refuses to start an agent against it. Every
// attempt re-runs the guard, because a pane that stays busy for a minute is a
// minute in which someone else can staff the workspace.
func (e *Executor) staff(action reconcile.Action, busyUntil time.Time) Result {
	var args []string
	mode := "started"
	if action.Resume {
		// Continue the existing conversation rather than opening a bare one.
		args = []string{"--continue"}
		mode = "resumed"
	}
	// Caller arguments go last so they cannot displace --continue: whether to
	// resume is this executor's decision, not the caller's.
	args = append(args, action.AgentArgs...)
	tokenArgs, err := workerTokenArgs(e.StateDir, action.Path)
	if err != nil {
		return Result{action, StatusFailed, err.Error()}
	}
	// Which of two --settings claude honours for GH_TOKEN is not something to
	// find out from a worker that got the wrong one.
	if slices.ContainsFunc(action.AgentArgs, func(a string) bool {
		return a == "--settings" || strings.HasPrefix(a, "--settings=")
	}) {
		return Result{action, StatusFailed,
			"the agent already has --settings, and worktender passes its own; refusing to start it with two"}
	}
	args = append(args, tokenArgs...)

	started := time.Now()
	for attempt := 1; ; attempt++ {
		if action.WorkspaceID != "" {
			staffed, err := e.workspaceStaffed(action.WorkspaceID)
			switch {
			case err != nil:
				// An unverifiable guard is not a satisfied one.
				return Result{action, StatusSkipped,
					fmt.Sprintf("could not confirm the workspace has no agent: %v", err)}
			case staffed:
				return Result{action, StatusSkipped, "an agent started here since the plan was made"}
			}
		}

		err := e.Client.AgentStart(action.AgentName, AgentKind, action.PaneID, args, agentStartTimeoutMS)
		switch {
		case err == nil:
			return Result{action, StatusDone,
				fmt.Sprintf("%s %s as %s in %s%s", mode, AgentKind, action.AgentName, action.PaneID,
					waited(attempt, time.Since(started)))}
		case !paneBusy(err):
			return Result{action, StatusFailed, fmt.Sprintf("start agent in %s: %v", action.PaneID, err)}
		case !time.Now().Before(busyUntil):
			return Result{action, StatusFailed, fmt.Sprintf(
				"start agent in %s: %v; %s — something in that shell (direnv, nix, a banner) "+
					"has not finished", action.PaneID, err, gaveUp(attempt, time.Since(started)))}
		}
		time.Sleep(agentBusyPoll)
	}
}

// busyRetryFor is the configured retry budget, or the default.
func (e *Executor) busyRetryFor() time.Duration {
	if e.BusyRetryFor > 0 {
		return e.BusyRetryFor
	}
	return AgentStartTimeout
}

// waited is the suffix reporting a wait that actually happened, empty when the
// first attempt succeeded. A staffing that took forty seconds and one that took
// none are not the same event, and the report is where that shows.
func waited(attempts int, elapsed time.Duration) string {
	if attempts <= 1 {
		return ""
	}
	return fmt.Sprintf(" after waiting %s for the shell to settle", round(elapsed))
}

// gaveUp says how much waiting this action actually got before it stopped. On
// the first action of a run that is the whole budget; on a later one it can
// honestly be none, because the budget belongs to the run and an earlier
// worktree may already have spent it — and a reader wondering why this one gave
// up immediately should be told that rather than shown "0s".
func gaveUp(attempts int, elapsed time.Duration) string {
	if attempts <= 1 {
		return "still busy, and this pass had already spent its wait on an earlier worktree"
	}
	return fmt.Sprintf("still busy after %s and %d attempts", round(elapsed), attempts)
}

// round trims a duration to something worth reading. Seconds alone would print
// a wait of a few hundred milliseconds as "0s".
func round(d time.Duration) time.Duration { return d.Round(100 * time.Millisecond) }

// paneBusy reports whether herdr refused because the pane is not yet a usable
// shell — the one agent.start failure a later attempt can change.
func paneBusy(err error) bool {
	var herr *herdrapi.Error
	return errors.As(err, &herr) && herr.Code == agentPaneBusy
}

// prune removes a finished worktree, re-checking every guard first: uncommitted
// work and a newly started agent are exactly what appears between the plan and
// the removal, and both make it wrong.
func (e *Executor) prune(action reconcile.Action) Result {
	workspaceID, reason, blocked := e.pruneBlocked(action)
	if blocked {
		return Result{action, StatusSkipped, reason}
	}

	if !e.ApplyPrune {
		return Result{action, StatusPlanned,
			fmt.Sprintf("would remove: %s%s", action.Reason, releaseNote(action))}
	}

	if err := e.removeCheckout(action, workspaceID); err != nil {
		return Result{action, StatusFailed, err.Error()}
	}
	return Result{action, StatusDone,
		fmt.Sprintf("removed: %s%s%s", action.Reason, releaseNote(action), e.dropBranch(action.Branch))}
}

// closeGhost closes the workspace of a ghost whose worker reported its PR
// merged. Under the same dry-run switch as a prune, because it ends whatever
// conversation the workspace's panes still hold.
//
// A merged PR stays merged, but everything around it can move between the plan
// and the act: the worker can be re-briefed and report again, an agent can pick
// work up or start in another pane, and a checkout can be recreated at the
// path. Any of those makes this workspace something other than the finished
// ghost the plan saw, so each is read again here.
func (e *Executor) closeGhost(action reconcile.Action) Result {
	skip := func(why string) Result { return Result{action, StatusSkipped, why} }

	if _, err := os.Stat(action.Path); err == nil {
		return skip("a checkout reappeared at " + action.Path + " since the plan was made")
	}

	if e.LookupReport == nil {
		return skip("could not re-read the worker's report, so the close has nothing to rest on")
	}
	r, err := e.LookupReport(action.PaneID)
	switch {
	case err != nil:
		return skip(fmt.Sprintf("could not re-read the worker's report: %v", err))
	case !r.Found || r.Status != "done" || r.PR != action.PR:
		return skip(fmt.Sprintf("the worker in %s has reported again since the plan was made", action.PaneID))
	}

	agents, err := e.Client.AgentList()
	if err != nil {
		return skip(fmt.Sprintf("could not confirm the workspace is idle: %v", err))
	}
	panes, err := e.Client.PaneList(action.WorkspaceID)
	if err != nil {
		return skip(fmt.Sprintf("could not confirm the workspace is idle: %v", err))
	}
	hosting := make(map[string]reconcile.AgentState, len(agents.Agents))
	for _, a := range agents.Agents {
		hosting[a.PaneID] = reconcile.AgentState(a.AgentStatus)
	}
	for _, p := range panes.Panes {
		status, ok := hosting[p.PaneID]
		switch {
		case !ok:
			continue
		case p.PaneID != action.PaneID:
			return skip(fmt.Sprintf("an agent started in pane %s since the plan was made", p.PaneID))
		case !status.Finished():
			return skip("the agent here started working since the plan was made")
		case !action.ReleasesAgents:
			return skip("an agent started here since the plan was made")
		}
	}

	if !e.ApplyPrune {
		return Result{action, StatusPlanned,
			"would close workspace " + action.WorkspaceID + ": " + action.Reason + releaseNote(action)}
	}
	if err := e.Client.WorkspaceClose(action.WorkspaceID); err != nil {
		return Result{action, StatusFailed, fmt.Sprintf("close workspace %s: %v", action.WorkspaceID, err)}
	}
	return Result{action, StatusDone,
		"closed workspace " + action.WorkspaceID + ": " + action.Reason + releaseNote(action)}
}

// releaseNote says when a removal also takes an agent's pane away. The reason
// covers why the worktree went; this covers what else went with it.
func releaseNote(action reconcile.Action) string {
	if !action.ReleasesAgents {
		return ""
	}
	return ", releasing the finished agent holding it"
}

// pruneBlocked re-reads the guards at execution time and explains any refusal.
// The workspace it returns is the one herdr currently holds the checkout in,
// which the plan may have named wrongly or not at all.
func (e *Executor) pruneBlocked(action reconcile.Action) (workspaceID, reason string, blocked bool) {
	// Guard a, re-checked: worktree.remove is called with force, which bypasses
	// git's own refusal to delete a dirty checkout.
	if gitx.IsDirty(action.Path) {
		return "", "uncommitted changes appeared since the plan was made", true
	}

	// Guard b, re-checked: an agent may have started in the gap.
	//
	// An empty workspace id is not the same fact as "no workspace holds this
	// checkout" — a path join that missed produces exactly this, an action that
	// looks standalone and is really an agent's ground. So herdr is asked again
	// and only its answer decides.
	workspaceID = action.WorkspaceID
	if workspaceID == "" {
		holder, err := e.workspaceHolding(action.Path)
		if err != nil {
			// An unverifiable guard is not a satisfied one.
			return "", fmt.Sprintf("could not confirm no workspace holds %s: %v", action.Path, err), true
		}
		workspaceID = holder
	}
	if workspaceID != "" {
		occupied, busy, err := e.workspaceOccupant(workspaceID)
		switch {
		case err != nil:
			return "", fmt.Sprintf("could not confirm the workspace is idle: %v", err), true
		case occupied && !action.ReleasesAgents:
			return "", "an agent started here since the plan was made", true
		case occupied && busy:
			// The plan was made against an agent that had finished. It has
			// picked work up since, and --release-agents does not reach an
			// agent that is doing something.
			return "", "the agent here started working since the plan was made", true
		}
	}

	// Removing the directory the caller is standing in would leave their shell
	// on a dead cwd, and a plugin subprocess cannot `cd` on their behalf.
	if e.CallerDir != "" && isInside(e.CallerDir, action.Path) {
		return "", fmt.Sprintf("you are in %s — cd out of it first", action.Path), true
	}

	return workspaceID, "", false
}

// workspaceHolding returns the id of the workspace herdr currently has the
// checkout open in, empty when there is none. Paths are compared normalised, or
// re-asking herdr buys nothing.
func (e *Executor) workspaceHolding(checkout string) (string, error) {
	// A nil client means the dial found nothing listening, so there are no
	// workspaces and nothing holds anything.
	//
	// This is the guard that runs closest to the removal, and what makes the
	// answer true is the probe rather than the nil: "we were not told where
	// herdr is" would be the same nil and would not license it, because a plain
	// shell is exactly where herdr goes unnamed while it runs. See
	// herdrapi.Probe — nothing may set this nil without dialling first.
	if e.Client == nil {
		return "", nil
	}

	workspaces, err := e.Client.WorkspaceList()
	if err != nil {
		return "", err
	}

	wanted := gitx.Resolve(checkout)
	for _, ws := range workspaces.Workspaces {
		if ws.Worktree != nil && gitx.Resolve(ws.Worktree.CheckoutPath) == wanted {
			return ws.WorkspaceID, nil
		}
	}
	return "", nil
}

// workspaceStaffed reports whether any pane of the workspace hosts an agent,
// whatever it is doing.
func (e *Executor) workspaceStaffed(workspaceID string) (bool, error) {
	staffed, _, err := e.workspaceOccupant(workspaceID)
	return staffed, err
}

// workspaceOccupant reports whether any pane of the workspace hosts an agent
// and whether any of those is busy, reading herdr fresh rather than trusting
// the plan. Busy-ness is decided by the reconciler's own rule, so the guard
// that plans a release and the guard that performs it cannot disagree.
func (e *Executor) workspaceOccupant(workspaceID string) (occupied, busy bool, err error) {
	agents, err := e.Client.AgentList()
	if err != nil {
		return false, false, err
	}
	panes, err := e.Client.PaneList(workspaceID)
	if err != nil {
		return false, false, err
	}

	hosting := make(map[string]reconcile.AgentState, len(agents.Agents))
	for _, a := range agents.Agents {
		hosting[a.PaneID] = reconcile.AgentState(a.AgentStatus)
	}
	for _, p := range panes.Panes {
		status, ok := hosting[p.PaneID]
		if !ok {
			continue
		}
		occupied = true
		if !status.Finished() {
			busy = true
		}
	}
	return occupied, busy, nil
}

// removeCheckout deletes the worktree, preferring herdr so the workspace is
// torn down with it. A worktree herdr has no workspace for is removed with git.
//
// workspaceID comes from the guards rather than the action, because that is the
// one they verified — otherwise herdr is left holding a workspace over a
// checkout that no longer exists.
func (e *Executor) removeCheckout(action reconcile.Action, workspaceID string) error {
	if workspaceID != "" {
		if err := e.Client.WorktreeRemove(workspaceID, true); err != nil {
			return fmt.Errorf("remove worktree: %w", err)
		}
		return nil
	}

	cmd := exec.Command("git", "worktree", "remove", "--force", action.Path)
	cmd.Dir = e.Root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dropBranch deletes the local branch when git agrees it is fully merged. It
// never forces: an unmerged branch here means the work exists nowhere else.
// The returned string is a suffix for the result detail.
func (e *Executor) dropBranch(branch string) string {
	if branch == "" {
		return ""
	}

	cmd := exec.Command("git", "branch", "-d", branch)
	cmd.Dir = e.Root
	if err := cmd.Run(); err != nil {
		return fmt.Sprintf("; kept branch %s (git will not delete it — `git branch -D %s` to force)", branch, branch)
	}

	detail := fmt.Sprintf("; deleted branch %s", branch)
	// The remote counterpart outlives the local one, and deleting a remote ref
	// is not something to do silently — an open PR may still point at it.
	if e.remoteHasBranch(branch) {
		detail += fmt.Sprintf("; origin/%s still exists — `git push origin --delete %s` when the PR is closed", branch, branch)
	}
	return detail
}

func (e *Executor) remoteHasBranch(branch string) bool {
	cmd := exec.Command("git", "ls-remote", "--exit-code", "--heads", "origin", branch)
	cmd.Dir = e.Root
	return cmd.Run() == nil
}

// isInside reports whether dir is root or sits beneath it. Both sides are
// resolved first: an unresolved symlink on either compares unequal and would
// silently disarm the guard.
func isInside(dir, root string) bool {
	rel, err := filepath.Rel(gitx.Resolve(root), gitx.Resolve(dir))
	if err != nil {
		return false
	}
	return rel == "." || !strings.HasPrefix(rel, "..")
}

// Render writes a human-readable report, one aligned line per action. The
// worktree is its own column because several commonly share a reason.
//
// Target and detail are escaped: both carry branch names, git allows a bidi
// override in one, and this report is the confirmation a human reads before
// applying a prune. The detail also carries git's stderr, so escaping keeps the
// promise of one line per action.
func Render(results []Result) string {
	if len(results) == 0 {
		return "nothing to do\n"
	}

	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Status, r.Action.Kind,
			safetext.Escape(target(r.Action)), safetext.Escape(r.Detail))
	}
	_ = tw.Flush()
	return b.String()
}

// ResultJSON is Result as a consumer reads it: the action's own fields beside
// the verdict, rather than the one "target" cell a table has room for.
//
// Values are raw, not terminal-escaped, for the reason wt.RowJSON's are — this
// is data, and an escaped path is no longer a path. Detail carries git's stderr
// verbatim and so can be several lines; the table flattens it, this does not.
//
// The shape may move before 1.0.
type ResultJSON struct {
	Status string `json:"status"`
	Kind   string `json:"kind"`
	// Target is the same cell the table prints, kept so a consumer showing a
	// one-line summary does not have to re-derive it.
	Target      string  `json:"target"`
	Detail      string  `json:"detail"`
	Branch      *string `json:"branch"`
	Path        *string `json:"path"`
	WorkspaceID *string `json:"workspace_id"`
	PaneID      *string `json:"pane_id"`
	AgentName   *string `json:"agent_name"`
	// Reason is why the reconciler planned this action, which is not always
	// what became of it — Detail is that.
	Reason *string `json:"reason"`
	// ReleasesAgents is set on a prune that takes a finished agent's pane away
	// with the worktree. A consumer that tracks its own workers wants to know
	// which removals ended one.
	ReleasesAgents bool `json:"releases_agents"`
}

// JSON projects results for a machine, from exactly the []Result the table
// renders.
func JSON(results []Result) []ResultJSON {
	out := make([]ResultJSON, 0, len(results))
	for _, r := range results {
		out = append(out, ResultJSON{
			Status:         string(r.Status),
			Kind:           string(r.Action.Kind),
			Target:         target(r.Action),
			Detail:         r.Detail,
			Branch:         jsonout.String(r.Action.Branch),
			Path:           jsonout.String(r.Action.Path),
			WorkspaceID:    jsonout.String(r.Action.WorkspaceID),
			PaneID:         jsonout.String(r.Action.PaneID),
			AgentName:      jsonout.String(r.Action.AgentName),
			Reason:         jsonout.String(r.Action.Reason),
			ReleasesAgents: r.Action.ReleasesAgents,
		})
	}
	return out
}

// target names the worktree an action is about, preferring the branch.
func target(action reconcile.Action) string {
	if action.Branch != "" {
		return action.Branch
	}
	if action.Path != "" {
		return filepath.Base(action.Path)
	}
	return "-"
}

// Counts summarises results by status, for a one-line tail on the report.
func Counts(results []Result) map[Status]int {
	counts := map[Status]int{}
	for _, r := range results {
		counts[r.Status]++
	}
	return counts
}
