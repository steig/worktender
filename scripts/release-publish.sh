#!/bin/sh
# Attach release.yml's build output to the GitHub release for a tag.
#
# Usage: scripts/release-publish.sh <tag> <asset>...
#
# This exists as a script rather than as a `run:` block because the decision it
# makes is the whole point of it, and a decision buried in workflow YAML can
# only be tested by cutting a real release. See release_publish_test.go.
#
# The rule it encodes: this workflow is not the only thing that creates releases
# for this repository. A release object with hand-written notes is sometimes
# created from a laptop, which is also what pushes the tag that starts the
# workflow — so by the time the build finishes, the release usually already
# exists. `gh release create` is fatal in that case:
#
#	a release with the same tag name already exists: v0.11.2
#
# That is exactly what happened to every release from v0.10.0 through v0.11.2
# (#182). `build` and `checksums` passed, `publish` failed, and the releases
# were published carrying no assets at all — which silently breaks
# scripts/build.sh's no-Go fallback, because it downloads
# releases/download/v<version>/worktender_<os>_<arch> and got a 404.
#
# So: creating the release is best-effort and only happens when nothing has
# created one yet, and uploading the assets is what this script is actually for.
# Notes already on an existing release are never touched — whoever wrote them
# meant them, and --generate-notes would replace a curated CHANGELOG excerpt
# with a commit list.
#
# A release this script creates is a prerelease, and so is a draft it finds.
# releases/latest skips prereleases, so scripts/install.sh keeps handing out the
# last promoted tag while this one soaks; scripts/release-promote.sh is what
# makes it latest. A release that is already stable is left alone: it is either
# one release-promote.sh promoted, which a re-run of this workflow must not
# quietly move releases/latest off, or one created from a laptop without
# --prerelease, which has already reached installers and is warned about.
set -eu

if [ "$#" -lt 2 ]; then
	echo "usage: release-publish.sh <tag> <asset>..." >&2
	exit 1
fi

tag="$1"
shift

# The caller is release.yml passing an unquoted `dist/*`, so an empty or
# missing dist/ does not arrive here as zero arguments — the shell leaves the
# glob unexpanded and this script is handed the literal string "dist/*" as one
# perfectly well-formed argument. Counting arguments therefore says nothing.
# Every asset has to actually be a file, or the upload below is a release job
# that succeeds having published nothing, which is the failure this script
# exists to end.
for asset in "$@"; do
	if [ ! -f "$asset" ]; then
		echo "release-publish: $asset is not a file; refusing to publish $tag with assets missing" >&2
		exit 1
	fi
done

# `gh release view` is the only way to ask whether a release exists; it exits
# non-zero when there is none, and prints "<isDraft> <isPrerelease>" when there
# is one. Its stderr matters too: a token problem or a network hiccup fails the
# same way "no release" does, and throwing the message away turns a diagnosable
# outage into a confusing `create` failure further down. So it is captured and
# printed rather than sunk into /dev/null.
view_err=$(mktemp)
trap 'rm -f "$view_err"' EXIT

if state=$(gh release view "$tag" --json isDraft,isPrerelease --jq '"\(.isDraft) \(.isPrerelease)"' 2>"$view_err"); then
	case "$state" in
	"false false")
		# Workflow commands are read from stdout, so the annotation goes there.
		echo "::warning::release-publish: $tag is already a stable release; leaving it stable rather than moving releases/latest off it, and uploading assets to it"
		;;
	*" false")
		echo "release-publish: $tag is a draft; marking it a prerelease and uploading assets to it" >&2
		gh release edit "$tag" --prerelease
		;;
	*)
		echo "release-publish: $tag is already a prerelease; uploading assets to it" >&2
		;;
	esac
else
	echo "release-publish: no release for $tag yet ($(tr -d '\n' <"$view_err")); creating one" >&2

	# Deliberately not fatal. `view` then `create` is check-then-act, and two
	# runs of this workflow for the same tag — a manual re-run, a retried job —
	# can both see no release; the loser's create then fails with the very "a
	# release with the same tag name already exists" that #182 is about, and
	# under `set -e` that would end the script without uploading anything.
	#
	# Whatever the reason create failed, the next line is the one that has to
	# work, and it is the honest test of whether a release is there to upload
	# to. A create that failed for a real reason — no token, no permission —
	# fails the upload too, with a message about the thing that actually
	# matters. Whatever won the race made the release, prerelease or not, and
	# this run does not second-guess it any more than it does on the path above.
	if ! gh release create "$tag" --prerelease --generate-notes --title "$tag"; then
		echo "release-publish: creating $tag failed; trying the upload anyway in case something else created it" >&2
	fi
fi

# --clobber rather than a bare upload: a re-run of a failed workflow, or a
# second push of the same tag, must converge on the assets this build produced
# instead of failing on an asset name that is already there. The upload is the
# step that has to succeed, so it is deliberately the last thing here and its
# failure is the script's failure.
gh release upload "$tag" "$@" --clobber
