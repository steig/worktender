package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/steig/worktender/internal/fleet"
	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
)

// watchInterval is how often the board re-reads the fleet. Each refresh is a
// handful of herdr calls plus one ledger read; the `gh` lookups have their own
// cache and do not run per tick.
const watchInterval = 3 * time.Second

// watchBoard is the interactive board: the same render the one-shot prints,
// redrawn as the fleet moves, with a cursor for the two navigations the
// charter allows — focus a worker's pane, open a row's pull request. Nothing
// here changes state.
//
// The terminal is driven with stty and ANSI sequences rather than a TUI
// dependency: this module has none, and the board needs exactly raw keys, an
// alternate screen and reverse video. stty makes watch mode unix-only, which
// is also the platform set of the plugin pane that hosts it.
func watchBoard(client *herdrapi.Client, out io.Writer) error {
	restore, err := enterRawMode()
	if err != nil {
		return withCode(exitEnvironment, fmt.Errorf("--watch needs a terminal on stdin: %w", err))
	}
	defer restore()

	// Alternate screen, cursor hidden; both undone before raw mode is, so the
	// shell prompt returns to the primary screen it left.
	fmt.Fprint(out, "\x1b[?1049h\x1b[?25l")
	defer fmt.Fprint(out, "\x1b[?1049l\x1b[?25h")

	keys := make(chan boardKey, 8)
	go readKeys(os.Stdin, keys)

	// Refreshes run beside the loop, not in it: a herdr call waits up to five
	// seconds, and keys must keep answering while it does. The request
	// channel holds one pending refresh; more would only repeat the answer.
	prs := newPRCache()
	type boardResult struct {
		board fleet.Board
		err   error
	}
	results := make(chan boardResult)
	requests := make(chan struct{}, 1)
	go func() {
		for range requests {
			board, err := gatherBoard(client, prs)
			results <- boardResult{board: board, err: err}
		}
	}()
	request := func() {
		select {
		case requests <- struct{}{}:
		default:
		}
	}
	request()

	ticker := time.NewTicker(watchInterval)
	defer ticker.Stop()

	// The render is width-aware — narrow panes drop columns — so the lines are
	// rebuilt from the last board whenever the terminal is a different size
	// than the frame it drew last. Refresh keeps the cursor by row identity,
	// which a width change does not move.
	model := fleet.Model{Sel: -1}
	var board fleet.Board
	haveBoard := false
	lastWidth := 0
	status := "loading fleet…"
	draw := func() {
		width, height := termSize()
		if haveBoard && width != lastWidth {
			model.Refresh(fleet.Lines(board, width))
			lastWidth = width
		}
		fmt.Fprint(out, "\x1b[H\x1b[2J"+model.Frame(width, height, status))
	}
	draw()

	for {
		select {
		case res := <-results:
			if res.err != nil {
				// The previous board stays up rather than being torn down: a
				// refresh that failed is a status line, not a blank fleet.
				status = res.err.Error()
			} else {
				board, haveBoard, lastWidth = res.board, true, -1
				status = ""
			}
			draw()
		case <-ticker.C:
			request()
		case key := <-keys:
			if key == keyQuit {
				return nil
			}
			if model.Help {
				// The overlay is modal the cheap way: any key puts the board
				// back, and does nothing else — a navigation pressed at a key
				// list should not navigate.
				model.Help = false
				draw()
				continue
			}
			switch key {
			case keyHelp:
				model.Help = true
			case keyDown:
				model.Move(1)
				status = ""
			case keyUp:
				model.Move(-1)
				status = ""
			case keyHome:
				model.Home()
			case keyEnd:
				model.End()
			case keyFocus:
				status = focusWorker(client, model.Current())
			case keyOpenPR:
				status = openRowPR(model.Current())
			case keyRefresh:
				status = "refreshing…"
				request()
			}
			draw()
		}
	}
}

// focusWorker is jump-to-worker: focus the pane the row's agent lives in.
// Through agent.focus first, because focusing an agent marks its `done` as
// seen and so clears it from herdr's attention queue; pane.focus is the
// fallback for a pane whose agent has exited, where the jump is wanted for
// exactly the look-at-what-stopped reason.
func focusWorker(client *herdrapi.Client, t *fleet.Target) string {
	switch {
	case t == nil:
		return "nothing selected"
	case t.PaneID == "":
		return t.Label + " has no pane to focus"
	}
	if err := client.AgentFocus(t.PaneID); err != nil {
		if err := client.PaneFocus(t.PaneID); err != nil {
			return "focus " + t.Label + ": " + err.Error()
		}
	}
	return "focused " + t.Label
}

