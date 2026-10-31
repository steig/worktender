package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
	"github.com/steig/worktender/internal/roles"
)

// TestMain points XDG_CONFIG_HOME at an empty directory for the whole package,
// so no test ever reads the role configuration of whoever runs them. A test
// that wants one writes it with writeRoles.
//
// XDG_STATE_HOME goes somewhere disposable for the same reason, in the other
// direction: report, gate and start append to the dispatch ledger, and a test
// that never set a state directory of its own would otherwise append to the
// real one under ~/.local/state.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "worktender-config")
	if err != nil {
		panic(err)
	}
	state, err := os.MkdirTemp("", "worktender-state")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	os.Setenv("XDG_STATE_HOME", state)
	code := m.Run()
	os.RemoveAll(dir)
	os.RemoveAll(state)
	os.Exit(code)
}

// writeRoles installs a role configuration, and a home directory holding the
// named agent definitions, for the length of one test.
func writeRoles(t *testing.T, config string, agents ...string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "worktender"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "worktender", "config.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, a := range agents {
		path := filepath.Join(home, ".claude", "agents", a+".md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("---\nname: "+a+"\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

const uxRoles = `{
  "labels": [ { "match": "role:ux", "role": "ux" } ],
  "roles": { "ux": { "agent": "ux-designer", "add_dirs": ["~/kb"] } }
}`

func agentStartArgs(t *testing.T, server *herdrtest.Server) []string {
	t.Helper()
	for _, call := range server.Calls() {
		if call.Method != "agent.start" {
			continue
		}
		raw, _ := call.Params["args"].([]any)
		args := make([]string, len(raw))
		for i, a := range raw {
			args[i], _ = a.(string)
		}
		return args
	}
	t.Fatal("no agent was started")
	return nil
}

// A label picks the role, and the role becomes --agent and --add-dir ahead of
// whatever flags the caller passed. The choice is printed, so the coordinator
// can check it instead of trusting it.
func TestStartStartsTheAgentALabelChose(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	home := writeRoles(t, uxRoles, "ux-designer")
	herdrtest.FakeGh(t, herdrtest.CatScript(t, `{"number":42,"title":"Fix the thing","labels":[{"name":"role:ux"}]}`))

	var out strings.Builder
	if err := startCommand([]string{"42", "--model", "opus"}, &out); err != nil {
		t.Fatalf("start: %v", err)
	}

	want := []string{"--agent", "ux-designer", "--add-dir", filepath.Join(home, "kb"), "--model", "opus"}
	if got := agentStartArgs(t, server); len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Errorf("agent args = %v; want them to start %v", got, want)
	}
	if !strings.Contains(out.String(), "role: ux (label role:ux) · agent: ux-designer") {
		t.Errorf("start did not say which role it chose:\n%s", out.String())
	}
}

func TestStartJSONCarriesTheLaunch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	fakeStart(t, repo, "w9", "w9:p1", "working")
	writeRoles(t, uxRoles, "ux-designer")
	herdrtest.FakeGh(t, herdrtest.CatScript(t, `{"number":42,"title":"Fix the thing","labels":[]}`))

	var out strings.Builder
	if err := startCommand([]string{"42", "--role", "ux", "--json"}, &out); err != nil {
		t.Fatalf("start: %v", err)
	}
	var doc startJSON
	decodeOne(t, out.String(), &doc)
	if doc.Launch == nil || doc.Launch.Role != "ux" || doc.Launch.Reason != "flag" ||
		doc.Launch.Agent == nil || *doc.Launch.Agent != "ux-designer" {
		t.Errorf("launch = %+v; want role ux by flag with agent ux-designer", doc.Launch)
	}
}

// Without a configuration nothing changes: no role, no line, a null launch.
func TestStartWithoutRolesIsUnchanged(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	fakeStart(t, repo, "w9", "w9:p1", "working")
	herdrtest.FakeGh(t, herdrtest.CatScript(t, `{"number":42,"title":"Fix the thing","labels":[{"name":"role:ux"}]}`))

	var out strings.Builder
	if err := startCommand([]string{"42", "--json"}, &out); err != nil {
		t.Fatalf("start: %v", err)
	}
	var doc startJSON
	decodeOne(t, out.String(), &doc)
	if doc.Launch != nil {
		t.Errorf("launch = %+v with no configuration; want null", doc.Launch)
	}
	if !strings.Contains(out.String(), `"launch": null`) && !strings.Contains(out.String(), `"launch":null`) {
		t.Errorf("launch should be present and null:\n%s", out.String())
	}
}

