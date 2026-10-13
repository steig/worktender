package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steig/worktender/internal/herdrtest"
)

// TestMain points XDG_CONFIG_HOME at an empty directory for the whole package,
// so no test ever reads the role configuration of whoever runs them. A test
// that wants one writes it with writeRoles.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "worktender-config")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CONFIG_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
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
	herdrtest.FakeGh(t, `echo '{"number":42,"title":"Fix the thing","labels":[{"name":"role:ux"}]}'`)

	var out strings.Builder
	if err := startCommand([]string{"42", "--model", "opus"}, &out); err != nil {
		t.Fatalf("start: %v", err)
	}

	want := []string{"--agent", "ux-designer", "--add-dir", filepath.Join(home, "kb"), "--model", "opus"}
	if got := agentStartArgs(t, server); !slices.Equal(got[len(got)-len(want):], want) {
		t.Errorf("agent args = %v; want them to end %v", got, want)
	}
	if !strings.Contains(out.String(), "role: ux (label role:ux) · agent: ux-designer") {
		t.Errorf("start did not say which role it chose:\n%s", out.String())
	}
}

func TestStartJSONCarriesTheLaunch(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	fakeStart(t, repo, "w9", "w9:p1", "working")
	writeRoles(t, uxRoles, "ux-designer")
	herdrtest.FakeGh(t, `echo '{"number":42,"title":"Fix the thing","labels":[]}'`)

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
	herdrtest.FakeGh(t, `echo '{"number":42,"title":"Fix the thing","labels":[{"name":"role:ux"}]}'`)

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
	herdrtest.FakeGh(t, `echo '{"number":42,"title":"Fix the thing","labels":[{"name":"role:ux"}]}'`)

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
	herdrtest.FakeGh(t, `echo '{"number":42,"title":"Fix the thing"}'`)

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
	herdrtest.FakeGh(t, `echo '{"number":42,"title":"Fix the thing"}'`)

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
		if c := rolesCheck(); c.state != stateOff || c.value != "none" {
			t.Errorf("check = %+v; want off/none", c)
		}
	})
	t.Run("invalid", func(t *testing.T) {
		writeRoles(t, `{"rolez": {}}`)
		if c := rolesCheck(); c.state != stateFail || !strings.Contains(c.note, "rolez") {
			t.Errorf("check = %+v; want fail naming the key", c)
		}
	})
	t.Run("healthy", func(t *testing.T) {
		home := writeRoles(t, uxRoles, "ux-designer")
		if err := os.MkdirAll(filepath.Join(home, "kb"), 0o755); err != nil {
			t.Fatal(err)
		}
		if c := rolesCheck(); c.state != stateOK || c.value != "1 role in "+filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "worktender", "config.json") {
			t.Errorf("check = %+v; want ok with one role", c)
		}
	})
	t.Run("missing agent and dir", func(t *testing.T) {
		writeRoles(t, uxRoles)
		c := rolesCheck()
		if c.state != stateWarn || !strings.Contains(c.note, "agent ux-designer") || !strings.Contains(c.note, "no directory") {
			t.Errorf("check = %+v; want a warning naming the agent and the directory", c)
		}
	})
}
