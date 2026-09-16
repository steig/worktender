package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// scripts/release-publish.sh is the step that attaches build output to a
// release, and it is the step that silently failed on every release from
// v0.10.0 through v0.11.2 (#182): a release object already existed for the tag,
// `gh release create` refused, and the tag shipped with no assets — which is a
// 404 for scripts/build.sh's no-Go fallback and for anything else fetching
// releases/download/v<version>/worktender_<os>_<arch>.
//
// The only real proof is cutting a release, which no test gets to do. What the
// tests below can prove is the decision: given a release that exists, the
// script uploads to it and does not try to create; given none, it creates one
// first. Both paths must end in an upload, because a path that ends anywhere
// else is the bug all over again.

// ghCall is one invocation of the faked `gh`, as the argv it was handed.
type ghCall []string

// runPublish runs the script with a `gh` that records its argv and answers
// `release view` according to exists — the one thing the script branches on.
//
// The fake is written here rather than taken from herdrtest because what
// matters is the sequence of calls, and herdrtest's fakes answer a single
// question. Recording argv per call is the whole assertion.
func runPublish(t *testing.T, exists bool, args ...string) ([]ghCall, error) {
	t.Helper()

	dir := t.TempDir()
	log := filepath.Join(dir, "gh.log")

	viewExit := "1"
	if exists {
		viewExit = "0"
	}
	// "$*" not "$@": one line per call, and the assets are fixed names here,
	// so nothing being compared has a space in it.
	fake := "#!/bin/sh\n" +
		"echo \"$*\" >> " + log + "\n" +
		"case \"$1 $2\" in\n" +
		"'release view') exit " + viewExit + " ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", append([]string{"scripts/release-publish.sh"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	t.Logf("release-publish.sh %v:\n%s", args, out)

	raw, readErr := os.ReadFile(log)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	var calls []ghCall
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		calls = append(calls, strings.Fields(line))
	}
	return calls, err
}

// subcommand names the gh call, e.g. "release upload", so an assertion reads as
// the thing it means rather than as an index into argv.
func (c ghCall) subcommand() string {
	if len(c) < 2 {
		return strings.Join(c, " ")
	}
	return c[0] + " " + c[1]
}

func (c ghCall) has(arg string) bool {
	for _, got := range c {
		if got == arg {
			return true
		}
	}
	return false
}

func subcommands(calls []ghCall) []string {
	var out []string
	for _, c := range calls {
		out = append(out, c.subcommand())
	}
	return out
}

// The case that has been failing since v0.10.0: the release is already there,
// created from a laptop with hand-written notes, and the workflow's job is to
// hang the assets off it.
func TestPublishUploadsToAReleaseThatAlreadyExists(t *testing.T) {
	calls, err := runPublish(t, true, "v1.2.3", "dist/worktender_linux_amd64", "dist/checksums.txt")
	if err != nil {
		t.Fatalf("release-publish.sh failed on an existing release: %v", err)
	}

	got := subcommands(calls)
	want := []string{"release view", "release upload"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("gh calls = %v, want %v.\n"+
			"A `release create` here is #182 exactly: it fails with 'a release with "+
			"the same tag name already exists' and the tag ships with no assets.", got, want)
	}

	upload := calls[1]
	for _, want := range []string{"v1.2.3", "dist/worktender_linux_amd64", "dist/checksums.txt"} {
		if !upload.has(want) {
			t.Errorf("upload call %v is missing %q", upload, want)
		}
	}
	if !upload.has("--clobber") {
		t.Errorf("upload call %v has no --clobber; re-running a failed release then "+
			"fails on asset names that are already there", upload)
	}
}

// The other path, and the one the old step handled: a tag pushed with nothing
// having created a release for it. That must still produce a release, with
// generated notes, carrying the same assets.
func TestPublishCreatesAReleaseWhenTheTagHasNone(t *testing.T) {
	calls, err := runPublish(t, false, "v1.2.3", "dist/worktender_linux_amd64", "dist/checksums.txt")
	if err != nil {
		t.Fatalf("release-publish.sh failed on a tag with no release: %v", err)
	}

	got := subcommands(calls)
	want := []string{"release view", "release create", "release upload"}
	if strings.Join(got, ", ") != strings.Join(want, ", ") {
		t.Fatalf("gh calls = %v, want %v", got, want)
	}

	create := calls[1]
	if !create.has("--generate-notes") {
		t.Errorf("create call %v has no --generate-notes; a release nobody wrote notes "+
			"for would ship with an empty body", create)
	}
	if !create.has("v1.2.3") {
		t.Errorf("create call %v does not name the tag", create)
	}
	// Notes are only ever generated on a release this script created. On the
	// existing-release path above there is no create call at all, which is what
	// keeps a curated CHANGELOG excerpt from being replaced by a commit list.
	if calls[2].has("--generate-notes") {
		t.Errorf("upload call %v carries --generate-notes", calls[2])
	}
}

// Called with no assets, the script has nothing to publish, and a release
// workflow that quietly succeeds having uploaded nothing is the failure mode
// this whole change is about. It must refuse.
func TestPublishRefusesWithNoAssets(t *testing.T) {
	calls, err := runPublish(t, true, "v1.2.3")
	if err == nil {
		t.Fatal("release-publish.sh succeeded with no assets to upload")
	}
	if len(calls) != 0 {
		t.Errorf("gh was called %v before the argument check", subcommands(calls))
	}
}

// A tested script the workflow does not run is worse than no script: the tests
// pass, the release still ships empty, and nothing says so. Pin the wiring.
func TestReleaseWorkflowRunsThePublishScript(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	workflow := string(raw)

	if !strings.Contains(workflow, "scripts/release-publish.sh") {
		t.Fatal("release.yml does not run scripts/release-publish.sh, so nothing in " +
			"release_publish_test.go says anything about what a real release does")
	}
	if strings.Contains(workflow, "gh release create") {
		t.Error("release.yml calls `gh release create` directly again. That is the #182 " +
			"failure: it is fatal when a release already exists for the tag. Go through " +
			"scripts/release-publish.sh.")
	}
}
