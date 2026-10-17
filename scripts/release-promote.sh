#!/bin/sh
# Make a soaked prerelease the release every installer gets.
#
# Usage: scripts/release-promote.sh <tag>
#
# release.yml publishes every tag as a prerelease (see release-publish.sh), so
# a tag reaches only installs that pin it — a plugin checkout at that commit,
# whose scripts/build.sh downloads releases/download/v<manifest version>. Once
# that binary has run clean factory passes, this marks the release latest, and
# scripts/install.sh starts resolving to it.
#
# Rollback is the same command with the previous tag: --latest is explicit, so
# GitHub points releases/latest at it even though a newer release exists.
set -eu

if [ "$#" -ne 1 ]; then
	echo "usage: release-promote.sh <tag>" >&2
	exit 1
fi

tag="$1"
case "$tag" in
v[0-9]*) ;;
*)
	echo "release-promote: $tag is not a release tag (v<version>)" >&2
	exit 1
	;;
esac

# First line isDraft, then one asset name per line.
if ! view=$(gh release view "$tag" --json isDraft,assets --jq '.isDraft, .assets[].name'); then
	echo "release-promote: could not read the release for $tag" >&2
	exit 1
fi
if [ "$(printf '%s\n' "$view" | head -n 1)" = true ]; then
	echo "release-promote: $tag is a draft; publish it before making it latest" >&2
	exit 1
fi
names=$(printf '%s\n' "$view" | tail -n +2)

# install.sh downloads checksums.txt and the binary it names from whatever
# latest resolves to, and refuses to install without both. A release that
# release.yml never finished uploading to (#182) would turn every fresh install
# into a failure, so checksums.txt has to be there — the download fails when it
# is not — and so does every file it names.
if ! sums=$(gh release download "$tag" --pattern checksums.txt --output -); then
	echo "release-promote: could not download checksums.txt for $tag; refusing to make it latest" >&2
	exit 1
fi
if [ -z "$sums" ]; then
	echo "release-promote: $tag's checksums.txt names no assets; refusing to make it latest" >&2
	exit 1
fi
for asset in $(printf '%s\n' "$sums" | awk '{ sub(/^\*/, "", $2); print $2 }'); do
	if ! printf '%s\n' "$names" | grep -qxF "$asset"; then
		echo "release-promote: $tag's checksums.txt names $asset, which is not uploaded; refusing to make it latest" >&2
		exit 1
	fi
done

gh release edit "$tag" --prerelease=false --latest
