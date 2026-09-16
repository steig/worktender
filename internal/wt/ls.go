package wt

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/jsonout"
	"github.com/steig/worktender/internal/safetext"
)

// Options is what a listing was asked for beyond which repositories to read.
//
// A struct rather than two more parameters: both are booleans, and `nil, true,
// false, true` at a call site says nothing about which switch is which. The
// pull request lookup is deliberately not here — see LsAll.
type Options struct {
	// Blocked keeps only the worktrees herdr reports a blocked agent in.
	Blocked bool
	// LookupReport asks a pane what the worker in it last reported. Nil turns
	// the column off, the same way a nil pull request lookup does — and it is
	// off by default because it costs one herdr call per pane, the trade --pr
	// makes with gh.
	//
	// A function rather than a flag because decoding the envelope belongs to
	// the command that defines it, not to the listing that displays it.
	LookupReport func(paneID string) (Report, error)
	JSON         bool
	// Header draws the label row above the columns. Positive rather than the
	// --no-header the flag spells, so there is one polarity in this file and
	// the negation happens once, where the flag is read.
	Header bool
	// Sort orders the rows before they are written. SortNone keeps the order
	// herdr answered in, which is git's.
	Sort SortField
}

// missing is printed for a column with nothing to show.
//
// It lives in the renderer and nowhere else. Inside a Row an absent fact is the
// empty string, because one "-" stands for four different absences — no
// workspace, no pane, no agent, no pull request — and a consumer needs them
// apart. A Row that stored the dash could only hand the ambiguity on.
const missing = "-"

// Row is one worktree joined with the herdr workspace that has it open. Every
// string field is empty when the fact is absent.
type Row struct {
	// Main is the repository's primary checkout rather than a linked worktree.
	Main        bool
	Branch      string
	WorkspaceID string
	// PaneID is the workspace's first pane, which is the one staffing starts an
	// agent in and therefore the one `dispatch --pane` wants.
	PaneID      string
	AgentStatus string
	// AgentStatusSeq is herdr's counter as of the last time the agent in PaneID
	// changed state, and nil when there is no such agent or herdr gave no
	// counter for it. See WithAgentSeqs for what it is and is not.
	AgentStatusSeq *uint64
	// PR is nil unless a pull request lookup ran for this row — see WithPRs.
	PR *PR
	// Report is nil unless a report lookup ran for this row — see WithReports.
	Report *Report
	Dir    string
	// Ghost marks a row that is a herdr workspace rather than a worktree: the
	// checkout it is held open on is not in git's worktree list. It has no
	// branch, because there is no checkout left to have one.
	Ghost bool
}

// Report is what the worker in a pane last told its coordinator.
//
// It is read back off the pane's own herdr metadata, which is where `report`
// attached it and where a gate reads it. That makes it recoverable rather than
// remembered: a coordinator whose context was cleared asks the fleet what it
// last said instead of having written it down.
//
// The catch worth knowing is that metadata lives on the pane, so a released
// worker's last report is gone. That is coherent rather than broken — the
// durable record of finished work is the pull request — but it means this is
// in-flight state and not a history.
type Report struct {
	// Found is false when the pane carried no report at all, which is an
	// ordinary answer: a worker that has not reported yet.
	Found  bool
	Status string
	// PR is 0 when the worker gave none.
	PR   int
	Note string
	// Err is why the pane could not be asked, and leaves the other fields
	// meaningless. Recorded rather than discarded for the reason a failed PR
	// lookup is: unreachable and has-nothing-to-say are different facts.
	Err error
}

// PR is what a pull request lookup established about a branch.
//
// A nil *PR is not an empty one: nobody asked, versus asked and told there is
// none. Err set means gh could not be asked at all — the fact the table's "-"
// cannot carry, since an unauthenticated gh reads there as "no pull request",
// and that is what makes prune keep everything.
type PR struct {
	// State is gh's own spelling — OPEN, MERGED, CLOSED — and empty when the
	// branch has no pull request. Meaningless when Err is set.
	State string
	Err   error
}

// Rows joins herdr's worktree list against its workspace list. A worktree
// carries open_workspace_id; the agent status lives on the workspace, so
// neither call alone answers which worktrees have an agent.
func Rows(worktrees *herdrapi.WorktreeListResponse, workspaces *herdrapi.WorkspaceListResponse) []Row {
	byID := map[string]herdrapi.WorkspaceInfo{}
	if workspaces != nil {
		for _, ws := range workspaces.Workspaces {
			byID[ws.WorkspaceID] = ws
		}
	}

	rows := make([]Row, 0, len(worktrees.Worktrees))
	for _, w := range worktrees.Worktrees {
		row := Row{
			Main:        !w.IsLinkedWorktree,
			Branch:      deref(w.Branch),
			WorkspaceID: deref(w.OpenWorkspaceID),
			Dir:         filepath.Base(w.Path),
		}
		// A worktree can name a workspace herdr has since closed, so only
		// trust a status we actually found.
		if w.OpenWorkspaceID != nil {
			if ws, ok := byID[*w.OpenWorkspaceID]; ok {
				row.AgentStatus = string(ws.AgentStatus)
			}
		}
		rows = append(rows, row)
	}
	return append(rows, ghostRows(worktrees, workspaces)...)
}

