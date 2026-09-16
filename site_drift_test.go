package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/wt"
)

// The pages under site/pages/ are hand-written HTML with no markdown source,
// because the narrative material has none. That is the right call and it is
// also why they are the only part of the documentation pinned to nothing:
// docs/*.md is rendered by site/build.py and cannot drift, while these froze in
// place while the binary moved on.
//
// Two things shipped wrong that way — an `ls` figure a column-and-a-half out of
// date, and a `start` command the page selling the plugin never mentioned — and
// nothing failed in between. These tests are the "fail loudly" half.
//
// Not trying to generate the pages. Only to make them fail when they lie.

const sitePages = "site/pages"

// tagOrEntity strips the markup out of a terminal figure so the text inside can
// be read as the terminal would show it.
var tagOrEntity = regexp.MustCompile(`<[^>]*>`)

// figureLine is one rendered line of a <div class="terminal"> block.
func stripMarkup(s string) string {
	s = tagOrEntity.ReplaceAllString(s, "")
	// The few entities these figures actually use. A figure needing more than
	// this is a figure worth simplifying.
	r := strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&", "&quot;", `"`, "&middot;", "·")
	return r.Replace(s)
}

// readPage returns one hand-written page.
func readPage(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(sitePages, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(raw)
}

// pageNames is every hand-written page, discovered rather than listed: a page
// added later must be covered without anyone remembering to add it here.
func pageNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(sitePages)
	if err != nil {
		t.Fatalf("read %s: %v", sitePages, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".html") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		t.Fatalf("no hand-written pages found in %s; this test is watching nothing", sitePages)
	}
	return names
}

// The homepage's `ls` figure showed four columns while the renderer printed six.
// It was wrong from the commit that added the state counter until somebody
// happened to look, which is the whole problem with a hand-drawn screenshot.
//
// The column *count* rather than the exact text: the figure is illustrative and
// its branch names and workspace ids are invented, but a column appearing or
// disappearing is the renderer changing shape underneath it.
func TestTheLsFigureHasAsManyColumnsAsTheRenderer(t *testing.T) {
	seq := uint64(1057)
	var real strings.Builder
	if err := wt.Render(&real, []wt.Row{{
		Main: true, Branch: "main", WorkspaceID: "w21", PaneID: "w21:p1",
		AgentStatus: "idle", AgentStatusSeq: &seq, Dir: "worktender",
	}}, wt.Columns{}); err != nil {
		t.Fatalf("render: %v", err)
	}
	want := len(strings.Fields(strings.TrimSpace(real.String())))

	page := readPage(t, "index.html")
	figure := terminalBlocks(page)
	if len(figure) == 0 {
		t.Fatal("index.html has no terminal figure; if the figure went away, so should this test")
	}

	found := false
	for _, block := range figure {
		lines := strings.Split(block, "\n")
		if !strings.Contains(lines[0], "worktender ls") {
			continue
		}
		found = true
		for _, line := range lines[1:] {
			text := strings.TrimRight(stripMarkup(line), " ")
			if strings.TrimSpace(text) == "" {
				continue
			}
			got := len(strings.Fields(text))
			// A row that is not the main checkout has a space where the `*`
			// goes, and splitting on whitespace cannot see it.
			if !strings.HasPrefix(strings.TrimLeft(text, " "), "*") {
				got++
			}
			if got != want {
				t.Errorf("the ls figure draws %d columns, the renderer prints %d:\n  %s", got, want, text)
			}
		}
	}
	if !found {
		t.Error("no `worktender ls` figure on index.html; the renderer's shape is now pinned to nothing")
	}
}

// terminalBlocks returns the contents of every <div class="terminal"> on a page.
func terminalBlocks(page string) []string {
	var blocks []string
	rest := page
	for {
		open := strings.Index(rest, `<div class="terminal">`)
		if open < 0 {
			return blocks
		}
		rest = rest[open+len(`<div class="terminal">`):]
		end := strings.Index(rest, "</div>")
		if end < 0 {
			return blocks
		}
		blocks = append(blocks, rest[:end])
		rest = rest[end:]
	}
}

// `start` shipped and the site never mentioned it — not the homepage, not
// Patterns, not Examples. The round trip from an issue to an agent working on it
// is what the README leads with, and the pages selling the plugin did not say it
// existed.
//
// One mention across the whole documentation is a low bar deliberately: this
// test is a tripwire for a command nobody wrote up, not an editor.
func TestEverySubcommandIsWrittenUpSomewhere(t *testing.T) {
	// on-event and startup are invoked by herdr and never by a person, so they
	// are described in docs/events.md as hooks rather than as commands anyone
	// runs. They are exempt from needing a mention by name.
	invokedByHerdr := map[string]bool{"on-event": true, "startup": true}

	var corpus strings.Builder
	for _, name := range pageNames(t) {
		corpus.WriteString(readPage(t, name))
	}
	docs, err := filepath.Glob("docs/*.md")
	if err != nil || len(docs) == 0 {
		t.Fatalf("no docs found to search: %v", err)
	}
	for _, path := range docs {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		corpus.Write(raw)
	}
	text := corpus.String()

	for _, name := range commands {
		if invokedByHerdr[name] {
			continue
		}
		if !strings.Contains(text, "worktender "+name) && !strings.Contains(text, "`"+name+"`") {
			t.Errorf("no page or doc mentions the %q subcommand; it shipped and the documentation does not know", name)
		}
	}
}

// A hand-written page linking into a rendered one by anchor is the link most
// able to rot silently: the heading it points at lives in a different file, in a
// different format, maintained separately. Nothing renders differently when it
// breaks — the reader just lands at the top of a long page.
//
// Checked against the markdown source rather than the build output, so this
// needs no Python and runs with the rest of the suite.
func TestEveryAnchorLinkFromASiteFileResolves(t *testing.T) {
	link := regexp.MustCompile(`href="([a-z-]+)\.html#([a-z0-9-]+)"`)
	mdLink := regexp.MustCompile(`\]\((?:\./)?([a-z-]+)\.md#([a-z0-9-]+)\)`)

	check := func(where, target, anchor string) {
		t.Helper()
		// A hand-written page has no markdown source, so an anchor into one
		// cannot be checked this way. None exist today; say so rather than
		// passing silently if one appears.
		if _, err := os.Stat(filepath.Join(sitePages, target+".html")); err == nil {
			t.Logf("%s links into hand-written %s.html#%s, which this test cannot verify", where, target, anchor)
			return
		}
		raw, err := os.ReadFile(filepath.Join("docs", target+".md"))
		if err != nil {
			t.Errorf("%s links to %s.html#%s, but there is no docs/%s.md", where, target, anchor, target)
			return
		}
		for line := range strings.SplitSeq(string(raw), "\n") {
			if _, title, ok := strings.Cut(line, "# "); ok && strings.HasPrefix(line, "#") {
				if githubAnchor(title) == anchor {
					return
				}
			}
		}
		t.Errorf("%s links to %s.html#%s, which is not a heading in docs/%s.md — the reader lands at the top of a long page and nothing looks wrong", where, target, anchor, target)
	}

	for _, name := range pageNames(t) {
		for _, m := range link.FindAllStringSubmatch(readPage(t, name), -1) {
			check(name, m[1], m[2])
		}
	}

	docs, _ := filepath.Glob("docs/*.md")
	for _, path := range docs {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, m := range mdLink.FindAllStringSubmatch(string(raw), -1) {
			check(filepath.Base(path), m[1], m[2])
		}
	}
}

