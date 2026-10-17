package main

import "testing"

func runPromote(t *testing.T, fake gh, tag string) ([]ghCall, error) {
	t.Helper()
	return runReleaseScript(t, "scripts/release-promote.sh", fake, tag)
}

// scripts/release-promote.sh is the only step that moves releases/latest now
// that release.yml publishes prereleases. It must flip both halves — not a
// prerelease, and latest — or install.sh keeps resolving the old tag.
func TestPromoteMarksTheReleaseLatest(t *testing.T) {
	calls, err := runPromote(t, gh{
		releaseExists: true,
		view:          "false\nworktender_linux_amd64\nworktender_darwin_arm64\nchecksums.txt\n",
		checksums:     "abc  worktender_linux_amd64\ndef *worktender_darwin_arm64\n",
	}, "v1.2.3")
	if err != nil {
		t.Fatalf("release-promote.sh failed: %v", err)
	}
	edit := last(t, calls)
	if edit.subcommand() != "release edit" || !edit.has("v1.2.3") ||
		!edit.has("--prerelease=false") || !edit.has("--latest") {
		t.Fatalf("promote ended on %v, want `release edit v1.2.3 --prerelease=false --latest`", edit)
	}
}

// install.sh downloads checksums.txt and the binary it names from latest, so
// promoting a release missing either breaks every fresh install. A partial
// upload has checksums.txt and still misses binaries.
func TestPromoteRefusesAnIncompleteRelease(t *testing.T) {
	for name, fake := range map[string]gh{
		"no checksums.txt": {releaseExists: true, view: "false\nworktender_linux_amd64\n"},
		"a binary checksums.txt names is missing": {
			releaseExists: true,
			view:          "false\nworktender_linux_amd64\nchecksums.txt\n",
			checksums:     "abc  worktender_linux_amd64\ndef  worktender_darwin_arm64\n",
		},
		"empty checksums.txt": {releaseExists: true, view: "false\nchecksums.txt\n", checksums: "\n"},
		"a draft": {
			releaseExists: true,
			view:          "true\nworktender_linux_amd64\nchecksums.txt\n",
			checksums:     "abc  worktender_linux_amd64\n",
		},
		"no release": {},
	} {
		calls, err := runPromote(t, fake, "v1.2.3")
		if err == nil {
			t.Errorf("%s: release-promote.sh made the release latest", name)
		}
		for _, c := range calls {
			if c.subcommand() == "release edit" {
				t.Errorf("%s: release-promote.sh ran %v", name, c)
			}
		}
	}
}

func TestPromoteRefusesSomethingThatIsNotATag(t *testing.T) {
	for _, tag := range []string{"--latest", "main", "1.2.3"} {
		calls, err := runPromote(t, gh{releaseExists: true}, tag)
		if err == nil {
			t.Errorf("release-promote.sh accepted %q as a tag", tag)
		}
		if len(calls) != 0 {
			t.Errorf("%q: gh was called (%v) before the tag was checked", tag, subcommands(calls))
		}
	}
}

// A checksums.txt that downloads but names no file would let the loop over its
// names verify nothing, and promote a release install.sh cannot install from.
func TestPromoteRefusesChecksumsThatNameNothing(t *testing.T) {
	for name, sums := range map[string]string{
		"one column":       "abc\ndef\n",
		"blank lines":      " \n\t\n",
		"star but no name": "abc  *\n",
	} {
		calls, err := runPromote(t, gh{
			releaseExists: true,
			view:          "false\nworktender_linux_amd64\nchecksums.txt\n",
			checksums:     sums,
		}, "v1.2.3")
		if err == nil {
			t.Errorf("%s: release-promote.sh made the release latest", name)
		}
		for _, c := range calls {
			if c.subcommand() == "release edit" {
				t.Errorf("%s: release-promote.sh ran %v", name, c)
			}
		}
	}
}

// Without -e, grep reads an asset name that starts with "-" as an option.
func TestPromoteChecksAnAssetNamedLikeAFlag(t *testing.T) {
	calls, err := runPromote(t, gh{
		releaseExists: true,
		view:          "false\n-v\nchecksums.txt\n",
		checksums:     "abc  -v\n",
	}, "v1.2.3")
	if err != nil {
		t.Fatalf("release-promote.sh failed: %v", err)
	}
	if edit := last(t, calls); edit.subcommand() != "release edit" {
		t.Fatalf("promote ended on %v, want release edit", edit)
	}
}