// openRowPR opens the row's pull request in the browser, through `gh pr view
// --web` so the resolution — number to URL, branch to number — stays gh's.
func openRowPR(t *fleet.Target) string {
	if t == nil {
		return "nothing selected"
	}
	ref := t.Branch
	if t.PR > 0 {
		ref = strconv.Itoa(t.PR)
	}
	if ref == "" || t.Root == "" {
		return t.Label + " has no pull request to open"
	}

	args := []string{"pr", "view", ref, "--web"}
	// The same disambiguation GhPRLookup applies, for the same reason: a
	// worktree's checkout can name several remotes, and the answer must come
	// from the repository this fleet pushes to.
	if origin := gitx.RemoteURL(t.Root); origin != "" {
		args = append(args, "--repo", origin)
	}
	cmd := exec.Command("gh", args...)
	cmd.Dir = t.Root
	if raw, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(raw))
		if msg == "" {
			msg = err.Error()
		}
		if line, _, found := strings.Cut(msg, "\n"); found {
			msg = line
		}
		return "gh pr view " + ref + ": " + msg
	}
	return "opened pull request for " + t.Label
}

// boardKey is one keypress, already resolved from bytes and escape sequences.
type boardKey int

const (
	keyQuit boardKey = iota
	keyUp
	keyDown
	keyHome
	keyEnd
	keyFocus
	keyOpenPR
	keyRefresh
	keyHelp
)

// readKeys turns raw stdin bytes into board keys. It exits when the reader
// does, which for the board is process exit; the goroutine parked on the last
// read goes down with it.
func readKeys(r io.Reader, keys chan<- boardKey) {
	buf := make([]byte, 1)
	esc := 0 // 0 plain, 1 after ESC, 2 after ESC-[
	for {
		if _, err := r.Read(buf); err != nil {
			return
		}
		b := buf[0]

		switch esc {
		case 1:
			if b == '[' {
				esc = 2
			} else {
				esc = 0
			}
			continue
		case 2:
			esc = 0
			switch b {
			case 'A':
				keys <- keyUp
			case 'B':
				keys <- keyDown
			case 'H':
				keys <- keyHome
			case 'F':
				keys <- keyEnd
			}
			continue
		}

		switch b {
		case 0x1b:
			esc = 1
		case 'q', 0x03, 0x04: // q, ctrl-C, ctrl-D
			keys <- keyQuit
			return
		case 'j':
			keys <- keyDown
		case 'k':
			keys <- keyUp
		case 'g':
			keys <- keyHome
		case 'G':
			keys <- keyEnd
		case '\r', '\n', 'f':
			keys <- keyFocus
		case 'o':
			keys <- keyOpenPR
		case 'r':
			keys <- keyRefresh
		case '?':
			keys <- keyHelp
		}
	}
}

// enterRawMode puts the terminal on stdin into raw, no-echo mode and returns
// the undo. Via stty rather than termios syscalls: this module has no
// dependencies to spell the ioctls portably, and stty is the tool whose whole
// job this is — its absence, or a stdin that is not a terminal, is the error
// the caller reports.
func enterRawMode() (func(), error) {
	saved, err := stty("-g")
	if err != nil {
		return nil, err
	}
	if _, err := stty("raw", "-echo"); err != nil {
		return nil, err
	}
	return func() {
		// Best effort: if the restore fails the process is exiting anyway,
		// and the terminal's owner has `reset`.
		_, _ = stty(strings.TrimSpace(saved))
	}, nil
}

// termSize asks the terminal how big it is, defaulting to the canonical 80×24
// when it will not say — a frame drawn at the wrong size beats no frame.
func termSize() (width, height int) {
	width, height = 80, 24
	out, err := stty("size")
	if err != nil {
		return width, height
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return width, height
	}
	if rows, err := strconv.Atoi(fields[0]); err == nil && rows > 0 {
		height = rows
	}
	if cols, err := strconv.Atoi(fields[1]); err == nil && cols > 0 {
		width = cols
	}
	return width, height
}

// stty runs one stty invocation against the process's own stdin, which in
// watch mode is the terminal being configured.
func stty(args ...string) (string, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("stty %s: %w", strings.Join(args, " "), err)
	}
	return string(out), nil
}
