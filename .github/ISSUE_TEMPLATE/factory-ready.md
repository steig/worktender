---
name: Cleared for an agent
about: Work an agent can take end to end, with checks that say when it is done.
title: ""
labels: []
---

## Acceptance

## Problem

## Proposal

<!--
How to fill this in. This comment sits at the end on purpose: nothing above the
Acceptance heading may contain a "<", because HTML opened there could hide the
checks from whoever approves the issue.

ACCEPTANCE is required. `worktender start` refuses an issue labelled
factory:ready when that section is missing, empty, or has a line it cannot
read. One check per line, exactly like this:

- [ ] `go test ./...` exits 0
- [ ] `test -f docs/acceptance.md` exits 0
- [ ] `worktender version` prints 0.14.0

That is "- [ ] " (or "- [x] "), the command in single backticks, a space, and
the outcome it should give, in plain words. Printable ASCII only. No backtick
except the two around the command, and no <, > or & anywhere on the line. So a
command cannot use && or redirection; write two checks, or a script in the
repository. Blank lines are fine. Anything else in the section is an error,
so put explanation under Problem or Proposal. The section ends at the next
heading. See docs/dispatch.md, "Acceptance checks".

PROBLEM: what is wrong or missing, and who it affects.
PROPOSAL: what to build. Describe the outcome, and leave the approach to the
worker.

Filing this does not clear it. Add the factory:ready label once the checks are
written and somebody with write access has read them. That label is what a
queue pulls from. It is also what makes `worktender start` hold the issue to
its Acceptance section, and any edit after it is applied voids it.
-->
