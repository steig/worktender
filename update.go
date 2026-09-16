package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/steig/worktender/internal/gitx"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/safetext"
)

// update moves an install forward, because nothing else can: herdr has no
// `plugin update`, so an install pins a commit and sits on it silently.
//
// The install is a shallow, detached clone with no local branch, so `git pull`
// cannot work in it. Fetching one commit deep and resetting onto FETCH_HEAD is
// the shape that does.
//
// It is a command rather than a herdr action: an action's output lands in the
// plugin log, and herdr running this as an action would be herdr executing the
// very binary the rebuild replaces.

// manifestName identifies a directory as a plugin install. It is also what
// scripts/build.sh reads its version pin out of.
const manifestName = "herdr-plugin.toml"

// buildOutEnv tells scripts/build.sh where to put the binary it builds. The
// rebuild never writes over the file it is replacing; see rebuild.
const buildOutEnv = "WORKTENDER_BUILD_OUT"

// releaseVersion is the release this binary was cut as, stamped at link time by
// .github/workflows/release.yml. It is empty for every other build, and that
// emptiness is load-bearing rather than a gap: a plugin install reads its
// version out of the manifest beside it, and a `go build` from a working tree
// has no release to name at all. A non-empty value therefore means exactly one
// thing — this is a published release binary standing on its own, installed by
// scripts/install.sh or downloaded by hand.
var releaseVersion string

// repoSlug is this repository, as scripts/install.sh and the release URLs name
// it.
const repoSlug = "steig/worktender"

// installerCommand is the one documented step a standalone install is moved
// forward by, and the line `update` prints when it is asked to do a job it
// cannot. Built from repoSlug rather than written out, so moving the repository
// moves this too.
const installerCommand = "curl -fsSL https://raw.githubusercontent.com/" + repoSlug + "/main/scripts/install.sh | sh"

// remoteTimeout bounds the one network read. `doctor` performs it on every run
// and must not hang on an unreachable origin.
const remoteTimeout = 10 * time.Second

// fetchTimeout bounds the fetch. One commit of one branch, so a minute is
// already generous; without a bound a wedged transport hangs the command.
const fetchTimeout = 60 * time.Second

func updateCommand(args []string, out io.Writer) error {
	if len(args) > 0 {
		return usagef("unexpected argument %q; %s", args[0], updateUsage)
	}
	root, err := installRoot()
	if err != nil {
		if standalone() {
			// Not an error about a missing file, which is what installRoot
			// would say. There is nothing wrong here: a standalone install has
			// no checkout to fetch into and no build script to run, so `update`
			// has no work it could do and the honest answer is the one step
			// that does move it forward.
			//
			// Deliberately not running the installer itself. Piping a script
			// from the network into a shell is a decision its author makes
			// once, on purpose, reading the URL; a binary doing it on their
			// behalf from inside an unrelated command is the same act with the
			// decision removed.
			return withCode(exitEnvironment, fmt.Errorf(
				"this is a standalone install of %s, not a plugin checkout, so there is nothing here to fetch and rebuild.\n"+
					"re-run the installer to move it forward:\n  %s",
				releaseVersion, installerCommand))
		}
		return err
	}
	return update(root, out)
}

// standalone reports whether this process is a released binary living outside a
// plugin install — the shape scripts/install.sh leaves on PATH.
//
// It is the stamp rather than the absence of a manifest, because those are
// different claims. No manifest beside a binary that was never stamped is a
// `go build` someone ran in a working tree, and telling that person to re-run an
// installer would be telling them to throw their own build away.
func standalone() bool { return releaseVersion != "" }

const updateUsage = "usage: worktender update"

// installRoot is the checkout the running binary was installed as.
//
// It comes from the executable's own path rather than the working directory.
// herdr runs ACTIONS with cwd set to the plugin root, but `update` is a command
// someone runs by hand from wherever they are standing, so cwd says nothing —
// and being wrong about this means hard-resetting somebody else's repository.
func installRoot() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	root := filepath.Dir(filepath.Dir(gitx.Resolve(exe)))
	if _, err := os.Stat(filepath.Join(root, manifestName)); err != nil {
		return "", fmt.Errorf("no %s beside %s; update only works from an installed plugin binary", manifestName, exe)
	}
	return root, nil
}

