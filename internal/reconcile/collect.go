package reconcile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
)

// Collector gathers the facts Reconcile needs. Every field is injectable so
// tests can substitute a fake herdr, a fake gh, and a temp HOME.
type Collector struct {
	Client *herdrapi.Client
	// Root is the repository's main checkout.
	Root string
	// ProjectsDir is where Claude Code keeps conversations, normally
	// ~/.claude/projects.
	ProjectsDir string
	// LookupPR resolves a branch's pull request state. Nil disables PR
	// lookups, which makes every verdict fall back to git.
	LookupPR func(branch string) PRState
	// LookupReport reads a pane's last worktender report. Nil leaves every
	// ghost a ghost, which is the direction that closes nothing.
	LookupReport func(paneID string) (PaneReport, error)
	// LookupPRNumber resolves a pull request named by number, which is how a
	// report names one. Nil leaves every reported PR unmerged.
	LookupPRNumber func(number int) PRState
	// Warn receives notices about facts that could not be read. Nil discards
	// them; it is never the channel an answer arrives on.
	Warn io.Writer
}

// NewCollector builds a Collector with the default gh-backed PR lookup.
func NewCollector(client *herdrapi.Client, root string) *Collector {
	c := &Collector{
		Client:      client,
		Root:        root,
		ProjectsDir: DefaultProjectsDir(),
		Warn:        os.Stderr,
	}
	c.LookupPR = func(branch string) PRState { return GhPRState(root, branch) }
	return c
}

// DefaultProjectsDir is ~/.claude/projects.
func DefaultProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

// Collect reads git, herdr, gh, and the transcript directory into a State.
//
// A nil Client means herdr is not running. The checkouts then come from git and
// the state carries no workspaces, panes or agents — see collectWithoutHerdr.
func (c *Collector) Collect() (State, error) {
	if c.Client == nil {
		return c.collectWithoutHerdr()
	}

	base := gitx.BaseRef(c.Root)

	worktrees, err := c.Client.WorktreeList(c.Root)
	if err != nil {
		return State{}, err
	}
	workspaces, err := c.Client.WorkspaceList()
	if err != nil {
		return State{}, err
	}
	agents, err := c.Client.AgentList()
	if err != nil {
		return State{}, err
	}

	state := State{Root: c.Root, Base: base, AgentPanes: map[string]AgentState{}}

	for _, a := range agents.Agents {
		// Carried across verbatim: a status this build has no constant for is
		// one AgentState.Finished treats as busy, which is the safe reading.
		state.AgentPanes[a.PaneID] = AgentState(a.AgentStatus)
	}

	for _, w := range worktrees.Worktrees {
		branch := deref(w.Branch)
		wt := c.resolve(Worktree{
			Path:        w.Path,
			Branch:      branch,
			IsLinked:    w.IsLinkedWorktree,
			IsBare:      w.IsBare,
			WorkspaceID: deref(w.OpenWorkspaceID),
		}, base)
		state.Worktrees = append(state.Worktrees, wt)
	}

	for _, ws := range workspaces.Workspaces {
		// herdr reports resolved paths; c.Root may not be. Compare normalised
		// or every workspace is filtered out and sync becomes a silent no-op.
		if ws.Worktree == nil || gitx.Resolve(ws.Worktree.RepoRoot) != gitx.Resolve(c.Root) {
			continue
		}
		panes, err := c.Client.PaneList(ws.WorkspaceID)
		if err != nil {
			// A workspace herdr listed a moment ago can be closed before we ask
			// for its panes, and one that no longer exists is one there is
			// nothing to decide about. Every other failure leaves the
			// repository's state unknown, so only this code is survivable.
			var herr *herdrapi.Error
			if errors.As(err, &herr) && herr.Code == "workspace_not_found" {
				c.warnf("workspace %s went away while its panes were being read; skipping it\n", ws.WorkspaceID)
				continue
			}
			return State{}, err
		}

		ids := make([]string, 0, len(panes.Panes))
		for _, p := range panes.Panes {
			ids = append(ids, p.PaneID)
		}
		state.Workspaces = append(state.Workspaces, Workspace{
			ID:           ws.WorkspaceID,
			CheckoutPath: ws.Worktree.CheckoutPath,
			IsLinked:     ws.Worktree.IsLinkedWorktree,
			PaneIDs:      ids,
		})
	}

	c.readGhostReports(&state)
	return state, nil
}

