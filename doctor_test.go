package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/execute"
	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/herdrtest"
	"github.com/steig/worktender/internal/wt"
)

// The whole point of the events line is that it reports what the GATE decided,
// not what the variable says. A value nobody wrote a rule for leaves events off,
// and the notice that would normally say so is printed by a hook that will not
// fire — so this is the only place a mistyped opt-in becomes visible.
func TestDoctorReportsEventsAsTheGateParsesThem(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		state state
		says  string
	}{
		{"", stateOff, "unset"},
		{"1", stateOK, "on"},
		{"off", stateOff, "off"},
		{"ture", stateWarn, "recognise"},
		{"  ", stateOff, "unset"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			t.Setenv(eventsEnv, tc.raw)

			got := eventsCheck()

			if got.state != tc.state {
				t.Errorf("state = %q, want %q", got.state, tc.state)
			}
			if line := got.value + " " + got.note; !strings.Contains(line, tc.says) {
				t.Errorf("%q should mention %q, got %q", tc.raw, tc.says, line)
			}
		})
	}
}

// Workers holding your token is the default, and the reason "a human approves
// and merges" is not a control. doctor says so without changing its exit code,
// and once a worker token is set it says what that buys and not the token.
func TestDoctorWarnsWhenWorkersShareYourToken(t *testing.T) {
	t.Setenv(execute.WorkerTokenEnv, "")
	if got := workerTokenCheck(); got.state != stateWarn || !strings.Contains(got.note, execute.WorkerTokenEnv) {
		t.Errorf("unset: %+v; want a warning naming %s", got, execute.WorkerTokenEnv)
	}

	const token = "github_pat_worker_only"
	t.Setenv(execute.WorkerTokenEnv, token)
	got := workerTokenCheck()
	if got.state != stateOK || !strings.Contains(got.note, "branch protection") {
		t.Errorf("set: %+v; want ok, naming branch protection as the control", got)
	}
	if strings.Contains(got.value+got.note, token) {
		t.Errorf("doctor printed the token: %+v", got)
	}
}

// Without an allowlist any write-access account's approval counts, a worker's
// own account among them, and doctor says so.
func TestDoctorWarnsWhenAnyWriterCanApprove(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		state state
		value string
	}{
		{"", stateWarn, "none"},
		{",", stateWarn, "none"},
		{"steig, @alice", stateOK, "steig,alice"},
	} {
		t.Setenv(approversEnv, tc.raw)
		if got := approversCheck(); got.state != tc.state || got.value != tc.value {
			t.Errorf("%q: %+v; want %s %s", tc.raw, got, tc.state, tc.value)
		}
	}
	unsetApprovers(t)
	if got := approversCheck(); got.state != stateWarn || got.value != "unset" {
		t.Errorf("unset: %+v; want warn unset", got)
	}
}

