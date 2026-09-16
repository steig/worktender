package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
)

// scripts/install.sh is the only install route that does not go through herdr,
// and it is a shell script, so nothing in the Go suite would otherwise touch it.
// These tests run the real script against a fake release server.
//
// What is worth testing is not the happy path — it is the refusals. The script
// downloads a binary over the network, makes it executable and puts it on the
// user's PATH, so every failure has to leave NOTHING behind: no worktender at the
// destination, and no half-verified download beside it. A checksum check that
// aborts while the file it rejected is already in place has verified nothing.

// installAsset is the release asset name the script builds for the machine the
// tests are running on, which is what the fake release has to publish.
func installAsset(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("scripts/install.sh is POSIX sh for linux and darwin; this is %s", runtime.GOOS)
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skipf("the release publishes amd64 and arm64; this is %s", runtime.GOARCH)
	}
	return fmt.Sprintf("worktender_%s_%s", runtime.GOOS, runtime.GOARCH)
}

// release is a GitHub releases endpoint as scripts/install.sh reads it: `latest`
// redirecting to a tag, and assets under `download/<tag>/`.
type release struct {
	server *httptest.Server
	latest string
	// files is keyed "<tag>/<name>"; anything absent is a 404, which is how the
	// "no such release" and "no checksums.txt" cases are built.
	files map[string][]byte

	mu   sync.Mutex
	hits []string
}

func newRelease(t *testing.T, latest string) *release {
	t.Helper()
	r := &release{latest: latest, files: map[string][]byte{}}
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.hits = append(r.hits, req.URL.Path)
		r.mu.Unlock()

		if req.URL.Path == "/latest" {
			http.Redirect(w, req, "/tag/"+r.latest, http.StatusFound)
			return
		}
		// GitHub answers the tag a release redirect lands on with an ordinary
		// page, and the script follows the redirect with `curl -f` — so a 404
		// here would fail the resolution for a reason GitHub never produces.
		if strings.HasPrefix(req.URL.Path, "/tag/") {
			fmt.Fprintln(w, req.URL.Path)
			return
		}
		body, ok := r.files[strings.TrimPrefix(req.URL.Path, "/download/")]
		if !ok {
			http.NotFound(w, req)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(r.server.Close)
	return r
}

// publish adds an asset and the checksums.txt line that vouches for it.
func (r *release) publish(tag, name string, body []byte) {
	r.files[tag+"/"+name] = body
	sum := sha256.Sum256(body)
	line := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), name)
	r.files[tag+"/checksums.txt"] = append(r.files[tag+"/checksums.txt"], line...)
}

func (r *release) asked(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, h := range r.hits {
		if h == path {
			return true
		}
	}
	return false
}

// install runs the real script against this release, into a fresh directory.
func (r *release) install(t *testing.T, args ...string) (dir, out string, err error) {
	t.Helper()
	dir = t.TempDir()
	cmd := exec.Command("sh", append([]string{"scripts/install.sh", "--dir", dir}, args...)...)
	cmd.Env = append(os.Environ(), "WORKTENDER_DOWNLOAD_BASE="+r.server.URL)
	combined, err := cmd.CombinedOutput()
	return dir, string(combined), err
}

// requireHTTPTool skips when the machine has neither downloader, which is the one
// thing the script cannot work around.
func requireHTTPTool(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err == nil {
		return
	}
	if _, err := exec.LookPath("wget"); err == nil {
		return
	}
	t.Skip("neither curl nor wget available")
}

