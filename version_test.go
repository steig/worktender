package main

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The version in herdr-plugin.toml is not decoration. scripts/build.sh builds
// the release download URL out of it —
//
//	base="https://github.com/$REPO/releases/download/v$version"
//
// — because a plugin install is a shallow clone with no tag in it, so the
// manifest is the only pin available on the installing machine.
//
// A manifest left at the previous version while a new tag is pushed therefore
// does not fail loudly. It sends every installer without a Go toolchain to a
// URL for the *old* release, or to one that does not exist. Whoever has Go
// compiles from source and never notices.
//
// So: the manifest version and the newest released CHANGELOG heading must
// agree. Nothing else in the repository checked this, and getting it wrong is
// invisible from the machine cutting the release.
func TestTheManifestVersionMatchesTheNewestRelease(t *testing.T) {
	manifest, err := os.ReadFile("herdr-plugin.toml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^version\s*=\s*"([^"]+)"`).FindSubmatch(manifest)
	if m == nil {
		t.Fatal("herdr-plugin.toml has no version; scripts/build.sh has nothing to pin the download to")
	}
	version := string(m[1])

	changelog, err := os.ReadFile("CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	// The newest heading that names a version. [Unreleased] carries no version
	// and is skipped: work in flight is not what an install downloads.
	released := regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`).FindSubmatch(changelog)
	if released == nil {
		t.Fatal("CHANGELOG.md has no released version heading")
	}
	newest := string(released[1])

	if version != newest {
		t.Errorf("herdr-plugin.toml says %s and the newest CHANGELOG release is %s.\n"+
			"scripts/build.sh downloads releases/download/v%s, so an installer without Go "+
			"gets the wrong release or a 404 — and an installer with Go compiles from source "+
			"and never notices. Move both together.", version, newest, version)
	}
}

// .claude-plugin/plugin.json is the same manifest problem again, one install
// path over. A Claude Code marketplace entry sourcing this repository reads its
// version from that file, and nothing else does — so a stale value never breaks
// anything, it just tells everyone who lists the plugin that they are on a
// release they are not. Pin it to the same CHANGELOG heading the herdr manifest
// is pinned to, so the release commit moves all three together.
func TestThePluginManifestVersionMatchesTheNewestRelease(t *testing.T) {
	raw, err := os.ReadFile(".claude-plugin/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Version == "" {
		t.Fatal(".claude-plugin/plugin.json has no version; the marketplace listing would show none")
	}

	changelog, err := os.ReadFile("CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	released := regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`).FindSubmatch(changelog)
	if released == nil {
		t.Fatal("CHANGELOG.md has no released version heading")
	}
	newest := string(released[1])

	if manifest.Version != newest {
		t.Errorf(".claude-plugin/plugin.json says %s and the newest CHANGELOG release is %s.\n"+
			"The Claude Code marketplace shows the plugin.json version, so everyone who lists "+
			"the plugin is told they are on the wrong release. Move both together.",
			manifest.Version, newest)
	}
}

// A version heading has to carry a date. Keep a Changelog wants one, and
// without it there is no way to tell a release that shipped from a heading
// somebody opened early and left.
func TestEveryReleasedVersionCarriesADate(t *testing.T) {
	changelog, err := os.ReadFile("CHANGELOG.md")
	if err != nil {
		t.Fatal(err)
	}
	heading := regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\](.*)$`)
	dated := regexp.MustCompile(`^\s+—\s+\d{4}-\d{2}-\d{2}\s*$`)

	found := 0
	for _, m := range heading.FindAllStringSubmatch(string(changelog), -1) {
		found++
		if !dated.MatchString(m[2]) {
			t.Errorf("CHANGELOG heading for %s is not dated (%q); a released version needs the day it shipped",
				m[1], strings.TrimSpace(m[2]))
		}
	}
	if found == 0 {
		t.Fatal("no released version headings found; this test is watching nothing")
	}
}
