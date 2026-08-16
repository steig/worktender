package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/reconcile"
	"github.com/steig/worktender/internal/repolock"
)

// eventsEnv opts a session in to the event fast path. Unset means events do
// nothing at all.
//
// Off by default because an event hook here starts coding agents without being
// asked, and a plugin that does that on install has handed its user an
// autonomous trigger they never requested.
const eventsEnv = "WORKTENDER_EVENTS"

// legacyEventsEnvs are what eventsEnv was called before each rename, newest
// first. They enable nothing; they exist so the gate can say so.
//
// Honouring one as an alias would keep an autonomous trigger live under a name
// that appears in no current README, and a silent rename would leave someone's
// hooks inert with nothing saying why. Detect, refuse, and name the replacement.
var legacyEventsEnvs = []string{"MUSTER_EVENTS", "HERDR_WT_EVENTS"}

// eventsEnabled reports whether the fast path is opted in to.
func eventsEnabled() bool {
	on, _ := parseEventsValue(os.Getenv(eventsEnv))
	return on
}

// parseEventsValue reads the gate, reporting whether it is on and whether the
// value is one this gate has a rule for.
//
// Falsey spellings are accepted case-insensitively and trimmed, because a
// switch that reads "off" as on starts coding agents in response to being told
// not to. An unrecognised value reads as off: a mistyped opt-out that fell open
// would arm an autonomous trigger, while a mistyped opt-in costs nothing and
// unrecognisedEventsNotice makes it loud.
func parseEventsValue(raw string) (on, recognised bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		// Unset, or nothing but whitespace.
		return false, true
	case "1", "true", "yes", "y", "on", "enabled":
		return true, true
	case "0", "false", "no", "n", "off", "disabled":
		return false, true
	default:
		return false, false
	}
}

// unrecognisedEventsNotice is the line owed to a session whose gate holds a
// value no rule covers, or "" when nothing is owed. It is what someone who
// mistyped an opt-in gets instead of silence.
func unrecognisedEventsNotice() string {
	raw := os.Getenv(eventsEnv)
	if _, recognised := parseEventsValue(raw); recognised {
		return ""
	}
	return fmt.Sprintf("%s=%q is not a value this gate recognises, so events stay off; export %s=1 to enable them\n",
		eventsEnv, raw, eventsEnv)
}

// renamedEnvNotice is the line owed to a session still exporting the old name,
// or "" when nothing is owed. An explicit WORKTENDER_EVENTS of any value silences
// it: at that point the caller knows the current spelling, including when they
// used it to opt out.
func renamedEnvNotice() string {
	if os.Getenv(eventsEnv) != "" {
		return ""
	}
	for _, old := range legacyEventsEnvs {
		if os.Getenv(old) == "" {
			continue
		}
		return fmt.Sprintf("%s is set, but it was renamed to %s and the old name is not honoured; export %s=1 instead\n",
			old, eventsEnv, eventsEnv)
	}
	return ""
}

// onEventUsage is what an invocation of the event hook may look like, which is
// the command and nothing else.
const onEventUsage = "usage: worktender on-event (herdr invokes this; it takes no arguments)"

// onEventCommand is the whole event fast path.
//
// An event is a trigger, never a fact. The payload is read for exactly one
// thing — which repository — and then the same collect/reconcile/execute
// pipeline `sync` runs runs again over the whole repository, reading live
// state. An event payload is herdr's snapshot from before this process existed,
// so it is stale on arrival by construction.
func onEventCommand(args []string, out io.Writer) error {
	// Refused before the opt-in is read, because a malformed invocation is
	// malformed either way and answering it with the events-off notice would
	// report the argument as accepted. Nothing is reached to do it: rejecting
	// argv runs no herdr call and loads no payload, which is what the opt-in
	// guards.
	//
	// herdr invokes this with a fixed array and no arguments — the payload
	// arrives in the environment — so nothing that reaches here legitimately
	// has any.
	if len(args) > 0 {
		return usagef("unexpected argument %q; %s", args[0], onEventUsage)
	}

	// Checked before anything else is parsed, so a plugin that has not been
	// opted in does nothing whatsoever.
	if !eventsEnabled() {
		fmt.Fprintf(out, "events are off; export %s=1 to enable the worktree fast path\n", eventsEnv)
		fmt.Fprint(out, unrecognisedEventsNotice())
		fmt.Fprint(out, renamedEnvNotice())
		return nil
	}

	envelope, err := herdrapi.LoadEvent()
	if err != nil {
		return err
	}

	scope, err := envelope.Scope()
	if err != nil {
		// An event we subscribe to but have no behaviour for is a no-op, not a
		// failure — the manifest and the binary can differ across an upgrade.
		if errors.Is(err, herdrapi.ErrUnhandledEvent) {
			fmt.Fprintf(out, "ignoring %s\n", envelope.Event)
			return nil
		}
		return err
	}

	client, err := herdrapi.New()
	if err != nil {
		return err
	}

	root := scope.RepoRoot
	if root == "" {
		// Derived from the checkout the event named, never from the process
		// cwd: an event names its own subject, so there is nothing to guess.
		if root, err = gitx.RepoRoot(scope.Checkout); err != nil {
			return fmt.Errorf("%s: %w", envelope.Event, err)
		}
	}

	// CallerDir is empty on purpose: nobody is standing in a directory here, and
	// the guard it feeds only governs removals, which this path never performs.
	s := &session{client: client, root: gitx.Resolve(root)}

	collector := reconcile.NewCollector(s.client, s.root)
	// Prunes are filtered out below, so the PR lookup that would authorise one
	// decides nothing — at a gh invocation per worktree per event.
	collector.LookupPR = nil

	// Claim the repository, or leave a mark and stand down. Standing down is not
	// a dropped event: the holder runs the same whole-repository reconcile, and
	// the mark stops it finishing on a snapshot older than this event.
	lock, err := repolock.AcquireOrMark(stateDir(), s.root)
	if err != nil {
		return err
	}
	if lock == nil {
		fmt.Fprintf(out, "%s: %s is already being reconciled; coalesced into that pass\n", envelope.Event, s.root)
		return nil
	}
	defer releaseLock(lock, out)

	// Adopt and staff only. Removal stays something a human asks for by name:
	// `prune` and `prune-apply` are separate actions precisely so nothing
	// removes a worktree as a side effect of something else.
	// Text: an event hook's output lands in the plugin log for a human to read
	// afterwards, and herdr invokes it with no argument surface to ask for
	// anything else.
	return s.reconcileOpen(lock, collector, func() *output { return newOutput(out, false) })
}