// An agent nobody defined would fail inside the pane, after the worktree is
// made. Refused first, nothing is created.
func TestStartRefusesAMissingAgentBeforeCreatingAnything(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	writeRoles(t, uxRoles) // no agent files
	herdrtest.FakeGh(t, herdrtest.CatScript(t, `{"number":42,"title":"Fix the thing","labels":[{"name":"role:ux"}]}`))

	err := startCommand([]string{"42"}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), `agent "ux-designer"`) {
		t.Fatalf("err = %v; want a refusal naming the agent", err)
	}
	if exitCode(err) != exitUsage {
		t.Errorf("exit code %d; want %d", exitCode(err), exitUsage)
	}
	if called(t, server, "worktree.create") {
		t.Error("a worktree was created for a role whose agent does not exist")
	}
}

func TestStartRefusesAnUnknownRole(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	writeRoles(t, uxRoles, "ux-designer")
	herdrtest.FakeGh(t, herdrtest.CatScript(t, `{"number":42,"title":"Fix the thing"}`))

	err := startCommand([]string{"42", "--role", "designer"}, &strings.Builder{})
	if err == nil || exitCode(err) != exitUsage || !strings.Contains(err.Error(), "ux") {
		t.Fatalf("err = %v; want a usage error listing the roles there are", err)
	}
	if called(t, server, "worktree.create") {
		t.Error("a worktree was created for a role that does not exist")
	}
}

func TestStartRefusesAConfigurationThatDoesNotParse(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	writeRoles(t, `{"roles": {"ux": {"agnet": "x"}}}`)
	herdrtest.FakeGh(t, herdrtest.CatScript(t, `{"number":42,"title":"Fix the thing"}`))

	err := startCommand([]string{"42"}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "agnet") || !strings.Contains(err.Error(), "config.json") {
		t.Fatalf("err = %v; want one naming the file and the bad key", err)
	}
	if called(t, server, "worktree.create") {
		t.Error("a worktree was created under a configuration that does not parse")
	}
}

func TestDispatchStartsTheRoleItWasGiven(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	server.HandleResult("pane.get", map[string]any{"type": "pane_info", "pane": map[string]any{
		"pane_id": "w9:p1", "workspace_id": "w9", "tab_id": "t1",
		"terminal_id": "term_1", "agent_status": "idle", "focused": false, "revision": 1,
	}})
	writeRoles(t, uxRoles, "ux-designer")

	var out strings.Builder
	if err := dispatchCommand([]string{"--pane", "w9:p1", "--name", "w1", "--role", "ux", "--json"}, &out); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if args := agentStartArgs(t, server); !slices.Contains(args, "ux-designer") {
		t.Errorf("agent args = %v; want --agent ux-designer", args)
	}
	var doc dispatchJSON
	decodeOne(t, out.String(), &doc)
	if doc.Launch == nil || doc.Launch.Role != "ux" {
		t.Errorf("launch = %+v; want role ux", doc.Launch)
	}
}

func TestDispatchRefusesAnUnknownRole(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	writeRoles(t, uxRoles, "ux-designer")

	err := dispatchCommand([]string{"--pane", "w9:p1", "--name", "w1", "--role", "nope"}, &strings.Builder{})
	var coded *codedError
	if !errors.As(err, &coded) || exitCode(err) != exitUsage {
		t.Fatalf("err = %v; want a usage error", err)
	}
	if called(t, server, "agent.start") {
		t.Error("an agent was started for a role that does not exist")
	}
}

func TestDoctorRolesCheck(t *testing.T) {
	t.Run("no configuration", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		if c, _ := rolesCheck(); c.state != stateOff || c.value != "none" {
			t.Errorf("check = %+v; want off/none", c)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		writeRoles(t, `{"rolez": {}}`)
		if c, _ := rolesCheck(); c.state != stateFail || !strings.Contains(c.note, "rolez") {
			t.Errorf("check = %+v; want fail naming the key", c)
		}
	})
	t.Run("healthy", func(t *testing.T) {
		home := writeRoles(t, uxRoles, "ux-designer")
		if err := os.MkdirAll(filepath.Join(home, "kb"), 0o755); err != nil {
			t.Fatal(err)
		}
		if c, _ := rolesCheck(); c.state != stateOK || c.value != "1 role in "+filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "worktender", "config.json") {
			t.Errorf("check = %+v; want ok with one role", c)
		}
	})
	t.Run("missing agent and dir", func(t *testing.T) {
		writeRoles(t, uxRoles)
		c, _ := rolesCheck()
		if c.state != stateWarn || !strings.Contains(c.note, "agent ux-designer") || !strings.Contains(c.note, "no directory") {
			t.Errorf("check = %+v; want a warning naming the agent and the directory", c)
		}
	})
}

