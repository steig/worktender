#!/bin/sh
# Install worktender onto PATH, without herdr and without Go.
#
# `herdr plugin install` is the other route, and it is the better one if you run
# herdr: it clones this repository and builds from the source you can read. This
# script exists because `ls`, `prune` and `prune-apply` need no herdr at all, and
# until there was a way to get the binary without installing herdr first that was
# an unreachable capability.
#
#   curl -fsSL https://raw.githubusercontent.com/steig/worktender/main/scripts/install.sh | sh
#
# Options, which need `sh -s --` when the script arrives down a pipe:
#
#   curl -fsSL .../install.sh | sh -s -- --version v0.11.2 --dir ~/bin
#
# Deliberately not a sibling of scripts/build.sh. That script serves herdr's
# installer — it builds into the plugin checkout it was cloned into, and pins its
# download to the manifest sitting beside it. Neither fact is available here, so
# the two answer the same question from different evidence and sharing code
# between them would mean one of them reasoning from the other's premises.
set -eu

REPO="steig/worktender"

# Where the binary goes. ~/.local/bin rather than /usr/local/bin: it is on the
# default PATH of most modern distributions, it is the user's own, and it means
# this script never wants sudo. A script that curls from the network and then
# asks for root is asking for two kinds of trust to answer one question.
DEFAULT_DIR="$HOME/.local/bin"

# Where release assets are fetched from. Overridable for a mirror or an offline
# copy of a release; the layout it must serve is GitHub's — `latest` redirecting
# to `tag/vX.Y.Z`, and `download/vX.Y.Z/<asset>`.
BASE="${WORKTENDER_DOWNLOAD_BASE:-https://github.com/$REPO/releases}"

dir="${WORKTENDER_INSTALL_DIR:-$DEFAULT_DIR}"
version=""

die() {
	echo "worktender: $1" >&2
	exit 1
}

usage() {
	cat <<'USAGE'
usage: install.sh [--version <vX.Y.Z>] [--dir <path>]

  --version  release to install; the default resolves the newest one
  --dir      where to put the binary (default: ~/.local/bin)

Environment: WORKTENDER_INSTALL_DIR, WORKTENDER_DOWNLOAD_BASE.
USAGE
}

while [ $# -gt 0 ]; do
	case "$1" in
	--version)
		[ $# -ge 2 ] || die "--version needs a value"
		version="$2"
		shift 2
		;;
	--version=*)
		version="${1#--version=}"
		shift
		;;
	--dir)
		[ $# -ge 2 ] || die "--dir needs a value"
		dir="$2"
		shift 2
		;;
	--dir=*)
		dir="${1#--dir=}"
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	*)
		usage >&2
		die "unknown argument $1"
		;;
	esac
done

case "$(uname -s)" in
Darwin) os="darwin" ;;
Linux) os="linux" ;;
*) die "unsupported OS $(uname -s); the release publishes a windows binary, so download worktender_windows_amd64.exe from https://github.com/$REPO/releases yourself" ;;
esac

case "$(uname -m)" in
arm64 | aarch64) arch="arm64" ;;
x86_64 | amd64) arch="amd64" ;;
*) die "unsupported architecture $(uname -m); install Go and \`go build\` this repository instead" ;;
esac

# Both tools are checked before anything is downloaded. Discovering there is no
# way to verify a file only after fetching it is how an unverified binary ends up
# looking like the sensible thing to keep.
if command -v curl >/dev/null 2>&1; then
	http="curl"
elif command -v wget >/dev/null 2>&1; then
	http="wget"
else
	die "neither curl nor wget available"
fi

if command -v sha256sum >/dev/null 2>&1; then
	sha256="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
	sha256="shasum -a 256"
else
	die "no sha256 tool to verify the download with; install one, or install Go and build from source"
fi

# fetch downloads one URL, or dies saying which one.
#
# Both branches say so themselves rather than leaning on `set -e` to end the
# script silently. curl under -s still writes its own reason to stderr and wget
# under -q writes nothing at all, so -nv is what keeps the two halves reporting
# the same amount — a 404 that exits 1 with no output is the worst failure this
# script has, because every other one names itself.
fetch() {
	case "$http" in
	curl) curl -fsSL "$1" -o "$2" || die "could not fetch $1" ;;
	wget) wget -nv -O "$2" "$1" || die "could not fetch $1" ;;
	esac
}

