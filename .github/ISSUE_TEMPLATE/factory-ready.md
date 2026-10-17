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
How to fill this in.

ACCEPTANCE is required. `worktender start` refuses an issue labelled
factory:ready when that section is missing, empty, or has a line it cannot
read. One check per line, exactly like this:

- [ ] `go test ./...` exits 0
- [ ] `test -f docs/acceptance.md` exits 0
- [ ] `worktender doctor` exits 0
- [ ] `go test -count=1 -run TestX -v . | grep -q -- '--- PASS: TestX'` exits 0

That is "- [ ] " (or "- [x] "), the command in single backticks, a space, and
the outcome it should give, in plain words. Use printable ASCII only, and no
backtick except the two around the command. Blank lines are fine. Anything
else in the section is an error, including a comment like this one, so put
explanation under Problem or Proposal. The section ends at the next heading.

Prefer a check that fails until the work is done, in the last form above:
`worktender verify` runs each check on the merge-base and on the PR, and `go
test -run TestX` alone exits 0 with "no tests to run" before TestX exists. A
`prints X` outcome needs X to match ^[A-Za-z0-9._-]{1,40}$; any other outcome is
unjudged.

PROBLEM: what is wrong or missing, and who it affects.
PROPOSAL: what to build. Describe the outcome, and leave the approach to the
worker.

Filing this does not clear it for an agent. Someone with write access clears it
by running `worktender approve <number>`. That prints exactly what an agent
will receive, raw, and on confirmation comments its sha256 and applies the
factory:ready label. Any edit afterwards voids the approval until it is run
again. See docs/dispatch.md, "Acceptance checks".
-->
