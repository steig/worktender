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

// nameTitleMax caps the issue title inside a workspace name, in runes. herdr's
// own limit is unmeasured, so this stays short enough for a tab bar.
const nameTitleMax = 24

// workspaceName is `<repo> #<issue> <short title>`. The title is third-party
// text, so unsafe runes are escaped before it is cut.
func workspaceName(repo string, number int, title string) string {
	title = safetext.Escape(strings.TrimSpace(title))
	if utf8.RuneCountInString(title) > nameTitleMax {
		title = strings.TrimSpace(string([]rune(title)[:nameTitleMax-1])) + "…"
	}
	return strings.TrimSpace(fmt.Sprintf("%s #%d %s", repo, number, title))
}

// A status prefix is a marker, and for done a PR slot after it. Only names
// worktender gave carry one, and a name without `#<issue>` is a person's own.
var (
	statusPrefix  = regexp.MustCompile(`^(?:… |! |✓ (?:PR \d+ )?)`)
	namedByUs     = regexp.MustCompile(`#\d+`)
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