// ghostRows are the workspaces herdr holds open on a checkout git no longer
// lists — the state where a listing built only from worktrees says nothing at
// all, which reads as a clean repository on exactly the question this listing
// exists to answer.
//
// Scoped by the repository the worktree list was answered for, because
// workspace.list is session-wide: without the filter every other repository's
// worktrees would arrive here as ghosts of this one.
func ghostRows(worktrees *herdrapi.WorktreeListResponse, workspaces *herdrapi.WorkspaceListResponse) []Row {
	if workspaces == nil {
		return nil
	}
	known := make(map[string]bool, len(worktrees.Worktrees))
	for _, w := range worktrees.Worktrees {
		known[gitx.Resolve(w.Path)] = true
	}
	root := gitx.Resolve(worktrees.Source.RepoRoot)

	var rows []Row
	for _, ws := range workspaces.Workspaces {
		if ws.Worktree == nil || gitx.Resolve(ws.Worktree.RepoRoot) != root {
			continue
		}
		if known[gitx.Resolve(ws.Worktree.CheckoutPath)] {
			continue
		}
		rows = append(rows, Row{
			WorkspaceID: ws.WorkspaceID,
			AgentStatus: string(ws.AgentStatus),
			Dir:         filepath.Base(ws.Worktree.CheckoutPath),
			Ghost:       true,
		})
	}
	return rows
}

// WithPanes fills the pane column, which is what a dispatch needs. A workspace
// whose panes cannot be read keeps its empty pane: a failed lookup is not the
// same fact as a workspace holding no panes.
func WithPanes(client *herdrapi.Client, rows []Row) {
	// Several worktrees can name one workspace, so ask once per workspace.
	first := map[string]string{}
	for i, row := range rows {
		if row.WorkspaceID == "" {
			continue
		}
		pane, asked := first[row.WorkspaceID]
		if !asked {
			pane = firstPane(client, row.WorkspaceID)
			first[row.WorkspaceID] = pane
		}
		rows[i].PaneID = pane
	}
}

// firstPane is the pane staffing would target, empty when herdr cannot say.
func firstPane(client *herdrapi.Client, workspaceID string) string {
	panes, err := client.PaneList(workspaceID)
	if err != nil || len(panes.Panes) == 0 {
		return ""
	}
	return panes.Panes[0].PaneID
}

// WithAgentSeqs fills the agent counter on every listing given, from one
// agent.list. It is variadic because `--all-repos` has a listing per repository
// and the counter is session-wide: asking once per repository would be N round
// trips for one answer, the same reason AllRows fetches the workspaces once.
//
// The counter is herdr's `state_change_seq`, verbatim. It is a counter rather
// than a clock because herdr has no clock to offer: measured against a live
// herdr 0.7.5 / protocol 18, not one field on a pane, a workspace or an agent
// carries a time. This is what it has instead.
//
// What it is, measured over a live session rather than read off the schema,
// which documents none of it: session-wide — nineteen agents held nineteen
// distinct values from one range — monotonic, and stamped on an agent when
// herdr sees its state change. Which is not the same set of moments as the
// status column changing, in both directions: it moved for an agent whose
// status read the same in two consecutive samples, so it catches the
// working→idle→working that column hides, and it stayed put across a done→idle
// transition, which is a worker that has done nothing being relabelled. The
// pane's own `revision` is not this signal — it sat still through every status
// change in the same window.
//
// What it is not is elapsed time. Nothing here converts it to seconds, because
// nothing here can: the rate depends on how busy the rest of the session is.
// Two listings and the caller's own clock are what turn it into a duration, and
// the caller is the one holding both.
//
// And it is a counter of state *changes*, which leaves it blind in one
// direction: an agent that stays in one state does not move it, and an agent
// thinking is exactly that. A frozen counter on an `idle` or `done` row is the
// case this field was built for — finished, or wedged. A frozen counter on a
// `working` row says nothing at all: long turn and wedged look identical, and
// nothing else in the listing separates them. Measured against a live herdr
// 0.7.5 / protocol 18, sampling one continuously working agent, the counter
// held still for the whole window and so did every other number herdr has for
// that pane — the agent's `revision`, the pane's, `pane.get`'s `scroll`, and
// the `revision` on `pane.read`. See docs/json.md for the full measurement and
// the composite that does work.
//
// Keyed by pane, so this is the agent in the row's PaneID — the pane staffing
// starts an agent in and `dispatch --pane` targets. An agent elsewhere in the
// workspace leaves the counter nil rather than lending the row its own.
//
// A failed lookup leaves every counter nil, exactly as a session with no agents
// would. That is the same trade WithPanes makes: this is one column, and losing
// it must not cost the listing the rows it decorates.
func WithAgentSeqs(client *herdrapi.Client, listings ...[]Row) {
	if !anyPane(listings) {
		return
	}

	agents, err := client.AgentList()
	if err != nil {
		return
	}
	seqs := map[string]*uint64{}
	for _, agent := range agents.Agents {
		if agent.StateChangeSeq != nil {
			seqs[agent.PaneID] = agent.StateChangeSeq
		}
	}

	for _, rows := range listings {
		for i, row := range rows {
			if row.PaneID != "" {
				rows[i].AgentStatusSeq = seqs[row.PaneID]
			}
		}
	}
}

