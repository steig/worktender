package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/steig/worktender/internal/roles"
)

// refocusIfDue is `refocus --if-due`: the goal chain on an interval, for the
// drift that sets in long before a compaction does. The worktender-watch mod
// runs it on every prompt a worker is sent, and attaches whatever it prints to
// that prompt as context the model reads and the person does not see. It
// prints nothing until the worker's role says a delivery is due.
//
// THE COUNT LIVES HERE, NOT IN THE MOD, because the interval is a role's and
// the mod has no role: roles are worktender's configuration, and the mod would
// otherwise need a second copy of the label rules to learn which one applies.
// worktender also already keeps per-worktree state, and a counter in the mod's
// memory would start over at every reload of it.
//
// A TURN is one prompt the worker is sent — the brief, a pr-feedback notice, an
// inbox reply, a coordinator's nudge — so the count is of the moments new
// direction arrives, which is when a worker is most likely to be pulled off
// its issue. MINUTES are since the last delivery, and come due at the next
// prompt after they elapse: the hook that carries the chain is the prompt's,
// so a worker inside one long turn is not interrupted. Either threshold
// reached makes a delivery due; both start over when one is made.
//
// THE ROLE is resolved the way `start` resolves it without --role: the issue's
// labels against the rules, else default_role. `start` does not record the
// role it chose, so a worker started with --role is timed by the role its
// labels would have picked. The labels are read once per worktree, on the
// first prompt (the brief), and only the role's name is kept: the interval is
// read from the configuration every time, so editing it takes effect without
// restarting anyone.
//
// COST. With no role setting an interval — everyone's configuration until
// someone writes one — this returns after reading the configuration file: no
// gh call, no state written. With one, the first prompt in a worktree costs
// one gh call (labels), and each delivery the two refocus always costs. There
// is no cache of the chain: the point of a delivery is the issue as it reads
// now, and a delivery is as rare as the interval the user chose.
func refocusIfDue(root, top string, number int, out io.Writer) error {
	config, err := roles.Load()
	if err != nil {
		return withCode(exitUsage, err)
	}
	if !config.AnyRefocus() {
		return nil
	}
	path, err := refocusTickPath(top)
	if err != nil {
		return err
	}
	now := refocusNow()

	tick := loadRefocusTick(path)
	if tick == nil || tick.Worktree != top || tick.Issue != number {
		// The first prompt this worktree has been seen at, which is the brief
		// itself: the role is settled and the clock starts, and there is nothing
		// to refocus on yet.
		iss, err := issueFor(root, number)
		if err != nil {
			return withCode(exitEnvironment, err)
		}
		launch, err := config.Resolve("", iss.labelNames(), root)
		if err != nil {
			return withCode(exitUsage, err)
		}
		tick = &refocusTick{Worktree: top, Issue: number, Last: now.Unix()}
		if launch != nil {
			tick.Role = launch.Role
		}
		return tick.save(path)
	}

	every := config.Roles[tick.Role].Refocus
	if tick.Role == "" || !every.On() {
		return nil
	}
	tick.Turns++
	due := (every.Turns > 0 && tick.Turns >= every.Turns) ||
		(every.Minutes > 0 && now.Sub(time.Unix(tick.Last, 0)) >= time.Duration(every.Minutes)*time.Minute)
	if !due {
		return tick.save(path)
	}

	chain, err := readChain(root, number)
	if err != nil {
		// The count is kept, so the next prompt tries again rather than waiting
		// out a whole interval for gh to come back.
		if sErr := tick.save(path); sErr != nil {
			return sErr
		}
		return withCode(exitEnvironment, err)
	}
	tick.Turns, tick.Last = 0, now.Unix()
	// Saved before printing: a delivery the state does not record would be
	// made again on the next prompt.
	if err := tick.save(path); err != nil {
		return err
	}
	fmt.Fprint(out, renderRefocus(chain, selfPath()))
	return nil
}

// refocusNow is the clock, a variable so a test can move it.
var refocusNow = time.Now

// refocusTick is one worktree's interval state.
type refocusTick struct {
	// Worktree and Issue say whose this is. A worktree removed and created
	// again at the same path for another issue starts over.
	Worktree string `json:"worktree"`
	Issue    int    `json:"issue"`
	// Role is the role the issue's labels resolved to, or "" for none.
	Role string `json:"role"`
	// Turns counts prompts since the last delivery; Last is when that was, or
	// when the count started, in Unix seconds.
	Turns int   `json:"turns"`
	Last  int64 `json:"last"`
}

// refocusTickPath is one file per worktree, so workers never write the same
// file: each runs this from its own session, concurrently with the others.
// Named by a hash of the worktree path, which is the one key that is unique
// and stable for the life of a worker.
func refocusTickPath(top string) (string, error) {
	base := stateDir()
	if base == "" {
		base = defaultStateDir()
	}
	if base == "" {
		return "", withCode(exitEnvironment, errors.New(
			"no plugin state directory: HERDR_PLUGIN_STATE_DIR is unset and no default location could be derived"))
	}
	dir := filepath.Join(base, "refocus")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", withCode(exitEnvironment, fmt.Errorf("create state directory: %w", err))
	}
	sum := sha256.Sum256([]byte(top))
	return filepath.Join(dir, hex.EncodeToString(sum[:8])+".json"), nil
}

// loadRefocusTick is nil for a missing or unreadable file. Unlike
// pr-feedback's state, starting a counter over costs at most one early or late
// delivery, so a damaged file is not worth refusing for.
func loadRefocusTick(path string) *refocusTick {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var t refocusTick
	if json.Unmarshal(raw, &t) != nil {
		return nil
	}
	return &t
}

// save writes through a rename, as pr-feedback's state does.
func (t *refocusTick) save(path string) error {
	raw, err := json.Marshal(t)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return withCode(exitEnvironment, err)
	}
	return withCode(exitEnvironment, os.Rename(tmp, path))
}