// readGhostReports asks each pane of every ghost workspace what its worker last
// reported, and gh whether the pull request it named merged.
//
// Ghosts only: a report decides nothing about a worktree that is still there,
// and each read is a herdr call per pane and a gh call per PR. A PR named twice
// is asked about once.
func (c *Collector) readGhostReports(state *State) {
	if c.LookupReport == nil {
		return
	}
	live := worktreesByPath(*state)
	asked := map[int]PRState{}

	for _, ws := range state.Workspaces {
		if _, ok := live[pathKey(ws.CheckoutPath)]; ok {
			continue
		}
		for _, pane := range ws.PaneIDs {
			r, err := c.LookupReport(pane)
			if err != nil {
				c.warnf("could not read the report on pane %s: %v; leaving workspace %s open\n", pane, err, ws.ID)
				r = PaneReport{Unreadable: true}
			}
			if r.Found && r.Status == reportDone && r.PR != 0 && c.LookupPRNumber != nil {
				pr, seen := asked[r.PR]
				if !seen {
					pr = c.LookupPRNumber(r.PR)
					asked[r.PR] = pr
				}
				r.PRState = pr
			}
			if state.PaneReports == nil {
				state.PaneReports = map[string]PaneReport{}
			}
			state.PaneReports[pane] = r
		}
	}
}

// resolve fills in the git and gh facts every guard turns on, given a worktree
// whose identity — path, branch, linked, bare — the caller has already read off
// whichever source enumerated it.
//
// One copy on purpose. The two collectors differ only in where the enumeration
// came from, and the verdicts must not be able to drift apart: written twice, a
// guard added to the herdr path would silently not exist on the git one, and
// the difference would show up as a prune the other collector would have
// refused.
//
// Only linked worktrees are staffed or pruned, so the per-checkout git and gh
// work is skipped for the main checkout.
func (c *Collector) resolve(wt Worktree, base string) Worktree {
	if !wt.IsLinked {
		return wt
	}

	wt.Dirty = gitx.IsDirty(wt.Path)
	wt.OwnCommits = gitx.OwnCommits(wt.Path, base)
	wt.HasTranscript = c.hasTranscript(wt.Path)
	if wt.Branch != "" {
		wt.MergedIntoBase = gitx.IsMergedInto(c.Root, wt.Branch, base)
		wt.UpstreamGone = gitx.UpstreamGone(c.Root, wt.Branch)
		if c.LookupPR != nil {
			wt.PR = c.LookupPR(wt.Branch)
		}
	}
	return wt
}

// collectWithoutHerdr fills a State from git alone.
//
// The prune guards are already entirely git and gh — dirty, own commits,
// merged-into-base, upstream gone, pull request state — so the verdicts do not
// need herdr and never did. The one fact that was only ever herdr's is the
// enumeration, and gitx.Worktrees answers it.
//
// Every workspace-shaped field is left empty rather than faked, and that is
// what makes the result honest rather than merely non-empty: with no workspaces
// there is nothing to adopt into and nothing to staff, and with no AgentPanes
// the prune guard that spares a checkout an agent is standing in cannot fire.
// The last one matters — it is a guard that has been disarmed, not one that
// passed — and the only thing that makes disarming it safe is that the client
// is nil because a dial found nothing, not because nobody named a socket. See
// herdrapi.Probe.
func (c *Collector) collectWithoutHerdr() (State, error) {
	base := gitx.BaseRef(c.Root)

	checkouts, err := gitx.Worktrees(c.Root)
	if err != nil {
		return State{}, err
	}

	state := State{Root: c.Root, Base: base, AgentPanes: map[string]AgentState{}, HerdrAbsent: true}
	for _, w := range checkouts {
		state.Worktrees = append(state.Worktrees, c.resolve(Worktree{
			Path:     w.Path,
			Branch:   w.Branch,
			IsLinked: w.IsLinked,
			IsBare:   w.IsBare,
		}, base))
	}
	return state, nil
}

// warnf says what could not be read, on the channel results never travel on.
func (c *Collector) warnf(format string, a ...any) {
	if c.Warn == nil {
		return
	}
	fmt.Fprintf(c.Warn, "worktender: "+format, a...)
}

// hasTranscript reports whether Claude Code has a stored conversation for the
// checkout — a directory named for the path holding at least one *.jsonl.
func (c *Collector) hasTranscript(checkout string) bool {
	if c.ProjectsDir == "" {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(c.ProjectsDir, TranscriptSlug(checkout), "*.jsonl"))
	return err == nil && len(matches) > 0
}

// GhPRState asks gh for a branch's pull request state. A missing gh, a missing
// PR, or any error yields PRNone, which leaves the verdict to git.
func GhPRState(root, branch string) PRState {
	state, _ := GhPRLookup(root, branch)
	return state
}