// leftovers is every entry in the install directory. The refusal tests assert
// this is empty, not merely that `worktender` is absent: a rejected download
// parked beside it under a staging name is the same failure one rename away.
func leftovers(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestTheInstallerResolvesLatestAndInstallsIt(t *testing.T) {
	requireHTTPTool(t)
	asset := installAsset(t)

	r := newRelease(t, "v9.9.9")
	r.publish("v9.9.9", asset, []byte("#!/bin/sh\necho worktender 9.9.9\n"))

	dir, out, err := r.install(t)
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}

	binary := filepath.Join(dir, "worktender")
	info, err := os.Stat(binary)
	if err != nil {
		t.Fatalf("nothing installed: %v\n%s", err, out)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Errorf("installed %s without an executable bit (%v); it is on PATH and cannot be run", binary, info.Mode())
	}
	body, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "worktender 9.9.9") {
		t.Errorf("installed file is not the published asset: %q", body)
	}

	// The version has to be printed. An install nobody can name is one nobody
	// can report a bug against, and `latest` is a moving target by definition.
	if !strings.Contains(out, "v9.9.9") {
		t.Errorf("the install never said which release it installed:\n%s", out)
	}
	if got := leftovers(t, dir); len(got) != 1 {
		t.Errorf("install directory holds %v; only the binary should survive", got)
	}
}