// build.py's PAGES list is the nav, and it names both the markdown-backed pages
// and the hand-written ones. A hand-written page not in that list is built into
// nothing; an entry with no file behind it fails the build.
func TestEveryHandWrittenPageIsInTheNav(t *testing.T) {
	raw, err := os.ReadFile("site/build.py")
	if err != nil {
		t.Fatalf("read site/build.py: %v", err)
	}
	build := string(raw)

	for _, name := range pageNames(t) {
		slug := strings.TrimSuffix(name, ".html")
		if !strings.Contains(build, `("`+slug+`"`) {
			t.Errorf("site/pages/%s is not in build.py's PAGES, so it is rendered into nothing", name)
		}
	}
}

// reportsFigure is the fleet the `ls --reports` figure draws: one worker that
// has not reported, one that reported without a pull request, and one with
// both.
func reportsFigure() []wt.Row {
	s1, s2, s3 := uint64(1057), uint64(1055), uint64(812)
	rows := []wt.Row{
		{Main: true, Branch: "main", WorkspaceID: "w21", PaneID: "w21:p1", AgentStatus: "idle", AgentStatusSeq: &s1, Dir: "worktender"},
		{Branch: "feat/1-reconcile-execute", WorkspaceID: "w22", PaneID: "w22:p1", AgentStatus: "working", AgentStatusSeq: &s2, Dir: "1-reconcile-execute"},
		{Branch: "fix/257-erasure-comments", WorkspaceID: "w1K", PaneID: "w1K:p1", AgentStatus: "idle", AgentStatusSeq: &s3, Dir: "257-erasure-comments"},
	}
	wt.WithReports(rows, func(pane string) (wt.Report, error) {
		switch pane {
		case "w22:p1":
			return wt.Report{Found: true, Status: "planned", Note: "reading the issue"}, nil
		case "w1K:p1":
			return wt.Report{Found: true, Status: "done", PR: 4, Note: "landed"}, nil
		}
		return wt.Report{}, nil
	})
	return rows
}