// GhPRLookup is GhPRState that also says when gh could not answer, instead of
// folding that into the same PRNone a branch with no pull request gets.
//
// The reconciler wants the folded answer: an unanswerable lookup leaves the
// verdict to git, which keeps the worktree, which is the safe direction. A
// consumer reading `ls --json` wants them apart, because "gh is not logged in"
// and "this branch was never opened as a pull request" call for opposite next
// steps.
//
// The question is asked with `gh pr list --head`, not `gh pr view`, because
// that puts the distinction in gh's exit status rather than in its prose: a
// branch with no pull request is an empty array and a success, and only a gh
// that could not look fails. `gh pr view` fails for both and they can be told
// apart only by matching gh's English, which is not an interface and can be
// reworded in a minor release without anything here noticing. Measured against
// gh 2.96.0.
func GhPRLookup(root, branch string) (PRState, error) {
	args := []string{"pr", "list", "--head", branch, "--state", "all", "--json", "state,createdAt"}
	if origin := gitx.RemoteURL(root); origin != "" {
		args = append(args, "--repo", origin)
	}

	cmd := exec.Command("gh", args...)
	cmd.Dir = root

	out, err := cmd.Output()
	if err != nil {
		return PRNone, fmt.Errorf("gh pr list %s: %s", branch, ghMessage(err))
	}

	var payload []prRecord
	if err := json.Unmarshal(out, &payload); err != nil {
		return PRNone, fmt.Errorf("gh pr list %s: unreadable answer: %w", branch, err)
	}
	if len(payload) == 0 {
		return PRNone, nil
	}

	// A branch can carry more than one pull request — close one, push again,
	// open another — and an open one is the answer whichever order gh returns
	// them in. Reading the closed one would prune a worktree with live work.
	//
	// With none open the answer is the newest, which is why createdAt is asked
	// for at all: gh's list order is not documented and not guaranteed, and a
	// branch that was merged, reused, and closed unmerged the second time holds
	// commits that exist nowhere else. Answering "merged" there prunes the
	// checkout and says so confidently.
	latest := payload[0]
	for _, pr := range payload[1:] {
		if pr.supersedes(latest) {
			latest = pr
		}
	}
	state := PRState(latest.State)
	for _, pr := range payload {
		if PRState(pr.State) == PROpen {
			state = PROpen
			break
		}
	}

	switch state {
	case PRMerged, PRClosed, PROpen:
		return state, nil
	default:
		return PRNone, fmt.Errorf("gh pr list %s: unknown state %q", branch, state)
	}
}

// GhPRNumberState asks gh for the state of a pull request named by number. Any
// failure yields PRNone, which closes nothing.
func GhPRNumberState(root string, number int) PRState {
	args := []string{"pr", "view", strconv.Itoa(number), "--json", "state"}
	if origin := gitx.RemoteURL(root); origin != "" {
		args = append(args, "--repo", origin)
	}

	cmd := exec.Command("gh", args...)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		return PRNone
	}

	var payload prRecord
	if err := json.Unmarshal(out, &payload); err != nil {
		return PRNone
	}
	switch state := PRState(payload.State); state {
	case PRMerged, PRClosed, PROpen:
		return state
	default:
		return PRNone
	}
}

// prRecord is one row of `gh pr list --json state,createdAt`.
type prRecord struct {
	State     string `json:"state"`
	CreatedAt string `json:"createdAt"`
}

// supersedes says whether pr should be read as the branch's current pull
// request in preference to other. Newer wins.
//
// Without two usable timestamps — equal ones, or a gh that stops sending the
// field — the tie goes to the state that keeps the worktree, rather than back
// to the array order this exists to stop trusting. Keeping costs disk; the
// other direction costs a checkout whose work was never merged.
//
// One usable timestamp folds into that same rule rather than comparing a real
// date against a zero one, so a dated MERGED can lose to an undated CLOSED.
// That is the direction the tie-break already leans, and gh sends createdAt
// for every row of a --json call or for none of them, which makes the
// asymmetric case a guard rather than a path.
func (pr prRecord) supersedes(other prRecord) bool {
	mine, ok := pr.created()
	theirs, otherOK := other.created()
	if ok && otherOK && !mine.Equal(theirs) {
		return mine.After(theirs)
	}
	return PRState(pr.State) == PRClosed && PRState(other.State) == PRMerged
}

func (pr prRecord) created() (time.Time, bool) {
	at, err := time.Parse(time.RFC3339, pr.CreatedAt)
	return at, err == nil
}

// ghMessage is what gh said, because its exit status alone names nothing a
// reader can act on — "not logged in" and "no such repository" are both 1.
func ghMessage(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if said := strings.TrimSpace(string(exit.Stderr)); said != "" {
			return said
		}
	}
	return err.Error()
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
