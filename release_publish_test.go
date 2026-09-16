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
// 404 for scripts/install.sh and for scripts/build.sh's no-Go fallback, both of
// which fetch releases/download/v<version>/worktender_<os>_<arch>.
//
// The only real proof is cutting a release, which no test gets to do. What the
// tests below can prove is the decision: whatever the state of the release for
// a tag, and whoever else is racing to create it, the script ends in an upload.
// A path that ends anywhere else is the bug all over again.

// ghCall is one invocation of the faked `gh`, as the argv it was handed.
type ghCall []string

// gh describes the `gh` a run is given: what it answers about the release, and
// whether creating one works. Both are things the real gh decides for reasons
// this script does not control, which is why they are the only knobs.
type gh struct {
	releaseExists bool
	createFails   bool
}

// runPublish runs the script against that gh, which records its argv per call.
//
// The fake is written here rather than taken from herdrtest because what
// matters is the sequence of calls, and herdrtest's fakes answer a single
// question. Recording argv per call is the whole assertion.
func runPublish(t *testing.T, fake gh, args ...string) ([]ghCall, error) {
	t.Helper()

	dir := t.TempDir()
	log := filepath.Join(dir, "gh.log")

	exit := func(fails bool) string {
		if fails {
			return "1"
		}
		return "0"
	}
	// "$*" not "$@": one line per call, and nothing being compared here has a
	// space in it.
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> " + log + "\n" +
		"case \"$1 $2\" in\n" +
		"'release view') exit " + exit(!fake.releaseExists) + " ;;\n" +
		"'release create') echo 'a release with the same tag name already exists' >&2; exit " + exit(fake.createFails) + " ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
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

// assets writes what a finished `build` step leaves in dist/ and hands back the
// paths. They have to be real files: the script checks, because release.yml
// passes an unexpanded glob when there are none.
func assets(t *testing.T, names ...string) []string {
	t.Helper()

	dir := t.TempDir()
	var paths []string
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("binary\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
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

func subcommands(calls []ghCall) string {
	var out []string
	for _, c := range calls {
		out = append(out, c.subcommand())
	}
	return strings.Join(out, ", ")
}

// last is the call the script ends on, which for every successful run has to be
// the upload.
func last(t *testing.T, calls []ghCall) ghCall {
	t.Helper()

	if len(calls) == 0 {
		t.Fatal("gh was never called")
	}
	return calls[len(calls)-1]
}

func assertUploads(t *testing.T, call ghCall, tag string, paths []string) {
	t.Helper()

	if call.subcommand() != "release upload" {
		t.Fatalf("the script ended on %q, not an upload; the tag ships with no assets", call.subcommand())
	}
	if !call.has(tag) {
		t.Errorf("upload call %v does not name %s", call, tag)
	}
	for _, path := range paths {
		if !call.has(path) {
			t.Errorf("upload call %v is missing %q", call, path)
		}
	}
	if !call.has("--clobber") {
		t.Errorf("upload call %v has no --clobber; re-running a failed release then "+
			"fails on asset names that are already there", call)
	}
}

// The case that has been failing since v0.10.0: the release is already there,
// created from a laptop with hand-written notes, and the workflow's job is to
// hang the assets off it.
func TestPublishUploadsToAReleaseThatAlreadyExists(t *testing.T) {
	paths := assets(t, "worktender_linux_amd64", "checksums.txt")

	calls, err := runPublish(t, gh{releaseExists: true}, append([]string{"v1.2.3"}, paths...)...)
	if err != nil {
		t.Fatalf("release-publish.sh failed on an existing release: %v", err)
	}

	if got, want := subcommands(calls), "release view, release upload"; got != want {
		t.Fatalf("gh calls = %v, want %v.\n"+
			"A `release create` here is #182 exactly: it fails with 'a release with "+
			"the same tag name already exists' and the tag ships with no assets.", got, want)
	}
	assertUploads(t, last(t, calls), "v1.2.3", paths)
}

// The other path, and the one the old step handled: a tag pushed with nothing
// having created a release for it. That must still produce a release, with
// generated notes, carrying the same assets.
func TestPublishCreatesAReleaseWhenTheTagHasNone(t *testing.T) {
	paths := assets(t, "worktender_linux_amd64", "checksums.txt")

	calls, err := runPublish(t, gh{}, append([]string{"v1.2.3"}, paths...)...)
	if err != nil {
		t.Fatalf("release-publish.sh failed on a tag with no release: %v", err)
	}

	if got, want := subcommands(calls), "release view, release create, release upload"; got != want {
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
	if last(t, calls).has("--generate-notes") {
		t.Errorf("upload call %v carries --generate-notes", last(t, calls))
	}
	assertUploads(t, last(t, calls), "v1.2.3", paths)
}

// `view` then `create` is check-then-act, so two runs for the same tag — a
// manual re-run, a retried job — can both see no release, and the loser's
// create fails with the exact error #182 is about. Losing that race must not
// end the script: the winner made the release this run still has to upload to.
func TestPublishStillUploadsWhenSomethingElseWonTheCreateRace(t *testing.T) {
	paths := assets(t, "worktender_linux_amd64", "checksums.txt")

	calls, err := runPublish(t, gh{createFails: true}, append([]string{"v1.2.3"}, paths...)...)
	if err != nil {
		t.Fatalf("a failed `release create` killed the script: %v.\n"+
			"Whoever won the race made a release, and this run's assets never reached it "+
			"— which is #182 again in a narrower window.", err)
	}

	if got, want := subcommands(calls), "release view, release create, release upload"; got != want {
		t.Fatalf("gh calls = %v, want %v", got, want)
	}
	assertUploads(t, last(t, calls), "v1.2.3", paths)
}

// release.yml calls the script with an unquoted `dist/*`. An empty or missing
// dist/ therefore does not arrive as zero arguments — sh leaves the glob
// unexpanded and passes the literal string, which counts as a perfectly good
// argument. Counting arguments cannot catch this; only checking the files can.
func TestPublishRefusesAnUnexpandedGlob(t *testing.T) {
	calls, err := runPublish(t, gh{releaseExists: true}, "v1.2.3", "dist/*")
	if err == nil {
		t.Fatal("release-publish.sh succeeded on an unexpanded dist/* glob, so a build " +
			"that produced nothing would publish a release with no assets and pass")
	}
	if len(calls) != 0 {
		t.Errorf("gh was called (%v) before the assets were checked", subcommands(calls))
	}
}

// The same rule one asset at a time: a dist/ missing one of the five binaries
// is a partial release, and a partial release is worse than a loud failure
// because only the platform that is missing ever finds out.
func TestPublishRefusesWhenOneAssetIsMissing(t *testing.T) {
	paths := assets(t, "worktender_linux_amd64")
	missing := filepath.Join(filepath.Dir(paths[0]), "checksums.txt")

	calls, err := runPublish(t, gh{releaseExists: true}, "v1.2.3", paths[0], missing)
	if err == nil {
		t.Fatal("release-publish.sh published a release with an asset that does not exist")
	}
	if len(calls) != 0 {
		t.Errorf("gh was called (%v) before the assets were checked", subcommands(calls))
	}
}

func TestPublishRefusesWithNoAssetsAtAll(t *testing.T) {
	calls, err := runPublish(t, gh{releaseExists: true}, "v1.2.3")
	if err == nil {
		t.Fatal("release-publish.sh succeeded with no assets to upload")
	}
	if len(calls) != 0 {
		t.Errorf("gh was called (%v) before the argument check", subcommands(calls))
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
