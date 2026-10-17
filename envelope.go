package main

import (
	"strconv"
	"strings"
	"unicode"
)

// Reading an envelope back out of a terminal is the other half of report.go's
// format: it accepts exactly what renderReport writes and re-runs every slot
// through the validators parseReport uses.
//
// That closes the forged-frame hole at this end. reportNote refuses a newline,
// so a note can never open a line of its own, and a candidate whose note fails
// reportNote is not an envelope at all.
//
// It does not authenticate the author. A pane is the worker's own output
// channel, so a worker that prints seven crafted lines gets an envelope that
// parses. See gate.go.

// envelopeFrame is the fixed shape renderReport emits, derived from the same
// constants so the reader cannot drift from the writer.
var envelopeFrame = func() (announce []string, lines int) {
	announce = strings.Split(noteOpen, "\n")
	// header, status, pr, sha, the announcement, the quoted note, the terminator.
	return announce, slotLines + len(announce) + 2
}

// slotLines is the header and the three slots that precede the announcement.
const slotLines = 4

// envelopesIn returns the last complete envelope in a terminal snapshot, and
// how many the snapshot holds. Last, not first: a pane accumulates, so the
// later envelope is the answer that stands.
//
// The count is what lets a gate tell a new report from one it already judged.
// An envelope's identity here is its position, so two byte-identical envelopes
// are two reports. A match consumes its whole frame rather than one line;
// envelopes cannot overlap, because no line inside one can pass isHeaderLine.
func envelopesIn(text string) (report, uint64) {
	lines := splitTerminalLines(text)
	_, height := envelopeFrame()

	var last report
	var count uint64
	for start := 0; start+height <= len(lines); {
		r, ok := parseEnvelope(lines[start : start+height])
		if !ok {
			start++
			continue
		}
		last, count = r, count+1
		start += height
	}
	return last, count
}

// parseEnvelope reads one candidate window of lines, all-or-nothing.
func parseEnvelope(lines []string) (report, bool) {
	announce, height := envelopeFrame()
	if len(lines) != height {
		return report{}, false
	}
	if !isHeaderLine(lines[0]) || lines[height-1] != noteClose {
		return report{}, false
	}
	for i, want := range announce {
		if lines[slotLines+i] != want {
			return report{}, false
		}
	}

	status, ok := strings.CutPrefix(lines[1], "status: ")
	if !ok || !isReportStatus(status) {
		return report{}, false
	}

	pr, ok := parsePRSlot(lines[2])
	if !ok {
		return report{}, false
	}

	raw, ok := strings.CutPrefix(lines[3], "sha: ")
	if !ok {
		return report{}, false
	}
	sha, ok := parseShaValue(raw)
	if !ok {
		return report{}, false
	}

	note, ok := strings.CutPrefix(lines[height-2], noteQuote)
	if !ok || reportNote(note) != nil {
		return report{}, false
	}

	return report{status: status, pr: pr, sha: sha, note: note}, true
}

// isHeaderLine finds the header under the decoration an agent TUI stamps on the
// first line of a message — Claude Code renders it as "⏺ worktender-report v1",
// so a parser demanding the bare header reads nothing from the pane of the very
// agent this exists to gate.
//
// The allowance is bounded to decoration: whatever precedes the header may hold
// no letter and no digit, so a sentence mentioning the header does not open a
// frame.
func isHeaderLine(line string) bool {
	prefix, ok := strings.CutSuffix(line, reportHeader)
	if !ok {
		return false
	}
	return !strings.ContainsFunc(prefix, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsDigit(r)
	})
}

// parsePRSlot reads the pr line.
func parsePRSlot(line string) (int, bool) {
	raw, ok := strings.CutPrefix(line, "pr: ")
	if !ok {
		return 0, false
	}
	return parsePRValue(raw)
}

// parsePRValue reads the slot's value, wherever it arrived from, where the dash
// means the worker gave none. Both channels read it through here so a pr that a
// terminal would refuse cannot be smuggled in as a metadata token instead.
func parsePRValue(raw string) (int, bool) {
	if raw == missing {
		return 0, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// splitTerminalLines normalises a snapshot into lines that can be compared for
// equality. Surrounding whitespace goes because a terminal supplies its own —
// cells padded to the pane width, and an agent TUI indenting command output.
//
// It costs the frame nothing: the claim that a note cannot begin a line is
// enforced at write time by reportNote, not by which column a line starts in.
func splitTerminalLines(text string) []string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(line)
	}
	return lines
}
