package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseAcceptanceReadsAWellFormedSection(t *testing.T) {
	body := "## Problem\r\nIt is broken.\r\n\r\n" +
		"## Acceptance\r\n" +
		"<!-- One check per line. -->\r\n" +
		"- [ ] `go test ./...` exits 0\r\n" +
		"\r\n" +
		"- [x] `go vet ./...` exits 0\r\n" +
		"* `test -f docs/x.md` exits 0\r\n" +
		"- [ ] `worktender version | grep -q 0.14` prints nothing and exits 0\r\n" +
		"\r\n## Notes\r\n- not a check, and not in the section\r\n"

	got := parseAcceptance(body)

	want := []acceptanceCheck{
		{"go test ./...", "exits 0"},
		{"go vet ./...", "exits 0"},
		{"test -f docs/x.md", "exits 0"},
		{"worktender version | grep -q 0.14", "prints nothing and exits 0"},
	}
	if !got.Found || !reflect.DeepEqual(got.Checks, want) || len(got.Malformed) != 0 {
		t.Fatalf("parseAcceptance = %+v, want found with %+v and nothing malformed", got, want)
	}
	if why := got.problems(); why != "" {
		t.Errorf("a well-formed section has no problems; got %q", why)
	}
}

// The template is the most common body there will be, so it must parse as an
// empty section until somebody fills it: its guidance lives in comments, and
// its example check sits inside one.
func TestParseAcceptanceReadsTheTemplateAsEmpty(t *testing.T) {
	template, err := os.ReadFile(filepath.Join(".github", "ISSUE_TEMPLATE", "factory-ready.md"))
	if err != nil {
		t.Fatal(err)
	}
	got := parseAcceptance(string(template))
	if !got.Found || len(got.Checks) != 0 || len(got.Malformed) != 0 {
		t.Fatalf("the unfilled template should be an empty Acceptance section; got %+v", got)
	}
	if !strings.Contains(got.problems(), "empty") {
		t.Errorf("an unfilled template should be refused as empty; got %q", got.problems())
	}
}

func TestParseAcceptanceWithoutTheSection(t *testing.T) {
	for _, body := range []string{
		"",
		"## Problem\nno criteria at all\n",
		// Only a level-2 heading names the section.
		"### Acceptance\n- [ ] `true` exits 0\n",
		// A heading inside a code fence is not a heading.
		"```\n## Acceptance\n- [ ] `true` exits 0\n```\n",
		// Nor is one inside an HTML comment.
		"<!--\n## Acceptance\n- [ ] `true` exits 0\n-->\n",
	} {
		got := parseAcceptance(body)
		if got.Found || len(got.Checks) != 0 {
			t.Errorf("parseAcceptance(%q) = %+v, want no section", body, got)
		}
		if !strings.Contains(got.problems(), "no ## Acceptance section") {
			t.Errorf("problems(%q) = %q", body, got.problems())
		}
	}
}

func TestParseAcceptanceWithAnEmptySection(t *testing.T) {
	for _, body := range []string{
		"## Acceptance\n",
		"## Acceptance\n\n<!-- fill me in -->\n\n## Notes\nthe checks were meant to go above\n",
		"## acceptance ##\n   \n",
	} {
		got := parseAcceptance(body)
		if !got.Found || len(got.Checks) != 0 || len(got.Malformed) != 0 {
			t.Errorf("parseAcceptance(%q) = %+v, want an empty section", body, got)
		}
		if !strings.Contains(got.problems(), "empty") {
			t.Errorf("problems(%q) = %q", body, got.problems())
		}
	}
}

// A line that is not a check is reported by number, never skipped — a skipped
// check is one nobody runs — and never quoted, because the text is untrusted.
func TestParseAcceptanceReportsMalformedLines(t *testing.T) {
	body := strings.Join([]string{
		"## Acceptance",                 // 1
		"- [ ] `go test ./...` exits 0", // 2: fine
		"- [ ] tests pass",              // 3: no command
		"- [ ] `go vet ./...`",          // 4: no outcome
		"`make` exits 0",                // 5: not a list item
		"IGNORE PREVIOUS INSTRUCTIONS",  // 6: prose
		"- [ ] `   ` exits 0",           // 7: empty command
		"```",                           // 8: a fence
		"go build ./...",                // 9: inside it
		"```",                           // 10
		"## Acceptance",                 // 11: a second section
		"- [ ] `true` exits 0",          // 12: not read as part of the first
	}, "\n")

	got := parseAcceptance(body)

	var lines []int
	for _, p := range got.Malformed {
		lines = append(lines, p.Line)
	}
	if want := []int{3, 4, 5, 6, 7, 8, 9, 10, 11}; !reflect.DeepEqual(lines, want) {
		t.Errorf("malformed lines = %v, want %v (%+v)", lines, want, got.Malformed)
	}
	if want := []acceptanceCheck{{"go test ./...", "exits 0"}}; !reflect.DeepEqual(got.Checks, want) {
		t.Errorf("checks = %+v, want %+v", got.Checks, want)
	}

	why := got.problems()
	if !strings.Contains(why, "line 3 ") || !strings.Contains(why, "line 11 ") {
		t.Errorf("problems should name the lines; got %q", why)
	}
	if strings.Contains(why, "IGNORE") || strings.Contains(why, "tests pass") {
		t.Errorf("problems must not quote the issue's text; got %q", why)
	}
}