// Every style a configuration may name has a sentence, and "code" is the brief
// start has always typed, byte for byte.
func TestEveryBriefStyleHasItsSentence(t *testing.T) {
	for style := range roles.Briefs {
		if briefStyles[style] == "" {
			t.Errorf("brief style %q is accepted by the configuration and has no sentence", style)
		}
	}
	if briefFor(42, "42-x", "code", false) != brief(42, "42-x") {
		t.Error("the code style must be the brief start has always sent")
	}
}

// What the gate and the coordinator rely on is the same in every style.
func TestEveryBriefStyleKeepsTheProtocol(t *testing.T) {
	for style := range briefStyles {
		for _, dirs := range []bool{false, true} {
			line := briefFor(42, "42-fix-the-thing", style, dirs)
			for _, want := range []string{"gh issue view 42", "UNTRUSTED DATA", "planned", "--pr", "blocked", "42-fix-the-thing", "pull request"} {
				if !strings.Contains(line, want) {
					t.Errorf("%s brief (dirs %v) lost %q:\n%s", style, dirs, want, line)
				}
			}
			if strings.ContainsAny(line, "\n\r") {
				t.Errorf("%s brief is not one line", style)
			}
		}
	}
}

// The longest brief any role can produce still arrives in one pane read.
func TestEveryBriefStyleFitsInOnePaneRead(t *testing.T) {
	branch := issueBranch(issue{Number: 999999, Title: strings.Repeat("word ", 40)})
	for style := range briefStyles {
		line := briefFor(999999, branch, style, true)
		line = strings.ReplaceAll(line, selfPath(), installedSelfPath)
		if len(line) > paneReadChunk {
			t.Errorf("the %s brief is %d bytes and a pane read carries %d", style, len(line), paneReadChunk)
		}
	}
}

func TestStartBriefsInTheRolesStyle(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	home := writeRoles(t, `{"roles": {"ux": {"agent": "ux-designer", "brief": "design", "add_dirs": ["~/kb"]}}}`, "ux-designer")
	if err := os.MkdirAll(filepath.Join(home, "kb"), 0o755); err != nil {
		t.Fatal(err)
	}
	herdrtest.FakeGh(t, herdrtest.CatScript(t, `{"number":42,"title":"Fix the thing"}`))

	var out strings.Builder
	if err := startCommand([]string{"42", "--role", "ux"}, &out); err != nil {
		t.Fatalf("start: %v", err)
	}
	var text string
	for _, call := range server.Calls() {
		if call.Method == "pane.send_text" {
			text, _ = call.Params["text"].(string)
		}
	}
	if !strings.Contains(text, briefStyles["design"]) || !strings.Contains(text, briefDirs) {
		t.Errorf("start did not brief in the design style with the directories clause:\n%s", text)
	}
	if !strings.Contains(out.String(), "brief: design") {
		t.Errorf("the role line should say the brief style:\n%s", out.String())
	}
}

// An interval counts up from zero, and a misspelt key in it is refused like
// any other: a "turn" that silently did nothing would be a worker never
// refocused.
func TestRolesRefocusIsValidated(t *testing.T) {
	for _, bad := range []string{
		`{"roles": {"a": {"refocus": {"turns": -1}}}}`,
		`{"roles": {"a": {"refocus": {"minutes": -5}}}}`,
		`{"roles": {"a": {"refocus": {"turn": 5}}}}`,
	} {
		if _, err := roles.Parse("config.json", []byte(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	c, err := roles.Parse("config.json", []byte(`{"roles": {"a": {"refocus": {"turns": 20, "minutes": 30}}, "b": {}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.AnyRefocus() || c.Roles["b"].Refocus.On() || *c.Roles["a"].Refocus != (roles.Refocus{Turns: 20, Minutes: 30}) {
		t.Errorf("parsed %+v", c.Roles)
	}
}
