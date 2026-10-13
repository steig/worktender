package roles

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sample = `{
  "default_role": "engineer",
  "labels": [
    { "match": "role:ux", "role": "ux" },
    { "match": "area:warehouse*", "role": "warehousing" }
  ],
  "roles": {
    "engineer":    { "brief": "code" },
    "ux":          { "agent": "ux-designer", "add_dirs": ["~/kb/design", "docs/design"] },
    "warehousing": { "agent": "warehousing-analyst" }
  }
}`

func parse(t *testing.T, data string) *Config {
	t.Helper()
	c, err := Parse("config.json", []byte(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

// No file is the configuration everyone starts with, and it must resolve to
// nothing at all — the behaviour worktender had before roles existed.
func TestLoadWithoutAFileIsNoConfigurationAndNoRole(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	c, err := Load()
	if err != nil || c != nil {
		t.Fatalf("Load() = %v, %v; want nil, nil", c, err)
	}
	launch, err := c.Resolve("", []string{"role:ux"}, "/repo")
	if err != nil || launch != nil {
		t.Fatalf("Resolve on no configuration = %v, %v; want nil, nil", launch, err)
	}
	if args := launch.Args(); args != nil {
		t.Errorf("no role contributed args %v", args)
	}
}

func TestLoadReadsTheXDGPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "worktender"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "worktender", "config.json")
	if err := os.WriteFile(path, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Path != path || len(c.Roles) != 3 {
		t.Errorf("loaded %s with %d roles; want %s with 3", c.Path, len(c.Roles), path)
	}
}

func TestPathFallsBackToDotConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)

	if got, want := Path(), filepath.Join(home, ".config", "worktender", "config.json"); got != want {
		t.Errorf("Path() = %s; want %s", got, want)
	}
}

func TestParseRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ data, want string }{
		"an unknown key":               {`{"roles":{"a":{"add_dir":["x"]}}}`, `unknown field "add_dir"`},
		"an unknown brief":             {`{"roles":{"a":{"brief":"poetry"}}}`, `roles.a.brief: "poetry" is not a brief style`},
		"a default that is not a role": {`{"default_role":"nope","roles":{}}`, `default_role: "nope"`},
		"a label pointing nowhere":     {`{"labels":[{"match":"x","role":"nope"}],"roles":{}}`, `labels[0].role: "nope"`},
		"a label matching everything":  {`{"labels":[{"match":"*","role":"a"}],"roles":{"a":{}}}`, `matches every label`},
		"a glob that is not a suffix":  {`{"labels":[{"match":"a*b","role":"a"}],"roles":{"a":{}}}`, `single trailing *`},
		"an agent name that is a path": {`{"roles":{"a":{"agent":"../../etc/x"}}}`, `is not an agent name`},
		"an empty add_dir":             {`{"roles":{"a":{"add_dirs":[" "]}}}`, `add_dirs[0] is empty`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse("config.json", []byte(tc.data))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v; want it to mention %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "config.json: ") {
				t.Errorf("error does not name the file: %v", err)
			}
		})
	}
}

// The flag beats a label, a label beats the default, and the first rule in the
// file decides between two labels — the user's order, not GitHub's.
func TestResolveOrder(t *testing.T) {
	c := parse(t, sample)
	for name, tc := range map[string]struct {
		flag         string
		labels       []string
		role, reason string
	}{
		"flag over label":       {"warehousing", []string{"role:ux"}, "warehousing", "flag"},
		"label over default":    {"", []string{"bug", "role:ux"}, "ux", "label role:ux"},
		"prefix glob":           {"", []string{"area:warehouse-ops"}, "warehousing", "label area:warehouse-ops"},
		"rule order wins":       {"", []string{"area:warehouse", "role:ux"}, "ux", "label role:ux"},
		"default when no label": {"", []string{"bug"}, "engineer", "default"},
		"default with none":     {"", nil, "engineer", "default"},
	} {
		t.Run(name, func(t *testing.T) {
			l, err := c.Resolve(tc.flag, tc.labels, "/repo")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if l.Role != tc.role || l.Reason != tc.reason {
				t.Errorf("resolved %s (%s); want %s (%s)", l.Role, l.Reason, tc.role, tc.reason)
			}
		})
	}
}

