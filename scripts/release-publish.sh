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
set -eu

if [ "$#" -lt 2 ]; then
	echo "usage: release-publish.sh <tag> <asset>..." >&2
	exit 1
fi

tag="$1"
shift

# `gh release view` is the only way to ask whether a release exists; it exits
# non-zero when there is none. Its output is noise here — the exit code is the
# answer — but stderr is kept quiet only for the not-found case, which is not
# an error condition from where this script stands.
if gh release view "$tag" >/dev/null 2>&1; then
	echo "release-publish: $tag already has a release; uploading assets to it" >&2
else
	echo "release-publish: no release for $tag yet; creating one" >&2
	gh release create "$tag" --generate-notes --title "$tag"
fi

# --clobber rather than a bare upload: a re-run of a failed workflow, or a
# second push of the same tag, must converge on the assets this build produced
# instead of failing on an asset name that is already there. The upload is the
# step that has to succeed, so it is deliberately the last thing here and its
# failure is the script's failure.
gh release upload "$tag" "$@" --clobber