func TestAPinnedVersionNeverAsksWhichIsLatest(t *testing.T) {
	requireHTTPTool(t)
	asset := installAsset(t)

	// `latest` here is a DIFFERENT release, and resolving it would install the
	// wrong one — which is the whole reason --version exists. The pin also has to
	// accept both spellings people type.
	r := newRelease(t, "v9.9.9")
	r.publish("v9.9.9", asset, []byte("newest\n"))
	r.publish("v0.9.0", asset, []byte("pinned\n"))

	for _, spelling := range []string{"v0.9.0", "0.9.0"} {
		dir, out, err := r.install(t, "--version", spelling)
		if err != nil {
			t.Fatalf("--version %s failed: %v\n%s", spelling, err, out)
		}
		body, err := os.ReadFile(filepath.Join(dir, "worktender"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(body)) != "pinned" {
			t.Errorf("--version %s installed %q, not the release it named", spelling, body)
		}
	}
	if r.asked("/latest") {
		t.Error("a pinned install resolved `latest` anyway; the pin is then only a suggestion")
	}
}

func TestAChecksumMismatchInstallsNothingAndLeavesNothing(t *testing.T) {
	requireHTTPTool(t)
	asset := installAsset(t)

	r := newRelease(t, "v9.9.9")
	r.publish("v9.9.9", asset, []byte("the real asset\n"))
	// The published checksum now vouches for something else entirely — a
	// corrupted download, or a substituted one.
	r.files["v9.9.9/"+asset] = []byte("not what was vouched for\n")

	dir, out, err := r.install(t)
	if err == nil {
		t.Fatalf("a checksum mismatch installed anyway:\n%s", out)
	}
	if !strings.Contains(out, "checksum mismatch") {
		t.Errorf("the failure does not say the checksum did not match:\n%s", out)
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("a rejected download left %v behind in the install directory", got)
	}
}

func TestAnAssetWithNoPublishedChecksumIsRefused(t *testing.T) {
	requireHTTPTool(t)
	asset := installAsset(t)

	// checksums.txt exists and simply has no line for this platform — a release
	// that was cut without one target, which is indistinguishable from a release
	// whose checksums were tampered with by deletion.
	r := newRelease(t, "v9.9.9")
	r.files["v9.9.9/"+asset] = []byte("unvouched\n")
	r.files["v9.9.9/checksums.txt"] = []byte("0000  worktender_somethingelse\n")

	dir, out, err := r.install(t)
	if err == nil {
		t.Fatalf("installed an asset no checksum covers:\n%s", out)
	}
	if !strings.Contains(out, "no checksum published") {
		t.Errorf("the failure does not name the missing checksum:\n%s", out)
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("left %v behind in the install directory", got)
	}
}

func TestAMissingAssetLeavesNothingAndNeverFetchesChecksums(t *testing.T) {
	requireHTTPTool(t)
	asset := installAsset(t)

	r := newRelease(t, "v9.9.9")
	r.files["v9.9.9/checksums.txt"] = []byte("0000  " + asset + "\n")

	dir, out, err := r.install(t)
	if err == nil {
		t.Fatalf("a 404 for the binary still reported success:\n%s", out)
	}
	// The script must stop at the first failed fetch rather than carry on and
	// then fail on a checksum. Both refuse, but only one of them says what
	// actually went wrong.
	if r.asked("/download/v9.9.9/checksums.txt") {
		t.Error("carried on to fetch checksums after the binary itself 404ed")
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("a failed download left %v behind in the install directory", got)
	}
}

func TestLatestFailingToResolveIsFatalRatherThanAGuess(t *testing.T) {
	requireHTTPTool(t)
	installAsset(t)

	// A base URL that answers nothing. Falling through to some assumed tag here
	// would install a release nobody asked for.
	r := newRelease(t, "v9.9.9")
	r.server.Close()

	dir, out, err := r.install(t)
	if err == nil {
		t.Fatalf("an unreachable release endpoint still installed something:\n%s", out)
	}
	if !strings.Contains(out, "--version") {
		t.Errorf("the failure does not offer the pin that would work around it:\n%s", out)
	}
	if got := leftovers(t, dir); len(got) != 0 {
		t.Errorf("left %v behind in the install directory", got)
	}
}

// The wget half of every network call is a separate implementation — including
// the redirect parsing that resolves `latest`, which curl does with one flag and
// wget does by reading its own headers back. Nothing else exercises it, and a
// machine with wget and no curl is exactly the machine that cannot fix it.
func TestTheInstallerWorksWithWgetAndNoCurl(t *testing.T) {
	if _, err := exec.LookPath("wget"); err != nil {
		t.Skip("no wget")
	}
	asset := installAsset(t)

	r := newRelease(t, "v9.9.9")
	r.publish("v9.9.9", asset, []byte("via wget\n"))

	dir := t.TempDir()
	cmd := exec.Command("sh", "scripts/install.sh", "--dir", dir)
	cmd.Env = append(os.Environ(),
		"WORKTENDER_DOWNLOAD_BASE="+r.server.URL,
		"PATH="+pathWithoutCurl(t))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("install without curl failed: %v\n%s", err, out)
	}
	body, err := os.ReadFile(filepath.Join(dir, "worktender"))
	if err != nil {
		t.Fatalf("nothing installed: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(body)) != "via wget" {
		t.Errorf("installed %q", body)
	}
}

// pathWithoutCurl builds a PATH holding every tool the script uses except curl,
// so the wget branch is the only one reachable. Symlinks rather than a copy of
// /usr/bin, because "hide one binary" has no portable spelling otherwise.
func pathWithoutCurl(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	// Everything scripts/install.sh execs. `command`, `echo` and `cat`'s heredoc
	// are shell builtins in every sh that matters, but cat is linked anyway
	// because dash's is not.
	for _, tool := range []string{"wget", "uname", "mktemp", "awk", "chmod", "mv", "rm", "tail", "cat", "sha256sum", "shasum", "mkdir"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			continue
		}
		if err := os.Symlink(path, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	for _, required := range []string{"wget", "uname", "mktemp", "awk"} {
		if _, err := os.Stat(filepath.Join(bin, required)); err != nil {
			t.Skipf("no %s to build a curl-free PATH with", required)
		}
	}
	return bin
}

// withReleaseVersion stamps the binary the way .github/workflows/release.yml
// does at link time, for the length of one test.
func withReleaseVersion(t *testing.T, v string) {
	t.Helper()
	was := releaseVersion
	releaseVersion = v
	t.Cleanup(func() { releaseVersion = was })
}

// `update` fetches and rebuilds a plugin checkout, and a standalone install has
// neither. What it must not do is report that as the missing-manifest error,
// which reads as a broken install and names a file the user was never supposed
// to have.
func TestUpdateOnAStandaloneInstallPointsAtTheInstaller(t *testing.T) {
	withReleaseVersion(t, "9.9.9")

	err := updateCommand(nil, io.Discard)
	if err == nil {
		t.Fatal("update reported success on an install it cannot move")
	}
	if !strings.Contains(err.Error(), "scripts/install.sh") {
		t.Errorf("the refusal does not name the one step that does move it forward: %v", err)
	}
	if !strings.Contains(err.Error(), "9.9.9") {
		t.Errorf("the refusal does not say which release is installed: %v", err)
	}
	if code := exitCode(err); code != exitEnvironment {
		t.Errorf("exit %d; this is the machine's shape rather than a mistyped command, so %d", code, exitEnvironment)
	}
}

// An unstamped binary with no manifest is somebody's own `go build`, and telling
// them to re-run an installer would be telling them to throw it away.
func TestUpdateFromAHandBuiltBinaryStillNamesTheMissingManifest(t *testing.T) {
	withReleaseVersion(t, "")

	err := updateCommand(nil, io.Discard)
	if err == nil {
		t.Fatal("update reported success outside any install at all")
	}
	if strings.Contains(err.Error(), "scripts/install.sh") {
		t.Errorf("sent a hand-built binary to the installer: %v", err)
	}
	if !strings.Contains(err.Error(), manifestName) {
		t.Errorf("does not say what it looked for: %v", err)
	}
}

// doctor's version line is the only thing that tells a standalone user which
// release they are on — there is no manifest to read and `herdr plugin list`
// does not know them. Reporting it as a warning would also be wrong: nothing is
// broken, and a warning nobody can clear is one people learn to skip.
func TestDoctorNamesTheReleaseAStandaloneInstallIs(t *testing.T) {
	withReleaseVersion(t, "9.9.9")

	c := versionCheck(nil)
	if c.value != "9.9.9" {
		t.Errorf("version reported as %q, not the release this binary was cut as", c.value)
	}
	if c.state != stateOK {
		t.Errorf("state %q; a standalone install is not a problem to be fixed", c.state)
	}
	if !strings.Contains(c.note, "standalone") {
		t.Errorf("the note does not say why nothing is compared against origin: %q", c.note)
	}
}

func TestDoctorStillCannotNameAnUnstampedBuild(t *testing.T) {
	withReleaseVersion(t, "")

	c := versionCheck(nil)
	if c.value != "unknown" || c.state != stateWarn {
		t.Errorf("reported %q/%s; a build that can name no release must say so", c.value, c.state)
	}
}

// scripts/install.sh asks for worktender_<os>_<arch> by name, and that name only
// resolves because .github/workflows/release.yml built exactly it. The two lists
// are written in different languages in different files and nothing connects
// them, so a target added to one and not the other is a 404 on somebody else's
// machine at install time — invisible here, and invisible to whoever cut the
// release.
func TestTheInstallerAsksForExactlyTheAssetsTheReleaseBuilds(t *testing.T) {
	script, err := os.ReadFile("scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	// The two case blocks: `Darwin) os="darwin" ;;` and `x86_64 | amd64) arch="amd64" ;;`.
	oses := captures(t, regexp.MustCompile(`os="([a-z0-9]+)"`), script)
	arches := captures(t, regexp.MustCompile(`arch="([a-z0-9]+)"`), script)

	workflow, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	targets := regexp.MustCompile(`for target in ([^;\n]+);`).FindSubmatch(workflow)
	if targets == nil {
		t.Fatal("no cross-compile target list in the release workflow; this test is watching nothing")
	}
	built := map[string]bool{}
	for _, target := range strings.Fields(string(targets[1])) {
		built[target] = true
	}

	asked := map[string]bool{}
	for _, o := range oses {
		for _, a := range arches {
			asked[o+"_"+a] = true
		}
	}

	for target := range asked {
		if !built[target] {
			t.Errorf("scripts/install.sh can ask for worktender_%s and the release does not build it; "+
				"that install is a 404 nobody cutting the release would see", target)
		}
	}
	for target := range built {
		if !asked[target] {
			t.Errorf("the release builds worktender_%s and scripts/install.sh cannot ask for it; "+
				"whoever is on that platform installs by hand or not at all", target)
		}
	}
}

// captures is every distinct first-group match, sorted so a failure reads the
// same way twice.
func captures(t *testing.T, re *regexp.Regexp, in []byte) []string {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range re.FindAllSubmatch(in, -1) {
		seen[string(m[1])] = true
	}
	if len(seen) == 0 {
		t.Fatalf("%s matched nothing; this test is watching the wrong file", re)
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