func TestResolveWithoutADefaultIsNoRole(t *testing.T) {
	c := parse(t, `{"roles":{"ux":{"agent":"ux"}},"labels":[{"match":"role:ux","role":"ux"}]}`)
	l, err := c.Resolve("", []string{"bug"}, "/repo")
	if err != nil || l != nil {
		t.Fatalf("Resolve = %v, %v; want no role", l, err)
	}
}

func TestResolveRefusesAFlagNamingNoRole(t *testing.T) {
	c := parse(t, sample)
	_, err := c.Resolve("designer", nil, "/repo")
	if !errors.Is(err, ErrUnknownRole) || !strings.Contains(err.Error(), "engineer, ux, warehousing") {
		t.Fatalf("err = %v; want ErrUnknownRole listing the roles", err)
	}

	var none *Config
	if _, err := none.Resolve("ux", nil, "/repo"); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("--role with no configuration: err = %v; want ErrUnknownRole", err)
	}
}

// A label can choose a role and nothing else. The arguments a role produces are
// the agent and the directories; the model and the permission mode never come
// from configuration at all.
func TestArgsCarryAgentAndDirsOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := parse(t, sample)

	l, err := c.Resolve("", []string{"role:ux"}, "/repo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{
		"--agent", "ux-designer",
		"--add-dir", filepath.Join(home, "kb", "design"),
		"--add-dir", "/repo/docs/design",
	}
	if got := l.Args(); !reflect.DeepEqual(got, want) {
		t.Errorf("Args() = %v; want %v", got, want)
	}
	for _, a := range l.Args() {
		if a == "--model" || a == "--permission-mode" {
			t.Errorf("a role produced %s", a)
		}
	}
	if l.Brief != DefaultBrief {
		t.Errorf("a role naming no brief got %q; want %q", l.Brief, DefaultBrief)
	}
}

func TestCheckAgentLooksInTheRepositoryThenHome(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	l := &Launch{Role: "ux", Agent: "ux-designer"}

	err := l.CheckAgent(repo)
	if err == nil {
		t.Fatal("no agent file anywhere, and the check passed")
	}
	for _, p := range []string{
		filepath.Join(repo, ".claude", "agents"),
		filepath.Join(home, ".claude", "agents"),
	} {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error does not name %s: %v", p, err)
		}
	}

	for _, dir := range []string{repo, home} {
		t.Run(dir, func(t *testing.T) {
			path := filepath.Join(dir, ".claude", "agents", "ux-designer.md")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("---\nname: ux-designer\n---\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)
			if err := l.CheckAgent(repo); err != nil {
				t.Errorf("agent at %s not found: %v", path, err)
			}
			if l.AgentFile != path {
				t.Errorf("AgentFile = %s; want %s", l.AgentFile, path)
			}
			if want := map[bool]string{true: "repo", false: "user"}[dir == repo]; l.AgentSource != want {
				t.Errorf("AgentSource = %s; want %s", l.AgentSource, want)
			}
		})
	}
}

func TestLineSaysWhatWasChosenAndWhy(t *testing.T) {
	l := &Launch{Role: "ux", Reason: "label role:ux", Agent: "ux-designer", Brief: DefaultBrief}
	if got, want := l.Line(), "role: ux (label role:ux) · agent: ux-designer"; got != want {
		t.Errorf("Line() = %q; want %q", got, want)
	}
	var none *Launch
	if none.Line() != "" || none.JSON() != nil {
		t.Error("no role should print nothing and serialise to null")
	}
}

