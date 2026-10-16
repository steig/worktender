---
name: Cleared for an agent
about: Work an agent can take end to end, with checks that say when it is done.
title: ""
labels: []
---

<!--
Filing this does not clear it. Add the factory:ready label once the checks
below are written and somebody has read them: that label is what a queue pulls
from, and what makes `worktender start` hold the issue to its Acceptance section.
-->


## Problem

<!-- What is wrong or missing, and who it affects. -->

## Proposal

<!-- What to build. Describe the outcome; leave the approach to the worker. -->

## Acceptance

<!--
Required. `worktender start` refuses an issue labelled factory:ready when this
section is missing, empty, or has a line it cannot read.

One check per line, each a list item with the command in backticks and the
outcome it should give after it:

- [ ] `go test ./...` exits 0
- [ ] `test -f docs/acceptance.md` exits 0
- [ ] `worktender version` prints `0.14.0`

Blank lines and comments like this one are ignored. Anything else here is an
error, so put explanation under another heading. The section ends at the next
heading. See docs/dispatch.md, "Acceptance checks".
-->
