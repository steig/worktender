package main

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/safetext"
)

// Names put on herdr's workspaces, tabs and panes so a person reading the tab
// bar can tell which worker is which. They are cosmetic: a rename that fails
// warns and the command goes on.

// Measured against herdr 0.9.5 in a scratch session: workspace, tab and pane
// labels all took 5000 characters and stored control characters as given, so
// herdr enforces no limit worth designing to. These caps are for the tab bar.
// A workspace label survived a server restart; tab and pane labels did not.
const (
	nameRepoMax  = 20
	nameTitleMax = 24
)

// workspaceName is `<repo> #<issue> <short title>`. The repo and the title are
// third-party text, so each is cut a raw rune at a time and escaped as it goes, so
// a cut never lands inside a `\u{XXXX}` and leaves a fragment that reads as
// something else.
func workspaceName(repo string, number int, title string) string {
	return strings.TrimSpace(fmt.Sprintf("%s #%d %s",
		cutEscaped(repo, nameRepoMax), number, cutEscaped(title, nameTitleMax)))
}

// cutEscaped escapes s into at most max runes, marking a cut. It takes raw runes
// whole while their escaped form fits, so the result is capped after escaping
// (a control rune grows to eight) and a cut cannot fall inside an escape.
func cutEscaped(s string, max int) string {
	s = strings.TrimSpace(s)
	if full := safetext.Escape(s); utf8.RuneCountInString(full) <= max {
		return full
	}
	var b strings.Builder
	used := 0
	for _, r := range s {
		piece := safetext.Escape(string(r))
		n := utf8.RuneCountInString(piece)
		if used+n > max-1 {
			break
		}
		b.WriteString(piece)
		used += n
	}
	return strings.TrimSpace(b.String()) + "…"
}

// A status prefix is a marker, and for done a PR slot after it. Only a name in
// the shape workspaceName gives is worktender's to relabel: `<repo> #<n>` at
// the start, so a person's own workspace that merely mentions an issue is not.
var (
	statusPrefix  = regexp.MustCompile(`^(?:… |! |✓ (?:PR \d+ )?)`)
	namedByUs     = regexp.MustCompile(`^\S+ #\d+(?: |$)`)
	statusMarkers = map[string]string{"planned": "… ", "blocked": "! ", "done": "✓ "}
)

// statusName re-prefixes a workspace name for a report. It reports false for a
// name worktender did not give.
func statusName(current string, r report) (string, bool) {
	base := statusPrefix.ReplaceAllString(current, "")
	if !namedByUs.MatchString(base) {
		return "", false
	}
	marker := statusMarkers[r.status]
	if r.status == "done" && r.pr > 0 {
		marker += "PR " + strconv.Itoa(r.pr) + " "
	}
	return marker + base, true
}

// relabelWorkspace puts a report's status on the reporting pane's workspace.
// Best-effort: the report is already delivered, so a failure is a warning.
func relabelWorkspace(client *herdrapi.Client, paneID string, r report) error {
	pane, err := client.PaneGet(paneID)
	if err != nil {
		return err
	}
	list, err := client.WorkspaceList()
	if err != nil {
		return err
	}
	id := pane.Pane.WorkspaceID
	for _, w := range list.Workspaces {
		if w.WorkspaceID != id {
			continue
		}
		if name, ok := statusName(w.Label, r); ok && name != w.Label {
			return client.WorkspaceRename(id, name)
		}
	}
	return nil
}

// warnName is where a failed rename ends up.
func warnName(w io.Writer, what string, err error) {
	if err != nil {
		fmt.Fprintf(w, "worktender: could not name %s: %v\n", what, err)
	}
}