// Staffing never deletes the token file, so one left behind after the variable
// is unset is doctor's to name, with the way to remove it.
func TestDoctorNamesAWorkerTokenLeftOnDisk(t *testing.T) {
	t.Setenv(execute.WorkerTokenEnv, "")
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	if got := workerTokenCheck(); strings.Contains(got.note, "still on disk") {
		t.Errorf("no file, yet: %+v", got)
	}

	left := filepath.Join(dir, execute.WorkerSettingsFile)
	if err := os.WriteFile(left, []byte(`{"env":{"GH_TOKEN":"github_pat_withdrawn"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := workerTokenCheck()
	if got.state != stateWarn || !strings.Contains(got.note, "rm "+left) {
		t.Errorf("%+v; want a warning saying how to remove %s", got, left)
	}
	if strings.Contains(got.note, "github_pat_withdrawn") {
		t.Errorf("doctor printed the old token: %+v", got)
	}
}

// A repository that does not use pull requests is entitled to no `gh` at all,
// so a missing or unauthenticated one must never read as a failure. It must not
// read as fine either: it silently costs prune everything a merged pull request
// would have authorised.
func TestDoctorTreatsMissingGhAsAWarningNotAFailure(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	got := ghCheck()

	if got.state != stateWarn {
		t.Errorf("state = %q, want %q", got.state, stateWarn)
	}
	if !strings.Contains(got.note, "prune") {
		t.Errorf("the note must say what it costs, got %q", got.note)
	}
}

// Version drift is the failure that hides longest: an install pins a commit and
// stays on it, and herdr has no `plugin update` to move it, so nothing in
// ordinary operation ever mentions being four releases behind.
func TestDoctorReportsVersionDrift(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, origin *herdrtest.Repo, root string)
		state state
		says  string
	}{
		{"at origin", func(*testing.T, *herdrtest.Repo, string) {}, stateOK, ""},
		{"behind origin", func(t *testing.T, origin *herdrtest.Repo, root string) {
			publish(t, origin, "0.2.0")
		}, stateWarn, "worktender update"},
		// A developer's own checkout is on a branch and is theirs to move;
		// reporting work in progress as drift would be noise.
		{"a linked checkout", func(t *testing.T, origin *herdrtest.Repo, root string) {
			origin.GitIn(root, "checkout", "-b", "work")
		}, stateOK, "linked checkout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			origin, root := newInstall(t)
			tc.setup(t, origin, root)

			got := installCheck(nil, root)

			if got.state != tc.state {
				t.Errorf("state = %q, want %q (note %q)", got.state, tc.state, got.note)
			}
			if !strings.Contains(got.note, tc.says) {
				t.Errorf("note should mention %q, got %q", tc.says, got.note)
			}
			if !strings.Contains(got.value, "0.1.0") {
				t.Errorf("the installed version must be named, got %q", got.value)
			}
		})
	}
}

// The drift an update creates rather than closes. herdr records the installed
// commit once and never re-reads the checkout, so `plugin list` answers "what am
// I running" with a commit that is not on disk — and nothing else marks it.
func TestDoctorNamesAStaleHerdrRecord(t *testing.T) {
	origin, root := newInstall(t)
	installed := origin.GitIn(root, "rev-parse", "HEAD")

	server := herdrtest.NewServer(t)
	server.HandleResult("plugin.list", pluginListReply(root, strings.Repeat("a", 40)))

	got := installCheck(herdrapi.NewWithSocket(server.SocketPath), root)

	if got.state != stateWarn {
		t.Errorf("state = %q, want %q", got.state, stateWarn)
	}
	if !strings.Contains(got.note, "plugin list") {
		t.Errorf("the note must name the command that lies, got %q", got.note)
	}
	if strings.Contains(got.note, short(installed)) {
		t.Errorf("the note quotes the installed commit rather than the recorded one: %q", got.note)
	}
}

func TestDoctorSummarisesAgentsBusiestFirst(t *testing.T) {
	rows := []wt.Row{
		{AgentStatus: "idle"},
		{AgentStatus: "working"},
		{AgentStatus: "working"},
		{AgentStatus: ""},
	}

	if got, want := summariseAgents(rows), "2 working, 1 idle"; got != want {
		t.Errorf("summariseAgents = %q, want %q", got, want)
	}
}

// Busiest-first put the one status that needs a human last on the line whenever
// it was the rarest, which is nearly always: one agent stops and five keep
// working. And a count is the wrong answer for it — nobody can act on "1
// blocked" without going and finding which one.
func TestDoctorPutsBlockedFirstAndNamesIt(t *testing.T) {
	rows := []wt.Row{
		{Branch: "a", AgentStatus: "working"},
		{Branch: "b", AgentStatus: "working"},
		{Branch: "77-cross-repo", AgentStatus: "blocked"},
		{Branch: "c", AgentStatus: "idle"},
	}

	if got, want := summariseAgents(rows), "1 blocked (77-cross-repo), 2 working, 1 idle"; got != want {
		t.Errorf("summariseAgents = %q, want %q", got, want)
	}
}

// A detached checkout has no branch and still has an agent sitting in it, so
// the name falls back to the directory: "1 blocked ()" tells nobody anything.
func TestDoctorNamesABlockedWorktreeThatHasNoBranch(t *testing.T) {
	rows := []wt.Row{{Dir: "brave-valley-66f8", AgentStatus: "blocked"}}

	if got, want := summariseAgents(rows), "1 blocked (brave-valley-66f8)"; got != want {
		t.Errorf("summariseAgents = %q, want %q", got, want)
	}
}

// A worktree with no agent must not be counted as one with an unnamed status:
// an empty status is what a workspace herdr has nothing for carries.
func TestDoctorCountsNoAgentsRatherThanBlankOnes(t *testing.T) {
	rows := []wt.Row{{AgentStatus: ""}, {AgentStatus: ""}}

	if got, want := summariseAgents(rows), "no agents"; got != want {
		t.Errorf("summariseAgents = %q, want %q", got, want)
	}
}

func TestDoctorPluralisesWorktrees(t *testing.T) {
	for _, tc := range []struct {
		n    int
		want string
	}{{0, "0 worktrees"}, {1, "1 worktree"}, {3, "3 worktrees"}} {
		if got := plural(tc.n, "worktree"); got != tc.want {
			t.Errorf("plural(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// With no default_model, a worker with no model: label runs on whatever model
// the user's own sessions do, and doctor says so until one is set — or until
// every worker is sure to start with an agent file, which carries its model.
func TestDoctorWarnsWhenWorkersInheritYourModel(t *testing.T) {
	modelLine := func() check {
		_, c := rolesCheck()
		return c
	}

	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if got := modelLine(); got.state != stateWarn || !strings.Contains(got.note, "workers inherit your default model") {
		t.Errorf("no configuration: %+v; want a warning that workers inherit your model", got)
	}

	writeRoles(t, `{"roles": {}}`)
	if got := modelLine(); got.state != stateWarn {
		t.Errorf("no default_model: %+v; want a warning", got)
	}

	// No default_role: an issue no label matches runs with no role at all.
	writeRoles(t, uxRoles, "ux-designer")
	if got := modelLine(); got.state != stateWarn {
		t.Errorf("every role an agent, no default_role: %+v; want a warning", got)
	}

	writeRoles(t, `{"default_role": "ux", "roles": {"ux": {"agent": "ux-designer"}}}`, "ux-designer")
	if got := modelLine(); got.state != stateOK {
		t.Errorf("every worker gets an agent: %+v; want ok", got)
	}

	writeRoles(t, `{"default_model": "sonnet"}`)
	if got := modelLine(); got.state != stateOK || got.value != "sonnet" {
		t.Errorf("default_model sonnet: %+v; want ok naming sonnet", got)
	}
}