// docs/*.md cannot drift from the site, which renders it — but it can drift
// from the binary, and a table drawn by hand is where that shows. The
// `ls --reports` figure showed a blank cell for the worker that had not
// reported, where the renderer prints "-". Small, and on the one column the
// surrounding section is about: a dash the reader is being told to distrust,
// omitted from the figure that demonstrates it.
//
// Column by column rather than byte for byte. The figures indent by one space
// where the tabwriter pads with two, and that is a legible figure rather than a
// lie — but a cell that is present in one and absent in the other is not.
func TestTheReportsFigureDrawsTheCellsTheRendererPrints(t *testing.T) {
	// With the label row, because that is what the figures draw and what the
	// command prints unasked — a figure pinned to the headerless table would be
	// pinned to output nobody sees by default.
	var real strings.Builder
	if err := wt.Render(&real, reportsFigure(), wt.Columns{Reports: true, Header: true}); err != nil {
		t.Fatalf("render: %v", err)
	}
	var want [][]string
	for line := range strings.SplitSeq(strings.TrimRight(real.String(), "\n"), "\n") {
		want = append(want, figureCells(line))
	}

	for _, path := range []string{"README.md", "docs/json.md"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		drawn := 0
		for _, block := range fencedBlocks(string(raw)) {
			lines := strings.Split(strings.TrimRight(block, "\n"), "\n")
			if len(lines) == 0 || !strings.Contains(lines[0], "worktender ls --reports") {
				continue
			}
			drawn++
			got := lines[1:]
			if len(got) != len(want) {
				t.Errorf("%s draws %d rows, the renderer prints %d", path, len(got), len(want))
				continue
			}
			for i, line := range got {
				if cells := figureCells(line); !slices.Equal(cells, want[i]) {
					t.Errorf("%s row %d draws %q, the renderer prints %q", path, i+1, cells, want[i])
				}
			}
		}
		if drawn == 0 {
			t.Errorf("no `worktender ls --reports` figure in %s; if the figure went away this test is watching nothing, and if it moved it is unpinned", path)
		}
	}
}

// columnGap is what separates two cells: the tabwriter pads with two spaces, so
// a single space is inside a cell — "done #4" is one column, not two.
var columnGap = regexp.MustCompile(`\s{2,}`)

// figureCells splits one table line into its cells, ignoring the main-checkout
// marker. The marker is the one place a figure and the renderer legitimately
// disagree on spacing, and it carries no fact the other columns do not.
func figureCells(line string) []string {
	line = strings.TrimSpace(strings.TrimPrefix(strings.TrimLeft(line, " "), "*"))
	return columnGap.Split(line, -1)
}

// fencedBlocks returns the contents of every ``` block in a markdown file.
func fencedBlocks(md string) []string {
	parts := strings.Split(md, "```")
	var blocks []string
	// Odd indices are inside a fence. The first line of each is the info
	// string — "sh", "json" — and is dropped with it.
	for i := 1; i < len(parts); i += 2 {
		if _, body, ok := strings.Cut(parts[i], "\n"); ok {
			blocks = append(blocks, body)
		}
	}
	return blocks
}