// update fetches the origin default branch, resets onto it, and rebuilds.
func update(root string, out io.Writer) error {
	fmt.Fprintf(out, "install: %s\n", root)

	// Both refusals name the same thing: this only knows how to move the
	// detached checkout herdr installs. A linked development checkout is on a
	// branch, and `git reset --hard` there would move that branch and throw away
	// whatever it was pointing at.
	if branch, err := gitIn(root, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		return fmt.Errorf("this checkout is on branch %s rather than the detached HEAD herdr installs; it looks like `herdr plugin link`, so update it with git yourself", safetext.Escape(branch))
	}
	if gitx.IsDirty(root) {
		return fmt.Errorf("%s has uncommitted changes and updating resets hard over them; commit or discard them first", root)
	}

	wasVersion, err := manifestVersion(root)
	if err != nil {
		return err
	}
	wasCommit, err := gitIn(root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}

	branch, commit, err := remoteHead(root)
	if err != nil {
		return err
	}
	if commit == wasCommit {
		fmt.Fprintf(out, "already current: %s @%s is origin/%s\n", wasVersion, short(wasCommit), safetext.Escape(branch))
		return nil
	}

	fmt.Fprintf(out, "origin/%s is at @%s; fetching\n", safetext.Escape(branch), short(commit))
	if _, err := gitWithin(fetchTimeout, root, "fetch", "--depth", "1", "origin", branch); err != nil {
		return err
	}
	if _, err := gitIn(root, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return err
	}

	version, err := manifestVersion(root)
	if err != nil {
		return err
	}
	if err := rebuild(root, out); err != nil {
		// Only the checkout is added here. What became of the binary is rebuild's
		// to say, because rebuild is what looked.
		return fmt.Errorf("%w; the checkout is now at %s @%s", err, version, short(commit))
	}

	fmt.Fprintf(out, "%s @%s -> %s @%s\n", wasVersion, short(wasCommit), version, short(commit))
	reportHerdrRecord(out, root)
	return nil
}

// reportHerdrRecord says the one thing an in-place update cannot fix.
//
// herdr records the installed commit when it installs and never re-reads the
// checkout — measured: after a hand update from 8ef0de9 to f074c65, `plugin
// list` reported the new manifest VERSION and the old commit. There is no API
// to correct it and no `plugin update` to do it for us, so the command that
// answers "what am I running" now answers wrongly, and nothing else marks it
// stale. Saying so plainly is the whole of the fix available here; `doctor`
// repeats it on every run.
func reportHerdrRecord(out io.Writer, root string) {
	if recorded := recordedCommit(root); recorded != "" {
		fmt.Fprintf(out, "\nherdr still records @%s for this plugin", short(recorded))
	} else {
		fmt.Fprint(out, "\nherdr recorded a commit when it installed this plugin and never re-reads the checkout")
	}
	// A reinstall rather than `--ref v<version>`: the manifest version names the
	// last RELEASE, and a checkout that just fetched the default branch is
	// usually past that tag, so pinning to it would move the install backwards.
	fmt.Fprint(out, ", so `herdr plugin list` will keep naming a commit that is no longer installed.\nnothing here can correct that record; a reinstall re-clones and re-records it:\n  herdr plugin install steig/worktender\n")
}

// recordedCommit is the commit herdr believes is installed at root, or empty
// when herdr cannot be asked or does not know this plugin.
func recordedCommit(root string) string {
	client, err := herdrapi.New()
	if err != nil {
		return ""
	}
	return recordedCommitVia(client, root)
}

func recordedCommitVia(client *herdrapi.Client, root string) string {
	if client == nil {
		return ""
	}
	plugins, err := client.PluginList()
	if err != nil {
		return ""
	}
	for _, p := range plugins.Plugins {
		if gitx.Resolve(p.PluginRoot) != gitx.Resolve(root) {
			continue
		}
		if p.Source != nil && p.Source.ResolvedCommit != nil {
			return *p.Source.ResolvedCommit
		}
	}
	return ""
}

