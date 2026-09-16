# Trust

**A herdr plugin is not sandboxed.** This one runs as you, with your files, your
shell and your credentials, and what it does with them is start coding agents and
delete git worktrees and branches. That is what it is *for* rather than a side
effect — most of this README is about the guards on the deleting half — but
installing it is a decision to let code from someone else's repository do those
things on your machine, and it is worth making on purpose. The two capabilities
most worth knowing: removal needs either a merged pull request or a deleted
upstream over commits base already has, and keeps anything ambiguous; and the
hooks that would start agents without being asked are off until you turn them on.

Installing runs `scripts/build.sh`, which prefers a local Go toolchain and falls
back to a prebuilt release binary, so it works with or without Go. On Windows the
build needs Go on `PATH`.

With Go present you get the stronger of the two paths by a distance: the binary is
compiled from the source that was just cloned, so what you can read is what you
run.

Without Go, the script downloads the release matching the version in the manifest
it cloned — pinned to that tag rather than to `latest`, so reading `v0.4.1` and
installing cannot hand you something newer — and checks it against the
`checksums.txt` published alongside. A missing or mismatched checksum aborts the
install rather than warning about it, and no unverified download is left behind on
any failure path.

That check proves the download arrived intact, and nothing beyond it. The binary
and its checksum come from the same release, so both are published by whoever can
publish releases here, and there is no signature and no attestation to say who
that was. On the no-Go path you are trusting this GitHub account rather than a
proof of authorship. That is the same trust nearly all software installed from
GitHub asks for — which is a reason to say so plainly, not a reason to imply the
checksum is doing more work than it is.

## Installing without herdr

`scripts/install.sh` is the other route — the one that puts the binary on `PATH`
for someone who does not run herdr at all. It is meant to be run the way these
things are run:

```sh
curl -fsSL https://raw.githubusercontent.com/steig/worktender/main/scripts/install.sh | sh
```

**That line is a decision, and it is worth naming what it grants.** You are
executing a script fetched from this repository's default branch before you have
anything that could verify it — `curl | sh` has no equivalent of the checksum the
script itself then applies to the binary. Whoever can push to `main` here can
change what that command does. The mitigation is not technical and there is no
point dressing it up as one: open the URL and read the script first. It is short,
it is deliberately boring, and it is the same file the one-liner runs.

What the script does do, once you have run it:

- **It resolves one release and pins both halves to it.** `releases/latest` is
  followed once, to a tag, and the binary and the `checksums.txt` are then both
  fetched from that tag — so a release landing mid-install cannot hand you a
  binary and somebody else's checksums. `--version` pins it yourself, which is
  what you want when you are reproducing an environment rather than starting one.
- **It verifies before it makes anything executable, and fails closed.** The
  download is staged under a dotted name inside the destination directory, never
  at `worktender`, and is `chmod +x`'d only after the published SHA-256 matches.
  A missing checksum line is as fatal as a mismatched one. Every failure path
  removes the staged file, which is a trap rather than a cleanup line at the
  bottom — `set -e` inside a download exits before any line below it runs.
- **The last step is a rename within one filesystem**, so nothing ever observes a
  half-written binary at the path it is about to run.
- **It never asks for sudo.** `~/.local/bin` is the user's own. A script that
  curls from the network and then asks for root is asking for two kinds of trust
  to answer one question.

The ceiling is exactly the ceiling of the no-Go plugin path above, and for the
same reason: the binary and its checksum come from the same release, published by
whoever can publish releases here. There is no signature and no attestation. If
you want the stronger story, install Go and build this repository — or install as
a herdr plugin on a machine that has Go, which does it for you.

## Upgrading

A standalone install has no plugin checkout, so `worktender update` has nothing
to fetch and rebuild and says so rather than improvising. Re-running the
installer is the upgrade, and `worktender doctor` names the release you are on so
you can tell whether you need to.

See [SECURITY.md](../SECURITY.md) for the trust boundary in full, and for how to
report something privately.


---

[← README](../README.md)
