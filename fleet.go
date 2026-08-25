package main

import (
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/steig/worktender/internal/fleet"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/jsonout"
	"github.com/steig/worktender/internal/reconcile"
	"github.com/steig/worktender/internal/wt"
)

const fleetUsage = "usage: worktender fleet board [--watch|--json]"

// fleetCommand dispatches the fleet subcommands. There is one today; a
// subcommand rather than `fleet-board` because the cockpit grows — a digest,
// a stale-loops listing — and each of those is a view of the same ledger.
func fleetCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return usagef("fleet needs a subcommand; %s", fleetUsage)
	}
	switch args[0] {
	case "board":
		return fleetBoardCommand(args[1:], out)
	default:
		return usagef("unknown fleet subcommand %q; %s", args[0], fleetUsage)
	}
}

// fleetBoardCommand renders the fleet board: the ledger's open loops merged
// with the live `ls --all-repos --reports` view, escalations on top.
//
// Read-only plus navigation, by charter: watch mode can focus a worker's pane
// and open a row's pull request, and nothing on the board changes state. Any
// future board action goes as a message to the controller instead.
func fleetBoardCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("fleet board", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	watch := fs.Bool("watch", false, "redraw the board as the fleet moves, with cursor navigation")
	asJSON := jsonFlag(fs)

	if err := fs.Parse(args); err != nil {
		return usagef("%v; %s", err, fleetUsage)
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q; %s", fs.Arg(0), fleetUsage)
	}
	if *watch && *asJSON {
		return usagef("--watch draws a terminal and --json writes a document; %s", fleetUsage)
	}

	// herdr is required the way `ls --all-repos` requires it: the live half of
	// the board is herdr's, and a board rendered without it would draw the
	// ledger's claims beside an empty fleet — which reads as every worker
	// vanished, on the one screen built for noticing exactly that.
	client, err := dialHerdrIfPresent(herdrRequired)
	if err != nil {
		return err
	}

	if *watch {
		return watchBoard(client, out)
	}

	board, err := gatherBoard(client, newPRCache())
	if err != nil {
		return err
	}
	if *asJSON {
		return jsonout.Write(out, fleet.JSON(board))
	}
	// Width zero: the one-shot is for scrollback and pipes, where every column
	// earns its place and no pane is asking for less.
	for _, line := range fleet.Lines(board, 0) {
		fmt.Fprintln(out, line.Text)
	}
	return nil
}

// gatherBoard is one full reading of the fleet: ledger, live rows, reports,
// and pull request state for the rows that claim one.
func gatherBoard(client *herdrapi.Client, prs *prCache) (fleet.Board, error) {
	path := fleet.LedgerPath()
	ledger, found, err := fleet.Load(path)
	if err != nil {
		// Unreadable is not missing: a board drawn without a ledger that
		// exists would silently drop the escalations, which are the rows the
		// board is for.
		return fleet.Board{}, fmt.Errorf("read fleet ledger: %w", err)
	}

	roots, err := openRepositories(client)
	if err != nil {
		return fleet.Board{}, fmt.Errorf("list workspaces: %w", err)
	}
	repos, err := wt.AllRows(client, roots)
	if err != nil {
		return fleet.Board{}, err
	}
	listings := make([][]wt.Row, 0, len(repos))
	for i := range repos {
		wt.WithPanes(client, repos[i].Rows)
		wt.WithReports(repos[i].Rows, paneReport)
		listings = append(listings, repos[i].Rows)
	}
	wt.WithAgentSeqs(client, listings...)

	return fleet.Build(repos, ledger, path, found, time.Now(), prs.lookup), nil
}

// prCache holds pull request states between refreshes. Each lookup is one
// `gh` call in series, so watch mode must not repeat them every redraw; a
// minute is stale enough to notice a merge and fresh enough not to hammer.
type prCache struct {
	ttl     time.Duration
	entries map[string]prCacheEntry
}

type prCacheEntry struct {
	state string
	err   error
	when  time.Time
}

func newPRCache() *prCache {
	return &prCache{ttl: time.Minute, entries: map[string]prCacheEntry{}}
}

func (c *prCache) lookup(root, branch string) (string, error) {
	key := root + "\x00" + branch
	if hit, ok := c.entries[key]; ok && time.Since(hit.when) < c.ttl {
		return hit.state, hit.err
	}
	state, err := reconcile.GhPRLookup(root, branch)
	c.entries[key] = prCacheEntry{state: string(state), err: err, when: time.Now()}
	return string(state), err
}
