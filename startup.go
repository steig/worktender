package main

import (
	"fmt"
	"io"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/reconcile"
	"github.com/steig/worktender/internal/repolock"
)

// startupUsage is what an invocation of the startup one-shot may look like.
const startupUsage = "usage: worktender startup (herdr invokes this; it takes no arguments)"

// startupCommand is what herdr's [[startup]] entry runs once, after the server
// is ready. Nothing here loops, sleeps, or stays resident: it is one reconcile
// pass per repository and then the process exits.
//
// The [[events]] hooks cover the whole session. What they cannot cover is the
// interval when herdr was not running — a worktree added from a plain shell, a
// workspace restored without its agent. That gap opens exactly once, at
// startup, which is when this runs.
func startupCommand(args []string, out io.Writer) error {
	// Refused ahead of the opt-in, for the reason on-event refuses it there:
	// answering a bad invocation with the events-off notice reports the
	// argument as accepted, and rejecting argv reaches nothing the opt-in
	// guards.
	if len(args) > 0 {
		return usagef("unexpected argument %q; %s", args[0], startupUsage)
	}

	// Startup shares the [[events]] opt-in rather than adding a switch of its
	// own: both start coding agents without being asked, and a second variable
	// would let someone opt out of one trigger while the other kept firing.
	if !eventsEnabled() {
		fmt.Fprintf(out, "events are off; export %s=1 to reconcile worktrees at startup\n", eventsEnv)
		fmt.Fprint(out, unrecognisedEventsNotice())
		fmt.Fprint(out, renamedEnvNotice())
		return nil
	}

	client, err := herdrapi.New()
	if err != nil {
		return err
	}

	roots, err := openRepositories(client)
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		fmt.Fprintln(out, "startup: no worktree workspaces open; nothing to reconcile")
		return nil
	}

	// One repository failing must not cost the others their only pass, so
	// failures are collected and reported at the end — herdr records a command
	// that exits 0 as succeeded.
	var failed int
	for _, root := range roots {
		fmt.Fprintf(out, "\nstartup: %s\n", root)
		if err := reconcileAtStartup(out, client, root); err != nil {
			failed++
			fmt.Fprintf(out, "startup: %s: %v\n", root, err)
		}
	}
	if failed > 0 {
		return fmt.Errorf("startup reconcile failed for %d of %d repositor(ies)", failed, len(roots))
	}
	return nil
}

// openRepositories is every distinct repository herdr has a worktree workspace
// for, in herdr's own order. This is the scope rather than the invocation
// context because at server start nobody is standing anywhere yet.
//
// Workspaces herdr does not report as worktrees are skipped: such a workspace
// may sit in a repository this plugin was never pointed at, and deriving a root
// from it would have startup staffing repositories nobody asked it to manage.
func openRepositories(client *herdrapi.Client) ([]string, error) {
	workspaces, err := client.WorkspaceList()
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var roots []string
	for _, ws := range workspaces.Workspaces {
		if ws.Worktree == nil || ws.Worktree.RepoRoot == "" {
			continue
		}
		// Normalised: Collect compares workspace roots by equality, and herdr's
		// paths are resolved while ours may not be.
		root := gitx.Resolve(ws.Worktree.RepoRoot)
		if seen[root] {
			continue
		}
		seen[root] = true
		roots = append(roots, root)
	}
	return roots, nil
}

// reconcileAtStartup runs the adopt-and-staff pass for one repository. It is
// the same collect/reconcile/execute pipeline `sync` and the event hook run,
// filtered the same way, so there is only one answer to "what does this
// repository need".
func reconcileAtStartup(out io.Writer, client *herdrapi.Client, root string) error {
	// CallerDir is empty: nobody is standing in a directory, and the guard it
	// feeds governs removals, which this path never performs.
	s := &session{client: client, root: root}

	collector := reconcile.NewCollector(client, root)
	// No gh: PR state only ever authorises a prune, and prunes are filtered out
	// below.
	collector.LookupPR = nil

	// Claim the repository, or leave a mark and stand down. herdr emits
	// worktree.opened as it restores workspaces, so an event hook may already be
	// running this same pass.
	lock, err := repolock.AcquireOrMark(stateDir(), root)
	if err != nil {
		return err
	}
	if lock == nil {
		fmt.Fprintf(out, "startup: %s is already being reconciled; coalesced into that pass\n", root)
		return nil
	}
	defer releaseLock(lock, out)

	// Adopt and staff only: startup has the least information about what a
	// human intends, so it is the last place that should remove a checkout.
	// Text, for the reason the event path is: herdr invokes this one too.
	return s.reconcileOpen(lock, collector, func() *output { return newOutput(out, false) })
}