// A repository's agent file wins over the user's, as it does in claude itself.
// That is reported rather than refused, so the line has to say it.
func TestCheckAgentPrefersAndReportsTheRepository(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	for _, dir := range []string{repo, home} {
		path := filepath.Join(dir, ".claude", "agents", "ux.md")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	l := &Launch{Role: "ux", Reason: "flag", Agent: "ux", Brief: DefaultBrief}
	if err := l.CheckAgent(repo); err != nil {
		t.Fatal(err)
	}
	if l.AgentSource != "repo" || !strings.Contains(l.Line(), "agent: ux (repo)") {
		t.Errorf("source %s, line %q; want the repository's file, said so", l.AgentSource, l.Line())
	}
}

// A label is text whoever can edit the issue wrote, and the reason carries it
// to the terminal.
func TestLineEscapesALabel(t *testing.T) {
	l := &Launch{Role: "ux", Reason: "label x\x1b[2Jy\u202e", Brief: DefaultBrief}
	if line := l.Line(); strings.ContainsRune(line, 0x1b) || strings.ContainsRune(line, 0x202e) {
		t.Errorf("unsafe runes reached the line: %q", line)
	}
}

// claude knows an agent by the name in its frontmatter, wherever under the
// agents directory the file sits. A repository file under any name that
// declares the user's agent name is the one that loads, and must be reported.
func TestCheckAgentMatchesTheFrontmatterNameNotTheFileName(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	userFile := filepath.Join(home, ".claude", "agents", "ux.md")
	write(userFile, "---\nname: ux\n---\n")
	write(filepath.Join(repo, ".claude", "agents", "ux.md"), "---\nname: something-else\n---\n")

	l := &Launch{Role: "ux", Agent: "ux"}
	if err := l.CheckAgent(repo); err != nil || l.AgentFile != userFile {
		t.Fatalf("file %s, err %v; a repository ux.md declaring another name is not agent ux", l.AgentFile, err)
	}

	shadow := filepath.Join(repo, ".claude", "agents", "nested", "harmless.md")
	write(shadow, "---\ndescription: x\nname: \"ux\"\n---\n")
	l = &Launch{Role: "ux", Agent: "ux"}
	if err := l.CheckAgent(repo); err != nil || l.AgentFile != shadow || l.AgentSource != "repo" {
		t.Errorf("file %s (%s), err %v; want the nested repository file that declares the name", l.AgentFile, l.AgentSource, err)
	}
}

func TestFrontmatterNameForms(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		body, want string
		ok         bool
	}{
		"plain":           {"---\nname: ux\n---\n", "ux", true},
		"crlf and bom":    {"\ufeff---\r\nname: ux\r\n---\r\n", "ux", true},
		"double quoted":   {"---\nname: \"ux\" # c\n---\n", "ux", true},
		"single quoted":   {"---\nname: 'ux'\n---\n", "ux", true},
		"comment":         {"---\nname: ux # the designer\n---\n", "ux", true},
		"fence spaces":    {"---  \nname: ux\n---\n", "ux", true},
		"indented is not": {"---\nmeta:\n  name: other\n---\n", "", false},
		"after the fence": {"---\ndescription: x\n---\nname: ux\n", "", false},
		"no frontmatter":  {"name: ux\n", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-")+".md")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			got, ok := frontmatterName(path)
			if got != tc.want || ok != tc.ok {
				t.Errorf("frontmatterName = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// A repository decides what is in its agent directory, so the lookup stops
// after a bounded number of files rather than reading whatever is there.
func TestFindAgentStopsAtTheScanLimit(t *testing.T) {
	dir := t.TempDir()
	for i := range agentScanLimit + 5 {
		path := filepath.Join(dir, fmt.Sprintf("a%04d.md", i))
		if err := os.WriteFile(path, []byte("---\nname: filler\n---\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "z.md"), []byte("---\nname: late\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, complete := FindAgent(dir, "late"); got != "" || complete {
		t.Errorf("found %q, complete %v past the scan limit; want nothing, incomplete", got, complete)
	}
	if got, _ := FindAgent(dir, "filler"); got == "" {
		t.Error("an agent inside the limit was not found")
	}

	// A repository that buries its definition past the limit cannot make the
	// user's file of the same name look like the one in use.
	home := t.TempDir()
	t.Setenv("HOME", home)
	user := filepath.Join(home, ".claude", "agents", "late.md")
	if err := os.MkdirAll(filepath.Dir(user), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(user, []byte("---\nname: late\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, filepath.Join(repo, ".claude", "agents")); err != nil {
		t.Fatal(err)
	}
	l := &Launch{Role: "x", Agent: "late"}
	if err := l.CheckAgent(repo); err == nil {
		t.Errorf("a truncated repository scan fell through to %s", l.AgentFile)
	}
}