// rebuild builds the new binary beside the live one and renames it into place,
// never over it: `update` is normally run by the binary it replaces, and writing
// over an executing image fails or corrupts the running process. A rename within
// the directory is atomic and leaves anything running holding the old inode.
//
// The staging is only a request, and the script receiving it comes from the
// checkout just fetched — a build.sh that ignores WORKTENDER_BUILD_OUT writes
// the live path anyway. So the live binary is stamped before the build and
// compared after, or "nothing staged" reads as "nothing built" when the binary
// has already been replaced.
func rebuild(root string, out io.Writer) error {
	staged := filepath.Join("bin", binaryName()+".new")
	live := filepath.Join(root, "bin", binaryName())
	before := stampOf(live)

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("go", "build", "-o", staged, ".")
	} else {
		cmd = exec.Command("sh", "scripts/build.sh")
		cmd.Env = append(os.Environ(), buildOutEnv+"="+staged)
	}
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rebuild: %w; %s", err, binaryFate(live, before))
	}

	built := filepath.Join(root, staged)
	if _, err := os.Stat(built); err != nil {
		if stampOf(live) != before {
			return fmt.Errorf("the build ignored %s and wrote %s itself, so the binary was replaced in place rather than renamed over — anything executing it could have been swapped mid-run; it is now the build just fetched and does not need running again", buildOutEnv, live)
		}
		return fmt.Errorf("the build produced no %s and left %s untouched, so nothing was rebuilt", staged, live)
	}
	return replaceBinary(root, built)
}

// stampOf identifies a file closely enough to tell "the build wrote this" from
// "the build left it alone". Empty when there is nothing there.
func stampOf(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("%d@%d", info.Size(), info.ModTime().UnixNano())
}

// binaryFate reports what became of the live binary, which is the half of a
// failed rebuild the caller cannot see for itself.
func binaryFate(live, before string) string {
	if stampOf(live) != before {
		return live + " was written over anyway, so it is neither the build that was running nor a build that succeeded"
	}
	return live + " is untouched and still the build that was running"
}

func binaryName() string {
	if runtime.GOOS == "windows" {
		return "worktender.exe"
	}
	return "worktender"
}

// replaceBinary moves the freshly built binary onto the live path.
//
// The live file is moved aside first rather than renamed over, because Windows
// refuses to replace a file that is open for execution and that file is usually
// this process. Removing what was moved aside is best-effort for the same
// reason: on Windows the old image stays locked until the process using it
// exits, and a leftover is harmless where a failed update is not.
func replaceBinary(root, built string) error {
	live := filepath.Join(root, "bin", binaryName())
	previous := live + ".old"

	_ = os.Remove(previous)
	if err := os.Rename(live, previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(built, live); err != nil {
		return err
	}
	_ = os.Remove(previous)
	return nil
}

// manifestVersion reads the plugin version out of the manifest, which is the
// version herdr reports and the version the release tags are named for.
//
// Hand-scanned for the same reason the manifest tests scan rather than parse:
// this module has no dependencies, and a TOML library is a poor trade for one
// key of one table.
func manifestVersion(root string) (string, error) {
	path := filepath.Join(root, manifestName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "version" {
			continue
		}
		var version string
		if err := json.Unmarshal([]byte(strings.TrimSpace(value)), &version); err != nil {
			return "", fmt.Errorf("version in %s is not a quoted string: %s", path, line)
		}
		return version, nil
	}
	return "", fmt.Errorf("no version in %s", path)
}

// remoteHead asks origin which branch it defaults to and what that branch
// points at, in one round trip and without writing anything into the checkout —
// `doctor` needs this answer and is read-only.
//
// The branch is READ rather than assumed. The install is a shallow clone of
// whatever origin's HEAD named at the time, so a repository defaulting to
// develop must not be fetched as main.
func remoteHead(dir string) (branch, commit string, err error) {
	out, err := gitWithin(remoteTimeout, dir, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return "", "", err
	}
	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "ref:":
			branch = strings.TrimPrefix(fields[1], "refs/heads/")
		default:
			if fields[1] == "HEAD" {
				commit = fields[0]
			}
		}
	}
	if branch == "" || commit == "" {
		return "", "", fmt.Errorf("origin did not report a default branch and commit: %q", out)
	}
	return branch, commit, nil
}

func gitIn(dir string, args ...string) (string, error) {
	return gitWithin(0, dir, args...)
}

// gitWithin runs git in dir, optionally bounded by a timeout. Zero means no
// bound, which is right for the local queries and wrong for anything that
// touches the network.
//
// The terminal prompt is disabled throughout: git asking for credentials in a
// command nobody is watching hangs with nothing to read.
func gitWithin(timeout time.Duration, dir string, args ...string) (string, error) {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return "", fmt.Errorf("git %s in %s: %s", strings.Join(args, " "), dir, strings.TrimSpace(string(exit.Stderr)))
		}
		return "", fmt.Errorf("git %s in %s: %w", strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// short renders a commit the way git and herdr's own output do.
func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