// anyPane reports whether any listing has a row an agent could be in. Nothing
// to decorate means nothing to ask about, and a round trip is not free.
func anyPane(listings [][]Row) bool {
	for _, rows := range listings {
		for _, row := range rows {
			if row.PaneID != "" {
				return true
			}
		}
	}
	return false
}

// WithPRs fills the pull request column from the supplied lookup. The main
// checkout is skipped: trunk has no pull request, and every lookup is a network
// round trip — which is why its PR stays nil rather than becoming an answer.
//
// A lookup that failed is recorded as a failure rather than discarded. The
// table still prints "-" for it, having one column and no room to say more, but
// the JSON says which of the two it was.
func WithPRs(rows []Row, lookup func(branch string) (string, error)) {
	for i, row := range rows {
		if row.Main || row.Branch == "" {
			continue
		}
		state, err := lookup(row.Branch)
		rows[i].PR = &PR{State: state, Err: err}
	}
}

// WithReports fills the report column by asking each staffed pane what its
// worker last reported.
//
// Only rows with a pane are asked: a report is attached to the pane its author
// occupies, so a worktree herdr has no pane for cannot have one. The main
// checkout is asked like any other — a coordinator may well be dispatching from
// somewhere else and have a worker in it.
func WithReports(rows []Row, lookup func(paneID string) (Report, error)) {
	for i, row := range rows {
		if row.PaneID == "" {
			continue
		}
		r, err := lookup(row.PaneID)
		r.Err = err
		rows[i].Report = &r
	}
}

// OnlyBlocked keeps the worktrees herdr reports a blocked agent in.
//
// This is herdr's own agent status for the workspace, not worktender's `report
// --status blocked` envelope. The two are different signals and nothing here
// folds one into the other: herdr's says a session has stopped and only a
// person can restart it, while the envelope is a worker telling whichever
// coordinator gated on it why it gave up — and reaches nobody else.
//
// It is the status worth a filter of its own because it is the one that stays
// put. Working resolves itself, idle is finished or waiting to be told what to
// do, and blocked sits there until somebody looks.
func OnlyBlocked(rows []Row) []Row {
	kept := make([]Row, 0, len(rows))
	for _, row := range rows {
		if row.AgentStatus == string(herdrapi.AgentStatusBlocked) {
			kept = append(kept, row)
		}
	}
	return kept
}

// SortField is the column a listing is ordered by.
//
// A named type rather than a string so the parse happens once, at the flag, and
// every caller downstream holds a value that is already known to be a column
// this table has.
type SortField string

const (
	// SortNone keeps the order herdr answered in, which is git's worktree order
	// with the ghosts appended. It is the default because it is stable across
	// runs in a way no content-derived order is: a listing people watch refresh
	// should not reshuffle because one agent changed state.
	SortNone SortField = ""
	// SortBranch is lexicographic by branch name.
	SortBranch SortField = "branch"
	// SortStatus is by how much the row wants a human — see statusRank.
	SortStatus SortField = "status"
	// SortSeq is by herdr's state counter, lowest first, which puts the worker
	// it last saw move longest ago at the top. See seqRank for why that end.
	SortSeq SortField = "seq"
)

// SortFields is every accepted --sort value, in the order the usage text lists
// them. Shared so the flag's error cannot name a field Sort does not implement.
var SortFields = []SortField{SortBranch, SortStatus, SortSeq}

// ParseSort resolves a --sort value, and reports the ones it would have taken
// when it cannot.
func ParseSort(s string) (SortField, error) {
	for _, field := range SortFields {
		if s == string(field) {
			return field, nil
		}
	}
	names := make([]string, 0, len(SortFields))
	for _, field := range SortFields {
		names = append(names, string(field))
	}
	return SortNone, fmt.Errorf("unknown sort field %q; want one of: %s", s, strings.Join(names, ", "))
}

