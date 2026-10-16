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
//	- [ ] `worktender doctor` exits 0
//
// THE GRAMMAR IS A PARSER, NOT A SECURITY BOUNDARY. What makes the commands
// safe to hand an agent is that a person approved them byte for byte with
// `worktender approve` (see approval.go). The grammar exists so that "the
// commands" is well defined — one parse, the same for the approver, start and
// a gate — and it is strict so that nothing ambiguous gets that far:
//
//   - The heading is exactly `## Acceptance`, at the start of a line, outside a
//     code fence. It runs to the next heading.
//   - Every non-blank line in it is `- [ ] ` or `- [x] `, the command in single
//     backticks, one space, and the expected outcome.
//   - Command and outcome are printable ASCII, 0x20 to 0x7E: no tab, no rune
//     outside it. Neither holds a backtick, so where the command ends is never
//     in doubt and the outcome cannot hold a second span that reads as one.
//   - Any other line refuses the whole issue, by line number. A check skipped
//     is a check nobody runs.
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
	// printable ASCII less the backtick, in both halves.
	checkLine = regexp.MustCompile("^- \\[[ x]\\] `([\\x20-\\x5f\\x61-\\x7e]+)` ([\\x20-\\x5f\\x61-\\x7e]+)$")

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
	inSection, inFence := false, false

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
						"is not a check: want - [ ] `command` expected outcome, in printable ASCII " +
							"with no other backtick"})
					continue
				}
				got.Checks = append(got.Checks, acceptanceCheck{Command: strings.TrimSpace(m[1]), Expect: strings.TrimSpace(m[2])})
			}
			continue
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
