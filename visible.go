package main

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/steig/worktender/internal/safetext"
)

// A person approves an issue as GitHub renders it. The agent gets the raw
// markdown. Whatever the raw text holds that the rendered page does not show is
// content nobody approved, arriving at the one reader that acts on it.
//
// So a ready issue's title and body are reduced to what a reader saw, or
// refused when that cannot be done exactly:
//
//   - HTML comments are STRIPPED. They render as nothing, so removing them
//     makes the raw text equal to the page rather than shorter than it — and
//     the factory-ready template's own guidance lives in comments, so refusing
//     them would refuse every issue filed from it. A comment left open hides the
//     rest of the page and is refused.
//   - Raw HTML tags are REFUSED. <details> folds content away, a style can hide
//     it, and telling which tags show their content would mean reimplementing
//     GitHub's sanitiser. Refusing costs an issue author one edit.
//   - Link reference definitions are REFUSED. `[x]: url` lines render as
//     nothing, and unlike a comment they are not something the template uses.
//   - Control, format and separator runes — zero-width and bidi among them —
//     are REFUSED, by safetext's one definition of them. Newline, carriage
//     return and tab are the exceptions a body is made of. Escaping them
//     instead would change what the approver saw into something else.
//
// Code is exempt from the comment and tag rules, because inside a fence or an
// inline code span GitHub draws `<!--` and `<b>` literally. Code is not exempt
// from the rune rule: a zero-width space is invisible in code too.

var (
	// htmlTag is an opening, closing or self-closing tag. An autolink such as
	// <https://example.com> has a colon where a tag name ends, so it is not one.
	htmlTag = regexp.MustCompile(`</?[A-Za-z][A-Za-z0-9-]*(?:\s[^<>]*)?/?>`)

	// linkRefDef is a link reference definition, which renders as nothing.
	linkRefDef = regexp.MustCompile(`^ {0,3}\[[^\]]+\]:`)
)

// visibleText is raw issue markdown reduced to what its rendered page shows,
// or the reasons it cannot be. The reasons name a kind and a line, never the
// text, which is untrusted.
func visibleText(raw string) (string, []string) {
	var problems []string
	note := func(line int, format string, a ...any) {
		problems = append(problems, fmt.Sprintf("line %d %s", line, fmt.Sprintf(format, a...)))
	}

	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(raw, "\n")
	for i, line := range lines {
		for _, r := range line {
			if safetext.IsUnsafe(r) && r != '\t' {
				note(i+1, "has an invisible or control character U+%04X", r)
				break
			}
		}
	}

	var out strings.Builder
	inFence, inComment, commentLine := false, false, 0
	for i, line := range lines {
		n := i + 1
		if i > 0 {
			out.WriteByte('\n')
		}
		if !inComment && fenceLine.MatchString(line) {
			inFence = !inFence
			out.WriteString(line)
			continue
		}
		if inFence {
			out.WriteString(line)
			continue
		}

		// prose is the line outside comments and code spans: what the tag and
		// definition rules are applied to.
		var prose strings.Builder
		for rest := line; rest != ""; {
			if inComment {
				end := strings.Index(rest, "-->")
				if end < 0 {
					rest = ""
					continue
				}
				rest, inComment = rest[end+len("-->"):], false
				continue
			}
			if strings.HasPrefix(rest, "<!--") {
				inComment, commentLine, rest = true, n, rest[len("<!--"):]
				continue
			}
			if strings.HasPrefix(rest, "`") {
				ticks := len(rest) - len(strings.TrimLeft(rest, "`"))
				fence := rest[:ticks]
				if end := strings.Index(rest[ticks:], fence); end >= 0 {
					span := rest[:ticks+end+ticks]
					out.WriteString(span)
					rest = rest[len(span):]
					continue
				}
				// No closing run: the backticks are literal text.
				out.WriteString(fence)
				prose.WriteString(fence)
				rest = rest[ticks:]
				continue
			}
			next := strings.IndexAny(rest[1:], "<`")
			chunk := rest
			if next >= 0 {
				chunk = rest[:next+1]
			}
			out.WriteString(chunk)
			prose.WriteString(chunk)
			rest = rest[len(chunk):]
		}

		if htmlTag.MatchString(prose.String()) {
			note(n, "has raw HTML, which can hide content from the page the approver read")
		}
		if linkRefDef.MatchString(prose.String()) {
			note(n, "is a link reference definition, which the page does not show")
		}
	}
	if inComment {
		note(commentLine, "opens an HTML comment that never closes, hiding the rest of the page")
	}
	return out.String(), problems
}

// visibleTitle is a title as GitHub draws it: plain text, so only the rune
// rule applies, and a tab is as invisible as anything else there.
func visibleTitle(raw string) []string {
	for _, r := range raw {
		if safetext.IsUnsafe(r) {
			return []string{fmt.Sprintf("the title has an invisible or control character U+%04X", r)}
		}
	}
	return nil
}