// Sort orders rows in place.
//
// Stable, so rows that tie keep git's order rather than an arbitrary one that
// moves between runs — which matters here more than it usually does, because
// this listing is watched on a refresh loop and a row that jumps for no reason
// costs the reader the scan they were in the middle of.
//
// It runs before both the table and the JSON, so the two cannot disagree about
// what order the answer was in. A consumer that wants its own order sorts the
// document itself; one that piped `--sort` in asked for this one.
func Sort(rows []Row, field SortField) {
	var less func(a, b Row) bool
	switch field {
	case SortBranch:
		// Empty last rather than first: a ghost and a detached head have no
		// branch, and they are not what someone sorting by branch is looking
		// for.
		less = func(a, b Row) bool {
			if (a.Branch == "") != (b.Branch == "") {
				return b.Branch == ""
			}
			return a.Branch < b.Branch
		}
	case SortStatus:
		less = func(a, b Row) bool { return statusRank(a.AgentStatus) < statusRank(b.AgentStatus) }
	case SortSeq:
		less = func(a, b Row) bool { return seqRank(a.AgentStatusSeq) < seqRank(b.AgentStatusSeq) }
	default:
		return
	}
	sort.SliceStable(rows, func(i, j int) bool { return less(rows[i], rows[j]) })
}

// statusRank orders the agent statuses by how much the row wants a person,
// which is the question this listing gets read for — not alphabetically, which
// only happens to put `blocked` first and would stop doing so the moment herdr
// names a new status.
//
// The reading is the README's own: blocked sits there until somebody looks,
// idle is finished-or-wedged and is therefore the coordinator's next move, and
// working resolves itself. A status herdr has that this list does not sorts
// after all of them but ahead of the rows with no agent at all, which are not
// waiting on anyone.
func statusRank(status string) int {
	switch herdrapi.AgentStatus(status) {
	case herdrapi.AgentStatusBlocked:
		return 0
	case herdrapi.AgentStatusIdle:
		return 1
	case herdrapi.AgentStatusDone:
		return 2
	case herdrapi.AgentStatusWorking:
		return 3
	case "":
		return 5
	default:
		return 4
	}
}

// seqRank is the sort key for the counter column: the counter itself, and the
// maximum for a row that has none.
//
// Lowest first, because the counter is only ever read as "who stopped moving
// first" — a high one is a worker herdr saw a moment ago, which is the row
// nobody needs to look at. A row with no counter has no agent to be stale, so
// it goes to the bottom with the rest of the unstaffed ones.
func seqRank(seq *uint64) uint64 {
	if seq == nil {
		return ^uint64(0)
	}
	return *seq
}

// Render writes the rows as an aligned table.
//
// The pull request column is omitted rather than dashed when it was not asked
// for: a "-" is the same cell a branch with no pull request prints, so the
// listing would read as a repository with no open work.
//
// Every cell is escaped. git accepts bidi overrides in a ref name, so a cell
// can otherwise draw as a branch it is not, and this listing is what a human
// reads before pruning. Per cell rather than per line, because the separators
// are themselves control characters.
func Render(w io.Writer, rows []Row, cols Columns) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	renderRows(tw, rows, cols, "")
	return tw.Flush()
}

// Columns is which optional columns the table draws. Both cost a lookup per
// row, so neither happens unless asked for — and both are omitted rather than
// dashed when they were not, because a "-" is the same cell "nothing to report"
// prints, and a listing full of those reads as a repository with no work in it.
type Columns struct {
	PR      bool
	Reports bool
	// Header draws a label row above the rows. Not a column, but the same
	// decision in the same place: what this table puts on the page.
	Header bool
}

// renderRows writes the row lines into an open tabwriter, each marker cell
// prefixed with indent. Shared so the grouped listing draws the same columns as
// the flat one rather than a second table that resembles it.
func renderRows(w io.Writer, rows []Row, cols Columns, indent string) {
	if cols.Header {
		renderHeader(w, cols, indent)
	}
	for _, row := range rows {
		marker := indent + " "
		switch {
		case row.Main:
			marker = indent + "*"
		case row.Ghost:
			// The marker column rather than a seventh one: this row is rare, the
			// table is already the widest thing this tool prints, and a column
			// that is blank on every ordinary listing earns nothing.
			marker = indent + "?"
		}
		cells := []string{marker, cell(row.Branch), cell(row.WorkspaceID),
			cell(row.PaneID), cell(row.AgentStatus), cell(seqCell(row.AgentStatusSeq))}
		if cols.Reports {
			cells = append(cells, cell(reportCell(row.Report)))
		}
		if cols.PR {
			cells = append(cells, cell(prCell(row.PR)))
		}
		cells = append(cells, cell(dirCell(row)))
		fmt.Fprintln(w, strings.Join(cells, "\t"))
	}
}

