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

# install.sh downloads checksums.txt from whatever latest resolves to and
# refuses to install without it. Promoting a release that release.yml never
# finished publishing to (#182) would turn every fresh install into a failure.
if ! gh release view "$tag" --json assets --jq '.assets[].name' | grep -qx checksums.txt; then
	echo "release-promote: $tag has no checksums.txt asset; refusing to make it latest" >&2
	exit 1
fi

gh release edit "$tag" --prerelease=false --latest