# resolve_latest turns `releases/latest` into the tag it currently points at.
#
# Both halves of a release are then pinned to that one tag. Fetching each from
# `latest/download/...` separately would be two resolutions, and a release landing
# between them would hand over a binary and somebody else's checksums — which
# fails closed, correctly, and is an infuriating thing to debug.
#
# It also means this script can SAY what it installed. A version nobody printed is
# a version nobody can report in an issue.
#
# The redirect rather than the GitHub API: no token, no rate limit, and the
# answer is the same one a browser gets.
resolve_latest() {
	case "$http" in
	curl)
		curl -fsSL -o /dev/null -w '%{url_effective}' "$BASE/latest"
		;;
	wget)
		# wget has no --write-out, so the redirect comes back out of its own
		# header trace. The trace is CAPTURED before it is parsed, deliberately:
		# piping wget straight into awk makes the pipeline's status awk's, so a
		# wget that failed outright — no network, no such host, a 404 — would be
		# indistinguishable from a resolution that simply found no Location, and
		# the caller's "could not ask which release is newest" could never fire.
		latest_headers=$(wget -S --spider -O /dev/null "$BASE/latest" 2>&1) || return 1
		printf '%s\n' "$latest_headers" | awk 'tolower($1) == "location:" { print $2 }' | tail -1
		;;
	esac
}

if [ -n "$version" ]; then
	tag="v${version#v}"
else
	resolved=$(resolve_latest) || die "could not ask $BASE which release is newest; pass --version to pin one"
	tag="${resolved##*/}"
	case "$tag" in
	v[0-9]*) ;;
	*) die "$BASE/latest did not resolve to a release tag (got \"$resolved\"); pass --version to pin one" ;;
	esac
fi

asset="worktender_${os}_${arch}"
release="$BASE/download/$tag"

mkdir -p "$dir" || die "cannot create $dir"

# Staged inside the destination directory, not in /tmp: the last step is then a
# rename within one filesystem, which is atomic, so nothing ever observes a
# half-written binary at the path it is about to run. The stage is a dotted
# directory rather than the final name, so an unverified download is never
# sitting at $dir/worktender even for an instant, and it is never made executable
# until the checksum has matched.
#
# The trap is the same rule scripts/build.sh learned: `set -e` inside fetch exits
# before any cleanup line below it, so cleanup has to be armed rather than
# written at the bottom.
stage=$(mktemp -d "$dir/.worktender-install.XXXXXX") || die "cannot stage a download in $dir"
trap 'rm -rf "$stage"' EXIT

echo "worktender: fetching $asset $tag" >&2
fetch "$release/$asset" "$stage/$asset"
fetch "$release/checksums.txt" "$stage/checksums.txt"

# A missing or mismatched checksum is fatal rather than a warning: the next line
# would make this file executable and put it on your PATH.
#
# What it proves and what it does not: the binary and checksums.txt come from the
# same release, so this says the download arrived intact and says nothing about
# who published it. There is no signature to check. See docs/trust.md.
expected=$(awk -v want="$asset" '$2 == want || $2 == "*"want {print $1}' "$stage/checksums.txt")
[ -n "$expected" ] || die "no checksum published for $asset in $tag; refusing to install it"

actual=$($sha256 "$stage/$asset" | awk '{print $1}')
if [ "$expected" != "$actual" ]; then
	echo "worktender: checksum mismatch for $asset" >&2
	echo "  expected $expected" >&2
	echo "  actual   $actual" >&2
	exit 1
fi

chmod +x "$stage/$asset"
mv -f "$stage/$asset" "$dir/worktender"
rm -rf "$stage"
trap - EXIT

echo "worktender: installed $tag to $dir/worktender"
echo "worktender: sha256 $actual"

case ":$PATH:" in
*":$dir:"*) ;;
*)
	echo >&2
	echo "worktender: $dir is not on your PATH. Add it to your shell profile:" >&2
	echo "  export PATH=\"$dir:\$PATH\"" >&2
	;;
esac