// renderHeader writes the label row, in the same cell order renderRows uses so
// the two cannot drift apart.
//
// The marker column gets a blank label rather than one of its own: the cell
// holds `*`, `?` or nothing, and no word covers all three — the README does,
// and a header is not where that explanation fits.
//
// Upper case because there is no rule above the labels to separate them from
// the rows, and every value in this table is lower case except a pull request
// state. The case is the separator.
func renderHeader(w io.Writer, cols Columns, indent string) {
	cells := []string{indent + " ", "BRANCH", "WORKSPACE", "PANE", "STATUS", "SEQ"}
	if cols.Reports {
		cells = append(cells, "REPORT")
	}
	if cols.PR {
		cells = append(cells, "PR")
	}
	cells = append(cells, "DIR")
	fmt.Fprintln(w, strings.Join(cells, "\t"))
}

// RenderRepos writes a cross-repository listing: a line naming the repository,
// then its rows indented beneath it. Grouped rather than given a repository
// column because a repository that could not be read has no rows to hang the
// name off, and that is the row the reader most needs.
//
// The full root rather than its basename: two checkouts on one machine
// routinely share a basename — `web`, `api` — and this listing exists to be
// read across all of them at once.
//
// A repository whose rows were all filtered away is skipped rather than drawn
// as a bare heading. `--blocked` across six repositories is a question with a
// short answer, and five empty headings bury it. A repository that could not be
// read is never skipped.
func RenderRepos(w io.Writer, repos []Repo, opts Options) error {
	if len(repos) == 0 {
		fmt.Fprintln(w, "no repositories: herdr has no worktree workspaces open")
		return nil
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	printed := 0
	for _, repo := range repos {
		if opts.Blocked && repo.Err == "" && len(repo.Rows) == 0 {
			continue
		}
		printed++
		fmt.Fprintln(tw, safetext.Escape(repo.Root))
		if repo.Err != "" {
			fmt.Fprintf(tw, "  cannot be read: %s\n", safetext.Escape(repo.Err))
			continue
		}
		// The header repeats per repository rather than once at the top, and
		// that is tabwriter's rule rather than a choice: the repository line
		// carries no tab, which ends the column block, so a header above it
		// would be aligned against nothing and sit at the wrong offsets.
		renderRows(tw, repo.Rows, Columns{Reports: opts.LookupReport != nil, Header: opts.Header}, "  ")
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	// Silence is not an answer here: the same empty output is what a listing
	// that failed to reach anything would print.
	if printed == 0 {
		noun := "repositories"
		if len(repos) == 1 {
			noun = "repository"
		}
		fmt.Fprintf(w, "no blocked agents in %d %s\n", len(repos), noun)
	}
	return nil
}

// dirCell is the directory column, empty when the directory is named after the
// branch — which is every worktree `start` made, since it derives one from the
// other. Printing both put the widest name in the table twice and pushed the
// default listing past 120 columns before any optional column was asked for.
//
// The column earns its place on the rows where it disagrees: a checkout herdr
// adopted rather than created sits in a directory with no relation to its
// branch, and that is the row a reader most needs the path for.
//
// Only the table collapses. `--json` carries both fields populated always, so
// nothing reading this listing as data has to reassemble the name.
func dirCell(row Row) string {
	if row.Dir == row.Branch {
		return ""
	}
	return row.Dir
}

// cell is one column's text: escaped, and a dash when there is nothing to show.
func cell(s string) string {
	if s == "" {
		return missing
	}
	return safetext.Escape(s)
}

// seqCell is the agent counter column, empty when there is no counter.
//
// The number is printed raw, and it is meant to be read down the column rather
// than across one row: on its own it says nothing, while beside its neighbours
// it says which worker herdr last saw move and which one it stopped seeing
// first. That is the reading the status column cannot give, and it is as far as
// a listing can go without a clock to subtract from.
func seqCell(seq *uint64) string {
	if seq == nil {
		return ""
	}
	return strconv.FormatUint(*seq, 10)
}

// reportCell is the report column: what the worker in this pane last said,
// compressed to fit beside five other columns.
//
// The note is deliberately not here. It runs to 200 characters, it is untrusted
// text a stranger may have written, and a table is the wrong frame for it — the
// JSON carries it where a consumer can decide what to do with it. What fits in a
// column is the pair a coordinator branches on anyway.
func reportCell(r *Report) string {
	switch {
	case r == nil, r.Err != nil, !r.Found:
		// Unreachable and nothing-to-say are different facts and the table has
		// room for neither, the same trade the pull request column makes. The
		// JSON says which.
		return ""
	case r.PR > 0:
		return r.Status + " #" + strconv.Itoa(r.PR)
	default:
		return r.Status
	}
}

// prCell is the pull request column, empty for every case the table has no room
// to distinguish — not asked, asked and told none, and gh unable to answer.
func prCell(pr *PR) string {
	if pr == nil || pr.Err != nil {
		return ""
	}
	return pr.State
}

// Repo is one repository's rows, or why they could not be read.
//
// The failure travels beside the rows rather than aborting the walk: one
// unreadable repository must not cost the others their place in a listing whose
// whole purpose is covering all of them.
type Repo struct {
	Root string
	// Err is why this repository could not be read, empty when it could. Rows is
	// nil rather than empty when it is set — "no worktrees" and "none readable"
	// are different facts.
	Err  string
	Rows []Row
}

// AllRows reads the supplied repositories, in the order given.
//
// The workspace list is fetched once here and joined against every
// repository's worktrees: the agent status lives on the workspace, and asking
// herdr for the same global list once per repository would be N round trips for
// one answer. Once here, not once per run — whoever discovered the roots has
// already read the same list to find them.
//
// Which repositories to read is the caller's to decide. Discovery already
// exists — herdr's open worktree workspaces — and a second one here would be a
// second answer to which repositories this plugin manages.
func AllRows(client *herdrapi.Client, roots []string) ([]Repo, error) {
	if len(roots) == 0 {
		return nil, nil
	}

	workspaces, err := client.WorkspaceList()
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}

	repos := make([]Repo, 0, len(roots))
	for _, root := range roots {
		repo := Repo{Root: root}
		if worktrees, err := client.WorktreeList(root); err != nil {
			repo.Err = err.Error()
		} else {
			repo.Rows = Rows(worktrees, workspaces)
		}
		repos = append(repos, repo)
	}
	return repos, nil
}

// ListJSON is what `ls --json` writes. An object rather than a bare array, so a
// later field has somewhere to go that does not change the type of the document.
type ListJSON struct {
	// Worktrees is the listing for a single repository, and null when the answer
	// was grouped by repository instead.
	Worktrees []RowJSON `json:"worktrees"`
	// Repositories is the `--all-repos` listing, and null when a single
	// repository was listed. Exactly one of the two fields is ever non-null, so
	// a consumer can tell which question was asked from the document alone.
	//
	// Grouped, rather than a repository field on every row — which was the other
	// candidate and the cheaper one. Three things decided it:
	//
	// A per-repository failure needs somewhere to live. `--all-repos` keeps
	// going when one repository cannot be read, and a flat array of worktrees
	// has nowhere to record that except by inventing a row that is not a
	// worktree.
	//
	// A repository whose rows are all filtered out — `--blocked` is the usual
	// reason — has to survive as an empty group. "Asked, and none" versus "not
	// asked" is the distinction this format exists to keep, and a flat array
	// erases it by simply having fewer rows.
	//
	// And `doctor --json` already reports per repository, so the two
	// cross-repository views nest the same way rather than each inventing one.
	Repositories []RepoJSON `json:"repositories"`
}

// RepoJSON is one repository in a cross-repository listing.
type RepoJSON struct {
	// Root is the repository path; Name is the basename the table would show,
	// carried so a consumer does not have to re-derive the label.
	Root string `json:"root"`
	Name string `json:"name"`
	// Error is why this repository could not be read. Worktrees is null when it
	// is set, empty when the repository was read and nothing matched.
	Error     *string   `json:"error"`
	Worktrees []RowJSON `json:"worktrees"`
}

// RowJSON is Row as a consumer reads it. Absence is null rather than "-",
// because the dash means four different things and JSON has a word for none of
// them.
//
// Values are raw, not terminal-escaped: this is data. A branch name with the
// escaping applied is no longer a branch name that can be handed back to git,
// and rendering a string a stranger chose is the consumer's job — the same job
// Render does for the table.
//
// The shape may move before 1.0.
type RowJSON struct {
	Main        bool    `json:"main"`
	Branch      *string `json:"branch"`
	WorkspaceID *string `json:"workspace_id"`
	PaneID      *string `json:"pane_id"`
	AgentStatus *string `json:"agent_status"`
	// AgentStatusSeq is herdr's state_change_seq for the agent in PaneID: the
	// value of a session-wide counter as of that agent's last state change, and
	// the only thing herdr has that moves when a worker does — it has no clock
	// to expose. Null when there is no agent in that pane.
	//
	// A number, not a duration, and deliberately not converted into one. Two
	// readings of it, taken by a caller that has a clock, are what say whether a
	// worker moved in between; how long a run of no movement has to be before it
	// counts as stalled is that caller's call and nobody else's.
	//
	// Two readings that are equal mean the agent did not change state, which is
	// not the same fact as it having done nothing. Beside `agent_status` of
	// `working` that is a long turn or a wedge and this field cannot say which
	// — the limit is in docs/json.md, and a stall detector built on this field
	// alone fires on healthy workers.
	AgentStatusSeq *uint64 `json:"agent_status_seq"`
	// PR is null when no lookup ran for this row: --pr was not passed, or this
	// is the main checkout, which is never asked about.
	PR *PRJSON `json:"pr"`
	// Report is null when no lookup ran: --reports was not passed, or this
	// worktree has no pane for a report to be attached to.
	Report *ReportJSON `json:"report"`
	Dir    string      `json:"dir"`
	// Ghost is true on a row that is a herdr workspace rather than a worktree:
	// herdr holds it open on a checkout git's worktree list does not have.
	// `branch` is null on such a row because there is no checkout left to have
	// one, and `dir` is the basename of the path herdr still believes in.
	//
	// It is diagnosis, never a removal: closing the workspace is a different
	// authority from removing a worktree, and this plugin does not take it.
	Ghost bool `json:"ghost"`
}

// ReportJSON is what the worker in this pane last told its coordinator.
//
// This is the field that makes a cleared coordinator's fleet recoverable rather
// than remembered — but it is read off the pane's metadata, so it is in-flight
// state. A released worker's last report is gone with its pane, and the durable
// record of finished work is the pull request it names.
type ReportJSON struct {
	// Found is false when the pane carried no report. An ordinary answer: a
	// worker that has not reported yet. Distinguished from Error, which is the
	// pane not having been readable at all — the table has room for neither.
	Found  bool    `json:"found"`
	Status *string `json:"status"`
	PR     *int    `json:"pr"`
	// Note is the worker's own 200 characters. It is UNTRUSTED text: a worker's
	// task usually arrived as a GitHub issue whose body anyone could have
	// written. Render it as data and branch on Status, never on this.
	Note  *string `json:"note"`
	Error *string `json:"error"`
}

func reportJSONFor(r *Report) *ReportJSON {
	if r == nil {
		return nil
	}
	out := &ReportJSON{Found: r.Found}
	if r.Err != nil {
		msg := r.Err.Error()
		out.Error = &msg
		return out
	}
	if !r.Found {
		return out
	}
	out.Status = jsonout.String(r.Status)
	out.Note = jsonout.String(r.Note)
	if r.PR > 0 {
		pr := r.PR
		out.PR = &pr
	}
	return out
}

// PRJSON is a pull request lookup's answer.
type PRJSON struct {
	// State is null when the branch has no pull request, and meaningless when
	// Error is set.
	State *string `json:"state"`
	// Error is why gh could not be asked. This is the distinction the table
	// loses: a gh that is missing or unauthenticated reads there as "no pull
	// request", and that is the reading that makes prune keep everything.
	Error *string `json:"error"`
}

// JSON projects rows for a machine.
//
// It is a view of exactly the []Row the table renders, built after the same
// lookups, so the two cannot disagree about what herdr said.
func JSON(rows []Row) []RowJSON {
	out := make([]RowJSON, 0, len(rows))
	for _, row := range rows {
		out = append(out, RowJSON{
			Main:           row.Main,
			Branch:         jsonout.String(row.Branch),
			WorkspaceID:    jsonout.String(row.WorkspaceID),
			PaneID:         jsonout.String(row.PaneID),
			AgentStatus:    jsonout.String(row.AgentStatus),
			AgentStatusSeq: row.AgentStatusSeq,
			Report:         reportJSONFor(row.Report),
			PR:             prJSON(row.PR),
			Dir:            row.Dir,
			Ghost:          row.Ghost,
		})
	}
	return out
}

// ReposJSON projects a cross-repository listing for a machine.
func ReposJSON(repos []Repo) []RepoJSON {
	out := make([]RepoJSON, 0, len(repos))
	for _, repo := range repos {
		entry := RepoJSON{Root: repo.Root, Name: filepath.Base(repo.Root), Error: jsonout.String(repo.Err)}
		if repo.Err == "" {
			entry.Worktrees = JSON(repo.Rows)
		}
		out = append(out, entry)
	}
	return out
}

func prJSON(pr *PR) *PRJSON {
	if pr == nil {
		return nil
	}
	if pr.Err != nil {
		return &PRJSON{Error: jsonout.String(pr.Err.Error())}
	}
	return &PRJSON{State: jsonout.String(pr.State)}
}

// listRows is the repository's rows from whichever source can answer.
//
// A nil client means herdr is not running, so the checkouts come from git and
// every herdr-shaped column stays empty. That is not the degradation the
// workspace-list failure below refuses: an absent herdr has no workspaces,
// where a herdr that failed to answer has some and will not say which, and
// only the second reads as a session with nothing open.
func listRows(client *herdrapi.Client, root string) ([]Row, error) {
	if client == nil {
		checkouts, err := gitx.Worktrees(root)
		if err != nil {
			return nil, err
		}
		rows := make([]Row, 0, len(checkouts))
		for _, w := range checkouts {
			rows = append(rows, Row{
				Main:   !w.IsLinked,
				Branch: w.Branch,
				Dir:    filepath.Base(w.Path),
			})
		}
		return rows, nil
	}

	worktrees, err := client.WorktreeList(root)
	if err != nil {
		return nil, fmt.Errorf("list worktrees: %w", err)
	}

	// Not degraded to an empty status column: that is the same row a worktree
	// with no workspace prints, so a herdr that failed to answer would read as
	// a session with nothing open.
	workspaces, err := client.WorkspaceList()
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	return Rows(worktrees, workspaces), nil
}

// Ls lists every worktree of the repository containing dir, with the workspace,
// pane and agent state herdr has for each.
//
// root, when non-empty, is a repository root herdr already resolved from the
// invocation context; it saves a git call and works even from a directory git
// would refuse.
//
// lookupPR is nil unless the caller asked for pull request state, which costs a
// round trip per branch and is therefore never the default.
func Ls(client *herdrapi.Client, root, dir string, lookupPR func(branch string) (string, error), opts Options, out io.Writer) error {
	if root == "" {
		var err error
		if root, err = gitx.RepoRoot(dir); err != nil {
			return err
		}
	}

	rows, err := listRows(client, root)
	if err != nil {
		return err
	}
	// Filtered before the lookups, not after: a pane costs a round trip, and a
	// row that will not be printed is not worth one.
	if opts.Blocked {
		rows = OnlyBlocked(rows)
	}
	// Every one of these is a herdr call and there is no herdr to make it to.
	// Skipped rather than attempted-and-tolerated: a nil client would panic,
	// and the columns are already empty because the facts do not exist.
	if client != nil {
		WithPanes(client, rows)
		WithAgentSeqs(client, rows)
	}
	if opts.LookupReport != nil {
		WithReports(rows, opts.LookupReport)
	}
	if lookupPR != nil {
		WithPRs(rows, lookupPR)
	}
	Sort(rows, opts.Sort)

	if opts.JSON {
		return jsonout.Write(out, ListJSON{Worktrees: JSON(rows)})
	}
	// An empty table is the same output as a listing that reached nothing, so a
	// filter that matched nothing says so.
	if opts.Blocked && len(rows) == 0 {
		fmt.Fprintln(out, "no blocked agents in "+safetext.Escape(root))
		return nil
	}
	return Render(out, rows, Columns{PR: lookupPR != nil, Reports: opts.LookupReport != nil, Header: opts.Header})
}

// LsAll lists every worktree of every supplied repository, grouped by
// repository. This is the view for someone with agents running in six
// checkouts, for whom the per-repository listing means six invocations and six
// directories to remember to visit.
//
// There is no pull request lookup here, and that is deliberate rather than
// pending. The lookup runs one `gh` call per branch in series, so across six
// repositories it is a listing nobody would wait for — and it is scoped to a
// single repository, so pointing it at another repository's branches would
// quietly ask the wrong GitHub repository and answer confidently. Pairing them
// needs concurrency and a per-repository lookup first.
func LsAll(client *herdrapi.Client, roots []string, opts Options, out io.Writer) error {
	repos, err := AllRows(client, roots)
	if err != nil {
		return err
	}

	listings := make([][]Row, 0, len(repos))
	for i := range repos {
		if opts.Blocked {
			repos[i].Rows = OnlyBlocked(repos[i].Rows)
		}
		WithPanes(client, repos[i].Rows)
		listings = append(listings, repos[i].Rows)
	}
	WithAgentSeqs(client, listings...)
	// Within each repository rather than across them: the listing is grouped,
	// so there is no single sequence to put in order, and the repositories keep
	// the order the caller asked for them in.
	//
	// After the counter lookup, not before — SortSeq reads a column WithAgentSeqs
	// is what fills.
	for i := range repos {
		Sort(repos[i].Rows, opts.Sort)
	}

	if opts.JSON {
		return jsonout.Write(out, ListJSON{Repositories: ReposJSON(repos)})
	}
	return RenderRepos(out, repos, opts)
}

func deref(s *string) string {
	if s == nil || strings.TrimSpace(*s) == "" {
		return ""
	}
	return *s
}
