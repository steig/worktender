package main

import "testing"

// scripts/release-promote.sh is the only step that moves releases/latest now
// that release.yml publishes prereleases. It must flip both halves — not a
// prerelease, and latest — or install.sh keeps resolving the old tag.
func TestPromoteMarksTheReleaseLatest(t *testing.T) {
	calls, err := runReleaseScript(t, "scripts/release-promote.sh",
		gh{releaseExists: true, assets: "worktender_linux_amd64\nchecksums.txt\n"}, "v1.2.3")
	if err != nil {
		t.Fatalf("release-promote.sh failed: %v", err)
	}
	edit := last(t, calls)
	if edit.subcommand() != "release edit" || !edit.has("v1.2.3") ||
		!edit.has("--prerelease=false") || !edit.has("--latest") {
		t.Fatalf("promote ended on %v, want `release edit v1.2.3 --prerelease=false --latest`", edit)
	}
}

// install.sh refuses a release with no checksums.txt, so promoting one that
// release.yml never finished publishing to breaks every fresh install.
func TestPromoteRefusesAReleaseWithoutChecksums(t *testing.T) {
	calls, err := runReleaseScript(t, "scripts/release-promote.sh",
		gh{releaseExists: true, assets: "worktender_linux_amd64\n"}, "v1.2.3")
	if err == nil {
		t.Fatal("release-promote.sh made a release with no checksums.txt latest")
	}
	if got := subcommands(calls); got != "release view" {
		t.Errorf("gh calls = %v, want only the view", got)
	}
}

func TestPromoteRefusesAMissingRelease(t *testing.T) {
	calls, err := runReleaseScript(t, "scripts/release-promote.sh", gh{}, "v1.2.3")
	if err == nil {
		t.Fatal("release-promote.sh succeeded for a tag with no release")
	}
	if got := subcommands(calls); got != "release view" {
		t.Errorf("gh calls = %v, want only the view", got)
	}
}
