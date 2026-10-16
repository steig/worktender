package main

import (
	"fmt"
	"regexp"
	"strings"
)

// An issue cleared for an agent states its "done" as checks a machine can run,
// not as prose a coordinator has to interpret. This file is the contract's
// reading half: where the checks live in an issue body, and what one looks like.
//
// The format, which .github/ISSUE_TEMPLATE/factory-ready.md and
// docs/dispatch.md both document:
//
//	## Acceptance
//
//	- [ ] `go test ./...` exits 0
//	- [ ] `test -f docs/acceptance.md` exits 0
//	- [ ] `worktender version` prints 0.14.0
//
// THE GRAMMAR IS AN ALLOW-LIST, AND IT IS STRICT ON PURPOSE. A person approves
// an issue as GitHub renders it; the agent runs the commands from the raw
// markdown. Anything in a check that the rendered page does not show — a
// comment, a tag, an entity, a zero-width or bidi rune — would be a command
// nobody approved. An earlier version tried to reduce the raw body to what the
// page shows, and every rule it added met a renderer behaviour it had not
// modelled. So instead the section admits only text that renders as itself:
//
//   - The heading is exactly `## Acceptance`, at the start of a line, outside a
//     code fence. Nothing before it contains `<`: an HTML comment or a
//     <details> opened earlier and never closed would fold the section away
//     while every line in it still parsed.
//   - Every non-blank line until the next heading is `- [ ] ` or `- [x] `, the
//     command in single backticks, one space, and the expected outcome.
//   - Command and outcome are printable ASCII, 0x20 to 0x7E — no tab, no rune
//     outside it. Neither holds a backtick, so the command's span cannot end
//     early and the outcome cannot hold a second span that reads as a command;
//     the line holds no `<`, `>` or `&`, so no tag, comment or entity can form.
//   - Any other line refuses the whole issue, by line number. A check skipped
//     is a check nobody runs.
//
// Every character such a line can hold is drawn on the page literally, so what
// the approver read is byte for byte what will run. The cost is that a command
// cannot use `&&`, `>` or `<`; two checks, or a script in the repository, do
// the same job visibly.
//
// Only the checks are held to this. The rest of the body is prose the worker is
// told is untrusted data, exactly as for any issue without the label.
//
// The outcome is recorded, not interpreted. Deciding what "exits 0" or
// "prints X" obliges a re-runner to check is the gate's, a separate step.

// acceptanceCheck is one line of an Acceptance section.
type acceptanceCheck struct {
	Command string
	Expect  string
}

// acceptanceSection is what parseAcceptance found in an issue body.
type acceptanceSection struct {
	// Found is whether the body has the Acceptance heading at all, which is a
	// different repair from having one with nothing under it.
	Found     bool
	Checks    []acceptanceCheck
	Malformed []acceptanceProblem
}

// acceptanceProblem names a line by number and says what is wrong with it. It
// does not carry the line's text: the issue body is untrusted, and this reaches
// a coordinating agent's stderr, which is where instructions go.
type acceptanceProblem struct {
	Line   int
	Reason string
}

func (p acceptanceProblem) String() string {
	return fmt.Sprintf("line %d %s", p.Line, p.Reason)
}

var (
	// headingLine is an ATX heading, which ends the section.
	headingLine = regexp.MustCompile(`^ {0,3}#{1,6}(?:[ \t]|$)`)

	// checkLine is one check. The character classes are the whole grammar:
	// printable ASCII less the backtick, <, > and &, in both halves.
	checkLine = regexp.MustCompile("^- \\[[ x]\\] `([\\x20-\\x25\\x27-\\x3b=\\x3f-\\x5f\\x61-\\x7e]+)` ([\\x20-\\x25\\x27-\\x3b=\\x3f-\\x5f\\x61-\\x7e]+)$")

	// fenceLine opens or closes a fenced code block, which can hold a line that
	// looks like the heading and is not one.
	fenceLine = regexp.MustCompile("^ {0,3}(```|~~~)")
)

// acceptanceHeading is the section's heading line, exactly.
const acceptanceHeading = "## Acceptance"

// parseAcceptance reads the Acceptance section out of an issue body.
//
// A second Acceptance heading is malformed rather than merged or ignored: it
// is either a copy-paste slip or a disagreement, and both want a person.
func parseAcceptance(body string) acceptanceSection {
	var got acceptanceSection
	inSection, inFence, seenAngle := false, false, false

	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i, line := range lines {
		n := i + 1

		if inSection {
			switch {
			case strings.TrimSpace(line) == "":
			case strings.TrimRight(line, " ") == acceptanceHeading:
				got.Malformed = append(got.Malformed, acceptanceProblem{n, "is a second Acceptance heading; an issue gets one"})
			case headingLine.MatchString(line):
				inSection = false
			default:
				m := checkLine.FindStringSubmatch(line)
				if m == nil || strings.TrimSpace(m[1]) == "" || strings.TrimSpace(m[2]) == "" {
					got.Malformed = append(got.Malformed, acceptanceProblem{n,
						"is not a check: want - [ ] `command` expected outcome, in printable ASCII, " +
							"with no backtick in the command and no <, > or & anywhere"})
					continue
				}
				got.Checks = append(got.Checks, acceptanceCheck{Command: strings.TrimSpace(m[1]), Expect: strings.TrimSpace(m[2])})
			}
			continue
		}

		// Counted before the fence test, not after it: a < inside what this
		// reader takes for a fence is still a < to a renderer that disagrees
		// about where the fence is, and the two need not agree.
		if strings.Contains(line, "<") {
			seenAngle = true
		}
		if fenceLine.MatchString(line) {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if strings.TrimRight(line, " ") == acceptanceHeading {
			if got.Found {
				got.Malformed = append(got.Malformed, acceptanceProblem{n, "is a second Acceptance heading; an issue gets one"})
				continue
			}
			if seenAngle {
				got.Malformed = append(got.Malformed, acceptanceProblem{n,
					"is the Acceptance heading with a < somewhere above it, which could open HTML that hides the section; " +
						"put the section first, or keep < out of what precedes it"})
			}
			got.Found, inSection = true, true
		}
	}
	return got
}

// problems is why an issue cleared for an agent cannot be started, or "" when
// it can.
//
// The three cases stay separate in the message because each has a different
// repair: add the section, fill it, or fix the named lines.
func (a acceptanceSection) problems() string {
	switch {
	case !a.Found:
		return "has no ## Acceptance section"
	case len(a.Malformed) > 0:
		parts := make([]string, len(a.Malformed))
		for i, p := range a.Malformed {
			parts[i] = p.String()
		}
		return "has an Acceptance section a gate cannot run: " + strings.Join(parts, "; ")
	case len(a.Checks) == 0:
		return "has an empty ## Acceptance section"
	}
	return ""
}

// hasLabel reports whether an issue carries a label, by exact name.
func (i issue) hasLabel(name string) bool {
	for _, l := range i.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}
