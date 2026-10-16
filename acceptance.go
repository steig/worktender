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
//	- [ ] `worktender version` prints `0.14.0`
//
// One check per line: a list item, optionally a task-list box, the command as
// the first inline code span, and the outcome it should give as the text after
// it. The command is delimited by backticks rather than by position or by a
// separator word because a command routinely contains spaces, pipes and the
// word "and", and an inline code span is the one delimiter markdown already
// keeps intact and GitHub already renders as a command.
//
// The outcome is free text here. Recording it is the contract; deciding what
// "exits 0" or "prints X" obliges a re-runner to check is the gate's, which is
// a separate step.

// acceptanceCheck is one line of an Acceptance section.
type acceptanceCheck struct {
	Command string
	Expect  string
}

// acceptance is what parseAcceptance found in an issue body.
//
// Malformed lines are kept apart from the checks rather than dropped, because
// a check silently skipped is a check nobody runs: a gate that re-runs the
// section verifies less than the issue says, and nothing would notice.
type acceptance struct {
	// Found is whether the body has an Acceptance heading at all, which is a
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
	// headingLine is an ATX heading: up to three spaces, one to six #s, a
	// space, and the text with any closing #s removed.
	headingLine = regexp.MustCompile(`^ {0,3}(#{1,6})(?:[ \t]+(.*?))?(?:[ \t]+#+)?[ \t]*$`)

	// checkLine is one check: a bullet, an optional task box, the command as an
	// inline code span, and at least one character of outcome after it.
	checkLine = regexp.MustCompile("^[ \t]*[-*+][ \t]+(?:\\[[ xX]\\][ \t]+)?`([^`]+)`[ \t]+(\\S.*)$")

	// fenceLine opens or closes a fenced code block, which can hold a line that
	// looks like a heading and is not one.
	fenceLine = regexp.MustCompile("^ {0,3}(```|~~~)")
)

// acceptanceHeading is the section's name. Level 2 exactly, matched without
// regard to case: the template writes it, and a person retyping it should not
// have to match capitalisation for the issue to count as specified.
const acceptanceHeading = "acceptance"

// parseAcceptance reads the Acceptance section out of an issue body.
//
// The section runs from its heading to the next heading of any level. Ending
// it at a level-3 subheading too, rather than only at the next level-2, keeps
// the rule one sentence long; nothing in the format needs a subheading inside
// it. HTML comments, which is where the template puts its guidance, are skipped
// everywhere; inside the section blank lines are too, and every other line must
// be a check.
//
// A second Acceptance section is malformed rather than merged or ignored: it is
// either a copy-paste slip or a disagreement, and both want a person.
func parseAcceptance(body string) acceptance {
	var got acceptance
	inSection, inFence, inComment := false, false, false

	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	for i, line := range lines {
		n := i + 1

		if inComment {
			if strings.Contains(line, "-->") {
				inComment = false
			}
			continue
		}
		// A fence inside the section is not skipped: whatever it holds was
		// written where checks go, and skipping it would drop them unseen.
		if fenceLine.MatchString(line) {
			inFence = !inFence
			if inSection {
				got.Malformed = append(got.Malformed, acceptanceProblem{n, "is a code fence; a check is one line"})
			}
			continue
		}
		if inFence {
			if inSection {
				got.Malformed = append(got.Malformed, acceptanceProblem{n, "is inside a code fence; a check is one line"})
			}
			continue
		}
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "<!--") {
			inComment = !strings.Contains(trimmed, "-->")
			continue
		}

		if m := headingLine.FindStringSubmatch(line); m != nil {
			isAcceptance := len(m[1]) == 2 && strings.EqualFold(strings.TrimSpace(m[2]), acceptanceHeading)
			switch {
			case isAcceptance && got.Found:
				got.Malformed = append(got.Malformed, acceptanceProblem{n, "is a second Acceptance section; an issue gets one"})
				inSection = false
			case isAcceptance:
				got.Found, inSection = true, true
			default:
				inSection = false
			}
			continue
		}
		if !inSection {
			continue
		}

		if strings.TrimSpace(line) == "" {
			continue
		}

		m := checkLine.FindStringSubmatch(line)
		if m == nil {
			got.Malformed = append(got.Malformed, acceptanceProblem{n, "is not a check: want - [ ] `command` expected outcome"})
			continue
		}
		command, expect := strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		if command == "" {
			got.Malformed = append(got.Malformed, acceptanceProblem{n, "has an empty command"})
			continue
		}
		got.Checks = append(got.Checks, acceptanceCheck{Command: command, Expect: expect})
	}
	return got
}

// problems is why an issue cleared for an agent cannot be started,
// or "" when it can.
//
// The three cases stay separate in the message because each has a different
// repair: add the section, fill it, or fix the named lines.
func (a acceptance) problems() string {
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
